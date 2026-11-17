package agent

import (
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// hooks_standard.go —— 七公共面的**标准实现**（主 agent / 驻留子注册用）。
//
// 用户裁定：「agent 基类提供基础字段与阶段，各个子 agent（含主 agent）
// 各自挂载各自的回调」。本文件就是"各自挂载"的标准那一份 —— 基座不认识它，
// 也不知道谁在用它。

// ContextBudgetRelevance 是**策略裁剪型**的预算（主 agent）。
func ContextBudgetRelevance(a *Agent, b TokenBudget) int {
	return b.ContextTokens
}

// ContextBudgetWholeWindow 是**整窗型**的预算（驻留子 / rtchild）。
//
// 只受模型能收多少这个硬上限约束，不做任何策略性裁剪。
func ContextBudgetWholeWindow(a *Agent, b TokenBudget) int {
	if b.MaxContext > 0 {
		return b.MaxContext
	}
	return defaultMaxContextTokens
}

// OverBudgetRatio 是"是否已超出窗口预算"的判据（主 agent）。
func OverBudgetRatio(a *Agent) bool {
	return a.overflowRatioNow() >= overflowRatio
}

// OverBudgetNever 给整窗型用（它们靠 contextfull 上报父 agent，不做超页处置）。
func OverBudgetNever(a *Agent) bool { return false }

// PruneByQuery 是按相关度裁剪（主 agent）。
func PruneByQuery(a *Agent, query string, protected int) int {
	if a == nil || a.context == nil {
		return 0
	}
	if protected >= 0 {
		return a.context.PruneWithProtected(query, a.contextTopKForProtected(protected), a.coldcontextStore, protected)
	}
	return a.context.PruneWithProtected(query, a.contextTopK(), a.coldcontextStore, a.effectiveProtectedCount())
}

// EnforceOverflowStandard 是标准超页处置（主 agent）。
func EnforceOverflowStandard(a *Agent, f *TaskFrame, query string, force bool) (bool, int) {
	return a.enforceContextBudgetDefault(f, query, force)
}

// RunStageStandard 执行插件阶段（主 agent / 驻留子）。
func RunStageStandard(a *Agent, stage string, ctx any) bool {
	sc, ok := ctx.(*sdk.StageContext)
	if !ok || sc == nil {
		return false
	}
	return a.runStageDefault(sdk.Stage(stage), sc)
}
