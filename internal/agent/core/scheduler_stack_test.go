package core

// 中断栈（嵌套抢占）验收测试。
//
// 用户明确：存在**中断被中断**的场景，所以被打断的现场要压进**中断栈**。
// 因此恢复纪律是**严格 LIFO（只比栈顶）**，而不是“全栈按优先级挑最优”。
//
// 为什么这个区别成立：抢占判据是 adopted.level > effectiveLevel(running)，
// 所以嵌套时栈自底向上的**基础级**天然递增；但饥饿防护的“有效级提升”会让
// 栈内某个更老的任务有效级超过栈顶，此时“只比栈顶”才保证嵌套语义不被破坏。

import (
	"context"
	"sync"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// nestingProvider 第 1、2 次调用阻塞到 ctx 取消；第 3 次起按脚本返回。
// 用序号精确对应 A（L1）→ B（L2）→ C（L3）→ 恢复 B → 恢复 A 的调用顺序。
type nestingProvider struct {
	mu      sync.Mutex
	calls   int
	entered chan int
	script  []string
}

func newNestingProvider(script ...string) *nestingProvider {
	return &nestingProvider{entered: make(chan int, 16), script: script}
}

func (p *nestingProvider) Name() string { return "nesting" }
func (p *nestingProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()

	p.entered <- n
	if n <= 2 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	i := n - 3
	if i < len(p.script) {
		return &agentAPI.CompletionResponse{Content: p.script[i]}, nil
	}
	return &agentAPI.CompletionResponse{Content: "?"}, nil
}
func (p *nestingProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return nil, context.Canceled
}
func (p *nestingProvider) MaxContextTokens() int { return 8192 }

func awaitEnter(t *testing.T, ch chan int, want int) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("LLM 进入序号=%d，期望 %d", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("等第 %d 次 LLM 调用超时", want)
	}
}

// 中断被中断：A(L1) → B(L2) → C(L3)，恢复必须按 LIFO（B 先，A 后）。
func TestStack_NestedPreemptionResumesLIFO(t *testing.T) {
	sp := newNestingProvider("c-done", "b-done", "a-done")
	a := newPreemptAgent(t, sp)

	// A（L1）开始运行
	if _, _ = enqueueQueued(t, a, "qq", "任务A"); true {
	}
	at, _, _ := a.sched.nextRef()
	doneA := make(chan struct{})
	go func() { a.executeNewTask(at); close(doneA) }()
	awaitEnter(t, sp.entered, 1)

	// B（L2）抢占 A
	bEvt, _ := textEvent("qq", "任务B")
	if !a.sched.requestPreempt(bEvt, LevelMessage) {
		t.Fatal("B(L2) 应抢占 A(L1)")
	}
	a.cancelCurrentLLM()
	<-doneA
	if n := len(a.DumpScheduler().SuspendStack); n != 1 {
		t.Fatalf("第一次抢占后栈深=%d，期望 1", n)
	}

	// B 开始运行
	bt, _, k := a.sched.nextRef()
	if k != nextImmediate || bt.Level != LevelMessage {
		t.Fatalf("应取到 B（pending），kind=%v level=%v", k, bt.Level)
	}
	doneB := make(chan struct{})
	go func() { a.executeNewTask(bt); close(doneB) }()
	awaitEnter(t, sp.entered, 2)

	// C（L3）抢占 B —— 这就是“中断被中断”
	cEvt, _ := textEvent("cli", "任务C")
	if !a.sched.requestPreempt(cEvt, LevelInteractive) {
		t.Fatal("C(L3) 应抢占 B(L2)")
	}
	a.cancelCurrentLLM()
	<-doneB

	snap := a.DumpScheduler()
	if len(snap.SuspendStack) != 2 {
		t.Fatalf("嵌套后栈深=%d，期望 2", len(snap.SuspendStack))
	}
	if snap.SuspendStack[0].Task.Class != TaskQueued {
		t.Fatalf("栈底应为排队任务 A（无级别），实际 %v", snap.SuspendStack[0].Task.Class)
	}
	if snap.SuspendStack[1].Task.Level != LevelMessage {
		t.Fatalf("栈顶应为 B(L2)，实际 %v", snap.SuspendStack[1].Task.Level)
	}

	// C 运行完毕（第三次调用，不阻塞）
	ct, _, k := a.sched.nextRef()
	if k != nextImmediate || ct.Level != LevelInteractive {
		t.Fatalf("应取到 C，kind=%v level=%v", k, ct.Level)
	}
	a.executeNewTask(ct)

	// LIFO：先恢复栈顶 B，再恢复 A
	rt, rf, k := a.sched.nextRef()
	if k != nextSuspended {
		t.Fatalf("应恢复栈顶，kind=%v", k)
	}
	if rt.Level != LevelMessage {
		t.Fatalf("应先恢复栈顶 B(L2)，实际 %v", rt.Level)
	}
	a.resumeTask(rt, rf)

	rt2, rf2, k2 := a.sched.nextRef()
	if k2 != nextSuspended {
		t.Fatalf("应继续恢复 A，kind=%v", k2)
	}
	if rt2.Class != TaskQueued {
		t.Fatalf("最后应恢复排队的 A（无级别），实际 %v", rt2.Class)
	}
	a.resumeTask(rt2, rf2)

	if n := len(a.DumpScheduler().SuspendStack); n != 0 {
		t.Fatalf("全部恢复后栈应清空，实际 %d", n)
	}
}

// 只比栈顶：栈内更老的任务即使（因有效级提升）优先级更高，也不得越过栈顶。
func TestStack_TopOnlyWinsOverHigherPrioritySuspended(t *testing.T) {
	a := newPreemptAgent(t, &scriptProvider{})

	// 人为构造“A(L3) 在栈底、B(L2) 在栈顶”。真实抢占不会产生这种顺序
	// （栈自底向上基础级递增），这里专门用来区分两种实现：
	//   · 只比栈顶  → 取 B
	//   · 全栈扫最优 → 取 A（L3 > L2）
	a.sched.suspend(&Task{ID: 1, Class: TaskInterrupt, Level: LevelInteractive, EnqueuedAt: time.Now()},
		a.newTaskFrame("A", a.stageCtxFromInput("A", "", "")))
	a.sched.suspend(&Task{ID: 2, Class: TaskInterrupt, Level: LevelMessage, EnqueuedAt: time.Now()},
		a.newTaskFrame("B", a.stageCtxFromInput("B", "", "")))

	rt, _, k := a.sched.nextRef()
	if k != nextSuspended {
		t.Fatalf("kind=%v，期望 nextSuspended", k)
	}
	if rt.ID != 2 {
		t.Fatalf("应取栈顶 B(ID=2)，实际 ID=%d —— 说明在做全栈优先级扫描而非栈语义", rt.ID)
	}
	if n := len(a.DumpScheduler().SuspendStack); n != 1 {
		t.Fatalf("取出栈顶后栈深=%d，期望 1", n)
	}
}

// 深度上限对嵌套同样成立：到顶后新的抢占请求不再下潜。
func TestStack_DepthCapDuringNesting(t *testing.T) {
	a := newPreemptAgent(t, &scriptProvider{})
	frame := func() *TaskFrame { return a.newTaskFrame("x", a.stageCtxFromInput("x", "", "")) }
	for i := 0; i < a.sched.maxInterruptFrames; i++ {
		a.sched.suspend(&Task{ID: uint64(i + 1), Class: TaskInterrupt, Level: Level(i + 1)}, frame())
	}
	if a.sched.canSuspend() {
		t.Fatal("栈已满，canSuspend 应为 false")
	}
	if n := len(a.DumpScheduler().SuspendStack); n != a.sched.maxInterruptFrames {
		t.Fatalf("栈深=%d，期望上界 %d", n, a.sched.maxInterruptFrames)
	}
}
