package core

// M7 验收测试：可观测性 + 压力 + 端到端。
//
// 设计依据 docs/zh/input-scheduler-design.md §11.5（O1/O2）、§11.6（E1/E2）。
//
// 这一组与前几组的区别：前几组直接驱动调度器（确定性、可断言内部状态），
// 这一组**完整启动** schedulerLoop + interceptLoop，经真实 channel 投递，
// 验证组装后的行为与不变量。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/events"
)

// countingProvider 只统计调用次数，永远成功。
type countingProvider struct{ n atomic.Int64 }

func (p *countingProvider) Name() string { return "counting" }
func (p *countingProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.n.Add(1)
	return &agentAPI.CompletionResponse{Content: "ok"}, nil
}
func (p *countingProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return nil, errors.New("counting provider: no stream")
}
func (p *countingProvider) MaxContextTokens() int { return 8192 }

func waitQuiescent(t *testing.T, a *Agent, wantExecuted uint64, timeout time.Duration) SchedulerSnapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		snap := a.DumpScheduler()
		if snap.Running == nil && len(snap.Queue) == 0 &&
			len(snap.PendingInterrupts) == 0 && len(snap.SuspendStack) == 0 &&
			snap.Stats.Executed >= wantExecuted {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("未在 %v 内排空：running=%v queue=%d pending=%d suspend=%d executed=%d",
				timeout, snap.Running != nil, len(snap.Queue), len(snap.PendingInterrupts),
				len(snap.SuspendStack), snap.Stats.Executed)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 压力：N 个排队输入 + M 个中断，全部经真实 loop 执行，结束时三集合必须排空。
func TestScheduler_StressMixedLoad(t *testing.T) {
	sp := &countingProvider{}
	a := New(AgentConfig{
		ID:              "stress",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	a.Start()
	defer a.Stop()

	const nInputs = 200
	const nInterrupts = 50

	for i := 0; i < nInputs; i++ {
		a.io.InjectInput("cli", "text", map[string]interface{}{"content": fmt.Sprintf("msg-%d", i)})
	}
	// 中断按 L1/L2/L3 轮转：把“四条中断队列按级别高→低扫描”真正压上，
	// 而不只是排空一条队列。
	levels := []string{"L1", "L2", "L3"}
	for i := 0; i < nInterrupts; i++ {
		a.io.InjectInterruptTextOpts("qq", "cli", fmt.Sprintf("intr-%d", i),
			agentIO.InjectOptions{Priority: levels[i%len(levels)]})
	}

	snap := waitQuiescent(t, a, nInputs+nInterrupts, 30*time.Second)

	if got := sp.n.Load(); got != int64(nInputs+nInterrupts) {
		t.Fatalf("LLM 调用=%d，期望 %d（每条输入/中断恰好一次）", got, nInputs+nInterrupts)
	}
	if snap.Stats.Rejected != 0 {
		t.Fatalf("容量充足却出现 Rejected=%d，说明背压/深度判定有误", snap.Stats.Rejected)
	}
	// 上次快照的计数在排空后应当稳定（不丢不重）：等于入队后的执行数。
	if snap.Stats.Executed != uint64(nInputs+nInterrupts) {
		t.Fatalf("Executed=%d，期望 %d", snap.Stats.Executed, nInputs+nInterrupts)
	}
}

// O2：每次挂起/恢复都产生一条 scheduler 事件。
func TestObservability_SchedulerEventsAndStatus(t *testing.T) {
	bus := events.NewBus()
	var mu sync.Mutex
	var actions []string
	bus.Subscribe(events.EventScheduler, func(e *events.Event) {
		mu.Lock()
		actions = append(actions, fmt.Sprint(e.Payload["action"]))
		mu.Unlock()
	})

	sp := newPreemptProvider("intr-done", "low-done")
	a := New(AgentConfig{
		ID:              "obs",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
		EventBus:        bus,
	})

	// 直接驱动一次抢占-挂起-恢复（与 M3b 相同的手法）。
	lowEvt, _ := textEvent("qq", "低优先级")
	lowTask := &Task{Kind: TaskKindInput, Level: LevelBackground, Event: lowEvt, EnqueuedAt: time.Now()}
	a.sched.enqueue(lowTask)
	lt, _, _ := a.sched.nextRef()

	done := make(chan struct{})
	go func() { a.executeNewTask(lt); close(done) }()
	select {
	case <-sp.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider 未进入")
	}

	intrEvt, _ := textEvent("cli", "紧急")
	intrEvt.Payload["interrupt"] = true
	a.sched.requestKernelPreempt(intrEvt)
	a.cancelCurrentLLM()
	<-done

	it, _, _ := a.sched.nextRef()
	a.executeNewTask(it)
	rt, rf, _ := a.sched.nextRef()
	a.resumeTask(rt, rf)

	mu.Lock()
	got := strings.Join(actions, ",")
	mu.Unlock()
	if !strings.Contains(got, "suspend") || !strings.Contains(got, "resume") {
		t.Fatalf("调度事件缺失：%q", got)
	}

	// 状态快照（供状态页/诊断）：计数一致、三集合为空。
	st := a.GetKernelStatus().Scheduler
	if st.SuspendStack != 0 || st.PendingInterrupts != 0 || st.ReadyQueueDepth != 0 {
		t.Fatalf("排空后状态非空：%+v", st)
	}
	if st.Suspended == 0 || st.Resumed == 0 {
		t.Fatalf("挂起/恢复计数缺失：%+v", st)
	}
	if st.Executed < 2 {
		t.Fatalf("Executed=%d，期望 >=2", st.Executed)
	}
	if st.MaxSuspendDepth != 4 {
		t.Fatalf("MaxSuspendDepth=%d，期望 4", st.MaxSuspendDepth)
	}
}

// E1/E2：完整启动 loop，经真实 channel 投递 L1 任务与 L4 中断，
// 断言「LLM 流式中断 → 挂起 → 中断先完成 → 原任务恢复」的整条链路。
func TestE2E_RealLoopPreemption(t *testing.T) {
	bus := events.NewBus()
	var mu sync.Mutex
	var actions []string
	bus.Subscribe(events.EventScheduler, func(e *events.Event) {
		mu.Lock()
		actions = append(actions, fmt.Sprint(e.Payload["action"]))
		mu.Unlock()
	})

	sp := newPreemptProvider("intr-done", "low-done")
	a := New(AgentConfig{
		ID:              "e2e",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
		EventBus:        bus,
	})
	a.Start()
	defer a.Stop()

	// 排队输入：qq 入站消息 → 阻塞在第一次 LLM 调用（排队任务无级别）
	a.io.InjectInput("qq", "text", map[string]interface{}{"content": "长任务"})
	select {
	case <-sp.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("低优先级任务未进入 LLM")
	}

	// 插件声明的 L3 中断：interceptLoop 应取消 LLM 并登记抢占。
	// 注意它能打断**排队任务**不是因为级别高，而是因为排队任务无级别——
	// 任何中断都大于它。
	a.io.InjectInterruptTextOpts("cli", "cli", "紧急打断", agentIO.InjectOptions{Priority: "L3"})
	a.io.InjectInput("cli", "text", map[string]interface{}{"content": "后续常规输入"})

	// 排空：中断任务 + 被恢复的原任务 + 后续常规输入
	snap := waitQuiescent(t, a, 3, 15*time.Second)

	mu.Lock()
	got := strings.Join(actions, ",")
	mu.Unlock()
	if !strings.Contains(got, "suspend") || !strings.Contains(got, "resume") {
		t.Fatalf("E2E 未发生抢占-挂起-恢复：%q", got)
	}
	if snap.Stats.Executed < 3 {
		t.Fatalf("Executed=%d，期望 >=3", snap.Stats.Executed)
	}
	// 第一次 LLM 调用被丢弃 + 中断 1 + 恢复 1 + 常规输入 1 = 4
	if sp.callCount() != 4 {
		t.Fatalf("LLM 调用=%d，期望 4（丢弃 1 + 中断 1 + 恢复 1 + 常规 1）", sp.callCount())
	}
}
