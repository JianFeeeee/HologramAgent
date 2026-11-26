package resident

// driver.go —— **驻留子自己的**调度驱动（用户裁定 2026-11-20）。
//
// ## 为什么它必须在本包
//
// 驻留子今天的行为与主 agent **逐字相同**（同队列、同四级抢占、同挂起栈），
// 但那是**当前事实**，不是保证：
//
//	驻留子     高优先级入队 + **不挂起**（撞窗口上报父决策）
//	主 agent   四级抢占 + 挂起/恢复现场栈
//
// 借用主 agent 的循环意味着"主 agent 改了取任务时机 / 背压策略"，
// 驻留子**被动跟着变**，而它没有选择权。这正是 §8.4 记录的那个病。

import (
	"log"
	"runtime/debug"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sched "github.com/JianFeeeee/HologramAgent/internal/agent/resident/sched"
)

// SchedulerLoop 是驻留子"取任务 → 跑任务"的驱动（本类型自己的那份）。
//
// ★ 调度器实例从基座取（`m.sched`，由 StartRuntime 用 `SetScheduler`
//
//	交出去），所以驱动代码在本包、实例仍是驻留子自己的 —— 改本函数
//	不影响主 agent，反之亦然。
func (m *M) SchedulerLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[resident] schedulerLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go m.SchedulerLoop()
		}
	}()

	for {
		// ★★ 退出检查必须在**取任务之前**（2026-10-18 修，§8.4.1）。
		//
		// 只在"队列空"分支里 select ctx.Done 会让队列有任务时永远不看退出信号
		// ⇒ cancel 了却停不下来、队列里的任务结果随销毁丢失。
		select {
		case <-m.A.Ctx().Done():
			return
		default:
		}

		m.pumpInbox()

		t, f, kind := m.sched.NextRef()
		if kind == sched.NextNone {
			select {
			case evt := <-m.A.IO().InputChan():
				if !m.sched.Enqueue(sched.NewInputTask(evt)) {
					m.A.EmitSkippedReply(evt, "queue_full")
				}
			case msg := <-m.A.SelfInputChan():
				_ = m.sched.Enqueue(sched.NewSelfTask(sched.SelfInputMsg(msg)))
			case <-m.sched.Wake():
				// 中断已入 pendingInterrupts，回到循环顶部重新挑选。
			case <-m.A.Ctx().Done():
				return
			}
			continue
		}

		if kind == sched.NextSuspended {
			// ★ 驻留子**不挂起**（策略事实，不是漏实现）：
			//   撞窗口时它向父上报 contextfull，让父决定（压缩/回收/销毁）。
			//   调度器若给出 suspended 帧（不该发生），如实记账后继续。
			_ = f
			m.A.Logf("resident: unexpected suspended selection (this type has no suspend), task#%d", t.ID)
			m.sched.Done(t)
			continue
		}
		m.executeNewTask(t)
	}
}

// pumpInbox 把 channel 里已就绪的输入搬进就绪队列（非阻塞）。
func (m *M) pumpInbox() {
	for m.sched.HasRoom() {
		select {
		case evt := <-m.A.IO().InputChan():
			if !m.sched.Enqueue(sched.NewInputTask(evt)) {
				m.sched.NoteBackpressure()
				m.A.EmitSkippedReply(evt, "queue_full")
			}
		case msg := <-m.A.SelfInputChan():
			if !m.sched.Enqueue(sched.NewSelfTask(sched.SelfInputMsg(msg))) {
				m.sched.NoteBackpressure()
			}
		case <-m.A.Ctx().Done():
			return
		default:
			m.sched.ClearBackpressure()
			return
		}
	}
	if m.sched.NoteBackpressure() {
		log.Printf("[resident] ready queue full (%d), input channel backpressured", m.sched.MaxQueue())
	}
}

// executeNewTask 跑一个新任务（panic 隔离）。
//
// ★ 帧推进（step 状态机）**当前仍是借主 agent 的**：它读的几乎全是基座公有面
//
//	（`a.IO()` / `a.Scheduler()` / `a.Notify` / `a.RunToolBatch`），
//	逐 step 拆包要先有 §8.4.3 说的共享原语载体。**本轮不拆**（宁可留着
//	同一份实现，也不自造 `Runtime` 抽象）—— 见 todo R-3 的诚实条文。
func (m *M) executeNewTask(t *sched.Task) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[resident] task#%d (%s) panic recovered: %v\n%s",
					t.ID, t.Level, r, debug.Stack())
				m.reportTaskPanic(t, r)
			}
		}()
		var evt *agentIO.InputEvent
		switch t.Kind {
		case sched.TaskKindInput:
			evt = t.Event
		case sched.TaskKindSelf:
			evt = selfEventOf(t.Self)
		}
		if evt == nil {
			return
		}
		// ★ 帧推进借主 agent 的（诚实条文，见本函数文档与 todo R-3）：
		//   待拆的是 8 个 step 函数（3,228 行），它们读的是基座公有面；
		//   本轮不拆 = 不留半成品。
		f, _ := m.RunInputTask(evt)
		// ★ 记账（2026-11-26 新旧对照实测缺陷）：
		//
		//	`residentLoop` 是"帧推进 + inputch 记账"的统一入口，但它**没有
		//	任何调用方** —— 驱动走的是本函数。后果：处理表恒 0 条，
		//	父用 `admin_child action=table` 永远读到「（尚无记录）」，
		//	而子其实处理过了（context.json 里 resp 有值）。
		//	旧内核同一场景读到 2 条，这是真回归。
		m.AutoRecordInputch(f)
	}()
	m.sched.Done(t)
}

// reportTaskPanic 把任务 panic 报告成内核 L4 中断（递归保护在向量表 Guard 里）。
func (m *M) reportTaskPanic(t *sched.Task, r interface{}) {
	m.raiseVector(vectorTaskPanic, vectorArgs{Task: t, Panic: r})
}
