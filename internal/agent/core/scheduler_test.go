package core

// M2 验收测试：调度器骨架（就绪队列、选择函数、快照、panic 隔离）。
//
// 设计依据 docs/zh/input-scheduler-design.md §11.4（Q1/Q4）与 §11.5（O1/K1）。

import (
	"fmt"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// 新模型的选择顺序：immediate → 中断队列 L4→L1 → 栈顶(与队头比级别) → 排队 FIFO。
func TestScheduler_SelectionOrder(t *testing.T) {
	s := newScheduler(16)

	// 四条中断队列各放一个，入队顺序与级别相反 —— 验证“按级别扫”而非 FIFO。
	for _, lv := range []Level{LevelBackground, LevelMessage, LevelInteractive, LevelCritical} {
		evt, _ := textEvent("qq", "中断")
		s.registerInterrupt(newInterruptTask(evt, lv))
	}
	// 排队任务两条（无级别，FIFO）。
	s.enqueue(newSelfTask(selfInputMsg{text: "q1"}))
	s.enqueue(newSelfTask(selfInputMsg{text: "q2"}))

	var order []Level
	for i := 0; i < 4; i++ {
		task, _, kind := s.nextRef()
		if kind != nextInterrupt {
			t.Fatalf("第 %d 个应来自中断队列，kind=%v", i+1, kind)
		}
		order = append(order, task.Level)
		s.done(task)
	}
	want := []Level{LevelCritical, LevelInteractive, LevelMessage, LevelBackground}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("中断执行顺序=%v，期望 %v", order, want)
		}
	}

	// 中断耗尽后才是排队任务，且保持 FIFO。
	for i := 1; i <= 2; i++ {
		task, _, kind := s.nextRef()
		if kind != nextReady {
			t.Fatalf("中断耗尽后应取排队任务，kind=%v", kind)
		}
		if task.Self.text != fmt.Sprintf("q%d", i) {
			t.Fatalf("排队任务应 FIFO，第 %d 个=%q", i, task.Self.text)
		}
		s.done(task)
	}
	if _, _, kind := s.nextRef(); kind != nextNone {
		t.Fatal("全空后应返回 nextNone")
	}
}

// immediate（刚抢占成功的中断）必须最先运行——哪怕队列里有更高级别的待处理中断。
// 这是“抢占立即生效”的实现方式，也是它不需要和栈顶比级别的原因。
func TestScheduler_ImmediateWins(t *testing.T) {
	s := newScheduler(16)
	evt1, _ := textEvent("cli", "L4 待处理")
	s.registerInterrupt(newKernelInterruptTask(evt1))

	evt2, _ := textEvent("qq", "抢占者")
	preemptor := newInterruptTask(evt2, LevelBackground)
	s.mu.Lock()
	s.setImmediateLocked(preemptor)
	s.mu.Unlock()

	task, _, kind := s.nextRef()
	if kind != nextImmediate || task != preemptor {
		t.Fatalf("immediate 必须先运行，kind=%v", kind)
	}
}

// 中断队列头与中断栈顶比级别，取高者；栈顶是排队任务（无级别）时任何中断都赢。
func TestScheduler_StackTopVsInterruptQueue(t *testing.T) {
	s := newScheduler(16)
	// 直接构造挂起现场：不走 suspend()，避免 PreemptCount/冷却干扰本用例
	// （本用例只测“选择顺序”这一件事）。
	pushSuspended := func(id uint64, class TaskClass, lv Level) {
		s.mu.Lock()
		s.suspendStack = append(s.suspendStack, &suspendedTask{
			Task: &Task{ID: id, Class: class, Level: lv}, Frame: &TaskFrame{},
		})
		s.mu.Unlock()
	}
	// 每次选取后清掉 running，让下一次 registerInterrupt 不把它当成运行任务。
	clearRunning := func() {
		s.mu.Lock()
		s.running = nil
		s.mu.Unlock()
	}

	// 栈顶 L3，队列只有 L2 → 恢复栈顶。
	pushSuspended(1, TaskInterrupt, LevelInteractive)
	evt, _ := textEvent("qq", "L2 待处理")
	s.registerInterrupt(newInterruptTask(evt, LevelMessage))
	if _, _, kind := s.nextRef(); kind != nextSuspended {
		t.Fatalf("栈顶 L3 > 队头 L2 → 应恢复栈顶，kind=%v", kind)
	}
	clearRunning()

	// 栈顶 L3，队列来了 L4 → 队头优先。
	pushSuspended(2, TaskInterrupt, LevelInteractive)
	evt2, _ := textEvent("cli", "L4 待处理")
	s.registerInterrupt(newKernelInterruptTask(evt2))
	if _, _, kind := s.nextRef(); kind != nextInterrupt {
		t.Fatalf("队头 L4 > 栈顶 L3 → 应先取中断，kind=%v", kind)
	}
	clearRunning()

	// 栈顶是排队任务（无级别）→ 任何中断都赢。
	pushSuspended(3, TaskQueued, 0)
	evt3, _ := textEvent("qq", "L1 待处理")
	s.registerInterrupt(newInterruptTask(evt3, LevelBackground))
	if _, _, kind := s.nextRef(); kind != nextInterrupt {
		t.Fatalf("排队栈顶可被任何中断打断，kind=%v", kind)
	}
}

// Q4：队列有界；满了必须拒绝并计数，而不是静默丢弃或无界增长。
func TestScheduler_EnqueueBackpressure(t *testing.T) {
	s := newScheduler(2)
	if !s.enqueue(newSelfTask(selfInputMsg{text: "a"})) {
		t.Fatal("第 1 个任务应入队成功")
	}
	if !s.enqueue(newSelfTask(selfInputMsg{text: "b"})) {
		t.Fatal("第 2 个任务应入队成功")
	}
	if s.hasRoom() {
		t.Fatal("队列已满，hasRoom 应为 false")
	}
	if s.enqueue(newSelfTask(selfInputMsg{text: "c"})) {
		t.Fatal("队列满时第 3 个任务必须被拒绝")
	}
	if s.stats.Rejected != 1 {
		t.Fatalf("Rejected=%d，期望 1", s.stats.Rejected)
	}
	if s.stats.Enqueued != 2 {
		t.Fatalf("Enqueued=%d，期望 2", s.stats.Enqueued)
	}
}

// 生命周期：next 置 running 并移出队列；done 清 running 并累加计数。
func TestScheduler_Lifecycle(t *testing.T) {
	s := newScheduler(4)
	s.enqueue(newSelfTask(selfInputMsg{text: "a"}))
	s.enqueue(newSelfTask(selfInputMsg{text: "b"}))

	t1 := s.next()
	if t1 == nil || s.running != t1 {
		t.Fatal("next 应取出任务并置为 running")
	}
	if n := s.queueLen(); n != 1 {
		t.Fatalf("取出后队列长度=%d，期望 1", n)
	}
	// 队列内不得同时出现 running（O1：三集合互不重叠）。
	for _, q := range s.queueSnapshot() {
		if q == t1 {
			t.Fatal("running 任务不得同时留在就绪队列")
		}
	}

	s.done(t1)
	if s.running != nil {
		t.Fatal("done 后 running 应为 nil")
	}
	if s.stats.Executed != 1 {
		t.Fatalf("Executed=%d，期望 1", s.stats.Executed)
	}
	if s.next() == nil {
		t.Fatal("队列里还有 b，next 不应为 nil")
	}
	if s.next() != nil {
		t.Fatal("队列已空，next 应返回 nil")
	}
}

// K1：任务 panic 必须被隔离——调度器统计仍然推进，且不向外抛出。
func TestScheduler_PanicIsolationOnExecuteTask(t *testing.T) {
	sp := &scriptProvider{}
	a := New(AgentConfig{
		ID:              "sched-panic",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
	})

	// Event 为 nil：handleInput 解引用即 panic，用来验证 recover 生效。
	task := &Task{Kind: TaskKindInput, Level: DefaultLevel, Event: nil}
	a.executeTask(task) // 若未隔离，这里会 panic 冒泡使测试失败

	if a.sched.stats.Executed != 1 {
		t.Fatalf("panic 后 Executed=%d，期望 1（任务失败但调度器存活）", a.sched.stats.Executed)
	}
	if a.sched.running != nil {
		t.Fatal("panic 后 running 必须被清空")
	}
}

// O1 轻量版：快照与内部状态一致，且 running 不出现在 queue 里。
func TestScheduler_SnapshotConsistency(t *testing.T) {
	sp := &scriptProvider{}
	a := New(AgentConfig{
		ID:              "sched-snap",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
	})

	a.sched.enqueue(newSelfTask(selfInputMsg{text: "a"}))
	a.sched.enqueue(newSelfTask(selfInputMsg{text: "b"}))
	snap := a.DumpScheduler()
	if snap.Running != nil {
		t.Fatal("尚未 next，快照的 running 应为 nil")
	}
	if len(snap.Queue) != 2 || snap.Stats.Enqueued != 2 {
		t.Fatalf("快照不一致：queue=%d enqueued=%d", len(snap.Queue), snap.Stats.Enqueued)
	}

	r := a.sched.next()
	a.executeTask(&Task{Kind: TaskKindSelf, Level: DefaultLevel, Self: selfInputMsg{text: "a"}})
	snap = a.DumpScheduler()
	if snap.Running != r {
		t.Fatal("执行完成后 running 应仍指向未 done 的任务")
	}
	for _, q := range snap.Queue {
		if q == r {
			t.Fatal("快照中 running 与 queue 不得重叠")
		}
	}

	// 队列快照必须是副本：改快照不得影响调度器。
	snap.Queue = append(snap.Queue, &Task{})
	if len(a.DumpScheduler().Queue) != 1 {
		t.Fatal("DumpScheduler 必须返回队列副本")
	}
}

// Level 的字面量是持久化/日志契约，改值必须是有意的。
func TestLevelContract(t *testing.T) {
	if LevelBackground != 1 || LevelMessage != 2 || LevelInteractive != 3 || LevelCritical != 4 {
		t.Fatalf("四级取值被改动：%d/%d/%d/%d",
			LevelBackground, LevelMessage, LevelInteractive, LevelCritical)
	}
	if DefaultLevel != LevelBackground {
		t.Fatalf("默认级必须是 L1（显式才是特权），实际 %v", DefaultLevel)
	}
}
