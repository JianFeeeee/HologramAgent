package agent

// posture.go —— **安全运行态显式化**（用户裁定 2026-11-18）。
//
// ## 为什么需要它
//
// 「安全模块未装配」今天只能靠 `Gate() == nil` **隐式推断**，而生产装配
// 永远装 Disabled Runtime（非 nil、零成本透传）⇒ 这个运行态在状态面与日志里
// 完全看不见。用户裁定：
//
//	「安全模块未装配则说明不需要安全能力，应该显式显现 agent 处于不安全运行态，
//	  此时工具与通道授权处于缺失渲染但子 agent 可直接调用的状态」
//
// 所以它是一种**运行态**，不是装配错误、不是拒绝。
//
// ## 与授权拆分的关系
//
//	PostureGuarded    闸门已装配（含 Disabled ⇒ 判定零成本透传）
//	                  ⇒ 渲染照旧过滤 + 调用走闸门
//	PostureUnguarded  未装配（裸 agent / 单测 / 独立工具）
//	                  ⇒ 渲染仍然过滤（缺失渲染不等于放开） + 调用**恒放行**
//
// ★ 关键：`Unguarded` 下**渲染照旧过滤** —— 否则模型会看见父明确禁用的
// 通道/工具，再在执行时被拒，白算一轮。

import "log"

// SecurityPosture 是安全能力的**运行态**（不是"是否出错"）。
type SecurityPosture int

const (
	// PostureGuarded 表示闸门已装配（可能是 Disabled Runtime：判定零成本透传）。
	PostureGuarded SecurityPosture = iota
	// PostureUnguarded 表示完全未装配：调用侧恒放行，渲染侧照旧过滤。
	PostureUnguarded
)

// String 给日志与状态面用。
func (p SecurityPosture) String() string {
	if p == PostureGuarded {
		return "guarded"
	}
	return "unguarded"
}

// SecurityPosture 报告本 agent 的运行态。
func (a *Agent) SecurityPosture() SecurityPosture {
	if a == nil || a.Gate() == nil {
		return PostureUnguarded
	}
	return PostureGuarded
}

// logPosture 在启动时把运行态**响亮**打一行。
//
// 为什么必须打：`Unguarded` 意味着"子 agent 可以直接调工具与通道，没有闸门"。
// 这在部署上可能是刻意的（单机、无凭据需求），但它不能是**沉默**的 ——
// 沉默会让"忘了装配"与"故意不装"长得一样。
func (a *Agent) logPosture() {
	if a == nil {
		return
	}
	switch a.SecurityPosture() {
	case PostureUnguarded:
		a.Logf("SECURITY POSTURE: unguarded — no security gate is assembled; " +
			"tool/channel authorization is NOT enforced at call time " +
			"(rendering still filters by the declared allowlist)")
	default:
		a.Logf("security posture: guarded (gate assembled)")
	}
}

// securitySection 是 `"security"` 状态段（基座注册：纯内核事实）。
type securitySection struct {
	Posture string `json:"posture"`
	// Enabled 是闸门自己的报告（Disabled Runtime ⇒ false 但 posture 仍是 guarded）。
	Enabled bool `json:"enabled"`
}

func (a *Agent) securitySectionValue() securitySection {
	posture := a.SecurityPosture()
	out := securitySection{Posture: posture.String()}
	if g := a.Gate(); g != nil {
		out.Enabled = g.Enabled()
	}
	return out
}

// LogPostureIfUnguarded 供装配点在 Start 之后调一次（幂等日志）。
func (a *Agent) LogPostureIfUnguarded() {
	if a != nil && a.SecurityPosture() == PostureUnguarded {
		log.Printf("[agent] %s running unguarded: no security gate assembled", a.id)
	}
}
