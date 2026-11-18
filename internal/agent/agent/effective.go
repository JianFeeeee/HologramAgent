package agent

// effective.go —— 上下文保留条数的**类型侧推导**（原在 context.go）。
//
// ★ 为什么它留在基座而不是 ctxmgr：它读 `a.ComputeTokenBudget()` 与
//   累计 token（**基座的事实**），而 ctxmgr 不认识 Agent。

import "github.com/JianFeeeee/HologramAgent/internal/agent/ctxmgr"

// EffectiveProtectedCount 返回本帧现场要保护的条数。
//
// 规则：配置给上限，但**不得超过预算能容纳的条数**。
//
//	budgetCap = ContextTokens 预算 / 单条平均 token
//	取 min(配置值, budgetCap)
//
// 于是小窗口 / 大事件 ⇒ 自动收紧（宁可有取舍，也不让裁剪无解）；
// 大窗口或小事件 ⇒ 仍用满配置值。下限保 1：0 会让"最近 N 条一定在候选里"
// 这条保证消失。
func (a *Agent) EffectiveProtectedCount() int {
	cfgCap := protectedCountDefault
	if a == nil || a.context == nil {
		return cfgCap
	}
	if n := a.context.ProtectedCount(); n > 0 {
		cfgCap = n
	}
	budget := a.ComputeTokenBudget()
	tokens := budget.ContextTokens
	if tokens <= 0 || a.context.Len() == 0 {
		return cfgCap
	}
	avg := a.AccumulatedTokens() / a.context.Len()
	if avg <= 0 {
		avg = ctxmgr.DefaultAvgEventTokens
	}
	budgetCap := tokens / avg
	if budgetCap < 1 {
		budgetCap = 1
	}
	if budgetCap < cfgCap {
		return budgetCap
	}
	return cfgCap
}

// protectedCountDefault 是未配置时的保护条数（与 kit.DefaultProtectedCount 一致）。
const protectedCountDefault = 6

// countPlanPages 统计计划里被剔除的页数。
func countPlanPages(evict map[string][]int) int {
	n := 0
	for _, pages := range evict {
		n += len(pages)
	}
	return n
}

// recallBudget 是召回内容的可用 token（下限 = ContextTokens 的 1/4）。
//
// ★ 下限的由来：时间线接近满窗时预算会变成 0 或负数，那样**任何**召回
//
//	都会被判超预算而全部剔空，等于召不回来。
func (a *Agent) recallBudget() int {
	if a == nil || a.context == nil {
		return 0
	}
	tb := a.ComputeTokenBudget()
	avail := tb.ContextTokens - a.AccumulatedTokens()
	floor := tb.ContextTokens / 4
	if avail < floor {
		avail = floor
	}
	if avail < 0 {
		avail = 0
	}
	return avail
}
