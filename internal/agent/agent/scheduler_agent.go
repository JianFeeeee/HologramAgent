package agent

// scheduler_agent.go —— 调度器**基座侧**的那几个方法（实现实体在 standard）。
//
// 它们是"要 agent 做事"的部分：跑一轮任务、pump 输入、panic 上报。
// 选择与排队在 standard。

import (
	"log"
	"runtime/debug"
	"strings"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// schedulerStatus 把快照转成对外的状态 DTO（不暴露帧内容）。
func (a *Agent) SchedulerStatus() sdk.SchedulerStatus {
	if a.sched == nil {
		return sdk.SchedulerStatus{}
	}
	snap := a.DumpScheduler()
	out := sdk.SchedulerStatus{
		ReadyQueueDepth:   len(snap.Queue),
		PendingInterrupts: len(snap.PendingInterrupts),
		SuspendStack:      len(snap.SuspendStack),
		MaxSuspendDepth:   snap.MaxInterruptFrames,
		InterruptsByLevel: snap.Stats.InterruptsByLevel,
		PreemptsByLevel:   snap.Stats.PreemptsByLevel,
		Enqueued:          snap.Stats.Enqueued,
		Executed:          snap.Stats.Executed,
		Rejected:          snap.Stats.Rejected,
		Suspended:         snap.Stats.Suspended,
		Resumed:           snap.Stats.Resumed,
		Backpressure:      snap.Stats.Backpressure,
	}
	// Preempted 是「各级抢占成功次数之和」，**不是** Suspended：受害者可能在
	// 让位信号生效前就自行结束，此时有抢占而没有挂起（见 PreemptsByLevel 注释）。
	// 此前这里直接拿 Suspended 顶替，导致 DTO 里 preempted 与 preempts_by_level 自相矛盾。
	for lv := LevelBackground; lv <= LevelCritical; lv++ {
		out.Preempted += snap.Stats.PreemptsByLevel[lv]
	}
	if snap.Running != nil {
		out.Running = &sdk.SchedulerTask{
			ID: snap.Running.ID, Level: int(snap.Running.Level), Kind: snap.Running.Kind.String(),
		}
	}
	if snap.Immediate != nil {
		out.Immediate = &sdk.SchedulerTask{
			ID: snap.Immediate.ID, Level: int(snap.Immediate.Level), Kind: snap.Immediate.Kind.String(),
		}
	}
	// 四级队列深度：下标即级别（1..4），下标 0 留 0。
	for lv := LevelBackground; lv <= LevelCritical; lv++ {
		out.InterruptQueues[lv] = len(snap.InterruptQueues[lv])
	}
	// 中断栈帧：栈底 → 栈顶（谁先被压进去、谁又打断了它）。
	for _, f := range snap.SuspendStack {
		if f == nil || f.Task == nil {
			continue
		}
		out.SuspendFrames = append(out.SuspendFrames, sdk.SchedulerFrame{
			Task: sdk.SchedulerTask{
				ID: f.Task.ID, Level: int(f.Task.Level), Kind: f.Task.Kind.String(),
			},
		})
	}
	return out
}

// isKernelLevelSource 报告某来源是否是**内核级插件**（编译期内置插件）。
//
// 只有它们能声明 L4（见 interruptLevel）。判据是插件注册表里的“内置工厂”，
// 而不是插件自报的名字本身——外部插件经 proc 桥时已被夹到 L3，这里是第二道闸。
//
// source 的约定是 `插件名` 或 `插件名/实例`（如 webui/<deviceID>），故取第一段。
func (a *Agent) IsKernelLevelSource(source string) bool {
	if source == "" {
		return false
	}
	name := source
	if i := strings.IndexByte(name, '/'); i > 0 {
		name = name[:i]
	}
	// ① 编译期内置插件（根 agent 的 L4 来源之一）。
	if a.MainState().pluginReg != nil && a.MainState().pluginReg.IsBuiltinPlugin(name) {
		return true
	}
	// ② **本 agent 的上级**（驻留子的父）—— 设计 §6.1 的 L4 通则：
	//    子的阶梯上只有父能产生 L4，所以父的"发送消息"一定能打断子。
	return a.kernelSource != "" && name == a.kernelSource
}

// DumpScheduler 返回调度器的原子快照（供状态页/测试断言）。
// roundsExecuted 返回本 agent 已执行的轮次数（供驻留子状态面展示）。
//
// 一轮 = 一次被执行的输入（排队与中断都算）。为什么不用 inputch 处理表的条数：
// 那张表记的是"当前上下文窗口内"的轮次，压缩会清空（设计 §8.3）——
// 拿它当轮次会让父看到轮次倒退。
func (a *Agent) roundsExecuted() int {
	if a.sched == nil {
		return 0
	}
	return a.sched.Executed()
}

func (a *Agent) DumpScheduler() SchedulerSnapshot {
	if a == nil || a.sched == nil {
		return SchedulerSnapshot{}
	}
	return a.sched.Snapshot()
}

// schedulerLoop 是唯一的任务执行者（取代原 eventLoop 的输入处理）。
func (a *Agent) schedulerLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] schedulerLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.schedulerLoop()
		}
	}()

	for {
		// ★★ 退出检查必须在**取任务之前**（2026-10-18 修）。
		//
		// 原实现只在"队列空"分支里 select ctx.Done：
		//
		//	if kind == nextNone {
		//		select { ... case <-a.ctx.Done(): return }
		//	}
		//
		// 于是只要队列里有任务，循环就**永远不会看 ctx.Done** ——
		// 一直取、一直执行，直到队列被取空才可能退出。
		//
		// 这就是"cancel 了却停不下来"的根因：销毁流程调 child.cancel()
		// 之后，子仍把队列里的任务全取走执行，而那些任务的结果随销毁丢失。
		// （之前我为此加了一个 freeze 标志绕过 —— 那是把一个状态机的
		//   缺陷用另一个状态掩盖；正确的修法是在这里尊重 ctx。）
		select {
		case <-a.ctx.Done():
			return
		default:
		}

		a.pumpInbox()

		t, f, kind := a.sched.NextRef()
		if kind == nextNone {
			// 无待办：阻塞等新输入、新中断（wake）或退出。
			select {
			case evt := <-a.io.InputChan():
				if !a.sched.Enqueue(newInputTask(evt)) {
					a.EmitSkippedReply(evt, "queue_full")
				}
			case msg := <-a.selfInputCh:
				_ = a.sched.Enqueue(newSelfTask(msg))
			case <-a.sched.Wake():
				// 中断已入 pendingInterrupts，回到循环顶部重新挑选。
			case <-a.ctx.Done():
				return
			}
			continue
		}

		if kind == nextSuspended {
			a.resumeTask(t, f)
			continue
		}
		a.executeNewTask(t)
	}
}

// pumpInbox 把 channel 里**已就绪**的输入搬进就绪队列（非阻塞）。
//
// 为什么不直接边收边执行：先把已到达的输入收进队列，选择函数才有意义——
// M3 起抢占必然要看"队列里还压着什么"，而 channel 不是可枚举的结构。
//
// 队列满即停止泵入（背压落回 channel，语义与设计文档 §4.4 一致）。
func (a *Agent) pumpInbox() {
	for a.sched.HasRoom() {
		select {
		case evt := <-a.io.InputChan():
			// 返回值必须处理：静默丢弃会让同步调用方永久挂起（回执路径 E）。
			if !a.sched.Enqueue(newInputTask(evt)) {
				a.sched.NoteBackpressure()
				a.EmitSkippedReply(evt, "queue_full")
			}
		case msg := <-a.selfInputCh:
			// 自循环输入没有同步调用方，满时记一次背压即可。
			if !a.sched.Enqueue(newSelfTask(msg)) {
				a.sched.NoteBackpressure()
			}
		case <-a.ctx.Done():
			return
		default:
			a.sched.ClearBackpressure()
			return
		}
	}
	// 队列满：输入留在 channel 里，发送方阻塞（设计 §4.4「阻塞发送方」）。
	// 必须计数并打日志——否则运维看到 Rejected=0 会以为没背压，而输入正卡在 channel。
	if a.sched.NoteBackpressure() {
		log.Printf("[agent] ready queue full (%d), input channel backpressured", a.sched.MaxQueue())
	}
}

// executeTask 执行一个任务（测试与旧调用方的入口）；见 executeNewTask。
// raiseKernelInterrupt 是 **L4 的唯一入口**：panic 中断与内核事件中断（selfip）。
//
// 它不经 io.InputChan（那是外部/插件输入），而是直接向调度器登记一条内核中断：
// 级别固定 L4、不夹取、不受插件声明影响。这正是“L4 只有内核持有”的落点。
//
// 能否抢占由调度器按统一判据决定；若会抢占，则顺手取消可取消的 LLM 流式步骤
// （与 interceptLoop 对插件中断的处理完全一致）。
func (a *Agent) raiseKernelInterrupt(source, channel, text string) {
	a.RaiseKernelInterruptAt(source, channel, text, LevelCritical)
}

// raiseKernelInterruptAt 是 raiseKernelInterrupt 的**带级别**版本。
//
// 级别由调用方给出（中断向量表里登记的那个），而不是在这里写死 L4：
// 向量表的每条都声明了自己的 Level，这条通道必须能表达它，
// 否则"级别由向量声明"就成了一句空话。
//
// 注意：外部插件走的不是这条路（它们经 interruptLevel 解析 + clampPluginLevel
// 夹取），这里只服务内核自己登记的中断源。
func (a *Agent) RaiseKernelInterruptAt(source, channel, text string, level Level) bool {
	if a.sched == nil {
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
	armed := a.sched.RegisterInterrupt(newKernelInterruptTaskAt(evt, level))
	if armed {
		a.CancelCurrentLLM()
	}
	return armed
}

// reportTaskPanic 把一个任务 panic 报告成内核 L4 中断。
//
// 递归保护是**结构性**的：若 panic 的任务本身就是 L4 内核中断，则不再产生新的
// L4——否则同一个 panic 会自我放大成中断风暴，与“内核事件”应有的语义相反。
func (a *Agent) reportTaskPanic(t *Task, r interface{}) {
	// 递归保护（"L4 panic 不再产生 L4"）已收进向量表的 Guard，
	// 不再硬编码在这里——新增中断源时不会漏掉这条规则。
	a.RaiseVector(VectorTaskPanic, vectorArgs{Task: t, Panic: r})
}

func (a *Agent) executeTask(t *Task) {
	a.executeNewTask(t)
}

// executeNewTask 执行一个**新建**任务，并做任务级 panic 隔离（不变量 I6）。
//
// 与改造前的差异（有意）：原 eventLoop 在 panic 后重启整个循环，
// 现在一个任务的 panic 只丢弃该任务，调度器与其它任务不受影响。
func (a *Agent) executeNewTask(t *Task) {
	var f *TaskFrame
	var out stepOutcome = outcomeDone

	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[agent] task#%d (%s) panic recovered: %v\n%s",
					t.ID, t.Level, r, debug.Stack())
				a.reportTaskPanic(t, r)
			}
		}()
		switch t.Kind {
		case TaskKindInput:
			f, out = a.RunInputTask(t.Event)
		case TaskKindSelf:
			f, out = a.RunInputTask(selfEvent(t.Self))
		}
	}()

	if out == outcomeSuspended && f != nil {
		// 挂起必须落日志：此前只发事件不落盘，导致生产日志里
		// 「谁把谁挤下去了」完全查不到（只有 interrupt from / cancelled 两行）。
		// 曾因此把时间上相邻的输入误判成凶手。
		log.Printf("[agent] suspend: %s (%s) yields to an interrupt; suspendStack=%d",
			describeTask(t), sourceOf(t, f), a.sched.SuspendDepth())
		a.sched.Suspend(t, f)
		a.PublishEvent(events.EventScheduler, map[string]interface{}{
			"action": "suspend", "task": t.ID, "level": int(t.Level),
		})
		return
	}
	a.sched.Done(t)
}

// resumeTask 从保存的现场继续一个被抢占的任务。
//
// 关键：不重建帧、不重跑 prepare 段——否则会重复提交上下文与事件。
// resumeTask 从保存的现场继续一个被抢占的任务。
//
// 关键：不重跑 prepare 段（否则会重复提交上下文与事件），而是先把基础前缀
// 重建到「中断任务之上」，再把本任务自己的现场接回去（见 rebaseFramePrefix）。
func (a *Agent) resumeTask(t *Task, f *TaskFrame) {
	a.sched.NoteResumed()
	log.Printf("[agent] resume: %s (%s) resumes after the interrupt finished",
		describeTask(t), sourceOf(t, f))
	a.PublishEvent(events.EventScheduler, map[string]interface{}{
		"action": "resume", "task": t.ID, "level": int(t.Level),
	})
	a.RebaseFramePrefix(f)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] resume task#%d panic recovered: %v\n%s",
				t.ID, r, debug.Stack())
			a.reportTaskPanic(t, r)
			a.sched.Done(t)
		}
	}()

	out := a.RunTaskSteps(f)
	if out == outcomeSuspended {
		a.sched.Suspend(t, f)
		return
	}
	a.FinishInputTask(f, out)
	a.sched.Done(t)
}
