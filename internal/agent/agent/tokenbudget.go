package agent

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
)

// computeTokenBudget 是本 agent 的预算（计算逻辑在 kit 里，这里只给参数）。
func (a *Agent) computeTokenBudget() kit.TokenBudget {
	return kit.ComputeTokenBudget(a.provider, a.systemPrompt, a.mainState().ctxTuning)
}
