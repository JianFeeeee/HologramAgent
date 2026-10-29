package security

// 把**部署方配置**（`core.security.*`）装配成内核全局策略基线。
//
// ## 为什么全局基线需要有配置入口（用户 2026-10-29 裁定：选项 B）
//
// 策略有两层，职责不同：
//
//	① 全局基线 security.Policy   ← 本文件负责：部署方兜底（"这台机器不许跑 cmd_*"）
//	② 档位 Profile / 作用域 Scope ← 插件声明：单次请求的收窄（"这个 QQ 访客不行"）
//
// ②是**声明式**且**只能收窄**（见 Runtime.CheckTool：全局黑名单先判，
// 档位绕不过去）。但②有它的结构性盲区：**部署方无法阻止一个恶意/写坏的
// 插件**——插件自己声明档位，等于被约束者决定约束。所以必须有一层
// 不受插件影响的基线，且它要能在不重编译的前提下改 —— 那就是①。
//
// ## 为什么不放在 internal/security 读配置
//
// `internal/security` 不 import `internal/config`（否则配置包与安全包
// 互相依赖，且 security 会被迫感知 SQLite）。装配放在本桥接层，
// security 保持纯策略/算法包。
//
// ## 为什么是启动期一次快照（不热重载）
//
// `Policy` 在**每次工具调用**的热路径上被查；查 SQLite 不可接受。
// 策略变更本就低频，且"安全策略在运行中被悄悄改变"本身是可疑行为。
// 因此：启动期读一次，改配置需重启（与 core.agent.offload_enabled 同纪律）。

import (
	"fmt"
	"log"
	"strings"

	sec "github.com/JianFeeeee/HologramAgent/internal/security"
)

// ConfigReader 是本桥接层需要的最小配置面（便于测试注入假实现）。
type ConfigReader interface {
	GetString(key, defaultVal string) string
	GetBool(key string, defaultVal bool) bool
}

// PolicyFromConfig 从配置读出一份全局策略快照。
//
// 缺省语义（重要）：所有项留空/false 时返回**零值 Policy**，
// 即"什么都不限制"，与加配置之前的存量行为逐字一致。
// 这条是升级安全的底线：老安装升级后行为不得改变。
func PolicyFromConfig(cfg ConfigReader) sec.Policy {
	if cfg == nil {
		return sec.DefaultPolicy()
	}
	p := sec.Policy{
		ToolDeny:  splitToolList(cfg.GetString("core.security.tool_deny", "")),
		ToolAllow: splitToolList(cfg.GetString("core.security.tool_allow", "")),
		Sandbox: sec.SandboxPolicy{
			Enabled:      cfg.GetBool("core.security.sandbox.enabled", false),
			ReadRoots:    splitPathList(cfg.GetString("core.security.sandbox.read_roots", "")),
			WriteRoots:   splitPathList(cfg.GetString("core.security.sandbox.write_roots", "")),
			AllowNetwork: cfg.GetBool("core.security.sandbox.allow_network", true),
		},
	}
	// 自相矛盾的配置（同名工具同时在黑白名单）不静默择一：
	// CheckTool 的语义是 deny 优先，但那种配置通常意味着写错了，
	// 而"写错的安全策略悄悄偏向一边"比启动失败更危险。这里只响亮记录。
	if both := intersect(p.ToolDeny, p.ToolAllow); len(both) > 0 {
		log.Printf("[security] WARNING: tool(s) %v appear in BOTH tool_deny and tool_allow; "+
			"deny wins (denylist is evaluated first), but this configuration is likely a mistake", both)
	}
	return p
}

// ApplyPolicyFromConfig 读取配置并把策略装进运行时。
//
// 返回装好的策略，便于调用方记录/断言。rt 为 nil 时安全跳过
// （未装配安全模块的独立工具/测试）。
func ApplyPolicyFromConfig(rt *sec.Runtime, cfg ConfigReader) sec.Policy {
	p := PolicyFromConfig(cfg)
	if rt != nil {
		rt.SetPolicy(p)
	}
	if !p.Sandbox.Enabled && len(p.ToolDeny) == 0 && len(p.ToolAllow) == 0 {
		// 零值策略：不打印噪音日志（存量安装的常态）。
		return p
	}
	log.Printf("[security] global policy applied: deny=%v allow=%v sandbox=%v (read=%d roots, write=%d roots, network=%v)",
		p.ToolDeny, p.ToolAllow, p.Sandbox.Enabled,
		len(p.Sandbox.ReadRoots), len(p.Sandbox.WriteRoots), p.Sandbox.AllowNetwork)
	return p
}

// splitToolList 解析逗号分隔的工具名单。
//
// 空串 ⇒ nil（而不是 [""]）—— 后者在 CheckTool 里会被当成"有一个空模式"，
// 虽然 matchTool 对空模式返回 false 不会误伤，但 nil 语义更干净：
// "没配置"与"配置了一个空项"应当可区分。
func splitToolList(s string) []string {
	return splitNonEmpty(s)
}

// splitPathList 解析逗号分隔的路径前缀。
//
// 去尾部分隔符：`/tmp/` 与 `/tmp` 应等价，否则 `filepath.HasPrefix` 式的
// 前缀匹配会把 `/tmpfoo` 误判进 `/tmp`。规范化交给 CheckPath（它会 Clean）。
func splitPathList(s string) []string {
	out := splitNonEmpty(s)
	for i, v := range out {
		out[i] = strings.TrimRight(v, "/")
		if out[i] == "" {
			// "/" 被 TrimRight 成空 —— 根目录是个合法的意图，还原回去。
			out[i] = "/"
		}
	}
	return out
}

func splitNonEmpty(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// intersect 返回两个名单的交集（用于诊断自相矛盾的配置）。
func intersect(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	set := make(map[string]bool, len(b))
	for _, v := range b {
		set[v] = true
	}
	var out []string
	for _, v := range a {
		if set[v] {
			out = append(out, v)
		}
	}
	return out
}

// String 便于日志里一眼看出策略是否为空（诊断用）。
func DescribePolicy(p sec.Policy) string {
	return fmt.Sprintf("deny=%d allow=%d sandbox=%v", len(p.ToolDeny), len(p.ToolAllow), p.Sandbox.Enabled)
}
