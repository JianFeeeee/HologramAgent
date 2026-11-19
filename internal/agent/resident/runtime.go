package resident

import "github.com/JianFeeeee/HologramAgent/internal/agent/agent"

// runtime.go —— **驻留子类型**的运行时装配（类型注册自己的运行行为）。
//
// ## 为什么在这里而不在基座
//
// 驻留子的中断与输入处置模式（队列 + 四级抢占阶梯）是**本类型的 agentloop
// 语义**。基座只有一个槽位（`RuntimeHooks.Start/Stop`），填什么由类型决定。
//
// ★ 与主 agent 当前行为相同，但**各自持有各自的实现**：
//   这里起的是驻留子自己的调度循环（`m.SchedulerLoop`），主 agent 起的是
//   它自己的那份。任一方改自己的抢占/挂起语义，不回基座、不影响对方。

// StartRuntime 起驻留子的运行期（它自己的调度循环）。
func StartRuntime(a *agent.Agent) {
	if a == nil {
		return
	}
	go a.SchedulerLoop()
}

// StopRuntime 停驻留子的运行期。
//
// ★ 这里**不回收子**：子不得比父活得久由内核侧 `Agent.Stop` 统一执行。
func StopRuntime(a *agent.Agent) {}
