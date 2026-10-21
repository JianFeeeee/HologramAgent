package core

import (
	"context"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// windowProvider 是只声明窗口的 Provider 桩（预算是纯函数，不需要真推理）。
type windowProvider struct{ n int }

func (windowProvider) Name() string { return "window-stub" }
func (windowProvider) Chat(context.Context, *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return nil, nil
}
func (windowProvider) ChatStream(context.Context, *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return nil, nil
}
func (p windowProvider) MaxContextTokens() int { return p.n }

// 回归（2026-09-19）：窗口声明为 1M 后，工作面不能被 80% 带到 838K ——
// 该源最优区间是 600K，超过就只剩成本与延迟。
func TestComputeTokenBudgetCapsTargetAtWorkingBand(t *testing.T) {
	const sys = "系统提示词"
	b := ComputeTokenBudget(windowProvider{1048576}, sys)

	if b.MaxContext != 1048576 {
		t.Errorf("MaxContext 应保留真实窗口：%d", b.MaxContext)
	}
	if b.TargetUsage != 600000 {
		t.Errorf("TargetUsage 应封顶在 600K（最优区间），实际 %d", b.TargetUsage)
	}

	avail := 600000 - EstimateTokens(sys)
	if b.MemoryTokens != avail/3 {
		t.Errorf("MemoryTokens=%d want %d", b.MemoryTokens, avail/3)
	}
	if b.MemoryTokens+b.ContextTokens != avail {
		t.Errorf("两部分应恰好用完可用预算：%d + %d != %d", b.MemoryTokens, b.ContextTokens, avail)
	}
}

// 封顶只作用于“窗口远大于工作区间”的情形；普通窗口仍按 80% 走，
// 不能让这条约束把小窗口的预算也一起改掉。
func TestComputeTokenBudgetSmallWindowUnchanged(t *testing.T) {
	b := ComputeTokenBudget(windowProvider{32768}, "x")
	if b.TargetUsage != 26214 {
		t.Errorf("小窗口应仍取 80%%：got %d want %d", b.TargetUsage, 26214)
	}
}
