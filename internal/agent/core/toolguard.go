package core

// 工具执行前的**路径闸**：按工具声明的 PathArgs 做沙箱判定与 L0 写前留档。
//
// ## 为什么这一层必须存在（2026-11-16）
//
// 安全模块有三段判定，此前只有 `CheckTool`（按工具名）接通了生产：
//
//	CheckTool(tool)                    ← 唯一接线点（toolcall.go 的 securityDecisionFor）
//	CheckPath(direction, path)         ← 零生产调用
//	CheckNetwork()                     ← 零生产调用
//
// 后果是 `core.security.sandbox.enabled=true` 打开也**不会拦任何东西**。
// 唯一真实生效的路径约束在 `files` 插件自己实现的 `pathWithinSandbox` ——
// 每个插件各写一套，而外部插件（独立子进程）根本还不出来。
//
// ## 契约：工具声明 PathArgs，内核照办
//
// 不在内核里"猜哪个参数是路径"：`command` / `text` 里的路径要按命令语法
// 解析才知道，瞎猜会误拒（future-plan §1.3 已明确否决）。所以由插件声明：
//
//	sdk.ToolDef{PathArgs: []string{"path"}, ...}
//
// 不声明 ⇒ 不判（存量插件不改一行，行为与升级前逐字一致）。
//
// ## 判定顺序（两个方向分开）
//
//	"read"  方向：按 sandbox.read_roots 判
//	"write" 方向：按 sandbox.write_roots 判，**通过后立刻留档**
//
// 不区分读取与写入会让「只允许读 /etc，不允许写」这种常见配置无法表达。
//
// ## 留档为什么在这里
//
// 写前留档必须发生在写**之前**（覆盖后就来不及了），而这里正是执行前的
// 唯一闸口。留档目标是受保护路径（默认 /etc/），存档供 guard 的 L1 还原读取。

import (
	"fmt"
	"log"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// guardToolPaths 检查本次工具调用声明的路径参数。
//
// 返回 (被拒结果, 是否被拒)。未被拒时返回零值，调用方继续执行。
//
// 拒绝必须**可执行**（与 CheckTool 同一纪律）：把具体路径与允许根回给模型，
// 它才能改路径或换工具，而不是反复重试同一条被拒的调用。
func (a *Agent) guardToolPaths(tc agentAPI.ToolCall) (toolOutcome, bool) {
	if a == nil || a.stageHost == nil {
		return toolOutcome{}, false
	}
	def := a.stageHost.ToolDef(tc.Name)
	if def == nil {
		return toolOutcome{}, false
	}

	// ★ 网络闸（与路径闸对称）：声明的 NetworkArgs 非空 ⇒ 判 allow_network。
	//
	// 放在路径之前：网络被禁是**更粗**的判定（它不依赖具体地址），
	// 先拦下来能给出更直接的理由（"这台机器不许访问网络"），
	// 而不是让模型去猜"是这个地址不行还是网络整体不行"。
	if len(def.NetworkArgs) > 0 {
		for _, argName := range def.NetworkArgs {
			if v, ok := tc.Arguments[argName]; ok {
				if s, _ := v.(string); s != "" {
					if d := a.checkNetworkAllowed(); !d.Allowed {
						log.Printf("[security] tool %s denied by network policy (%s)", tc.Name, argName)
						te := newToolError(ErrReasonDenied, "",
							fmt.Sprintf("Tool %s is not permitted: %s", tc.Name, d.Reason),
							"Network access is disabled by policy on this machine. Do not retry; report back to the user.")
						return toolOutcome{Text: te.Error(), Raw: te}, true
					}
					break // 一处命中即整条调用被禁，不必逐参数重复判
				}
			}
		}
	}

	if len(def.PathArgs) == 0 {
		return toolOutcome{}, false
	}

	for _, argName := range def.PathArgs {
		raw, ok := tc.Arguments[argName]
		if !ok {
			continue
		}
		path, ok := raw.(string)
		if !ok || path == "" {
			continue
		}

		direction := pathDirection(tc.Name)
		if d := a.checkSandboxPath(direction, path); !d.Allowed {
			log.Printf("[security] tool %s denied by sandbox (%s): %s", tc.Name, argName, d.Reason)
			te := newToolError(ErrReasonDenied, "",
				fmt.Sprintf("Tool %s is not permitted: %s", tc.Name, d.Reason),
				"The path is outside the sandbox. Use a path inside the allowed roots, or report back to the user.")
			return toolOutcome{Text: te.Error(), Raw: te}, true
		}
		// 写方向：判定通过后立刻留档 —— 必须在真正写之前。
		if direction == "write" {
			a.archiveProtectedPath(path)
		}
	}
	return toolOutcome{}, false
}

// pathDirection 由工具名推断本次是读还是写。
//
// ★ 为什么按工具名而不是再加一个声明字段：
// 工具名是**已经存在**的事实（files_read / files_write / files_edit），
// 再加一个 `PathDirections` 字段会让插件维护两份可能不一致的声明。
// 判定规则保守：**只有明确含写语义的才算写**，其余按只读判
// （宁可按只读放过读操作，也不要把读误判成写而多留一份档）。
func pathDirection(toolName string) string {
	switch {
	case strings.Contains(toolName, "write"),
		strings.Contains(toolName, "edit"),
		strings.Contains(toolName, "append"),
		strings.Contains(toolName, "create"),
		strings.Contains(toolName, "delete"),
		strings.Contains(toolName, "remove"),
		strings.Contains(toolName, "move"),
		strings.Contains(toolName, "rename"),
		strings.Contains(toolName, "mkdir"):
		return "write"
	}
	return "read"
}

// checkNetworkAllowed 判本机是否允许网络访问。
//
// 未装配安全运行时（独立工具 / 单测）时一律放行 —— 与 checkSandboxPath 同语义。
func (a *Agent) checkNetworkAllowed() security.Decision {
	if a.security == nil {
		return security.Decision{}
	}
	return a.security.CheckNetwork()
}

// checkSandboxPath 判路径是否在沙箱允许根内。
//
// 未装配安全运行时（独立工具 / 单测）时一律放行 —— 与 CheckTool 同语义。
func (a *Agent) checkSandboxPath(direction, path string) security.Decision {
	if a.security == nil {
		return security.Decision{}
	}
	return a.security.CheckPath(direction, path)
}

// archiveProtectedPath 对受保护路径做 L0 写前留档。
//
// 留档失败**不阻断执行**：留档是"事后可还原"的保险，不是执行的前置条件。
// 拦下写入会让"磁盘满 / 权限不足"变成"整个写入功能不可用"，那更糟 ——
// 所以只响亮记日志（guard 事后会发现自己没有基线可还原）。
func (a *Agent) archiveProtectedPath(path string) {
	if a.dataDir == "" {
		return
	}
	if archived, err := security.ArchiveBeforeWrite(a.dataDir, path); err != nil {
		log.Printf("[security] write-ahead archive %s failed (write proceeds): %v", path, err)
	} else if archived {
		log.Printf("[security] write-ahead archived %s (baseline for L1 restore)", path)
	}
}
