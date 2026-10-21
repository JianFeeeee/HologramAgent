package core

import (
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// 这组判据把「上下文管理的阈值」从硬编码变成可配置，并守住两条纪律：
//
//  1. **零值等价**：ContextTuning 零值必须与硬编码时代**逐值相同**。
//     否则升级内核就等于悄悄改了所有实例的上下文策略。
//  2. **没有隐形断层**：写死 0.8 与 600000 两个常量会合成一个没人能一眼看出的
//     分段函数 —— 窗口 ≤750k 时工作面是 80%，>750k 时退化成 600000/window。
//     现在拐点必须是**显式配置**，而不是两个常量相乘的副产品。

// windowProvider 已在 tokenbudget_test.go 声明（字段 n，值接收者）——
// 直接复用，不重复定义（同名类型重复声明会让整包编译失败）。

// legacyBudget 复刻**硬编码时代**的算法（0.8 / 600000 / available/3）。
// 判据拿它当规格基准 —— 与 C 侧 golden 测试用参考实现当基准同一思路。
func legacyBudget(maxCtx, fixed int) TokenBudget {
	if maxCtx <= 0 {
		maxCtx = 32768
	}
	targetUsage := int(float64(maxCtx) * 0.8)
	if targetUsage > 600000 {
		targetUsage = 600000
	}
	reserved := maxCtx - targetUsage
	if reserved < 0 {
		reserved = 0
	}
	available := targetUsage - fixed
	if available < 0 {
		available = 0
	}
	mem := available / 3
	return TokenBudget{
		MaxContext:    maxCtx,
		TargetUsage:   targetUsage,
		FixedTokens:   fixed,
		MemoryTokens:  mem,
		ContextTokens: available - mem,
		Reserved:      reserved,
	}
}

// TestContextTuningZeroValueEqualsLegacy 是**升级安全**判据。
//
// 覆盖小窗口、200k（本次要测的目标）、以及 600k/750k/1M（断层两侧）。
func TestContextTuningZeroValueEqualsLegacy(t *testing.T) {
	for _, w := range []int{0, 8192, 32768, 131072, 200000, 262144, 600000, 750000, 1048576, 2000000} {
		p := windowProvider{n: w}
		got := ComputeTokenBudgetTuned(p, "", ContextTuning{})
		want := legacyBudget(w, 0)
		if got != want {
			t.Errorf("窗口 %d：零值 tuning 与硬编码不一致\n got=%+v\nwant=%+v", w, got, want)
		}
	}
}

// TestContextTuningUtilizationPercent 验利用率可调（阈值 1）。
func TestContextTuningUtilizationPercent(t *testing.T) {
	p := windowProvider{n: 200000}
	// 80% ⇒ 160000（与历史一致）
	if got := ComputeTokenBudgetTuned(p, "", ContextTuning{UtilizationPercent: 80}).TargetUsage; got != 160000 {
		t.Errorf("80%% 时 TargetUsage=%d，期望 160000", got)
	}
	// 50% ⇒ 100000
	if got := ComputeTokenBudgetTuned(p, "", ContextTuning{UtilizationPercent: 50}).TargetUsage; got != 100000 {
		t.Errorf("50%% 时 TargetUsage=%d，期望 100000", got)
	}
	// 95% ⇒ 190000
	if got := ComputeTokenBudgetTuned(p, "", ContextTuning{UtilizationPercent: 95}).TargetUsage; got != 190000 {
		t.Errorf("95%% 时 TargetUsage=%d，期望 190000", got)
	}
}

// TestContextTuningMaxTargetUncappedRemovesKink 是本轮的核心动机。
//
// 历史行为：窗口 1M 时工作面只有 600000（≈60%），因为被 600000 封顶；
// 而窗口 700k 时工作面是 560000（80%）。同一个「0.8」在两侧给出不同利用率。
//
// MaxTargetTokens < 0 ⇒ 不封顶 ⇒ 利用率对所有窗口都真的等于配置值。
func TestContextTuningMaxTargetUncappedRemovesKink(t *testing.T) {
	const uncapped = -1

	// 1M 窗口、80% ⇒ 838860（而不是被截到 600000）
	got := ComputeTokenBudgetTuned(windowProvider{n: 1048576}, "",
		ContextTuning{UtilizationPercent: 80, MaxTargetTokens: uncapped})
	if got.TargetUsage != 838860 {
		t.Errorf("不封顶时 1M 窗口 TargetUsage=%d，期望 838860（80%%）", got.TargetUsage)
	}

	// 关键：不封顶后，利用率必须是**常数**（这才是"按总上下文自动调整"）。
	for _, w := range []int{200000, 400000, 600000, 750000, 1048576, 2000000} {
		b := ComputeTokenBudgetTuned(windowProvider{n: w}, "",
			ContextTuning{UtilizationPercent: 70, MaxTargetTokens: uncapped})
		want := w * 70 / 100
		if b.TargetUsage != want {
			t.Errorf("窗口 %d：70%% 不封顶时 TargetUsage=%d，期望 %d（利用率应为常数）",
				w, b.TargetUsage, want)
		}
		if b.Reserved != w-want {
			t.Errorf("窗口 %d：Reserved=%d，期望 %d", w, b.Reserved, w-want)
		}
	}

	// 而历史默认（封顶 600000）在 1M 窗口上仍然是 600000 —— 未被改变。
	hist := ComputeTokenBudgetTuned(windowProvider{n: 1048576}, "", ContextTuning{})
	if hist.TargetUsage != 600000 {
		t.Errorf("默认仍应封顶 600000，实际 %d", hist.TargetUsage)
	}
}

// TestContextTuningMemoryRatio 验记忆/事件的切分可调（阈值 2）。
func TestContextTuningMemoryRatio(t *testing.T) {
	p := windowProvider{n: 200000}
	// 可用预算 = 160000 - 0 = 160000
	// 自动（0）⇒ 1/3 = 53333（与历史一致）
	auto := ComputeTokenBudgetTuned(p, "", ContextTuning{})
	if auto.MemoryTokens != 160000/3 {
		t.Errorf("自动记忆预算=%d，期望 %d", auto.MemoryTokens, 160000/3)
	}
	if auto.MemoryTokens+auto.ContextTokens != 160000 {
		t.Errorf("切分不守恒：%d + %d != 160000", auto.MemoryTokens, auto.ContextTokens)
	}
	// 手动 50% ⇒ 80000 / 80000
	half := ComputeTokenBudgetTuned(p, "", ContextTuning{MemoryRatioPercent: 50})
	if half.MemoryTokens != 80000 || half.ContextTokens != 80000 {
		t.Errorf("50%% 记忆 ⇒ mem=%d ctx=%d，期望 80000/80000",
			half.MemoryTokens, half.ContextTokens)
	}
	// 手动 20% ⇒ 32000 / 128000
	fifth := ComputeTokenBudgetTuned(p, "", ContextTuning{MemoryRatioPercent: 20})
	if fifth.MemoryTokens != 32000 || fifth.ContextTokens != 128000 {
		t.Errorf("20%% 记忆 ⇒ mem=%d ctx=%d，期望 32000/128000",
			fifth.MemoryTokens, fifth.ContextTokens)
	}
	// 任意比例都必须守恒（不能因为取整丢 token）
	for _, pct := range []int{1, 7, 33, 49, 51, 66, 93, 99} {
		b := ComputeTokenBudgetTuned(p, "", ContextTuning{MemoryRatioPercent: pct})
		if b.MemoryTokens+b.ContextTokens != 160000 {
			t.Errorf("%d%% 时不守恒：%d + %d != 160000",
				pct, b.MemoryTokens, b.ContextTokens)
		}
		if b.MemoryTokens < 0 || b.ContextTokens < 0 {
			t.Errorf("%d%% 时出现负预算：mem=%d ctx=%d", pct, b.MemoryTokens, b.ContextTokens)
		}
	}
}

// TestContextTuningProtectedCount 验裁剪时保留的最近条数可调（阈值 3）。
//
// 这一条影响的是**记忆召回**：Prune 永远保留最近 N 条，其余按相关性淘汰。
// N 越大，越不容易丢近处信息；N 越小，越依赖向量检索的准确度。
//
// ⚠️ 两条踩过的坑，都写进判据里：
//
//  1. 必须给**真实** embedder：Prune 用它算相关性，nil 会空指针。
//     生产不会遇到（New() 在 cfg.Embedder 为 nil 时兜底建一个）。
//  2. 保留条数的上界是 **max(topK, protectedCount)**，不是 topK。
//     「无条件保留最近 N 条」优先于 topK —— 这是**既有契约**
//     （硬编码时代的 pCount=10 在 12 事件/topK=3 下同样保留 10）。
//     本判据第一版错写成「≤ topK」，被实测纠回来：
//     实现没变，是我的断言写了实现从未有过的契约。
//
// 默认值下（protectedCount=10 << topK≈29）两者不会冲突，
// 所以这个交互只在用户把保护条数调得很大时才会显现。
func TestContextTuningProtectedCount(t *testing.T) {
	emb := memory.NewStaticEmbedder()

	// 常规：protectedCount(3) < topK(5) ⇒ 总保留 = 保护 3 + 按相关选 2 = 5
	rc := NewRelevanceContext("", emb)
	rc.protectedCount = 3
	for i := 0; i < 20; i++ {
		rc.Append(ContextEvent{Source: "t", Input: "无关内容", Response: "无关回复"})
	}
	rc.Prune("查询", 5, nil)
	if got := rc.Len(); got != 5 {
		t.Errorf("protectedCount=3/topK=5：裁剪后条数=%d，期望 5（保护 3 + 相关 2）", got)
	}

	// 边界：protectedCount(50) > topK(3) ⇒ 保护优先，且不得 panic。
	// pCount 被夹到 len(events)=12 ⇒ candidates 空 ⇒ 直接不裁 ⇒ 留 12。
	rc2 := NewRelevanceContext("", emb)
	rc2.protectedCount = 50
	for i := 0; i < 12; i++ {
		rc2.Append(ContextEvent{Source: "t", Input: "x", Response: "y"})
	}
	rc2.Prune("q", 3, nil) // 不能 panic
	if got := rc2.Len(); got != 12 {
		t.Errorf("protectedCount=50/topK=3：条数=%d，期望 12（保护优先，全部保留）", got)
	}

	// 保护条数可调必须**真的生效**：同一个输入，不同 protectedCount 给出不同结果。
	// 这是本判据的核心 —— 否则「可配置」只是说说。
	small := NewRelevanceContext("", emb)
	small.protectedCount = 2
	big := NewRelevanceContext("", emb)
	big.protectedCount = 6
	for i := 0; i < 20; i++ {
		ev := ContextEvent{Source: "t", Input: "同样内容", Response: "同样回复"}
		small.Append(ev)
		big.Append(ev)
	}
	small.Prune("查询", 8, nil)
	big.Prune("查询", 8, nil)
	if small.Len() != 8 || big.Len() != 8 {
		// topK=8 且 protectedCount 都 ≤ 8 ⇒ 两者都应收敛到 8
		t.Errorf("topK=8 时 small=%d big=%d，期望都是 8（topK 未被保护条数突破）",
			small.Len(), big.Len())
	}
}

// TestAgentCarriesContextTuning 是**接线**判据。
//
// 存在的理由：本仓刚发现 `core.agent.max_context_size` 被注册进配置面板、
// 也有默认值，但**从来没有人把它读进 AgentConfig** —— 一个"死配置"，
// 界面上改它没有任何效果，且不报任何错。所以这里断言 tuning 真的到了 Agent 上。
func TestAgentCarriesContextTuning(t *testing.T) {
	a := New(AgentConfig{
		ID: "t", Provider: windowProvider{n: 200000},
		ProviderManager: agentAPI.NewProviderManager(),
		CtxTuning: ContextTuning{
			UtilizationPercent: 70,
			MaxTargetTokens:    -1,
			MemoryRatioPercent: 25,
			ProtectedCount:     7,
		},
	})
	b := a.computeTokenBudget()
	// 窗口 200000、70% ⇒ 工作面 140000（-1 不封顶）
	if b.TargetUsage != 140000 {
		t.Errorf("Agent 未带上 UtilizationPercent：TargetUsage=%d，期望 140000",
			b.TargetUsage)
	}
	// 25% 记忆 ⇒ mem=35000、ctx=105000
	if b.MemoryTokens != 35000 || b.ContextTokens != 105000 {
		t.Errorf("Agent 未带上 MemoryRatioPercent：mem=%d ctx=%d，期望 35000/105000",
			b.MemoryTokens, b.ContextTokens)
	}
	gotProtected := -1
	if a.context != nil {
		gotProtected = a.context.protectedCount
	}
	if gotProtected != 7 {
		t.Errorf("Agent 未把 ProtectedCount 传到上下文裁剪：protectedCount=%d，期望 7",
			gotProtected)
	}
}
