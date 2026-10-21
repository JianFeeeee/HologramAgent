package core

// 回归测试：安全点「重新求值」、抢占计数语义、停机补终态、背压计数。
//
// 对照设计稿原文修正的四条：
//
//  1. §4.3/§5.2 —— 临界区期间到达的抢占请求「不丢失：按级别进入中断队列，
//     在**临界区结束后的第一个安全点重新求值**」。实现里此前没有这一步：
//     唯一的武装点是 registerInterrupt，凡被拦成「入队」的中断只能等当前任务
//     **自然结束**。可复现症状：WebUI 终止按钮连按两次，第二次落在 2s 抢占冷却
//     窗内 → 入队 → 再也不会被求值，「终止」看起来没反应。
//  2. SchedulerStats.PreemptsByLevel 的语义是「判定可抢占**并进入 immediate** 的
//     次数」；此前在 setImmediateLocked 之前就计数，于是同一安全点前到达的两条同级
//     中断里、被降级入队的那条也被计入（immediate 是单槽，降级是设计要求的路径）。
//  3. 状态面 Preempted 此前直接拿 Stats.Suspended 顶替，与 preempts_by_level 自相矛盾。
//  4. §4.4 / §11.4 Q4 —— 就绪队列满必须「阻塞发送方 + **计数并打日志**」。

import (
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// 被冷却拦成入队的中断，在冷却期满后的第一个安全点必须被重新武装。
//
// 这是「终止按钮连按两次」的最小复现：第一次抢占成功（受害者进入 2s 冷却），
// 第二次在冷却窗内只能入队——修复前它就永远等不到执行了。
func TestRearm_CooldownExpiryPromotesQueuedInterrupt(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())

	victim := &Task{ID: 1, Class: TaskInterrupt, Level: LevelBackground, EnqueuedAt: time.Now()}
	a.sched.immediate = victim
	a.sched.nextRef() // running = victim
	// 模拟「刚被抢占过」：冷却起点就在此刻，且抢占提升已生效（有效级 L2）。
	victim.PreemptCount = 1
	victim.LastPreemptAt = time.Now()

	evt, _ := textEvent("cli", "第二次终止")
	evt.Payload["interrupt"] = true
	if a.sched.requestPreempt(evt, LevelCritical) {
		t.Fatal("抢占冷却期内不得抢占（应入队）")
	}
	if got := a.sched.stats.PreemptsByLevel[LevelCritical]; got != 0 {
		t.Fatalf("被冷却拦成入队的中断不得计入抢占数，实际 %d", got)
	}
	if n := len(a.sched.interruptQueues[LevelCritical]); n != 1 {
		t.Fatalf("应恰好入队一条，实际 %d（>1 说明入队路径重复）", n)
	}

	// 冷却期满 → 安全点的「重新求值」必须把它武装起来（修复前缺失的正是这一步）。
	victim.LastPreemptAt = time.Now().Add(-3 * time.Second)
	a.sched.rearmPending()
	if !a.sched.preemptGrantedFor() {
		t.Fatal("冷却期结束后应重新武装让位信号（设计 §4.3「第一个安全点重新求值」）")
	}
	snap := a.DumpScheduler()
	if snap.Immediate == nil {
		t.Fatal("重新求值后应把该中断提升进 immediate 槽")
	}
	if got := snap.Stats.PreemptsByLevel[LevelCritical]; got != 1 {
		t.Fatalf("真正占住 immediate 才能计一次抢占，实际 %d", got)
	}
	// 重新求值不该把任务复制一份：队列必须空、immediate 恰好一条。
	if n := len(snap.InterruptQueues[LevelCritical]); n != 0 {
		t.Fatalf("提升后 L4 队列应空，实际 %d", n)
	}
}

// 同一安全点前到达的两条同级中断：immediate 是单槽，第二条只能降级入队；
// 它**没有**进入 immediate，因此不得计入 PreemptsByLevel，也不得被入队两次。
func TestRearm_SameLevelSecondPreempterIsQueuedNotCounted(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())

	victim := &Task{ID: 1, Class: TaskQueued, EnqueuedAt: time.Now()}
	a.sched.immediate = victim
	a.sched.nextRef() // running = 排队任务（有效级 0，任何中断都能抢）

	e1, _ := textEvent("cli", "irq-1")
	if !a.sched.requestPreempt(e1, LevelInteractive) {
		t.Fatal("第一条 L3 应抢占排队任务")
	}
	e2, _ := textEvent("cli", "irq-2")
	a.sched.requestPreempt(e2, LevelInteractive) // 同级 → 降级入队

	snap := a.DumpScheduler()
	if got := snap.Stats.PreemptsByLevel[LevelInteractive]; got != 1 {
		t.Fatalf("被降级的同级第二条不得计入抢占数（期望 1，实际 %d）", got)
	}
	if n := len(snap.InterruptQueues[LevelInteractive]); n != 1 {
		t.Fatalf("被降级的那条应在 L3 队列里**恰好**出现一次，实际 %d", n)
	}
	if snap.Immediate == nil {
		t.Fatal("第一条应留在 immediate 槽，两条都不能丢")
	}
}

// 状态面 Preempted 必须是「各级抢占数之和」，不能拿 Suspended 顶替。
func TestStatus_PreemptedEqualsSumOfLevels(t *testing.T) {
	a := New(AgentConfig{ID: "rt-sum", ProviderManager: agentAPI.NewProviderManager(), IO: agentIO.NewIOManager()})
	if a.sched == nil {
		t.Fatal("agent 应带调度器")
	}
	a.sched.mu.Lock()
	a.sched.stats.PreemptsByLevel[LevelBackground] = 2
	a.sched.stats.PreemptsByLevel[LevelInteractive] = 3
	a.sched.stats.Suspended = 99 // 故意与抢占数不等
	a.sched.mu.Unlock()

	got := a.schedulerStatus()
	if got.Preempted != 5 {
		t.Fatalf("Preempted 应为各级抢占数之和 5，实际 %d（拿 Suspended 顶替会得 99）", got.Preempted)
	}
	if got.Suspended != 99 {
		t.Fatalf("Suspended 应原样透传，实际 %d", got.Suspended)
	}
}

// 停机必须给「从未运行」与「已挂起」的同步任务补终态，
// 否则 cli / clawhubadapter 这类无超时的同步注入方会永久挂起（设计 §7 I5、§11.3 X2/X4）。
func TestStop_DrainsPendingSyncTasks(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())

	queuedEvt, queuedCh := textEvent("cli", "排队中，永远不会被调度")
	if !a.sched.enqueue(newInputTask(queuedEvt)) {
		t.Fatal("入队失败")
	}
	suspEvt, suspCh := textEvent("cli", "已挂起，停机时不会恢复")
	a.sched.suspend(
		&Task{ID: 2, Class: TaskInterrupt, Level: LevelInteractive, EnqueuedAt: time.Now(), Event: suspEvt},
		a.newTaskFrame("挂起", a.stageCtxFromInput("挂起", "", "")),
	)

	a.Stop()

	for name, ch := range map[string]chan *agentIO.OutputEvent{"排队": queuedCh, "挂起": suspCh} {
		select {
		case r := <-ch:
			if r == nil || r.Payload["skipped"] != true {
				t.Fatalf("%s 任务停机时应补 skipped 终态，实际 %+v", name, r)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s 任务停机未补终态（同步调用方会永久挂起）", name)
		}
	}
}

// 背压计数：持续满只报一次「翻转」不发生；计数本身每次都要累加。
func TestBackpressure_CounterAndTransition(t *testing.T) {
	s := newScheduler(1)
	if !s.noteBackpressure() {
		t.Fatal("首次背压应报告「翻转」")
	}
	if s.noteBackpressure() {
		t.Fatal("持续背压不得重复报告翻转（否则日志会被刷爆）")
	}
	if s.stats.Backpressure != 2 {
		t.Fatalf("背压计数应为 2，实际 %d", s.stats.Backpressure)
	}
	s.clearBackpressure()
	if !s.noteBackpressure() {
		t.Fatal("队列恢复后再满应再次报告翻转")
	}
}

// 集成：就绪队列满时 pumpInbox 必须计一次背压（Rejected 保持 0——背压不是丢弃）。
func TestBackpressure_PumpInboxCountsWhenFull(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.sched.maxQueue = 1

	a.io.InjectInput("cli", "text", map[string]interface{}{"content": "第一条"})
	a.io.InjectInput("cli", "text", map[string]interface{}{"content": "第二条"})
	a.pumpInbox()

	snap := a.DumpScheduler()
	if len(snap.Queue) != 1 {
		t.Fatalf("maxQueue=1 时队列应恰好 1 条，实际 %d", len(snap.Queue))
	}
	if snap.Stats.Backpressure == 0 {
		t.Fatal("队列满必须计一次背压（设计 §4.4/Q4：阻塞发送方 + 计数）")
	}
	if snap.Stats.Rejected != 0 {
		t.Fatalf("背压不是丢弃，Rejected 必须保持 0，实际 %d", snap.Stats.Rejected)
	}
}
