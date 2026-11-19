package mainagent

// module.go —— 主 agent 类型的**实现载体**（组合基座句柄 + 本类型状态）。
//
// 组合，不内嵌；需要基座什么就直接调 `m.A.Xxx()`。没有 `Host` 接口、
// 没有映射层。主 agent 是**根**（不是谁的子），所以它的载体由本包自己持有。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

// Module 是主 agent 类型的实现载体。
type Module struct {
	a *agent.Agent
	// sched 是**本类型自己的**输入调度器实例（类型内部实现，不占基座字段）。
	//
	// ★ 驻留子当前调度行为相同，但那是**当前**的事实 —— 各自持有一份，
	//   任一方改自己的抢占/挂起语义时不用回基座改、也不影响另一方。
	sched *agent.Scheduler
}

// Agent 返回组合的基座句柄。
func (m *Module) Agent() *agent.Agent {
	if m == nil {
		return nil
	}
	return m.a
}
