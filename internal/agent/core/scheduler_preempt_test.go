package core

// M3b 验收测试：四级优先级 + 严格大于抢占 + 现场保存/恢复 + suspendStack。
//
// 设计依据 docs/zh/input-scheduler-design.md §11（P1–P4、R1、R5、D1T、Q3）。
//
// 测试手法：用一个「第一次调用阻塞到 ctx 取消、之后按脚本返回」的 provider，
// 让测试可以确定性地把运行任务停在 S_LLM 上，再注入中断观察让位与恢复。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// preemptProvider 第 1 次 Chat 阻塞直到 ctx 取消；第 2 次起返回脚本。
type preemptProvider struct {
	mu        sync.Mutex
	calls     int
	entered   chan struct{}
	enteredOn sync.Once
	responses []string
}

func newPreemptProvider(responses ...string) *preemptProvider {
	return &preemptProvider{entered: make(chan struct{}), responses: responses}
}

func (p *preemptProvider) Name() string { return "preempt" }

func (p *preemptProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()

	if n == 1 {
		p.enteredOn.Do(func() { close(p.entered) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	i := n - 2
	if i < len(p.responses) {
		return &agentAPI.CompletionResponse{Content: p.responses[i]}, nil
	}
	return &agentAPI.CompletionResponse{Content: "done"}, nil
}

func (p *preemptProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return nil, context.Canceled
}
func (p *preemptProvider) MaxContextTokens() int { return 8192 }

func (p *preemptProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newPreemptAgent(t *testing.T, sp agentAPI.Provider) *Agent {
	t.Helper()
	return New(AgentConfig{
		ID:              "preempt",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
}

// enqueueQueued 入队一个**排队任务**（无级别）——对应 InjectText*/InjectInputSync*。
func enqueueQueued(t *testing.T, a *Agent, source, content string) (*Task, *agentIO.InputEvent) {
	t.Helper()
	evt, _ := textEvent(source, content)
	task := newInputTask(evt)
	if !a.sched.enqueue(task) {
		t.Fatal("入队失败")
	}
	return task, evt
}

// enqueueInterrupt 登记一次**中断**（走 requestPreempt：能抢占则进 immediate 槽，
// 否则按级别进中断队列），返回被登记的任务。
func enqueueInterrupt(t *testing.T, a *Agent, level Level, source, content string) (*Task, *agentIO.InputEvent) {
	t.Helper()
	evt, _ := textEvent(source, content)
	evt.Payload["interrupt"] = true
	a.sched.requestPreempt(evt, level)
	snap := a.DumpScheduler()
	if snap.Immediate != nil && snap.Immediate.Event == evt {
		return snap.Immediate, evt
	}
	q := snap.InterruptQueues[clampPluginLevel(level)]
	for i := len(q) - 1; i >= 0; i-- {
		if q[i].Event == evt {
			return q[i], evt
		}
	}
	return nil, evt
}

// P1 + R1 + R5 + D1=A：高优先级抢占 → 挂起在 S_LLM → 中断任务带只读前缀 →
// 恢复后从 S_LLM 重发，且原任务的 msgs 未被改动。
func TestPreempt_HigherPreemptsAndResumes(t *testing.T) {
	sp := newPreemptProvider("intr-done", "low-done")
	a := newPreemptAgent(t, sp)

	lowTask, _ := enqueueQueued(t, a, "qq", "低优先级任务")
	lt, _, kind := a.sched.nextRef()
	if kind != nextReady || lt != lowTask {
		t.Fatalf("应取到低优先级任务，kind=%v", kind)
	}

	done := make(chan struct{})
	go func() { a.executeNewTask(lt); close(done) }()

	select {
	case <-sp.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider 第 1 次调用未发生")
	}

	// 注入 L4 中断（cli）
	intrEvt, _ := textEvent("cli", "紧急打断")
	intrEvt.Payload["interrupt"] = true
	if !a.sched.requestKernelPreempt(intrEvt) {
		t.Fatal("L4 应请求抢占并返回 true（应取消 LLM）")
	}
	a.cancelCurrentLLM()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("低优先级任务未在取消后挂起")
	}

	snap := a.DumpScheduler()
	if snap.Running != nil {
		t.Fatal("挂起后不应还有 running")
	}
	if len(snap.SuspendStack) != 1 {
		t.Fatalf("suspendStack=%d，期望 1", len(snap.SuspendStack))
	}
	if len(snap.PendingInterrupts) != 1 {
		t.Fatalf("pendingInterrupts=%d，期望 1", len(snap.PendingInterrupts))
	}
	sf := snap.SuspendStack[0].Frame
	if sf.Terminal != terminalSuspended {
		t.Fatalf("挂起任务终态=%v，期望 terminalSuspended", sf.Terminal)
	}
	if sf.Step != StepLLM {
		t.Fatalf("应在 StepLLM 安全点挂起，实际 step=%v", sf.Step)
	}
	msgsBefore := len(sf.Msgs)

	// R5：第一次 LLM 调用被丢弃，未产生新消息。
	if sp.callCount() != 1 {
		t.Fatalf("挂起前 LLM 调用=%d，期望 1（不完整请求被丢弃）", sp.callCount())
	}

	// Q3：三集合统一比较 → 下一轮取中断（L4 > L1）。
	it, _, k := a.sched.nextRef()
	if k != nextImmediate || it.Level != LevelCritical {
		t.Fatalf("应取到 pending 中断，kind=%v level=%v", k, it.Level)
	}
	// D1=B：中断任务在**上一个任务之前的完整状态**上开始运行，不继承本任务的现场。
	// 因此它能看到的唯一输入就是它自己携带的内容。
	if it.Event == nil {
		t.Fatal("中断任务必须携带自己的输入事件")
	}
	if got, _ := it.Event.Payload["content"].(string); got != "紧急打断" {
		t.Fatalf("中断任务输入=%q，期望 紧急打断", got)
	}
	a.executeNewTask(it)
	if len(a.DumpScheduler().PendingInterrupts) != 0 {
		t.Fatal("中断任务执行后 pendingInterrupts 应清空")
	}

	// R1：恢复被抢占任务。基础前缀被重建到「中断任务之上」（含中断已提交的上下文），
	// 本任务自己的现场接回其后，然后从 S_LLM 重发。
	rt, rf, k2 := a.sched.nextRef()
	if k2 != nextSuspended || rt != lowTask {
		t.Fatalf("应恢复被抢占任务，kind=%v", k2)
	}
	if rf.Step != StepLLM {
		t.Fatalf("恢复游标=%v，期望 StepLLM", rf.Step)
	}
	a.resumeTask(rt, rf)

	// 「加载回中断之上」的判据：恢复后的消息序列里必须出现中断任务的上下文。
	if !msgsContain(rf.Msgs, "紧急打断") {
		t.Fatal("恢复后的任务应看见中断任务的上下文（现场未加载回中断之上）")
	}
	// 本任务自己的现场（工具轮产物）仍然在。
	if len(rf.Msgs) < msgsBefore {
		t.Fatalf("恢复后消息数=%d，不应少于被抢占前的 %d", len(rf.Msgs), msgsBefore)
	}

	if sp.callCount() != 3 {
		t.Fatalf("LLM 总调用=%d，期望 3（丢弃 1 + 中断 1 + 恢复 1）", sp.callCount())
	}
	snap = a.DumpScheduler()
	if len(snap.SuspendStack) != 0 || snap.Running != nil {
		t.Fatalf("全部结束后应无挂起与运行任务：%+v", snap)
	}
	if snap.Stats.Executed != 2 {
		t.Fatalf("Executed=%d，期望 2（中断任务 + 被抢占任务）", snap.Stats.Executed)
	}
}

// P2/P3：同级与更低级都不得抢占，请求进中断队列。
//
// 注意：能比“同级/更低不得抢占”的只可能是**中断之间**——排队任务无级别，
// 任何中断都能打断它（这是模型的规定，不是漏洞）。
func TestPreempt_LowerOrEqualDoesNotPreempt(t *testing.T) {
	sp := newPreemptProvider("low-done", "intr-done")
	a := newPreemptAgent(t, sp)

	lowTask, _ := enqueueInterrupt(t, a, LevelInteractive, "cli", "运行中的 L3")
	if _, _, kind := a.sched.nextRef(); kind != nextInterrupt {
		t.Fatal("应取到运行中的 L3 中断")
	}

	done := make(chan struct{})
	go func() { a.executeNewTask(lowTask); close(done) }()
	select {
	case <-sp.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider 未被调用")
	}

	// 同级 L3
	e1, _ := textEvent("webui", "同级打断")
	if a.sched.requestPreempt(e1, LevelInteractive) {
		t.Fatal("同级不得抢占")
	}
	// 更低级 L1
	e2, _ := textEvent("system", "低优先级打断")
	if a.sched.requestPreempt(e2, LevelBackground) {
		t.Fatal("更低级不得抢占")
	}
	if a.sched.preemptGrantedFor() {
		t.Fatal("未 arm 让位信号，preemptGrantedFor 应为 false")
	}

	// 运行任务没有被取消，仍在等它自己的 ctx；用取消让它收尾，便于清理。
	a.cancelCurrentLLM()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("运行任务未结束")
	}
	if len(a.DumpScheduler().PendingInterrupts) != 2 {
		t.Fatalf("两条未抢占中断都应保留在中断队列，实际 %d",
			len(a.DumpScheduler().PendingInterrupts))
	}
}

// D1T：suspendStack 满时不再下潜（canSuspend=false），让位信号也不会 arm。
func TestPreempt_DepthCapBlocksSuspension(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())

	if a.sched.maxInterruptFrames != 4 {
		t.Fatalf("默认栈深上界=%d，期望 4（= 中断级数，结构推论）", a.sched.maxInterruptFrames)
	}
	frame := func() *TaskFrame { return a.newTaskFrame("x", a.stageCtxFromInput("x", "", "")) }
	for i := 0; i < a.sched.maxInterruptFrames; i++ {
		a.sched.suspend(&Task{ID: uint64(i + 1), Class: TaskInterrupt, Level: LevelBackground}, frame())
	}
	if a.sched.canSuspend() {
		t.Fatal("深度已达上限，canSuspend 应为 false")
	}
	// 超限兜底：仍保留帧（不丢副作用记录），但计数 Rejected。
	before := a.DumpScheduler().Stats.Rejected
	a.sched.suspend(&Task{ID: 99, Class: TaskInterrupt, Level: LevelBackground}, frame())
	if a.DumpScheduler().Stats.Rejected != before+1 {
		t.Fatal("超限挂起必须计数 Rejected")
	}
	if len(a.DumpScheduler().SuspendStack) != a.sched.maxInterruptFrames+1 {
		t.Fatal("兜底路径必须保留帧而不是丢弃")
	}
}

// 中断不丢：空闲时请求抢占 → 不 arm 信号，但请求进 pendingInterrupts 并被选出。
func TestPreempt_IdleInterruptIsQueuedNotLost(t *testing.T) {
	a := newPreemptAgent(t, &scriptProvider{script: []*agentAPI.CompletionResponse{{Content: "已处理中断"}}})

	evt, respCh := textEvent("qq", "空闲时的中断")
	if a.sched.requestPreempt(evt, LevelMessage) {
		t.Fatal("空闲时不应请求取消 LLM（没有运行任务）")
	}
	if len(a.DumpScheduler().PendingInterrupts) != 1 {
		t.Fatal("空闲时的中断必须进中断队列（不能丢）")
	}
	task, _, kind := a.sched.nextRef()
	if kind != nextInterrupt || task.Level != LevelMessage {
		t.Fatalf("应取到待处理中断，kind=%v", kind)
	}
	a.executeNewTask(task)
	if len(respCh) != 1 {
		t.Fatal("中断任务应完成并回执")
	}
}

// msgsContain 报告消息序列里是否出现过某段文本（用于“现场是否合回”的断言）。
func msgsContain(msgs []agentAPI.Message, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, sub) {
			return true
		}
	}
	return false
}

// 恢复时的重建必须把 prepare 段对尾部消息的两处改写补回：
// 中断标记（IsInterrupt）与多模态块（InputBlocks）。
func TestPreempt_ResumeRebaseRestoresTailDecorations(t *testing.T) {
	sp := &scriptProvider{script: []*agentAPI.CompletionResponse{{Content: "ok"}}}
	a := newPreemptAgent(t, sp)

	block := agentAPI.ContentBlock{Type: "text", Text: "图"}
	f := a.newTaskFrame("打断文本", a.stageCtxFromInput("打断文本", "", ""))
	f.IsInterrupt = true
	f.InputBlocks = []agentAPI.ContentBlock{block}
	// 模拟 prepare 后的形状：基础前缀 + 一段“本任务自己的现场”
	f.Msgs = []agentAPI.Message{
		{Role: "system", Content: "S"},
		{Role: "user", Content: "[中断消息] 打断文本"},
		{Role: "assistant", Content: "进行中"},
	}
	f.PrefixLen = 2

	a.rebaseFramePrefix(f)

	if f.PrefixLen <= 0 || f.PrefixLen >= len(f.Msgs) {
		t.Fatalf("重建后 PrefixLen=%d，消息数=%d，前缀应短于总数", f.PrefixLen, len(f.Msgs))
	}
	// 尾部现场（assistant 进行中）必须还在最后。
	if last := f.Msgs[len(f.Msgs)-1]; last.Content != "进行中" {
		t.Fatalf("本任务现场应接回最后，实际 %+v", last)
	}
	prefixLast := f.Msgs[f.PrefixLen-1]
	if prefixLast.Role != "system" || !strings.HasPrefix(prefixLast.Content, "[中断消息]") {
		t.Fatalf("中断标记未补回：%+v", prefixLast)
	}
	if len(prefixLast.Blocks) != 1 || prefixLast.Blocks[0].Text != "图" {
		t.Fatalf("多模态块未补回：%+v", prefixLast.Blocks)
	}
}
