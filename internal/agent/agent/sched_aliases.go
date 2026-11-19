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
)

// newScheduler 构造一个调度器实例（主 agent / 驻留子各自一份）。
func newScheduler(maxQueue int) *Scheduler { return standard.New(maxQueue) }

func newInputTask(evt *agentIO.InputEvent) *Task { return standard.NewInputTask(evt) }
func newSelfTask(msg SelfInputMsg) *Task         { return standard.NewSelfTask(msg) }
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
