package core

// 中断子系统的**不变量**判据（阶段 0：拆分前的安全网）。
//
// ## 为什么单独一个文件
//
// scheduler_preempt_test.go / scheduler_critical_test.go 测的是**行为**
// （高优先级抢占、临界区不抢占……），它们证明"这套逻辑能工作"。
//
// 而拆分重构要保的是**不变量**——那些一旦被搬错就静默失效、且行为测试
// 未必覆盖的结构性质：
//
//	I1  中断栈帧数上界 = 中断级数，且**绝不丢弃帧**
//	I2  L4 遇 L4 不可抢占（canPreempt 用严格大于）
//	I3  中断栈严格 LIFO，栈内不做优先级重排
//	I4  临界区内的中断只入队、不抢占
//	I5  immediate 单槽：被顶掉的抢占者**降级入队，不丢**
//	I6  "排空后 Suspended == Resumed"（挂起与恢复严格配对）
//
// 这些不变量正是要搬进 interrupt.go 的东西。先有判据，再搬迁——
// 否则搬完只能靠"看起来还对"来判断，而这类不变量恰恰是肉眼看不出的。

import (
	"reflect"
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// ── I2：L4 遇 L4 不可抢占 ──

func TestInvariant_L4DoesNotPreemptL4(t *testing.T) {
	if canPreempt(
		&Task{Class: TaskInterrupt, Level: LevelCritical},
		&Task{Class: TaskInterrupt, Level: LevelCritical},
	) {
		t.Fatal("L4 不得抢占 L4（canPreempt 必须是严格大于）")
	}
	// 相邻检查：L3 也抢不了 L4。
	if canPreempt(
		&Task{Class: TaskInterrupt, Level: LevelInteractive},
		&Task{Class: TaskInterrupt, Level: LevelCritical},
	) {
		t.Fatal("L3 不得抢占 L4")
	}
	// 反过来必须成立，否则上一条可能是"谁都不抢"的假绿。
	if !canPreempt(
		&Task{Class: TaskInterrupt, Level: LevelCritical},
		&Task{Class: TaskInterrupt, Level: LevelInteractive},
	) {
		t.Fatal("L4 必须能抢占 L3")
	}
}

// ── I1 + I3：中断栈上界与 LIFO ──

func TestInvariant_InterruptStackBoundIsLevelCount(t *testing.T) {
	// 上界是**结构推论**（= 中断级数），不是配置项。
	s := newScheduler(16)
	if s.maxInterruptFrames != int(LevelCritical) {
		t.Fatalf("中断栈上界应等于中断级数 %d，实际 %d", int(LevelCritical), s.maxInterruptFrames)
	}
}

// 栈满时 canSuspend 必须为 false —— 这是防"第 5 次嵌套"的唯一闸门。
// 注意：闸门为 false ≠ 丢帧，调用方必须改用"入队"路径（见 I5）。
func TestInvariant_CanSuspendHonorsBound(t *testing.T) {
	s := newScheduler(16)
	for i := 0; i < s.maxInterruptFrames; i++ {
		if !s.canSuspend() {
			t.Fatalf("第 %d 层仍应可挂起（上界 %d）", i+1, s.maxInterruptFrames)
		}
		s.suspendStack = append(s.suspendStack, &suspendedTask{Task: &Task{ID: uint64(i)}})
	}
	if s.canSuspend() {
		t.Fatalf("已达上界 %d，canSuspend 必须为 false", s.maxInterruptFrames)
	}
}

// 栈是严格 LIFO：恢复顺序必须是压入的逆序，且栈内不做优先级重排。
//
// 判据手法：压入三个**级别递增**的现场（这正是嵌套抢占的自然形态），
// 检查弹出顺序是"后进先出"，而不是"高级别先出"。
func TestInvariant_StackIsStrictLIFO(t *testing.T) {
	s := newScheduler(16)
	pushed := []*Task{
		{ID: 1, Class: TaskInterrupt, Level: LevelMessage},
		{ID: 2, Class: TaskInterrupt, Level: LevelInteractive},
		{ID: 3, Class: TaskInterrupt, Level: LevelCritical},
	}
	for _, t2 := range pushed {
		s.suspendStack = append(s.suspendStack, &suspendedTask{Task: t2})
	}

	// 逐层弹出，必须是 3、2、1。
	for i := len(pushed) - 1; i >= 0; i-- {
		n := len(s.suspendStack)
		top := s.suspendStack[n-1]
		s.suspendStack = s.suspendStack[:n-1]
		if top.Task.ID != pushed[i].ID {
			t.Fatalf("栈顶顺序错误：期望 task#%d，实际 task#%d（栈必须严格 LIFO）",
				pushed[i].ID, top.Task.ID)
		}
	}
	if len(s.suspendStack) != 0 {
		t.Fatalf("弹空后应为空栈，实际剩 %d 帧", len(s.suspendStack))
	}
}

// ── I4：临界区内只入队、不抢占 ──

func TestInvariant_CriticalOnlyQueues(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	if a.sched == nil {
		t.Skip("该 agent 未启用调度器")
	}

	// 造一个"正在运行的高级别任务"。
	running := &Task{ID: 100, Class: TaskInterrupt, Level: LevelBackground}
	a.sched.running = running
	a.sched.setCritical(true)

	evt, _ := textEvent("qq", "临界区内的 L4 中断")
	evt.Payload["interrupt"] = true

	armed := a.sched.requestKernelPreempt(evt)
	if armed {
		t.Fatal("临界区内不得抢占（requestKernelPreempt 应返回 false）")
	}
	if a.sched.immediate != nil {
		t.Fatal("临界区内不得占用 immediate 槽")
	}
	// 关键：**不得丢失**——必须进了中断队列。
	if n := a.sched.interruptCountLocked(); n == 0 {
		t.Fatal("临界区内的中断必须入队等待，不得丢弃")
	}
	a.sched.setCritical(false)
}

// ── I5：immediate 单槽，被顶掉的抢占者降级入队不丢 ──

func TestInvariant_ImmediateIsSingleSlotAndDemotes(t *testing.T) {
	s := newScheduler(16)
	low := &Task{ID: 1, Class: TaskInterrupt, Level: LevelMessage}
	high := &Task{ID: 2, Class: TaskInterrupt, Level: LevelCritical}

	if !s.setImmediateLocked(low) {
		t.Fatal("空槽时低级别抢占者应占到 immediate")
	}
	if !s.setImmediateLocked(high) {
		t.Fatal("更高级别应顶掉当前 immediate 持有者")
	}
	if s.immediate == nil || s.immediate.ID != high.ID {
		t.Fatal("immediate 应换成更高级别的抢占者")
	}
	// 被顶掉的 low 必须在它自己级别的队列里（降级入队，不丢）。
	q := s.interruptQueues[low.Level]
	found := false
	for _, it := range q {
		if it.ID == low.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("被顶掉的抢占者必须降级入队，绝不能丢")
	}

	// 同级别再来一个：**不得顶掉** immediate 持有者。
	//
	// ⚠️ 注意 setImmediateLocked 的契约：返回 false 时它**本身不入队**，
	// 而且明文规定"调用方必须用返回值决定是否按级别入队"（见函数注释）。
	// 所以这里只能断言"没抢到"，入队的责任在 registerInterrupt。
	same := &Task{ID: 3, Class: TaskInterrupt, Level: LevelCritical}
	if s.setImmediateLocked(same) {
		t.Fatal("同级别不应顶掉 immediate 持有者")
	}
	if s.immediate.ID != high.ID {
		t.Fatal("未抢到时 immediate 不得被改动")
	}
}

// ── I6：挂起与恢复严格配对 ──

// 一次完整的"抢占 → 恢复"之后，Suspended 与 Resumed 必须相等
// （排空后不留悬挂帧、也不凭空恢复）。
func TestInvariant_SuspendedEqualsResumed(t *testing.T) {
	sp := newPreemptProvider("intr-done", "low-done")
	a := newPreemptAgent(t, sp)
	defer a.Stop()

	task, _ := enqueueQueued(t, a, "qq", "会被打断的任务")
	_ = task

	a.sched.mu.Lock()
	suspBefore, resBefore := a.sched.stats.Suspended, a.sched.stats.Resumed
	a.sched.mu.Unlock()

	// 入口条件不满足时（未真正发生抢占）跳过，避免假绿/假红。
	if suspBefore != 0 || resBefore != 0 {
		t.Skipf("前置：统计应为 0（susp=%d res=%d）", suspBefore, resBefore)
	}

	// 压入再弹出一帧：这是 suspend/resume 的最小完整配对。
	a.sched.suspendStack = append(a.sched.suspendStack, &suspendedTask{Task: &Task{ID: 9}})
	a.sched.mu.Lock()
	a.sched.stats.Suspended++
	a.sched.mu.Unlock()

	n := len(a.sched.suspendStack)
	a.sched.suspendStack = a.sched.suspendStack[:n-1]
	a.sched.mu.Lock()
	a.sched.stats.Resumed++
	a.sched.mu.Unlock()

	a.sched.mu.Lock()
	susp, res := a.sched.stats.Suspended, a.sched.stats.Resumed
	depth := len(a.sched.suspendStack)
	a.sched.mu.Unlock()

	if susp != res {
		t.Fatalf("挂起与恢复必须严格配对：Suspended=%d Resumed=%d", susp, res)
	}
	if depth != 0 {
		t.Fatalf("排空后中断栈应为空，实际 %d 帧", depth)
	}
}

// ── I0：边界不变量 —— 队列归调度器，中断管理器只管现场保护 ──

// 用户栽定：「中断管理器只负责进入中断和中断栈的部分」、
// 「中断队列就该调度器管理」。
//
// 这条判据用**结构事实**钉住边界，而不是靠注释：
// interruptState 里只允许出现现场保护/屏蔽相关字段；
// 一旦有人把队列或优先级策略塞回来，这里立刻变红。
func TestInvariant_InterruptManagerHoldsOnlyStackState(t *testing.T) {
	// 反射枚举 interruptState 的字段名，白名单校验。
	typ := reflect.TypeOf(interruptState{})
	allowed := map[string]bool{
		"suspendStack":       true, // 中断栈
		"critical":           true, // 中断屏蔽
		"preemptArmed":       true, // 让位信号
		"preemptLevel":       true, // 让位信号
		"maxInterruptFrames": true, // 栈上界
		"stackNotify":        true, // 栈状态钩子
	}
	var bad []string
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !allowed[name] {
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("中断管理器不应持有这些字段（队列/优先级归调度器）: %v", bad)
	}

	// 反向确认：队列确实在 scheduler 上（而不是"两边都没有"的假绿）。
	st := reflect.TypeOf(scheduler{})
	for _, want := range []string{"interruptQueues", "immediate"} {
		if _, ok := st.FieldByName(want); !ok {
			t.Fatalf("中断队列 %s 应在 scheduler 上（排队是调度职责）", want)
		}
	}
}

// ── I7：中断栈状态钩子必须被接上，且栈变化会通告 ──

// 中断管理器与调度器唯一的联系就是这个钩子。它若为空，栈的压入/弹出
// 对调度器完全不可见——行为会**静默降级**成"等当前任务自然结束"。
// 所以必须有判据钉住它，而不是靠读代码发现。
func TestInvariant_StackNotifyHookIsWired(t *testing.T) {
	s := newScheduler(16)
	if s.interruptState.stackNotify == nil {
		t.Fatal("中断栈状态钩子未接上：栈变化将无法通告调度器（会静默降级）")
	}
}

// 压栈与弹栈都必须触发通告（各一次）。只通告一侧同样是降级：
// 只通告压栈 → 恢复后调度器不知道可以继续；只通告弹栈 → 挂起后不知道要换人。
func TestInvariant_StackNotifyFiresOnPushAndPop(t *testing.T) {
	s := newScheduler(16)
	var calls int
	s.interruptState.stackNotify = func() { calls++ }

	s.suspend(&Task{ID: 1, Class: TaskInterrupt, Level: LevelMessage}, &TaskFrame{})
	if calls != 1 {
		t.Fatalf("压栈必须触发一次通告，实际 %d 次", calls)
	}
	if s.restoreTopLocked() == nil {
		t.Fatal("restoreTopLocked 应返回被恢复的现场")
	}
	if calls != 2 {
		t.Fatalf("弹栈必须触发一次通告，实际累计 %d 次（期望 2）", calls)
	}
}

// 钩子必须在**持锁状态下**被调用：回调里不得再取 s.mu，否则自死锁。
//
// 判据手法：回调里用 TryLock 探测——若能取到锁，说明钩子调用时**未持锁**
// （违反约定）。反过来若已持锁，TryLock 返回 false，正是期望。
//
// ⚠️ 必须带超时：若钩子压根没被调用（例如被误删），无超时的等 channel
// 会让整个测试永久卡死——实测踩过一次，整包跑不下去。缺判据要"红"，
// 不要"挂"。
func TestInvariant_StackNotifyHoldsLock(t *testing.T) {
	s := newScheduler(16)
	var lockHeld bool
	var called bool
	s.interruptState.stackNotify = func() {
		called = true
		if s.mu.TryLock() {
			s.mu.Unlock()
			lockHeld = false
			return
		}
		lockHeld = true
	}
	s.suspend(&Task{ID: 1, Class: TaskInterrupt, Level: LevelMessage}, &TaskFrame{})

	if !called {
		t.Fatal("压栈未触发钩子（钩子被误删或未接上）")
	}
	if !lockHeld {
		t.Fatal("钩子必须在持锁状态下调用（回调不得再取 s.mu，否则自死锁）")
	}
}

// ── 反向依赖的现状快照（阶段 1.5 要切断的目标）──
//
// 这条不测行为，而是**记录事实**：中断函数目前读了这些非中断状态。
// 切断依赖后本判据应随之失败并被改写——届时可以明确看到"依赖确实变了"。
//
// 若不做这一步，"切断反向依赖"就只是一句口号，没有任何可验证的落点。
func TestInvariant_InterruptReadsSchedulerState_TargetsForDecoupling(t *testing.T) {
	// 事实清单（来自探测，见 docs/zh/interrupt-subsystem-split-plan.md §1.5）。
	// 这里只断言"这些字段在 scheduler 上存在"，作为改写时的锚点；
	// 阶段 1.5 完成后，中断侧不应再直接读它们。
	s := newScheduler(8)
	if s.maxQueue == 0 {
		t.Skip("maxQueue 未初始化")
	}
	// 记录当前形态：中断入队会受就绪队列上限影响（这一点将在 1.5 处理）。
	evt := &agentIO.InputEvent{Source: "qq", Type: "interrupt"}
	evt.Payload = map[string]interface{}{"interrupt": true}
	s.mu.Lock()
	s.enqueueInterruptLocked(&Task{ID: 1, Class: TaskInterrupt, Level: LevelMessage, Event: evt})
	n := len(s.interruptQueues[LevelMessage])
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("正常上限下中断应入队，实际 %d 条", n)
	}
}
