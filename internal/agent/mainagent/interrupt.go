package mainagent

// interrupt.go —— **主 agent 自己的**内核级中断产生原语（L4）。
//
// ★ 原先住在基座（`agent/kernelinterrupt.go` + `agent/interrupt_vectors.go`）。
//   按 docs §0.3（agent 间是隔离实例，任何情况不允许共享），中断栈与
//   中断产生归**用它的那个类型**：主 agent 这份在这里；驻留子有它自己的
//   （resident 自己发 L4 的路径）；rt 没有四级阶梯，不需要它。

import (
	"fmt"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sched "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/sched"
)

// raiseKernelInterruptAt 登记一条内核级中断（L4，只有内核源可产生）。
func (m *Module) raiseKernelInterruptAt(source, channel, text string, level sched.Level) bool {
	if m == nil || m.sched == nil {
		return false
	}
	evt := &agentIO.InputEvent{
		Source:        source,
		Type:          "interrupt",
		OutputChannel: channel,
		Payload: map[string]interface{}{
			"content":           text,
			"interrupt":         true,
			"interrupt_source":  source,
			"interrupt_channel": channel,
			"kernel":            true,
		},
	}
	armed := m.sched.RegisterInterrupt(sched.NewKernelInterruptTaskAt(evt, level))
	if armed {
		m.a.CancelCurrentLLM()
	}
	return armed
}

// raiseTaskPanic 把一个任务 panic 报告成内核 L4 中断。
//
// 递归保护是**结构性**的：若 panic 的任务本身就是 L4 内核中断，则不再产生
// 新的 L4 —— 否则同一个 panic 会自我放大成中断风暴。
func (m *Module) raiseTaskPanic(t *sched.Task, r interface{}) {
	if t == nil {
		return
	}
	if t.Class == sched.TaskInterrupt && t.Level >= sched.LevelCritical {
		return
	}
	m.raiseKernelInterruptAt(
		fmt.Sprintf("task/%d", t.ID), "kernel",
		fmt.Sprintf("内核事件：任务 #%d 发生 panic：%v（该任务已被丢弃，调度器存活）", t.ID, r),
		sched.LevelCritical)
}
