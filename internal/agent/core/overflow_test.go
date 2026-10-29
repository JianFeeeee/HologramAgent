package core

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// ── 测试替身 ──

// fakeOverflowProvider 只提供 MaxContextTokens（超页判据需要它），
// 其余 Provider 方法不参与本组测试。
type fakeOverflowProvider struct {
	agentAPI.Provider
	window int
}

func (p *fakeOverflowProvider) MaxContextTokens() int { return p.window }

// newOverflowAgent 造一个够用的超页测试 Agent。
func newOverflowAgent(window, maxContextSize int, events int, chunkChars int) *Agent {
	a := &Agent{
		io:             agentIO.NewIOManager(),
		provider:       &fakeOverflowProvider{window: window},
		maxContextSize: maxContextSize,
		context:        NewRelevanceContext("", nil),
	}
	// 固定向量，避免依赖稠密模型；也不进 coldcontextStore（nil ⇒ Prune 只重排不写盘）。
	for i := 0; i < events; i++ {
		a.context.Append(ContextEvent{
			Input:    fmt.Sprintf("第%d批输入", i) + strings.Repeat("x", chunkChars),
			Response: fmt.Sprintf("第%d批回执", i) + strings.Repeat("y", chunkChars),
		})
	}
	return a
}

// ── T1：积累量超限才触发裁剪 ──

func TestOverflow_T1_积累超125触发(t *testing.T) {
	// 窗口 1000 token；maxContextSize=2（条数），制造远超容量的积累。
	a := newOverflowAgent(2000, 2, 20, 400) // 单条827 < 预算~1067，但总量 16540 远超
	before := a.context.Len()

	ratio := a.overflowRatioNow()
	if ratio < overflowRatio {
		t.Fatalf("构造后应已超页，ratio=%.2f 阈值=%.2f", ratio, overflowRatio)
	}

	handled, pruned := a.maybeHandleContextOverflow(nil, "退款导出")
	if !handled {
		t.Fatal("超限时 maybeHandleContextOverflow 应返回 handled=true")
	}
	// ★ 空中断设计：发中断这一步**不裁剪**（pruned 如实为 0），
	// 裁剪由空中断任务被调度执行时做（pruneForAirInterrupt）。
	// 这条原来断言 pruned>0，那对应的是"先裁剪再发回执"的旧实现。
	_ = pruned
	if after := a.context.Len(); after != before {
		t.Fatalf("发中断这一步不应改动上下文：%d → %d", before, after)
	}
}

// 未超页时必须零动作（这是热路径，每轮都调）。
func TestOverflow_T1_未超页不动手(t *testing.T) {
	// 窗口 10_000_000 token：任何合理积累都远低于 1.25×
	a := newOverflowAgent(10_000_000, 2, 5, 100)
	before := a.context.Len()

	handled, pruned := a.maybeHandleContextOverflow(nil, "查询")
	if handled || pruned != 0 {
		t.Fatalf("未超页不应处理：handled=%v pruned=%d", handled, pruned)
	}
	if after := a.context.Len(); after != before {
		t.Fatalf("未超页时上下文不应变化：%d → %d", before, after)
	}
}

// 轻量内核（驻留子）走的是另一套策略，不做按相关度裁剪。
func TestOverflow_T1_轻量内核跳过(t *testing.T) {
	a := newOverflowAgent(1000, 2, 20, 400)
	a.parentID = "parent-1" // 触发 isLightKernel
	handled, pruned := a.maybeHandleContextOverflow(nil, "查询")
	if handled || pruned != 0 {
		t.Fatalf("轻量内核不应按相关度裁剪：handled=%v pruned=%d", handled, pruned)
	}
}

// ── T2：裁剪后积累量确实下降 ──

func TestOverflow_T2_裁剪后累积下降(t *testing.T) {
	a := newOverflowAgent(2000, 2, 20, 400) // 单条装得下，总量超页
	before := a.accumulatedTokens()

	a.maybeHandleContextOverflow(nil, "退款导出")

	// ★ 空中断设计：裁剪延后到任务边界（pruneForAirInterrupt）。
	// 这里直接调它，验证"裁剪确实能降 token"本身仍成立。
	a.pruneForAirInterrupt()

	after := a.accumulatedTokens()
	if after >= before {
		t.Fatalf("pruneForAirInterrupt 后积累 token 未下降：%d → %d", before, after)
	}
}

// ── T4（变异项）：裁不出东西 ⇒ 终止且不触发 L4 ──
//
// 构造「装得下却超页」：maxContextSize 给得足够大，Prune 会返回 0。
// 期望：记账 Aborted、不调 raiseKernelInterrupt（用 stat.Triggered 验证）。
func TestOverflow_T4_裁不动则终止不中断(t *testing.T) {
	// 「裁不动」的真正构造：**只有 1 条事件** —— 裁剪不可能减少任何东西
	// （Prune 里 protected 至少 1 条、keep 也至少要留 1 条）。
	// 旧构造（maxContextSize=9999 + 5 条）在自适应 protected 下反而能裁掉 3 条，
	// 因为预算 534 token 装不下 10 条保护 ⇒ 自动收紧到 1 ⇒ topK=2。
	a := newOverflowAgent(1000, 9999, 1, 4000)
	if a.overflowRatioNow() < overflowRatio {
		t.Fatal("前置条件不成立：构造的会话应超页")
	}

	a.maybeHandleContextOverflow(nil, "查询")

	a.overflowStat.Lock()
	aborted, triggered := a.overflowStat.Aborted, a.overflowStat.Triggered
	a.overflowStat.Unlock()

	if aborted != 1 {
		t.Fatalf("应记一次 Aborted，实际 %d", aborted)
	}
	if triggered != 0 {
		t.Fatalf("裁不出东西时**不应**触发 L4，实际 Triggered=%d", triggered)
	}
}

// ── 超页只发空中断，裁剪留给任务边界 ──
//
// 推翻的旧判据：TestOverflow_T6_中断消息含认知提示（断言正文含
// "查不到不等于不存在"、"文档记忆"）。那段文案连同 overflowNotice 一起删除，
// 理由见 raiseContextOverflow 的注释（把"补偿 L4 看不到上下文"错当成了
// 中断该带正文的理由）。
//
// 现在钉住的是：**超页时发出的是空中断**，且裁剪**不在发中断这一步发生**。
func TestOverflow_RaisesAirInterruptWithoutPruning(t *testing.T) {
	a := newOverflowAgent(2000, 2, 20, 400)

	before := a.context.Len()
	a.handleContextOverflow("", 2.0)

	// ① 中断已发出（Triggered 计数 +1）
	a.overflowStat.Lock()
	triggered := a.overflowStat.Triggered
	a.overflowStat.Unlock()
	if triggered == 0 {
		t.Fatal("超页必须发出 L4 中断")
	}
	// ② 裁剪**没有**在这一步发生：上下文条数不变
	//
	// 这是空中断设计的核心——裁剪由空中断任务被调度执行时触发
	// （pruneForAirInterrupt），不是 raise 的副作用。
	// 变异验证：在 handleContextOverflow 里加回 pruneByQuery ⇒ 本判据变红。
	if got := a.context.Len(); got != before {
		t.Fatalf("发中断这一步不应裁剪上下文：之前 %d 条，之后 %d 条", before, got)
	}
}

// ── 上游 ErrContextFull 走同一条裁剪路径 ──

func TestOverflow_上游ErrContextFull走同路径(t *testing.T) {
	a := newOverflowAgent(2000, 2, 20, 400) // 单条装得下，总量超页

	// 累积不足以下次超页也没关系：上游已经明说装不下了。
	// ★ 关键：走的是**同一个** enforceContextBudget（force=true），
	// 不是另一条独立路径。这正是「contextfull 复用中断裁剪语义」的落点。
	if !isUpstreamContextFull(&agentAPI.ProviderError{
		StatusCode: 400,
		Kind:       agentAPI.ErrContextFull,
		Message:    "context_length_exceeded",
	}) {
		t.Fatal("上游 ErrContextFull 应被识别")
	}
	handled, _ := a.enforceContextBudget(nil, "", true)
	if !handled {
		t.Fatal("上游 ErrContextFull 应被接住并返回 true（可恢复）")
	}
	a.overflowStat.Lock()
	upstream, triggered := a.overflowStat.Upstream, a.overflowStat.Triggered
	a.overflowStat.Unlock()
	if upstream != 1 {
		t.Fatalf("应记一次上游超页，实际 %d", upstream)
	}
	if triggered != 1 {
		t.Fatalf("上游超页也应经 L4 上报一次，实际 %d", triggered)
	}
}

// 非上下文类错误不得被误接。
func TestOverflow_上游其他错误不误接(t *testing.T) {
	for _, kind := range []agentAPI.ErrKind{agentAPI.ErrCredential, agentAPI.ErrTransient, agentAPI.ErrUnknown} {
		if isUpstreamContextFull(&agentAPI.ProviderError{
			StatusCode: 500, Kind: kind, Message: "boom",
		}) {
			t.Fatalf("kind=%v 不应被当作上下文超限接住", kind)
		}
	}
}

// nil context / nil provider 不得 panic。
func TestOverflow_零值不panic(t *testing.T) {
	var a *Agent
	if handled, _ := a.maybeHandleContextOverflow(nil, "q"); handled {
		t.Fatal("nil agent 不应处理")
	}
	empty := &Agent{}
	if handled, _ := empty.maybeHandleContextOverflow(nil, "q"); handled {
		t.Fatal("零值 agent 不应处理")
	}
	_ = empty.overflowRatioNow()
	_ = empty.accumulatedTokens()
}

// ── T8（回归）：并发调用安全 ──
// 超页判据在每轮 LLM 请求前调用，是热路径；AccumulatedTokens 会遍历事件，
// 必须与 Append 并发安全，否则数据竞争。
func TestOverflow_并发安全(t *testing.T) {
	a := newOverflowAgent(1000, 2, 50, 200)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				a.context.Append(ContextEvent{
					Input:    fmt.Sprintf("并发%d-%d", i, j),
					Response: strings.Repeat("z", 300),
				})
				_ = a.overflowRatioNow()
			}
		}(i)
	}
	wg.Wait()
}

// ── T5（变异项）：帧内恢复预算耗尽 ⇒ 终止而非死循环 ──
//
// 为什么要单独测：这个防护一度是**死代码**（TaskFrame.OverflowRecover 声明了
// 但从没被读取）。没接线的防护等于没有防护 —— 变异测试正是抓这种。
func TestOverflow_T5_恢复预算耗尽则终止(t *testing.T) {
	// (4000,800) 实测：单条 1627 token 装得进预算 2134，但总量 ratio=8.13 超页
	a := newOverflowAgent(4000, 30, 20, 800)
	f := &TaskFrame{}

	// 第一次超页：发出空中断（裁剪延后，pruned 如实为 0）
	handled, _ := a.maybeHandleContextOverflow(f, "退款导出")
	if !handled {
		t.Fatalf("首次应处理并发出空中断：handled=%v", handled)
	}
	if f.OverflowRecover != 1 {
		t.Fatalf("首次应记一次恢复，实际 %d", f.OverflowRecover)
	}

	// 制造「裁不动」：每次检查前把上下文压到只剩 1 条大事件
	// （实测 ratio=8.03），裁剪无从下手。
	// 注：必须每次重新压 —— 第一次超页已把上下文裁小，不重压就不会再超页，
	// 那样测的就不是「反复裁不动」而是「裁一次就好了」。
	collapse := func() {
		for a.context.Len() > 1 {
			a.context.events = a.context.events[:len(a.context.events)-1]
		}
		a.context.events[0] = &ContextEvent{
			Input: strings.Repeat("A", 8_000), Response: strings.Repeat("B", 8_000),
		}
	}

	// 预算内的第二次：仍会尝试
	collapse()
	handled, _ = a.maybeHandleContextOverflow(f, "退款导出")
	if !handled {
		t.Fatal("预算内仍应处理")
	}
	if f.OverflowRecover != 2 {
		t.Fatalf("预算内应累计到 2，实际 %d", f.OverflowRecover)
	}

	// 第三次：超预算 ⇒ 终止，且写明原因
	collapse()
	handled, _ = a.maybeHandleContextOverflow(f, "退款导出")
	if !handled {
		t.Fatal("超预算也应算「已处理」（终止），以免调用方再重跑")
	}
	if f.Terminal != terminalError {
		t.Fatalf("超预算应置终止态，实际 terminal=%v", f.Terminal)
	}
	if f.Err == nil || !strings.Contains(f.Err.Error(), "max_context_size") {
		t.Fatalf("终止原因应指向根因（max_context_size 与窗口不匹配）：%v", f.Err)
	}
}

// 变异自证：把预算调到极大 ⇒ 上面那条必须变红（证明它真的在数）。
func TestOverflow_T5_变异_预算极大则不终止(t *testing.T) {
	backup := overflowRecoverBudget
	overflowRecoverBudget = 1 << 30
	defer func() { overflowRecoverBudget = backup }()

	a := newOverflowAgent(1000, 2, 20, 400)
	a.maxContextSize = 99999 // 裁不动
	f := &TaskFrame{}
	for i := 0; i < 5; i++ {
		for a.context.Len() > 1 {
			a.context.events = a.context.events[:len(a.context.events)-1]
		}
		a.context.events[0] = &ContextEvent{
			Input: strings.Repeat("A", 8_000), Response: strings.Repeat("B", 8_000),
		}
		a.maybeHandleContextOverflow(f, "退款导出")
	}
	if f.Terminal == terminalError {
		t.Fatalf("预算=2^30 时不应终止 ⇒ 预算判定可能没生效")
	}
}

// ── T9：topK 随窗口换算（不再与窗口脱钩）──
//
// 这是 v4 跑分归因的根因二：max_context_size 是条数、与窗口无关，
// 所以「只调 context_window 到 1M 就更好」不成立。
func TestOverflow_T9_topK随窗口变化(t *testing.T) {
	// 同样 20 条事件、同样的 max_context_size，只改窗口 ⇒ topK 必须跟着变
	build := func(window int) *Agent {
		return newOverflowAgent(window, 30, 20, 400) // maxContextSize=30（默认）
	}

	small := build(10_000)    // 小窗口
	large := build(1_000_000) // 大窗口（用户关心的 1M 场景）

	topKSmall := small.contextTopK()
	topKLarge := large.contextTopK()

	if topKLarge <= topKSmall {
		t.Fatalf("窗口变大后 topK 应变大：%d → %d（说明仍与窗口脱钩）", topKSmall, topKLarge)
	}
	// 小窗口下预算只够留很少事件；大窗口下应更宽松
	if topKLarge < 10 {
		t.Fatalf("1M 窗口下 topK=%d 过小，说明预算反推失效", topKLarge)
	}
}

// max_context_size 仍是硬上限（改了配置必须生效，否则运维会困惑）。
func TestOverflow_T9_运维配置仍是上限(t *testing.T) {
	a := newOverflowAgent(1_000_000, 5, 40, 200) // 窗口很大但硬上限 5
	if got := a.contextTopK(); got > 4 {
		t.Fatalf("topK=%d 超过 max_context_size-1=4，运维配置失效", got)
	}
}

// 无积累样本时不得除零/返回 0。
func TestOverflow_T9_空上下文topK至少为1(t *testing.T) {
	a := newOverflowAgent(50_000, 30, 0, 200)
	if got := a.contextTopK(); got < 1 {
		t.Fatalf("空上下文时 topK=%d，至少应为 1", got)
	}
}

// 变异自证：把 contextTopK 退回「固定用 maxContextSize」，
// 上面那条「窗口变大 topK 变大」必须变红（证明该测试真能抓到脱钩）。
func TestOverflow_T9_变异_固定topK则脱钩(t *testing.T) {
	build := func(window int) *Agent { return newOverflowAgent(window, 30, 20, 400) }

	// 旧行为：topK 只看 max_context_size，与窗口无关 ⇒ 两个窗口得到同一个值。
	oldTopK := func(a *Agent) int { return a.maxContextSize - 1 }
	if oldTopK(build(10_000)) != oldTopK(build(1_000_000)) {
		t.Fatalf("前置不成立：旧行为本应与窗口无关")
	}

	// 新实现必须与旧行为不同，否则「T9_topK随窗口变化」那条主测试是假绿。
	newSmall := build(10_000).contextTopK()
	newLarge := build(1_000_000).contextTopK()
	if newSmall == oldTopK(build(10_000)) && newLarge == oldTopK(build(1_000_000)) {
		t.Fatalf("新实现与旧实现不可区分 ⇒ topK 实际仍与窗口脱钩，主测试是假绿")
	}
	if newLarge <= newSmall {
		t.Fatalf("新实现下窗口变大 topK 未变大：%d → %d", newSmall, newLarge)
	}
}

// ── provider 未就绪时不得崩 ──
//
// 回归：contextTopK → computeTokenBudget → ComputeTokenBudgetTuned 里曾直接调
// provider.MaxContextTokens()，provider 为 nil 时 SIGSEGV（实测由
// TestMemoryPass_PruneAndRecallTogether 撞出来）。它同样会发生在
// 「配置早于 provider 就绪」的启动序列上，不只是测试场景。
func TestOverflow_零值Agent不崩(t *testing.T) {
	// 完全零值 Agent（非 nil）：无 provider、无 context、无配置
	empty := &Agent{}
	if got := empty.contextTopK(); got < 1 {
		t.Fatalf("零值 Agent 的 topK=%d，至少应为 1", got)
	}
	// 硬上限为 0 时也不能崩
	empty.maxContextSize = 0
	if got := empty.contextTopK(); got < 1 {
		t.Fatalf("maxContextSize=0 时 topK=%d，至少应为 1", got)
	}
}

// ComputeTokenBudgetTuned 直接吃 nil provider（不经 Agent）也要安全。
func TestComputeTokenBudgetTuned_nilProvider(t *testing.T) {
	b := ComputeTokenBudgetTuned(nil, "", ContextTuning{})
	if b.MaxContext <= 0 {
		t.Fatalf("nil provider 时应回退到默认窗口，实际 %d", b.MaxContext)
	}
	if b.ContextTokens < 0 {
		t.Fatalf("ContextTokens 不应为负：%d", b.ContextTokens)
	}
}

// ── topK 下限：不能被算成 1（会让裁剪完全无效）──
//
// 实测缺陷：v4 的工具大回执型事件平均 48000 token，而 50k 窗口的
// ContextTokens 预算只有 40000 ⇒ 预算反推的 topK = 0 ⇒ 钳到 1。
// 而 Prune 里 keepCount = topK - protected = 1 - 10 < 0 ⇒ keep 为空
// ⇒ **全部事件被归档**：裁完一轮上下文照样超页，超页处理空转。
//
// 正确下限是 protectedCount+1：宁可暂时超页，等预算或事件尺寸回到正常区间。
func TestOverflow_大事件时topK不塌到1(t *testing.T) {
	// 窗口 50000 ⇒ ContextTokens 预算约 40000
	a := newOverflowAgent(50_000, 30, 30, 0)
	// 造「单条就超预算」的事件：Response 48000 token
	for i := 0; i < 30; i++ {
		a.context.Append(ContextEvent{
			Input:    strings.Repeat("A", 160_000), // 约 40000 token
			Response: strings.Repeat("B", 192_000), // 约 48000 token
		})
	}

	topK := a.contextTopK()
	protected := a.effectiveProtectedCount()
	if topK <= protected {
		t.Fatalf("topK=%d <= protectedCount=%d ⇒ 裁剪会把全部事件归档（keep 为空），"+
			"超页处理空转", topK, protected)
	}
	if topK < protected+1 {
		t.Fatalf("topK=%d 必须至少是 protectedCount+1=%d", topK, protected+1)
	}
}

// 真实形状验证：topK 下限生效时，Prune 确实还能裁掉东西。
func TestOverflow_大事件时裁剪仍有效(t *testing.T) {
	a := newOverflowAgent(50_000, 30, 30, 0)
	for i := 0; i < 30; i++ {
		a.context.Append(ContextEvent{
			Input:    strings.Repeat("A", 8_000),
			Response: strings.Repeat("B", 40_000),
		})
	}
	before := a.context.Len()
	// ★ 空中断设计：handleContextOverflow 只发空中断，裁剪在任务边界做。
	// 这里先发中断（验证它不改上下文），再由 pruneForAirInterrupt 真正裁。
	a.handleContextOverflow("退款导出", 10.0)
	if mid := a.context.Len(); mid != before {
		t.Fatalf("发空中断这一步不应改动上下文：%d → %d", before, mid)
	}
	a.pruneForAirInterrupt()
	after := a.context.Len()
	if after >= before {
		t.Fatalf("裁剪无效：%d → %d 条", before, after)
	}
	// 裁剪后仍应留下 protected 条以上，不能清空
	if after < a.effectiveProtectedCount() {
		t.Fatalf("裁剪后只剩 %d 条，少于有效保护数 %d（记忆被清空）",
			after, a.effectiveProtectedCount())
	}
}

// ── 工具回灌必须计入累积量 ──
//
// 实测缺陷（T10 诊断日志 ratio=0.00 / events=1 抓到）：ContextEvent 里工具输出
// 是**独立字段** ToolResults，accumulatedTokens 只算 Input+Response 时，
// 一个「用户说 10 字、模型回 20 字、调了 8 个工具各回 3 万 token」的轮次
// 算出来是 30 token ⇒ 超页判据永远不触发。
//
// 而 prompt 峰值的真正来源恰恰是工具回灌。
func TestOverflow_工具回灌计入累积(t *testing.T) {
	a := newOverflowAgent(50_000, 30, 0, 0)
	a.context.Append(ContextEvent{
		Input:    "查一下 order-gw 的端口",
		Response: "好的，正在查",
		ToolResults: []ToolResultItem{
			{Name: "files_read", Output: strings.Repeat("X", 120_000)}, // 约 30000 token
			{Name: "cmd_run", Output: strings.Repeat("Y", 120_000)},    // 约 30000 token
		},
	})

	acc := a.accumulatedTokens()
	if acc < 50_000 {
		t.Fatalf("工具回灌未计入：accumulatedTokens=%d（两条工具各 3 万 token）", acc)
	}

	// 且应能触发超页：2 轮同样的事件即远超窗口
	a.context.Append(ContextEvent{
		Input:       "再查一次",
		Response:    "好",
		ToolResults: a.context.Recent(1)[0].ToolResults,
	})
	if ratio := a.overflowRatioNow(); ratio < overflowRatio {
		t.Fatalf("两条工具回灌轮次应触发超页，实际 ratio=%.2f", ratio)
	}
}

// 反向判据：没有工具结果时，累积量应只等于 Input+Response。
func TestOverflow_无工具时只算对话(t *testing.T) {
	a := newOverflowAgent(50_000, 30, 0, 0)
	a.context.Append(ContextEvent{Input: strings.Repeat("a", 400), Response: strings.Repeat("b", 400)})
	acc := a.accumulatedTokens()
	// 800 字符 ≈ 400 token（EstimateTokens 是字符/2 量级），不应有额外放大
	if acc > 1000 {
		t.Fatalf("无工具结果的短对话被算成 %d token（应≈400）", acc)
	}
	if acc < 100 {
		t.Fatalf("无工具结果的短对话算成 %d token，明显漏算", acc)
	}
}

// ── B 方案：protected 按预算自适应（配置值是上限，不是固定值）──
//
// T10c 实测踩出的自相矛盾：protected=10 × 单条 5800 token = 58000 >
// ContextTokens 预算 40000 ⇒「钉住 10 条」本身就装不下 ⇒ 裁剪无解 ⇒
// 每轮触发两次超页 → 预算耗尽 → 任务终止（prompt=0）。召回 44%→22%。
//
// 现在：预算装不下时自动收紧，装得下时用满配置。
func TestOverflow_protected按预算收紧(t *testing.T) {
	// 小窗口 + 大事件 ⇒ 预算装不下 10 条
	small := newOverflowAgent(20_000, 30, 12, 20_000)
	cfgCap := small.context.ProtectedCount() // 配置上限仍是 10
	eff := small.effectiveProtectedCount()

	if cfgCap != 10 {
		t.Fatalf("配置上限应仍为 10，实际 %d", cfgCap)
	}
	if eff >= cfgCap {
		t.Fatalf("预算装不下时应收紧，但 eff=%d >= cfg=%d", eff, cfgCap)
	}
	// 收紧后若仍装不下，必须能识别出「单条事件本身就超预算」这个真相，
	// 让调用方走终止路径，而不是反复裁剪到预算耗尽。
	if eff*small.accumulatedTokens()/small.context.Len() > small.computeTokenBudget().ContextTokens {
		if small.singleEventFitsBudget() {
			t.Fatalf("收紧后装不下却报告单条装得下 ⇒ 判定自相矛盾")
		}
	}

	// 大窗口 ⇒ 用满配置值（这正是「1M 下这套调度器能更好」的机制）
	big := newOverflowAgent(1_000_000, 30, 12, 200)
	if got := big.effectiveProtectedCount(); got != big.context.ProtectedCount() {
		t.Fatalf("大窗口下应达满配置值 %d，实际 %d", big.context.ProtectedCount(), got)
	}
}

// 自适应后裁剪必须真的有效（旧逻辑下裁不动）。
func TestOverflow_自适应后裁剪有效(t *testing.T) {
	// (4000,800)：单条 1627 装得进预算 2134，总量 ratio=8.13 ⇒ 裁剪可帮上忙。
	// 对照组是「单条本身就超预算」——那种情况裁剪无解，走终止路径，
	// 由TestOverflow_单条超预算则终止 覆盖。
	a := newOverflowAgent(4000, 30, 5, 800)
	before := a.context.Len()
	// 空中断设计：先发中断，再由任务边界裁剪。
	a.handleContextOverflow("退款导出", 1.3)
	a.pruneForAirInterrupt()
	after := a.context.Len()

	if after >= before {
		t.Fatalf("自适应后裁剪应有效：%d → %d", before, after)
	}
	if a.effectiveProtectedCount() >= before {
		t.Fatalf("有效保护数 %d 不应 ≥ 裁剪前条数 %d",
			a.effectiveProtectedCount(), before)
	}
}

// 单条事件本身就超预算 ⇒ 裁剪无解 ⇒ 直接终止并报真实原因
// （而不是裁两轮后 Budget 耗尽，后者会丢失已经裁掉的记忆）。
func TestOverflow_单条超预算则终止(t *testing.T) {
	// (2000,800)：单条 1627 > 预算 1067 ⇒ fits=false
	a := newOverflowAgent(2000, 30, 5, 800)
	if a.singleEventFitsBudget() {
		t.Fatal("前置不成立：单条应装不下预算")
	}
	before := a.context.Len()

	pruned := a.handleContextOverflow("退款导出", 5.0)

	if pruned != 0 {
		t.Fatalf("单条超预算时不应裁剪（裁了也装不下），实际 pruned=%d", pruned)
	}
	if after := a.context.Len(); after != before {
		t.Fatalf("单条超预算时上下文不应被改动：%d → %d", before, after)
	}
	a.overflowStat.Lock()
	aborted := a.overflowStat.Aborted
	a.overflowStat.Unlock()
	if aborted == 0 {
		t.Fatal("应记一次 Aborted")
	}
}

// ── 裁剪是**基础设施**：各注入/恢复入口都必须能触发它 ──

// 任务恢复入口（rebaseFramePrefix）必须做容量裁剪。
//
// 为什么需要它：中断任务跑完会把它的输入/输出提交进 a.context，
// 于是恢复时重建出的前缀**可能比被挂起时更大**。不在这里裁，这份更大的
// 前缀会被原样发出去，下一轮再靠 stepLLM 的闸门补救——那时已经付过
// 一次超限请求的代价。
//
// 变异：删掉 rebaseFramePrefix 末尾的 enforceContextBudget 调用 ⇒ 变红。
func TestOverflow_任务恢复入口触发裁剪(t *testing.T) {
	a := newOverflowAgent(2000, 2, 20, 400) // 总量远超窗口
	f := &TaskFrame{
		Input:     "恢复后的输入",
		PrefixLen: 1,
		Msgs: []agentAPI.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "tail"},
		},
	}
	before := a.context.Len()

	a.rebaseFramePrefix(f)

	after := a.context.Len()
	if after >= before {
		t.Fatalf("任务恢复后应触发容量裁剪（上下文 %d → %d 未减少）", before, after)
	}
	if f.OverflowRecover == 0 {
		t.Fatal("恢复入口的裁剪应计入该帧的恢复预算（否则同一帧可无限裁）")
	}
}

// 恢复时相关性必须按**本帧最新上下文**算，不能拿旧的 f.Input。
//
// 被恢复的任务可能已挂起很久，f.Input 与当前对话早已不同步；
// 拿旧 Input 当 query，会裁掉"对该任务重要、但字面上不匹配旧输入"的事件，
// 恢复出来的任务反而失去了自己的现场。
//
// 判据：resumeQueryFrom 取的是 Msgs 尾部第一条有内容的消息，
// 而不是 Input。
func TestOverflow_恢复用本帧最新上下文作查询(t *testing.T) {
	f := &TaskFrame{
		Input:     "很久以前用户说的话",
		PrefixLen: 1,
		Msgs: []agentAPI.Message{
			{Role: "system", Content: "sys"},
			{Role: "tool", Content: "最新的工具结果：部署已完成"},
		},
	}
	got := resumeQueryFrom(f)
	if got != "最新的工具结果：部署已完成" {
		t.Fatalf("恢复查询应取本帧最新有效上下文，实际取到 %q", got)
	}
	if got == f.Input {
		t.Fatal("不得回退到旧的 f.Input —— 那与当前任务已不同步")
	}
}

// 尾部全是空消息时必须回退到 Input（不能返回空查询）。
func TestOverflow_恢复查询回退(t *testing.T) {
	f := &TaskFrame{
		Input: "原始输入",
		Msgs:  []agentAPI.Message{{Role: "assistant", Content: "  "}},
	}
	if got := resumeQueryFrom(f); got != "原始输入" {
		t.Fatalf("全空时应回退到 Input，实际 %q", got)
	}
}

// 恢复裁剪必须保护**本帧自己的现场**，而不是全局末尾 N 条。
//
// 这是最容易写错的一点：PruneWithProtected 的默认保护是 c.events 末尾 N 条
// （"最近的对话"），而被挂起任务的现场可能是一整批工具结果，未必在末尾。
// 保护错了，恢复后的任务引用的内容已被归档，任务自相矛盾。
//
// 变异：把 pruneForResume 的 protected 改成 a.effectiveProtectedCount() ⇒ 变红。
func TestOverflow_恢复裁剪保护本帧现场(t *testing.T) {
	// 上下文 20 条，帧现场声明 8 条（模拟攒下的工具轮产物）。
	a := newOverflowAgent(2000, 2, 20, 400)
	before := a.context.Len()

	frameTail := 8
	a.pruneForResume("部署已完成", frameTail)

	// ⚠️ 判据必须看**事件数**，不能看返回值：pruneForResume 返回的是
	// 「写入 coldcontextStore 的条数」，coldcontextStore==nil 时它恒为 0，而裁剪其实发生了。
	// （handleContextOverflow 的注释里记过同一个坑。）
	after := a.context.Len()
	// 保留条数必须覆盖被保护的部分：裁剪后条数不得少于 protected。
	if after < frameTail {
		t.Fatalf("裁剪后仅剩 %d 条，少于本帧保护的 %d 条 —— 现场被破坏", after, frameTail)
	}
	if after >= before {
		t.Fatalf("应减少事件数（%d → %d）", before, after)
	}
}

// 上游 ErrContextFull 与本地预判必须走**同一个**处理函数。
//
// 这条守的是"复用中断裁剪语义"本身：一旦有人再拆出独立路径，
// 恢复预算 / 轻量内核判断这些防护就会在其中一条上缺失。
//
// 变异：恢复 handleUpstreamContextFull（直调 handleContextOverflow）⇒ 变红。
func TestOverflow_上游与本地共用同一个处理函数(t *testing.T) {
	a := newOverflowAgent(2000, 2, 20, 400) // 总量超页，但下面要看 force 的效果

	// force=true 时**必须**进入处理，即使水位判定不成立。
	// 这正是"上游说满了"的语义：不信本地估算，直接裁。
	//
	// 造一个"水位很低但上游报满"的场景：空上下文 ⇒ ratio=0，
	// 本地判据绝不会触发，只有 force 能让它进入处理分支。
	empty := &Agent{
		io:       agentIO.NewIOManager(),
		provider: &fakeOverflowProvider{window: 100000},
		context:  NewRelevanceContext("", nil),
	}
	empty.context.Append(ContextEvent{Input: "短", Response: "短"})
	a = empty
	if a.overflowRatioNow() >= overflowRatio {
		t.Fatal("前置：水位应低于阈值")
	}
	handled, pruned := a.enforceContextBudget(nil, "", true)
	if !handled {
		t.Fatal("force=true 应在水位不足时仍然处理（上游已明确报错）")
	}
	_ = pruned
}

// 恢复预算对**所有**入口一致生效——包括上游报错那条。
//
// 这条是 T5 的补强。旧实现里 handleUpstreamContextFull 直调内部函数，
// 绕过了 f.OverflowRecover 检查，于是同一帧可以反复 raise L4；
// 而文档 §四.1 明确「L4 遇 L4 不能抢占」，反复触发只会排队堆积。
//
// 变异：把 force 分支移到预算检查之前 ⇒ 变红。
func TestOverflow_恢复预算对所有入口生效(t *testing.T) {
	a := newOverflowAgent(2000, 2, 20, 400)
	f := &TaskFrame{Input: "x"}
	f.OverflowRecover = overflowRecoverBudget // 预算已耗尽

	handled, _ := a.enforceContextBudget(f, "", true)
	if !handled {
		t.Fatal("预算耗尽也算「已处理」——调用方据此直接终止，不应再重跑")
	}
	if f.Terminal != terminalError {
		t.Fatal("预算耗尽必须置 terminalError，否则会静默重试")
	}
}

// ── 空中断端到端：识别 + 裁剪 + 不产生回复 ──────────────────────
//
// 这几条钉住 2026-10-25 按原始设计做的修正。此前实现有三个偏移：
//  ① 中断带正文（"已裁剪 N 条…"）——它变成一条要模型回应的消息
//  ② 裁剪在 raise 之前就做完——中断沦为回执，晚了一步
//  ③ 上游 contextfull 后原地重跑——拿同一份超限内容再发一次，烧光恢复预算

// 判据：空中断必须被 resolveInput 认作有效输入。
//
// 变异验证：删掉 isAirInterrupt 那一支 ⇒ 它会被当成 empty_input 丢弃，
// 本判据变红。
func TestAirInterrupt_ResolvedAsValidInput(t *testing.T) {
	a := newOverflowAgent(2000, 2, 20, 400)
	evt := &agentIO.InputEvent{
		Type:    "interrupt",
		Source:  "kernel/overflow",
		Payload: map[string]interface{}{"interrupt": true, "kernel": true, "content": ""},
	}
	in, ok := a.resolveInput(evt)
	if !ok {
		t.Fatal("空中断必须是有效输入（否则 L4 变成什么也不做的空转任务）")
	}
	if !in.airInterrupt {
		t.Fatal("必须被标记为 airInterrupt")
	}
}

// 判据：普通空输入**不能**被误认成空中断。
//
// 理由：客户端空包若也走裁剪路径，会变成一次无声的上下文裁剪，行为不可预测。
func TestAirInterrupt_RejectsOrdinaryEmptyInput(t *testing.T) {
	a := newOverflowAgent(2000, 2, 20, 400)
	cases := []struct {
		name    string
		payload map[string]interface{}
	}{
		{"非内核来源", map[string]interface{}{"interrupt": true, "content": ""}},
		{"非中断类型", map[string]interface{}{"kernel": true, "content": ""}},
	}
	for _, c := range cases {
		evt := &agentIO.InputEvent{Type: "interrupt", Source: "x", Payload: c.payload}
		if c.name == "非中断类型" {
			evt.Type = "text"
		}
		if in, ok := a.resolveInput(evt); ok && in.airInterrupt {
			t.Errorf("%s 不应被认作空中断", c.name)
		}
	}
	// 插件发的带正文中断也不是空中断
	evt := &agentIO.InputEvent{
		Type:    "interrupt",
		Payload: map[string]interface{}{"kernel": true, "content": "设备上线了"},
	}
	if in, ok := a.resolveInput(evt); ok && in.airInterrupt {
		t.Error("带正文的中断不应被认作空中断")
	}
}
