package core

import (
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// 会话级用量累计：跨调用求和，且命中率口径必须正确。
//
// ── 为什么需要 ──
//
// 单次用量一直在（StageCtx、LLM chain 事件），但没有任何地方把多次调用加起来，
// 于是「这个会话花了多少、缓存省了多少」答不出来。这是「无法统计缓存命中」
// 的另一半：数据流出来了，却没有落点。

// TestLedgerSumsAcrossCalls 跨调用求和。
func TestLedgerSumsAcrossCalls(t *testing.T) {
	var l usageLedger
	l.record(agentAPI.TokenUsage{
		Prompt: 1000, Completion: 50, Total: 1050,
		CacheRead: 768, CacheReported: true,
	})
	l.record(agentAPI.TokenUsage{
		Prompt: 2000, Completion: 80, Total: 2080,
		CacheRead: 1024, CacheMiss: 256, CacheReported: true,
		ReasoningTokens: 300,
	})

	got := l.snapshot()
	if got.Calls != 2 {
		t.Errorf("Calls = %d，期望 2", got.Calls)
	}
	if got.Prompt != 3000 {
		t.Errorf("Prompt = %d，期望 3000", got.Prompt)
	}
	if got.Completion != 130 {
		t.Errorf("Completion = %d，期望 130", got.Completion)
	}
	if got.Total != 3130 {
		t.Errorf("Total = %d，期望 3130", got.Total)
	}
	if got.CacheRead != 1792 {
		t.Errorf("CacheRead = %d，期望 1792（768+1024）", got.CacheRead)
	}
	if got.Reasoning != 300 {
		t.Errorf("Reasoning = %d，期望 300", got.Reasoning)
	}
	if got.CacheReportedCalls != 2 {
		t.Errorf("CacheReportedCalls = %d，期望 2", got.CacheReportedCalls)
	}
}

// TestLedgerHitRateExcludesUnreportedCalls 是这组判据里最重要的一条：
// **命中率的分母只能算「上游报了缓存」的调用**。
//
// 为何：没报缓存的提供商/版本不是「命中 0」而是「不知道」。
// 把它们算进分母会把命中率稀释成一个无意义的低值，
// 让人去优化一个本来就没开的功能 —— 拿假数据做决定。
func TestLedgerHitRateExcludesUnreportedCalls(t *testing.T) {
	var l usageLedger
	// ① 报了缓存：768 命中 / 256 未命中
	l.record(agentAPI.TokenUsage{
		Prompt: 1024, CacheRead: 768, CacheMiss: 256, CacheReported: true,
	})
	// ② 没报缓存：只有总量，绝不能拿它稀释命中率
	l.record(agentAPI.TokenUsage{Prompt: 100000, Completion: 500})

	got := l.snapshot()
	rate, ok := got.CacheHitRate()
	if !ok {
		t.Fatal("CacheHitRate ok=false —— 有一次调用报过缓存，应可计算")
	}
	// 正确：768/(768+256) = 0.75
	if rate < 0.7499 || rate > 0.7501 {
		t.Errorf("CacheHitRate = %.4f，期望 0.75（768/1024）—— "+
			"若把未报缓存的 100000 token 算进分母会得到 0.0076 这种无意义值", rate)
	}
}

// TestLedgerHitRateUnavailableWhenNothingReported 守住「不知道」与「0%」的分野。
//
// 混为一谈的后果是撒谎：把「无数据」显示成 0% 命中率。
func TestLedgerHitRateUnavailableWhenNothingReported(t *testing.T) {
	var l usageLedger
	l.record(agentAPI.TokenUsage{Prompt: 5000, Completion: 100}) // 上游没提缓存
	got := l.snapshot()
	if _, ok := got.CacheHitRate(); ok {
		t.Error("没有任何调用报过缓存，CacheHitRate 应返回 ok=false（表示「不知道」），" +
			"而不是一个 0 值让人读成「命中率 0%」")
	}
}

// TestLedgerHitRateReportedButZero 覆盖「上游报了缓存、这次全未命中」。
//
// 这时**必须**可计算并给出 0 —— 这与「没报缓存」是两件事。
func TestLedgerHitRateReportedButZero(t *testing.T) {
	var l usageLedger
	l.record(agentAPI.TokenUsage{
		Prompt: 1000, CacheRead: 0, CacheMiss: 1000, CacheReported: true,
	})
	got := l.snapshot()
	rate, ok := got.CacheHitRate()
	if !ok {
		t.Fatal("上游报了缓存（命中为 0），应可计算 —— 这是「这次全未命中」，" +
			"不是「不知道」")
	}
	if rate != 0 {
		t.Errorf("CacheHitRate = %.4f，期望 0", rate)
	}
}

// TestLedgerFillsMissingTotal 上游没给 total 时按分量补，
// 避免累计出现「total 小于 prompt+completion」的自相矛盾记录。
func TestLedgerFillsMissingTotal(t *testing.T) {
	var l usageLedger
	l.record(agentAPI.TokenUsage{Prompt: 100, Completion: 20}) // Total 缺失
	got := l.snapshot()
	if got.Total != 120 {
		t.Errorf("Total = %d，期望 120（prompt 100 + completion 20，"+
			"上游未给 total 时应补出）", got.Total)
	}
}

// TestLedgerConcurrentRecordIsSafe 并发记账不丢数。
//
// 为何要测：LLM 调用来自多个 goroutine（主 agent、驻留子 agent、
// 工具内部发起的子调用）。计数丢失是**静默**的 —— 少算一点没人看得出来，
// 所以这里用 -race 与总量双重验证。
func TestLedgerConcurrentRecordIsSafe(t *testing.T) {
	var l usageLedger
	const goroutines, each = 8, 250

	done := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		go func() {
			for i := 0; i < each; i++ {
				l.record(agentAPI.TokenUsage{Prompt: 1, Completion: 1, Total: 2})
			}
			done <- struct{}{}
		}()
	}
	for g := 0; g < goroutines; g++ {
		<-done
	}

	got := l.snapshot()
	want := int64(goroutines * each)
	if got.Calls != want {
		t.Errorf("Calls = %d，期望 %d —— 并发记账丢了计数", got.Calls, want)
	}
	if got.Prompt != want {
		t.Errorf("Prompt = %d，期望 %d", got.Prompt, want)
	}
}

// TestLedgerReset 清零（供「重置统计」入口使用）。
func TestLedgerReset(t *testing.T) {
	var l usageLedger
	l.record(agentAPI.TokenUsage{Prompt: 500, Completion: 10, Total: 510})
	l.reset()
	got := l.snapshot()
	if got.Calls != 0 || got.Prompt != 0 || got.Total != 0 {
		t.Errorf("reset 后仍有残留: %+v", got)
	}
}
