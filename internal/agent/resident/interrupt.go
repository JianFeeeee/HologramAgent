package resident

// interrupt.go —— **驻留子自己的**内核级中断产生原语（L4）。
//
// ★ 原先在基座（`agent/kernelinterrupt.go` + `interrupt_vectors.go`）。
//   按 docs §0.3（隔离，不允许共享），归用它的类型：本包这份。
//   主 agent 有它自己的（mainagent/interrupt.go）；rt 没有四级阶梯。

import (
	"fmt"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/resident/sched"
)

// vectorTaskPanic 是"任务 panic"这条内核中断的标识（本类型自己的向量）。
const vectorTaskPanic = "task_panic"

// vectorArgs 是一条中断向量的载荷。
type vectorArgs struct {
	Task  *sched.Task
	Panic interface{}
}

// raiseKernelInterruptAt 登记一条内核级中断（L4）。
func (m *M) raiseKernelInterruptAt(source, channel, text string, level sched.Level) bool {
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
		m.A.CancelCurrentLLM()
	}
	return armed
}

// raiseVector 按向量标识产生一条内核中断（递归保护在调用方判）。
func (m *M) raiseVector(id string, args vectorArgs) bool {
	if id != vectorTaskPanic || args.Task == nil {
		return false
	}
	// 递归保护：panic 的任务本身已是 L4 ⇒ 不再产生新的 L4。
	if args.Task.Class == sched.TaskInterrupt && args.Task.Level >= sched.LevelCritical {
		return false
	}
	return m.raiseKernelInterruptAt(
		fmt.Sprintf("task/%d", args.Task.ID), "kernel",
		fmt.Sprintf("内核事件：任务 #%d 发生 panic：%v（该任务已被丢弃，调度器存活）", args.Task.ID, args.Panic),
		sched.LevelCritical)
}

// selfEventOf 把内核自循环消息归一成输入事件（本类型自己的归一化）。
func selfEventOf(msg sched.SelfInputMsg) *agentIO.InputEvent {
	ch := msg.Channel
	if ch == "" {
		ch = "consolidation"
	}
	payload := map[string]interface{}{"content": msg.Text}
	if msg.Kind != "" {
		payload["self_kind"] = msg.Kind
	}
	return &agentIO.InputEvent{
		Source:        "system",
		Type:          "text",
		Payload:       payload,
		OutputChannel: ch,
	}
}
