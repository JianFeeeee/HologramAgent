package core

// 中断子系统 —— 只做三件事：**按优先级打断、保护被打断者的现场、恢复它**。
//
// ## 边界（2026-10-25 用户裁定，OS 语义）
//
// 中断类似操作系统中的中断语义，只提供：
//
//	· 中断优先级 —— 谁打断谁的唯一依据（Level / effectiveLevel / canPreempt）
//	· 中断队列   —— 待处理中断按优先级分组排队（interruptQueues / immediate）
//	· 现场保护   —— 被打断任务的现场压栈、以及**恢复决策**（suspendStack）
//	· 中断屏蔽   —— 不可抢占临界区（critical，对应 CPU 的关中断）
//
// **不含**：就绪队列、停止配额、任务 ID 分配、统计、背压、"执行任务"本身。
// 调度器建立在本子系统之上——它问"中断侧该出谁"，然后执行那个任务。
//
// ## 职责边界（不要越界）
//
// 本文件**只提供机制，不做决策**：
//
//	进中断     —— enqueueInterruptLocked / setImmediateLocked
//	中断栈     —— suspend（压栈）/ 弹栈原语
//	中断屏蔽   —— critical
//	让位信号   —— preemptArmed/preemptLevel 的读写原语
//
// 而**策略**住在 scheduler.go：
//
//	谁比谁优先     —— effectiveLevel / canPreempt / clampPluginLevel
//	能不能打断     —— registerInterrupt（含冷却期/临界区判定）
//	该恢复谁       —— nextInterruptSelection（栈顶 vs 中断队列比较）
//
// 换句话说：调度器问"现在恢复谁"，然后调用这里的原语执行。
// 中断管理器**不自行比较优先级**，也不看就绪队列。
//
// ## 核心不变量
//
//	链条 = 就绪(L0) ← I(L1) ← I(L2) ← I(L3) ← I(L4 运行中)，被挂起 4 帧
//
// 即 maxInterruptFrames = int(LevelCritical)。这是**结构推论**，不是配置项：
// L4 之上没有更高级别，链到此为止。超限只可能是内核 bug，因此只做防御性计数，
// **不降级、不丢弃帧**。
//
// 判据见 interrupt_invariant_test.go（I1..I6）。

import (
	"sync/atomic"
	"time"
)

// interruptState 是中断子系统的全部状态。
//
// 内嵌进 scheduler（字段提升），因此访问写法仍是 s.interruptQueues 等——
// 这样拆分对既有代码与测试透明，风险最低。
//
// ⚠️ 这些字段全部由 scheduler.mu 保护，与就绪队列共用同一把锁：
// 中断判定需要看"当前运行任务/是否临界区"，与调度状态在同一临界区内被
// 一起读写。拆锁是并发模型变更，不属于本次重构。
type interruptState struct {
	// interruptQueues[level]：四条**中断队列**（level 1..4），同级 FIFO。
	// 未能立即抢占的中断（级别不足，或运行任务在临界区）按级别入队。
	interruptQueues [5][]*Task

	// immediate：已判定的最高优先待运行者。抢占必须**立即生效**，
	// 所以它不经队列，在下一个安全点直接运行。
	// 这也消除了"抢占者与被抢占者同级"的比较问题——抢占者不需和栈顶比。
	immediate *Task

	// suspendStack：**中断栈**。被抢占后保存现场的任务压栈（LIFO）。
	// 只有**栈顶**参与恢复选择，栈内不做优先级重排。
	suspendStack []*suspendedTask

	// preemptArmed/preemptLevel：运行任务的"让位信号"。
	// 表示"已有更高优先级的中断被判定，请在下一个安全点让位"。
	preemptArmed bool
	preemptLevel Level

	// critical 报告运行任务是否在不可抢占临界区（如记忆整理）。
	// 对应 CPU 的关中断：此时中断只能入队，不能抢占。
	// 由于 interruptLoop 也要读它，必须是原子的。
	critical atomic.Bool

	// maxInterruptFrames 是中断栈帧数的**结构上界**（= 中断级数，非配置项）。
	// 见文件头的核心不变量。
	maxInterruptFrames int

	// stackNotify 是**中断栈状态钩子**：栈发生变化时通告调度器。
	//
	// 为什么需要它（用户裁定）：中断栈的操作者只有中断管理器；调度器不做
	// 压栈/弹栈，但它必须知道"栈里有没有可恢复的现场、栈顶是什么级别"，
	// 否则无法决定"该从队列取哪个优先级的任务"。钩子就是这个通告通道。
	//
	// 约定：**在持锁状态下调用**，回调不得再取 s.mu（否则自死锁）。
	// 实现方只做"记下状态 + 唤醒调度循环"，不做决策。
	stackNotify func()
}

// suspendedTask 是一个被抢占任务的现场。
//
// 现场保护的粒度是**整个 TaskFrame**（含 f.Msgs）：恢复时不能复用超限/过期的
// 那份消息数组，必须由 rebaseFramePrefix 重建（见 resumeTask 的注释）。
type suspendedTask struct {
	Task  *Task
	Frame *TaskFrame
}

// nextSelection 标识 nextRef 从哪个集合取出任务。
type nextSelection int

const (
	nextNone nextSelection = iota
	nextReady
	nextInterrupt
	nextImmediate
	nextSuspended
)

// ── 中断队列操作 ──

// highestInterruptLocked 返回当前最高级非空中断队列的队头及其级别。
func (s *scheduler) highestInterruptLocked() (*Task, Level) {
	for lv := LevelCritical; lv >= LevelBackground; lv-- {
		if q := s.interruptQueues[lv]; len(q) > 0 {
			return q[0], lv
		}
	}
	return nil, 0
}

// popInterruptLocked 弹出某级别中断队列的队头（调用方已确认非空）。
func (s *scheduler) popInterruptLocked(lv Level) {
	s.interruptQueues[lv] = s.interruptQueues[lv][1:]
}

// interruptCountLocked 统计所有待处理中断（含 immediate 槽）。
func (s *scheduler) interruptCountLocked() int {
	n := 0
	for lv := LevelBackground; lv <= LevelCritical; lv++ {
		n += len(s.interruptQueues[lv])
	}
	if s.immediate != nil {
		n++
	}
	return n
}

// enqueueInterruptLocked 把中断按级别入队。
func (s *scheduler) enqueueInterruptLocked(t *Task) {
	s.allocateIDLocked(t)
	if s.interruptCountLocked() >= s.maxQueue {
		for lv := LevelBackground; lv <= LevelCritical; lv++ {
			if len(s.interruptQueues[lv]) > 0 {
				s.popInterruptLocked(lv)
				s.stats.Rejected++
				break
			}
		}
	}
	lv := t.Level
	if lv < LevelBackground || lv > LevelCritical {
		lv = DefaultLevel
	}
	s.interruptQueues[lv] = append(s.interruptQueues[lv], t)
	s.stats.Enqueued++
}

// installImmediateLocked 把 t 放进 immediate 槽（**纯机制**，不比较优先级）。
//
// "该不该由 t 占槽"是策略，由调度器判定（见 scheduler.go 的 setImmediateLocked）。
// 这里只做：把现任降级入队 → 新任务占槽。
//
// 返回被顶掉的旧持有者（可能为 nil），调用方据此保持不变量的可观测性。
func (s *scheduler) installImmediateLocked(t *Task) *Task {
	var demoted *Task
	if s.immediate != nil {
		// 被顶掉的抢占者降级入队，**不丢**（不变量 I5）。
		demoted = s.immediate
		s.enqueueInterruptLocked(demoted)
	}
	s.allocateIDLocked(t)
	s.stats.Enqueued++
	s.immediate = t
	return demoted
}

// ── 中断登记（抢占或入队）──

// registerInterrupt 是登记中断的公共实现（任务已带好 Class/Level）。
//
// 返回 true 表示**真的占住了 immediate 槽**（会真抢占），调用方据此决定
// 是否取消可取消的步骤（如当前 LLM 流式请求）。


// requestPreempt 登记一次**外部**（插件/用户）中断请求，级别由来源声明。


// requestKernelPreempt 是**内核**中断入口（panic / 内核事件 / 上下文超页）。
//
// 级别固定 L4，且**不夹取**——这是 L4 的唯一来源，插件永远够不到。


// ── 让位协议 ──

// preemptGrantedFor 报告是否有已判定的让位请求（安全点据此决定是否挂起）。
func (s *scheduler) preemptGrantedFor() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.preemptArmed || s.running == nil {
		return false
	}
	return s.preemptLevel > effectiveLevel(s.running)
}

// clearPreempt 清除让位信号（抢占者已开始执行）。
func (s *scheduler) clearPreempt() {
	s.mu.Lock()
	s.preemptArmed = false
	s.preemptLevel = 0
	s.mu.Unlock()
}

// rearmPending 重新武装一个"被拦成入队"的中断。
//
// 临界区（或抢占冷却期）内到达的中断会被拦成入队；一旦离开那个窗口，
// 必须在这里重新求值——否则它只能等当前任务自然结束（设计 §4.3/§5.2）。


// ── 现场保护 ──

// suspend 保存现场压栈。
func (s *scheduler) suspend(t *Task, f *TaskFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.suspendStack) >= s.maxInterruptFrames {
		s.stats.Rejected++
	}
	s.suspendStack = append(s.suspendStack, &suspendedTask{Task: t, Frame: f})
	s.stats.Suspended++
	// 饥饿防护：抢占计数 +1（提升有效级）并记录冷却起点。
	t.PreemptCount++
	t.LastPreemptAt = time.Now()
	if s.running == t {
		s.running = nil
	}

	// D1=B：中断任务在上一个任务之前的完整状态上开始运行，
	// 因此这里**不**把被打断任务的任何内容交给它。
	s.preemptArmed = false
	s.preemptLevel = 0

	// 栈状态已变化 → 通告调度器（钩子在持锁下调用，只做记录+唤醒）。
	s.notifyStackLocked()
}

// popSuspendLocked 弹出中断栈顶并交出现场（调用方须持锁且确认非空）。
//
// ⚠️ 本函数**只应由中断管理器内部调用**：中断栈的操作者只有中断管理器。
// 调度器不得直接弹栈——它只能查询栈状态（suspendTopLocked 等），
// 然后**请求**中断管理器执行恢复（见 restoreTopLocked）。
//
// 弹栈是**机制**；"该不该弹、弹出来给谁跑"是策略（在调度器）。
// 中断管理器不判断优先级，只负责把栈顶原样交出去。
func (s *scheduler) popSuspendLocked() *suspendedTask {
	n := len(s.suspendStack)
	if n == 0 {
		return nil
	}
	top := s.suspendStack[n-1]
	s.suspendStack = s.suspendStack[:n-1]
	// Resumed 不在这里计：只有真正恢复执行时（resumeTask）才算一次，
	// 否则"排空后 Suspended == Resumed"这条不变量会因弹栈而虚高。
	s.notifyStackLocked()
	return top
}

// notifyStackLocked 触发栈状态钩子（调用方须持锁）。
//
// 钩子的语义是"通告"，不是"决策"：中断管理器不知道也不关心调度器会怎么做，
// 它只说"栈变了"。这正是两者解耦的关键——将来换掉调度器的选取策略，
// 或给中断子系统加向量表/多栈，都不需要改对方。
func (s *scheduler) notifyStackLocked() {
	if s.stackNotify != nil {
		s.stackNotify()
	}
}

// restoreTopLocked 执行一次"恢复栈顶"（**中断管理器是唯一操作者**）。
//
// 调度器只做决策（"现在该恢复栈顶"），具体弹栈由这里完成——
// 这是"中断栈的操作者只有中断管理器"这条不变量的落点。
//
// 返回被恢复的现场；栈空时返回 nil。
func (s *scheduler) restoreTopLocked() *suspendedTask {
	top := s.popSuspendLocked()
	if top == nil {
		return nil
	}
	s.running = top.Task
	return top
}

// ── 栈状态查询（调度器据此决策；**不提供任何操作栈的方法**）──

// stackDepthLocked 返回中断栈深度。
func (s *scheduler) stackDepthLocked() int { return len(s.suspendStack) }

// stackTopLocked 返回栈顶现场的**只读快照**（Task + Frame 指针）。
//
// 只读：调度器拿它做优先级比较，但**不能**用它弹栈——弹栈只能由
// 中断管理器在 restoreTopLocked 里做。这样"中断栈的操作者只有中断管理器"
// 这条不变量在类型层面就无法被绕过。
func (s *scheduler) stackTopLocked() *suspendedTask {
	n := len(s.suspendStack)
	if n == 0 {
		return nil
	}
	return s.suspendStack[n-1]
}

// canSuspend 报告中断栈是否还能压入新帧。
func (s *scheduler) canSuspend() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.suspendStack) < s.maxInterruptFrames
}

// suspendDepth 返回当前中断栈深度（仅用于日志）。
//
// 单独一个方法而不是让调用方直接读 s.suspendStack：那个字段只允许在 s.mu 下访问。
func (s *scheduler) suspendDepth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.suspendStack)
}

// nextInterruptSelection 是**恢复决策**：中断侧该出谁。
//
// 顺序（这是"谁先跑"的唯一实现处）：
//
//	① immediate —— 刚抢占成功的，必须立即生效
//	② 最高级待处理中断 vs 中断栈顶，取高者（持平归栈顶，维持 LIFO 与公平）
//
// 返回 ok=false 表示中断侧没有可出的任务，调用方（nextRef）应转去就绪队列。
//
// 为什么恢复决策在这里而不是调度器：见文件头「为什么恢复决策在这里」。

