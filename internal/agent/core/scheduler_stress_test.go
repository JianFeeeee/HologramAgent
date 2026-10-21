package core

// 优先级压力测试：**各级中断混合打入 + 排队输入**，全部经真实调度 loop 执行。
//
// 形状（用户指定）：100 条中断（L1/L2/L3/L4 各 25，混合打入）+ 100 条排队输入。
//
// 为什么需要「等待合适的受害者再注入」：调度器是单线程的，同一时刻只有**一个**
// 运行任务。如果闭着眼睛猛灌，绝大多数中断会落在「没有受害者」或「受害者级别
// 不够」的时刻，于是全部退化成排队——压力测试就只压到了队列，没有压到抢占。
// 因此每条中断都等到「运行中的任务按规则**应当**被它打断」时再注入：
//   - 受害者是排队任务（无级别）→ 任何中断都该抢占它；
//   - 受害者是中断 Li         → 只有 Lj > Li 才该抢占它。
//
// 同时验证分级计数：各级登记了多少、各级真正抢断了多少次（只看总数会掩盖
// 「总数一样但级别分布完全不同」这种情况）。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// fixedDelayProvider 返回固定内容，并在被取消时立刻返回 ctx.Err()。
//
// 延迟是必需的：任务必须先「在跑」才谈得上被打断；取消感知也是必需的，
// 否则抢占只能等它自然结束，测不到挂起/恢复。
type fixedDelayProvider struct {
	n         atomic.Int64
	delay     time.Duration
	cancelled atomic.Int64 // 被 ctx 取消（即“流式段被抢占打断”）的次数
}

func (p *fixedDelayProvider) Name() string { return "fixed-delay" }

func (p *fixedDelayProvider) Chat(ctx context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		p.cancelled.Add(1)
		return nil, ctx.Err()
	}
	p.n.Add(1)
	return &agentAPI.CompletionResponse{Content: "fixed-reply"}, nil
}

func (p *fixedDelayProvider) ChatStream(context.Context, *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return nil, errors.New("fixed-delay provider: 非流式")
}

func (p *fixedDelayProvider) MaxContextTokens() int { return 8192 }

// waitForVictim 等到「适合被 lv 打断的受害者」正在运行，**且没有别的待处理中断**。
//
// 后半个条件很重要：若有待处理中断，本次注入只会进队列（中断队列先于排队任务
// 被消费），压力就落在队列上而不是抢占/挂起路径上。
// 返回 false 表示等到超时（调用方仍应注入，保持总量不变）。
func waitForVictim(a *Agent, lv Level, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		snap := a.DumpScheduler()
		if r := snap.Running; r != nil && len(snap.PendingInterrupts) == 0 {
			if r.Class == TaskQueued {
				return true // 排队任务：任何中断都该抢占
			}
			if r.Class == TaskInterrupt && lv > r.Level {
				return true // 中断之间：严格更高级才该抢占
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func TestStress_MixedLevelInterruptsPlusQueuedInputs(t *testing.T) {
	const (
		nQueued     = 100
		nInterrupts = 100
		// 窗口要够宽：受害者必须先"在跑"，cancel 才来得及把它打断成挂起。
		// 太短（如 15ms）时任务常在让位信号生效前就自己跑完——抢占判为可行，
		// 但不会有挂起发生，压力就测不到现场保存/恢复。
		llmDelay = 40 * time.Millisecond
	)

	// L4 只有内核级来源能声明，所以注册一个内置（编译期工厂）插件名。
	plugin.RegisterFactory("stress_builtin", func(string, map[string]interface{}) (sdk.Plugin, error) {
		return nil, nil
	})

	sp := &fixedDelayProvider{delay: llmDelay}
	a := New(AgentConfig{
		ID:              "stress-levels",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
		PluginReg:       plugin.NewRegistry(),
	})
	a.Start()
	defer a.Stop()

	// 第一波：100 条排队输入（无级别，FIFO）。
	for i := 0; i < nQueued; i++ {
		a.io.InjectInput("cli", "text", map[string]interface{}{
			"content": fmt.Sprintf("queued-%d", i),
		})
	}

	// 给调度器一点时间真正开始跑排队任务，这样第一条中断就有受害者。
	time.Sleep(20 * time.Millisecond)

	// 第二波：100 条中断，级别 L1→L2→L3→L4 轮转（各 25 条）。
	// 每条都等到「该被它打断的受害者正在跑」时再注入。
	levels := []Level{LevelBackground, LevelMessage, LevelInteractive, LevelCritical}
	names := map[Level]string{
		LevelBackground: "L1", LevelMessage: "L2", LevelInteractive: "L3", LevelCritical: "L4",
	}
	// L4 必须来自内核级插件来源；其余用普通来源（cli 是内置名，但级别只有 L1..L3 时无所谓）。
	src := func(lv Level) string {
		if lv == LevelCritical {
			return "stress_builtin"
		}
		return "cli"
	}
	for i := 0; i < nInterrupts; i++ {
		lv := levels[i%len(levels)]
		waitForVictim(a, lv, 3*time.Second)
		a.io.InjectInterruptTextOpts(src(lv), "cli", fmt.Sprintf("irq-%s-%d", names[lv], i),
			agentIO.InjectOptions{Priority: names[lv]})
	}

	// 排空：200 个任务必须全部到达终态，且四容器全空。
	snap := waitQuiescent(t, a, nQueued+nInterrupts, 90*time.Second)

	// ---- 不丢不重 ----
	if snap.Stats.Executed != nQueued+nInterrupts {
		t.Fatalf("Executed=%d，期望 %d（每个任务恰好一个终态）",
			snap.Stats.Executed, nQueued+nInterrupts)
	}
	if snap.Stats.Rejected != 0 {
		t.Fatalf("容量充足却出现 Rejected=%d（背压/深度判定有误）", snap.Stats.Rejected)
	}
	wantPerLevel := uint64(nInterrupts / len(levels))
	for _, lv := range levels {
		if got := snap.Stats.InterruptsByLevel[lv]; got != wantPerLevel {
			t.Fatalf("%s 登记数=%d，期望 %d", names[lv], got, wantPerLevel)
		}
	}

	// ---- 抢占确实发生在**每一级**上 ----
	if snap.Stats.Suspended == 0 {
		t.Fatalf("100 条中断没有造成任何抢占：%+v", snap.Stats)
	}
	for _, lv := range levels {
		if got := snap.Stats.PreemptsByLevel[lv]; got == 0 {
			t.Fatalf("%s 一次都没抢断成功（分级计数=%v）", names[lv], snap.Stats.PreemptsByLevel)
		}
	}
	// 每一次“取消流式段”都必须换来一次挂起（取消→stepLLM 以 Canceled 收尾→安全点让位）。
	// 反向不成立：挂起也可能发生在别的步骤边界上（那时 LLM 已经成功返回、来不及取消）。
	if cancelled := uint64(sp.cancelled.Load()); snap.Stats.Suspended < cancelled {
		t.Fatalf("被取消的 LLM 调用=%d 但有 %d 次挂起：有取消没换来挂起（现场丢了？）",
			cancelled, snap.Stats.Suspended)
	}
	// 排空后挂起必须等于恢复——挂起来的任务都被接回去了。
	if snap.Stats.Suspended != snap.Stats.Resumed {
		t.Fatalf("Suspended=%d Resumed=%d，排空后必须相等（否则有现场丢了）",
			snap.Stats.Suspended, snap.Stats.Resumed)
	}
	if snap.Stats.Suspended > nInterrupts {
		t.Fatalf("挂起次数=%d 超过中断总数 %d（不该有任务被反复挂起这么多次）",
			snap.Stats.Suspended, nInterrupts)
	}

	// ---- LLM 调用次数：每个任务至少一次；被抢断的任务重发会增加 ----
	if got := sp.n.Load(); got < int64(nQueued+nInterrupts) {
		t.Fatalf("LLM 调用=%d，少于任务数 %d（有任务没跑到 LLM）", got, nQueued+nInterrupts)
	}

	t.Logf("压力通过：%d 排队 + %d 中断（L1/L2/L3/L4 各 %d）",
		nQueued, nInterrupts, nInterrupts/len(levels))
	t.Logf("  Executed=%d Rejected=%d Suspended=%d Resumed=%d LLM完成=%d LLM被取消=%d",
		snap.Stats.Executed, snap.Stats.Rejected, snap.Stats.Suspended, snap.Stats.Resumed,
		sp.n.Load(), sp.cancelled.Load())
	t.Logf("  登记分级=%v 抢断分级=%v",
		snap.Stats.InterruptsByLevel, snap.Stats.PreemptsByLevel)
}

// ---------------------------------------------------------------------------
// Phase B：把嵌套压到**结构上限**——排队(L0) ← L1 ← L2 ← L3 ← L4(运行中) = 4 帧。
//
// Phase A 的形状（100+100 混合）里，中断按严格优先级排队，同一时刻通常只有一层
// 嵌套；真正难的是「中断被中断」逐级下潜。这里按级别**逐级**注入：每一级都等到
// 上一级正在运行才注入，于是必然层层挂起，直到 L4 之上没有更高级别为止。
//
// 然后再验证恢复是**严格 LIFO**：L3 → L2 → L1 → 排队任务。
// ---------------------------------------------------------------------------

func waitForRunning(a *Agent, pred func(*Task) bool, wait time.Duration) *Task {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if r := a.DumpScheduler().Running; r != nil && pred(r) {
			return r
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}

func TestStress_NestingReachesStructuralBoundThenUnwindsLIFO(t *testing.T) {
	const llmDelay = 300 * time.Millisecond // 窗口要够宽，让每次 cancel 都来得及生效

	plugin.RegisterFactory("stress_nest_builtin", func(string, map[string]interface{}) (sdk.Plugin, error) {
		return nil, nil
	})

	sp := &fixedDelayProvider{delay: llmDelay}
	bus := newLevelRecorder()
	a := New(AgentConfig{
		ID:              "stress-nest",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
		EventBus:        bus.bus,
		PluginReg:       plugin.NewRegistry(),
	})
	a.Start()
	defer a.Stop()

	// 放一个排队任务（无级别）当栈底。
	a.io.InjectInput("cli", "text", map[string]interface{}{"content": "nest-base"})
	if r := waitForRunning(a, func(tk *Task) bool { return tk.Class == TaskQueued }, 5*time.Second); r == nil {
		t.Fatal("排队任务未开始运行")
	}

	// 逐级下潜：L1 → L2 → L3 → L4（L4 必须来自内核级来源）。
	type step struct {
		lv  Level
		src string
		run Level // 注入前必须在运行的级别
	}
	steps := []step{
		{LevelBackground, "cli", 0},                              // 受害者=排队任务
		{LevelMessage, "cli", LevelBackground},                   // 受害者=L1
		{LevelInteractive, "cli", LevelMessage},                  // 受害者=L2
		{LevelCritical, "stress_nest_builtin", LevelInteractive}, // 受害者=L3
	}
	for i, st := range steps {
		if waitForRunning(a, func(tk *Task) bool {
			if st.lv == LevelBackground {
				return tk.Class == TaskQueued
			}
			return tk.Class == TaskInterrupt && tk.Level == st.run
		}, 5*time.Second) == nil {
			t.Fatalf("第 %d 级（%v）注入前未等到预期的受害者运行", i+1, st.lv)
		}
		a.io.InjectInterruptTextOpts(st.src, "cli", fmt.Sprintf("nest-%d", i+1),
			agentIO.InjectOptions{Priority: map[Level]string{
				LevelBackground: "L1", LevelMessage: "L2",
				LevelInteractive: "L3", LevelCritical: "L4",
			}[st.lv]})

		// 等这一层真的压进栈（否则下一级的"受害者"条件会被误判）。
		deadline := time.Now().Add(5 * time.Second)
		for {
			if len(a.DumpScheduler().SuspendStack) >= i+1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("第 %d 级注入后栈深未达 %d：%d",
					i+1, i+1, len(a.DumpScheduler().SuspendStack))
			}
			time.Sleep(time.Millisecond)
		}
	}

	// 结构上限：4 帧 = 排队(L0) + L1 + L2 + L3 挂起，L4 运行中。
	if n := len(a.DumpScheduler().SuspendStack); n != 4 {
		t.Fatalf("嵌套峰值栈深=%d，期望 4（结构上限）", n)
	}
	if a.sched.canSuspend() {
		t.Fatal("已到结构上限，L4 之上不该再有下潜余量")
	}

	// 排空：4 层必须逐层弹回，且顺序严格 LIFO。
	snap := waitQuiescent(t, a, 5, 30*time.Second)
	if n := len(snap.SuspendStack); n != 0 {
		t.Fatalf("排空后中断栈=%d，期望 0", n)
	}
	if got, want := bus.resumeLevels(), []int{3, 2, 1, 0}; !equalInts(got, want) {
		t.Fatalf("恢复顺序=%v，期望 LIFO %v", got, want)
	}
	// 峰值栈深也就是本次全部挂起帧数：4。
	if snap.Stats.Suspended != 4 || snap.Stats.Resumed != 4 {
		t.Fatalf("Suspended=%d Resumed=%d，期望各 4", snap.Stats.Suspended, snap.Stats.Resumed)
	}
	if sp.cancelled.Load() == 0 {
		t.Fatal("逐级下潜必须靠取消流式段生效，却没有一次 LLM 调用被取消")
	}
	t.Logf("嵌套压力通过：栈深峰值=4（结构上限），恢复顺序 LIFO=%v，流式段被取消=%d 次",
		bus.resumeLevels(), sp.cancelled.Load())
}

// levelRecorder 记录 scheduler 事件的挂起/恢复级别。
type levelRecorder struct {
	bus    *events.Bus
	mu     sync.Mutex
	resume []int
}

func newLevelRecorder() *levelRecorder {
	rec := &levelRecorder{bus: events.NewBus()}
	rec.bus.Subscribe(events.EventScheduler, func(e *events.Event) {
		if fmt.Sprint(e.Payload["action"]) != "resume" {
			return
		}
		lv, _ := e.Payload["level"].(int)
		rec.mu.Lock()
		rec.resume = append(rec.resume, lv)
		rec.mu.Unlock()
	})
	return rec
}

func (r *levelRecorder) resumeLevels() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.resume...)
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
