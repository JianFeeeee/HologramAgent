package toolexec

// guard.go —— **声明式边界判定**（路径 / 网络）。
//
// ## 为什么按"声明的参数名"判
//
// SDK 的 `ToolDef.PathArgs` / `NetworkArgs` 是工具作者显式写下的：
//
//	PathArgs:    []string{"path"}        ← 这个参数是文件路径
//	NetworkArgs: []string{"url"}         ← 这个参数是网络地址
//
// 内核照办；**不声明 ⇒ 不判**（存量插件零改动）。
//
// ★ 为什么不能"扫描所有像 URL/路径的字符串"：`command` 里可能内嵌 curl、
//   `text` 里可能贴了个链接 —— 按模式扫描会把"描述一个 URL"误判成
//   "访问这个 URL"，直接拒掉正常对话（future-plan §1.3 已明确否决）。
//
// ## 判定归安全模块
//
// 本文件只做两件事：**取声明**、**按声明问安全模块**。
// 任何"自己判路径/网络"的写法都是绕过闸门。

import (
	"fmt"
	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
)

// guardDeclaredArgs 按工具声明判路径与网络。
func guardDeclaredArgs(host hostFace, tc agentPkg.ToolCall) (toolOutcome, bool) {
	def := toolDefOf(host, tc.Name)

	// 网络闸：**先判**（它是更粗的判定，不依赖具体地址，理由更直接）。
	if len(def.NetworkArgs) > 0 {
		for _, argName := range def.NetworkArgs {
			if v, ok := tc.Arguments[argName]; ok {
				if s, _ := v.(string); s != "" {
					if d := host.CheckNetwork(); !d.Allowed {
						te := kit.NewToolError(kit.ErrReasonDenied, "",
							fmt.Sprintf("Tool %s is not permitted: %s", tc.Name, d.Reason),
							"Network access is disabled by policy on this machine. Do not retry; report back to the user.")
						return toolOutcome{Text: te.Error(), Raw: te, Denied: true}, true
					}
					break // 一处命中即整条调用被禁
				}
			}
		}
	}

	if len(def.PathArgs) == 0 {
		return toolOutcome{}, false
	}

	direction := pathDirection(tc.Name)
	for _, argName := range def.PathArgs {
		raw, ok := tc.Arguments[argName]
		if !ok {
			continue
		}
		path, ok := raw.(string)
		if !ok || path == "" {
			continue
		}
		if d := host.CheckPath(direction, path); !d.Allowed {
			te := kit.NewToolError(kit.ErrReasonDenied, "",
				fmt.Sprintf("Tool %s is not permitted: %s", tc.Name, d.Reason),
				"The path is outside the sandbox. Use a path inside the allowed roots, or report back to the user.")
			return toolOutcome{Text: te.Error(), Raw: te, Denied: true}, true
		}
		// 写方向：判定通过后立刻留档 —— 必须在真正写之前。
		//
		// 留档是**数据面动作**、与判定同层（安全模块的 `Archive`），基座把它
		// 暴露成 `ToolHost.ArchiveBeforeWrite`。漏掉它的后果实测过：受保护路径
		// 被写掉之后 guard 没有基线可还原。
		if direction == "write" {
			if _, err := host.ArchiveBeforeWrite(path); err != nil {
				host.Logf("[security] 写前留档失败 %s: %v（继续执行）", path, err)
			}
		}
	}
	return toolOutcome{}, false
}

// pathDirection 判这次调用是读还是写（决定按 read_roots 还是 write_roots 判）。
//
// ★ 判据是**工具名前缀**，不是参数内容：`fs_write` / `files_write` /
//
//	`cmd_*` 里的写命令无从解析（未来由声明的 `WriteArgs` 取代 —— 见 todo）。
//	当前规则与改造前逐字一致，避免新增误拒。
func pathDirection(toolName string) string {
	switch {
	case strings.HasPrefix(toolName, "fs_write"),
		strings.HasPrefix(toolName, "files_write"),
		strings.HasPrefix(toolName, "write_"),
		strings.HasPrefix(toolName, "edit_"),
		strings.HasPrefix(toolName, "append_"):
		return "write"
	}
	return "read"
}

// resolveToolTimeout 取某工具应使用的超时。
//
// 顺序：ToolDef.Timeout（声明）→ defaultToolTimeout，且夹到 maxToolTimeout。
// 声明值非法（解析失败 / ≤0）时**回退默认并记日志**：写错单位（"130" 少了 s）
// 是最容易犯的错，默默变成 60s 会让人以为"声明没生效"却查不出原因。
func resolveToolTimeout(host hostFace, toolName string) time.Duration {
	def := toolDefOf(host, toolName)
	if def.Timeout == "" {
		return defaultToolTimeout
	}
	d, err := time.ParseDuration(def.Timeout)
	if err != nil || d <= 0 {
		host.Logf("tool %s 的 Timeout 声明 %q 非法，回退默认 %v", toolName, def.Timeout, defaultToolTimeout)
		return defaultToolTimeout
	}
	if d > maxToolTimeout {
		host.Logf("tool %s 的 Timeout 声明 %q 超过硬上限，夹到 %v", toolName, def.Timeout, maxToolTimeout)
		return maxToolTimeout
	}
	return d
}

// fmtDuration 把时长渲染成给人/模型看的短形式。
func fmtDuration(d time.Duration) string {
	if d >= time.Minute {
		if d%time.Minute == 0 {
			return fmt.Sprintf("%d分钟", int(d/time.Minute))
		}
		return fmt.Sprintf("%.1f分钟", float64(d)/float64(time.Minute))
	}
	return fmt.Sprintf("%d秒", int(d/time.Second))
}
