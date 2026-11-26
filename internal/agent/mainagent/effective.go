package mainagent

// effective.go —— 上下文保留条数的**类型侧推导**（原在 context.go）。
//
// ★ 为什么它留在基座而不是 ctxmgr：它读 `m.ComputeTokenBudget()` 与
//   累计 token（**基座的事实**），而 ctxmgr 不认识 agent.Agent。

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
func (m *Module) recallBudget() int {
	if m == nil || m.a == nil || m.ctx == nil {
		return 0
	}
	tb := m.ComputeTokenBudget()
	avail := tb.ContextTokens - m.AccumulatedTokens()
	floor := tb.ContextTokens / 4
	if avail < floor {
		avail = floor
	}
	if avail < 0 {
		avail = 0
	}
	return avail
}
