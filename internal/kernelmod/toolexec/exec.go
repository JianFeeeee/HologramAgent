package toolexec

// exec.go —— 工具执行设施的**实现载体**。
//
// ★ 它不持有任何 agent 状态：宿主（`agent.ToolHost`）每次调用随
//   `agentPkg.BatchOpts.Host` 传进来。所以一个设施实例服务全部 agent（1 份 = N 个消费者）。

import (
	"context"
	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

// Executor 是设施实例（无状态：宿主每次随调用传入）。
type Executor struct{}

// New 构造设施实例（装配期一次）。
//
// ★ 无状态是刻意的：它服务全部 agent，任何 per-agent 字段都会让"主 agent 的
// 设置泄漏给子 agent"（本仓在安全档位上踩过这个形态）。
func New() *Executor { return &Executor{} }

// Execute 执行一批工具调用，返回与入参**同序**的结果。
func (x *Executor) Execute(ctx context.Context, calls []agentPkg.ToolCall, opts agentPkg.BatchOpts) []agentPkg.BatchResult {
	return runBatch(ctx, calls, opts)
}
