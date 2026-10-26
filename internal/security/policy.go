package security

// 安全策略：**工具黑白名单 + 沙箱约束**。
//
// ## 为什么它属于安全模块（用户裁定 2026-10-26）
//
// > 沙箱检查和工具的黑白名单也应该是安全模块负责，并通过 SDK 提供接口。
//
// 归属理由：允许/禁止某类能力是**安全决定**，不是执行细节。放在
// agent/core 会让每个调用点各自维护一份清单（本仓已有实测教训：
// healthcheck 自带一份只读名单，与工具真实能力靠人肉对齐）；
// 放在插件里则等于“被约束者自己决定约束”。
//
// ## 与执行闸的分工
//
//	安全模块（本文件）：能不能做 —— 策略、沙箱、名单
//	agent/core       ：怎么做 —— 参数校验、授权范围、超时、panic 恢复
//
// 两者都必须过，不能互相顶替：策略放行不等于参数合法，
// 参数合法也不等于策略允许。

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Policy 是安全策略快照（启动时从配置读一次，重载时整体替换）。
//
// ★ 为什么用快照而不是每次查配置：策略会在热路径（每次工具调用）被查，
// 查配置要走 SQLite；而策略变更本身是低频的、本就该走重载路径。
type Policy struct {
	// ToolDeny 是被**明确禁止**的工具名（支持 `*` 后缀前缀匹配，如 "cmd_*"）。
	//
	// 名单语义（刻意如此，各有用途）：
	//   · 在黑名单里 ⇒ 拒绝（先于白名单判定：禁止永远胜过允许）
	//   · 白名单非空且不在其中 ⇒ 拒绝（白名单是"只有这些"）
	//   · 白名单为空 ⇒ 不限制（默认；存量行为不变）
	ToolDeny  []string
	ToolAllow []string

	// Sandbox 是执行侧约束。
	Sandbox SandboxPolicy
}

// SandboxPolicy 是工具执行的沙箱约束。
//
// ★ 只放**可判定**的事实（路径前缀、是否允许网络、允许的二进制）；
// 不做"猜测式"安全检查（如"这段 shell 看起来危险"）—— 那类判定既误伤
// 又给不出可执行的修复动作。
type SandboxPolicy struct {
	// Enabled 为 false 表示不施加额外约束（默认；行为与之前一致）。
	Enabled bool
	// ReadRoots / WriteRoots 是允许读/写的前缀（空 = 不限制该方向）。
	ReadRoots  []string
	WriteRoots []string
	// AllowNetwork 为 false 时拒绝带 URL / 网络意图的调用。
	AllowNetwork bool
}

// Decision 是一次策略判定的结果。
//
// ★ 带 Reason：拒绝必须能回答"为什么"，否则模型只会反复重试同一调用。
type Decision struct {
	Allowed bool
	Reason  string
}

// Allow 表示放行。
func allowed() Decision { return Decision{Allowed: true} }

// deny 构造一次拒绝（reason 会原样回到模型，必须可执行）。
func deny(format string, args ...interface{}) Decision {
	return Decision{Allowed: false, Reason: fmt.Sprintf(format, args...)}
}

// CheckTool 判定某工具是否被策略允许。
func (p Policy) CheckTool(name string) Decision {
	if name == "" {
		return deny("empty tool name")
	}
	// ① 黑名单优先：禁止永远胜过允许。
	// 为什么先判黑名单：否则"白名单里含 cmd_run"和"黑名单里含 cmd_run"
	// 同时存在时会放行 —— 那违背"安全策略里禁止项总是赢"的直觉。
	for _, pat := range p.ToolDeny {
		if matchTool(pat, name) {
			return deny("tool %q is blocked by security policy (denylist entry %q)", name, pat)
		}
	}
	// ② 白名单非空则是"只有这些"。
	if len(p.ToolAllow) > 0 {
		for _, pat := range p.ToolAllow {
			if matchTool(pat, name) {
				return allowed()
			}
		}
		return deny("tool %q is not in the security allowlist", name)
	}
	return allowed()
}

// matchTool 做工具名匹配：精确，或 `前缀*` 形式。
//
// ★ 只支持尾部 `*`：中间的 `*` 会诱导写出难以推理的规则
// （"cmd_*_run" 这类），而安全规则必须一眼可判。
func matchTool(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == name
}

// CheckPath 判定某路径在给定方向（read/write）是否被沙箱允许。
func (p Policy) CheckPath(direction, path string) Decision {
	sb := p.Sandbox
	if !sb.Enabled || path == "" {
		return allowed()
	}
	roots := sb.ReadRoots
	if direction == "write" {
		roots = sb.WriteRoots
	}
	if len(roots) == 0 {
		return allowed() // 该方向不限制
	}
	// 规范化：避免 `..` 绕过前缀匹配。
	clean := filepath.Clean(path)
	for _, root := range roots {
		r := filepath.Clean(root)
		if clean == r || strings.HasPrefix(clean, r+string(filepath.Separator)) {
			return allowed()
		}
	}
	return deny("%s path %q is outside the sandbox (%s roots: %v)", direction, path, direction, roots)
}

// CheckNetwork 判定是否允许网络访问。
func (p Policy) CheckNetwork() Decision {
	if !p.Sandbox.Enabled || p.Sandbox.AllowNetwork {
		return allowed()
	}
	return deny("network access is disabled by sandbox policy")
}

// DefaultPolicy 返回"不限制"的策略（未配置时的行为，与之前一致）。
func DefaultPolicy() Policy { return Policy{} }
