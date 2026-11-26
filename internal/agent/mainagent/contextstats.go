package mainagent

// contextstats.go —— 主 agent 把自己上下文的**只读统计面**挂给基座。
//
// ★ 基座不持有上下文实现（§6.2 #1）：它只在状态面/诊断里读条数与水位。
//   这里把读取闭包接上（`agent.AgentConfig.ContextStats`）。

import "github.com/JianFeeeee/HologramAgent/internal/agent/agent"

// contextStats 实现 agent.ContextStats（读的是本类型自己的 ctx）。
type contextStats struct{ m *Module }

func (c contextStats) Events() int {
	if c.m == nil || c.m.ctx == nil {
		return 0
	}
	return c.m.ctx.Len()
}

func (c contextStats) Accumulated() int {
	if c.m == nil {
		return 0
	}
	return c.m.AccumulatedTokens()
}

func (c contextStats) Window() int {
	if c.m == nil {
		return 0
	}
	return c.m.ContextWindowTokens()
}

// statsFace 返回挂给基座的统计面。
func statsFace(m *Module) agent.ContextStats { return contextStats{m: m} }
