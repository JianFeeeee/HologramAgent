package resident

import (
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/resident/ctx"
	sched "github.com/JianFeeeee/HologramAgent/internal/agent/resident/sched"
)

// runtime.go —— **驻留子类型**的运行时装配（类型注册自己的运行行为）。
//
// ## 为什么在这里而不在基座
//
// 驻留子的中断与输入处置模式（队列 + 四级抢占阶梯）是**本类型的 agentloop
// 语义**。基座只有一个槽位（`agent.RuntimeHooks.Start/Stop`），填什么由类型决定。
//
// ★ 与主 agent 当前行为相同，但**各自持有各自的实现**：
//   这里起的是驻留子自己的调度循环（`m.SchedulerLoop`），主 agent 起的是
//   它自己的那份。任一方改自己的抢占/挂起语义，不回基座、不影响对方。

// StartRuntime 起驻留子的运行期（它自己的调度循环）。
//
// ★ 调度器实例**只在类型里**（`m.sched`）：基座不持有、不需要知道。
//
//	四个类型各自一份，任一方改自己的排队/抢占语义不回基座。
func StartRuntime(a *agent.Agent) {
	if a == nil {
		return
	}
	mm := Wrap(a)
	if mm == nil {
		return
	}
	if mm.sched == nil {
		mm.sched = sched.New(256)
	}
	// ★ 驱动是**本类型自己的**（resident/driver.go），不是借主 agent 的。
	go mm.SchedulerLoop()
}

// StopRuntime 停驻留子的运行期。
//
// ★ 这里**不回收子**：子不得比父活得久由内核侧 `Agent.Stop` 统一执行。
func StopRuntime(a *agent.Agent) {}

// OpenContext 用**内核分配的实例目录**开一份本实例的上下文实现。
//
// ★ 驻留子的上下文归**子自己的载体**（`rc.agent`），不是父的：它有自己的
// 调度循环，读写的是自己的那份。
//
// 目录路径来自内核（`a.InstanceDir()`），文件名归本类型 ——
// 与主 agent 同一口径（用户裁定 2026-11-19）。
func (m *M) OpenContext() {
	if m == nil || m.A == nil {
		return
	}
	dir := m.A.EnsureWorkDir()
	savePath := ""
	if dir != "" {
		savePath = filepath.Join(dir, contextFileName)
	}
	// ★ embedder 走基座 syscall（与主 agent 同一口径，docs §3.4）。
	rc := ctx.NewRelevanceContext(savePath, embedderFaceOf(m.A))
	// 整窗型：预算 = 整个模型窗口（不按相关性裁）。
	m.ctx = rc
}
