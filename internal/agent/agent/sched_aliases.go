package agent

// sched_aliases.go —— 调度器的**类型别名**（实现实体在 internal/agent/standard）。
//
// 主 agent 与驻留子各自**持有**一份 `*standard.Scheduler` 实例，
// 但它们对它的写法（`Enqueue` / `NextRef` / `RequestPreempt` …）逐字不变。

import (
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/standard"
)

type (
	Scheduler         = standard.Scheduler
	Level             = standard.Level
	Task              = standard.Task
	TaskClass         = standard.TaskClass
	TaskKind          = standard.TaskKind
	SchedulerStats    = standard.SchedulerStats
	SchedulerSnapshot = standard.SchedulerSnapshot
	offloadCandidate  = standard.OffloadCandidate
	OffloadCandidate  = standard.OffloadCandidate
	suspendedTask     = standard.SuspendedTask
	nextSelection     = standard.NextSelection
)

const (
	LevelBackground  = standard.LevelBackground
	LevelMessage     = standard.LevelMessage
	LevelInteractive = standard.LevelInteractive
	LevelCritical    = standard.LevelCritical
	DefaultLevel     = standard.DefaultLevel

	TaskQueued    = standard.TaskQueued
	TaskInterrupt = standard.TaskInterrupt
	TaskKindInput = standard.TaskKindInput
	TaskKindSelf  = standard.TaskKindSelf

	nextNone      = standard.NextNone
	nextReady     = standard.NextReady
	nextInterrupt = standard.NextInterrupt
	nextImmediate = standard.NextImmediate
	nextSuspended = standard.NextSuspended

	// schedStopTimeout 是等调度器停下的上限。
	schedStopTimeout = standard.StopTimeout
	// SchedStopTimeout 同上（导出形态，类型包要等自己的调度器停）。
	SchedStopTimeout = standard.StopTimeout
)

// newScheduler 构造一个调度器实例（主 agent / 驻留子各自一份）。
func newScheduler(maxQueue int) *Scheduler { return standard.New(maxQueue) }

// NewScheduler 造一个**本类型自己的**调度器实例（不进基座字段）。
//
// ★ 主 agent 与驻留子当前调度语义相同，但**各自持有一份实例**：
//
//	任一方未来改自己的抢占/挂起语义时，不需要回基座改、也不影响另一方。
func NewScheduler(maxQueue int) *Scheduler { return standard.New(maxQueue) }

func newInputTask(evt *agentIO.InputEvent) *Task { return standard.NewInputTask(evt) }

// NewInputTask 是 `newInputTask` 的导出形态（类型包要构造自己的排队任务）。
func NewInputTask(evt *agentIO.InputEvent) *Task { return standard.NewInputTask(evt) }

// NewSelfTask 是 `newSelfTask` 的导出形态（类型自投 self input）。
func NewSelfTask(msg SelfInputMsg) *Task { return standard.NewSelfTask(msg) }
func newSelfTask(msg SelfInputMsg) *Task { return standard.NewSelfTask(msg) }
func newInterruptTask(evt *agentIO.InputEvent, level Level) *Task {
	return standard.NewInterruptTask(evt, level)
}
func newKernelInterruptTask(evt *agentIO.InputEvent) *Task {
	return standard.NewKernelInterruptTask(evt)
}
func newKernelInterruptTaskAt(evt *agentIO.InputEvent, level Level) *Task {
	return standard.NewKernelInterruptTaskAt(evt, level)
}
func interruptLevel(evt *agentIO.InputEvent) Level { return standard.InterruptLevel(evt) }
func isKernelNotice(evt *agentIO.InputEvent) bool  { return standard.IsKernelNotice(evt) }
func sourceOf(t *Task, f *TaskFrame) string        { return standard.SourceOf(t, f) }
func describeTask(t *Task) string                  { return standard.DescribeTask(t) }

// Scheduler 返回本 agent 的调度器实例。
//
// ⚠️ **过渡态**：`a.sched` 字段仍在基座（`task.go` 的 52 处引用尚未搬走）。
// 终态是调度器随类型的 agentloop 进类型包，基座零持有 —— 见 todo.md N-11。
func (a *Agent) Scheduler() *Scheduler {
	if a == nil {
		return nil
	}
	return a.sched
}
