package mainagent

import (
	mainctx "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/ctx"

	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
)

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
func (m *Module) EffectiveProtectedCount() int {
	cfgCap := kit.DefaultProtectedCount
	if m == nil || m.a == nil || m.ctx == nil {
		return cfgCap
	}
	if n := m.ctx.ProtectedCount(); n > 0 {
		cfgCap = n
	}
	budget := m.ComputeTokenBudget()
	tokens := budget.ContextTokens
	if tokens <= 0 || m.ctx.Len() == 0 {
		return cfgCap
	}
	avg := m.AccumulatedTokens() / m.ctx.Len()
	if avg <= 0 {
		avg = mainctx.DefaultAvgEventTokens
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
