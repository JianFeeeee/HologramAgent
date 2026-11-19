package worker

// worker_type.go —— 工作式子的**实现载体**（组合基座句柄 + 本包状态）。
//
// 按 §1.3：组合，不内嵌。需要基座的什么就直接调 `M.A.Xxx()` ——
// 没有 Host 接口，没有映射层。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// M 是工作式子类型的实现载体（每个父 agent 一个）。
type M struct {
	A *agent.Agent
}

// Of 取回某个父 agent 的工作式子载体（未装配 ⇒ nil）。
func Of(a *agent.Agent) *M {
	if a == nil {
		return nil
	}
	if m, ok := a.Sub(agent.SubKeyWorker).(*M); ok {
		return m
	}
	return nil
}

// ensure 取回或创建载体。
func ensure(a *agent.Agent) *M {
	if a == nil {
		return nil
	}
	if m := Of(a); m != nil {
		return m
	}
	m := &M{A: a}
	a.SetSub(agent.SubKeyWorker, m)
	return m
}

// state 返回本载体的任务表（惰性创建）。
func (m *M) state() *State {
	if m == nil || m.A == nil {
		return nil
	}
	if st, ok := m.A.Sub(agent.SubKeyWorker + "/state").(*State); ok {
		return st
	}
	st := NewState()
	m.A.SetSub(agent.SubKeyWorker+"/state", st)
	return st
}

func init() {
	agent.BindWorker(
		func(a *agent.Agent, tc agentAPI.ToolCall, ch string) string { return ensure(a).SpawnChild(tc, ch) },
		func(a *agent.Agent, tc agentAPI.ToolCall) string { return ensure(a).ChildResult(tc) },
		func(a *agent.Agent, tc agentAPI.ToolCall) string { return ensure(a).ExecuteLLMTool(tc) },
	)
}
