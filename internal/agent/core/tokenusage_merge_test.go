package core

import (
	"context"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// 用量**分帧到达**时必须合并，不能被后帧覆盖。
//
// ── 为什么需要这条判据 ──
//
// accumulateStream 原先是 `resp.TokenUsage = *ck.Usage` —— 一句直接赋值。
// 对「只在末帧报一次用量」的协议（OpenAI include_usage）它恰好正确，
// 所以长期看不出问题。但 Anthropic 是**分两帧**报的：
//
//	message_start → input_tokens + cache_read_input_tokens + cache_creation
//	message_delta → output_tokens
//
// 覆盖的后果：最终只剩 completion，prompt 与缓存计数全归零。
// 而这一切**不报任何错**，只是数字小了一个量级 ——
// 看起来像「这个模型的输入真的很短」，于是缓存收益显得不存在。
//
// 「恰好正确」不等于正确：它依赖上游把用量塞在同一帧里，
// 而这既不是协议保证，也不是我们的契约。

// TestTokenUsageMergesAcrossFrames 模拟 Anthropic 的两帧分离形态。
func TestTokenUsageMergesAcrossFrames(t *testing.T) {
	ch := make(chan agentAPI.StreamChunk, 4)
	go func() {
		// 首帧：只有输入侧与缓存（message_start 的形态）
		ch <- agentAPI.StreamChunk{Usage: &agentAPI.TokenUsage{
			Prompt:        1896,
			CacheRead:     768,
			CacheMiss:     128,
			CacheReported: true,
		}}
		// 次帧：只有输出侧（message_delta 的形态），其余字段为零值
		ch <- agentAPI.StreamChunk{
			Done:         true,
			FinishReason: "stop",
			Usage:        &agentAPI.TokenUsage{Completion: 250},
		}
		close(ch)
	}()

	resp, err := accumulateStream(context.Background(), ch, nil, "cli", 4096)
	if err != nil {
		t.Fatalf("accumulateStream 失败: %v", err)
	}
	u := resp.TokenUsage

	if u.Prompt != 1896 {
		t.Errorf("Prompt = %d，期望 1896 —— 被后帧的零值覆盖了（后帧只带 completion）",
			u.Prompt)
	}
	if u.CacheRead != 768 {
		t.Errorf("CacheRead = %d，期望 768 —— 缓存命中数被后帧覆盖", u.CacheRead)
	}
	if u.CacheMiss != 128 {
		t.Errorf("CacheMiss = %d，期望 128", u.CacheMiss)
	}
	if u.Completion != 250 {
		t.Errorf("Completion = %d，期望 250（后帧的值必须生效）", u.Completion)
	}
	if !u.CacheReported {
		t.Error("CacheReported = false —— 首帧已声明「上游报了缓存」，" +
			"后帧没提这件事不等于要撤销它")
	}
}

// TestTokenUsageMergeKeepsCacheReportedSticky 守住「一旦报过就一直是报过」。
//
// 为什么单独一条：后续帧通常**不带**缓存字段（例如只有 output_tokens 的
// message_delta）。若用「后帧为准」的实现，会把首帧刚立起来的
// CacheReported 抹成 false ⇒ 界面从「命中 40%」退回「无数据」，
// 用户会以为监控坏了。
func TestTokenUsageMergeKeepsCacheReportedSticky(t *testing.T) {
	ch := make(chan agentAPI.StreamChunk, 3)
	go func() {
		ch <- agentAPI.StreamChunk{Usage: &agentAPI.TokenUsage{
			Prompt:        100,
			CacheRead:     40,
			CacheReported: true,
		}}
		ch <- agentAPI.StreamChunk{Usage: &agentAPI.TokenUsage{Completion: 10}}
		ch <- agentAPI.StreamChunk{Done: true, FinishReason: "stop"}
		close(ch)
	}()

	resp, err := accumulateStream(context.Background(), ch, nil, "cli", 4096)
	if err != nil {
		t.Fatalf("accumulateStream 失败: %v", err)
	}
	if !resp.TokenUsage.CacheReported {
		t.Error("CacheReported 被后续不带缓存字段的帧抹掉了")
	}
	if resp.TokenUsage.CacheRead != 40 {
		t.Errorf("CacheRead = %d，期望 40", resp.TokenUsage.CacheRead)
	}
}

// TestTokenUsageSingleFrameUnchanged 守住既有正确行为不被合并逻辑破坏：
// 单帧报全量（OpenAI include_usage 的形态）时结果必须与直接赋值一致。
func TestTokenUsageSingleFrameUnchanged(t *testing.T) {
	ch := make(chan agentAPI.StreamChunk, 2)
	go func() {
		ch <- agentAPI.StreamChunk{
			Done:         true,
			FinishReason: "stop",
			Usage: &agentAPI.TokenUsage{
				Prompt:        1000,
				Completion:    50,
				Total:         1050,
				CacheRead:     768,
				CacheReported: true,
			},
		}
		close(ch)
	}()

	resp, err := accumulateStream(context.Background(), ch, nil, "cli", 4096)
	if err != nil {
		t.Fatalf("accumulateStream 失败: %v", err)
	}
	u := resp.TokenUsage
	if u.Prompt != 1000 || u.Completion != 50 || u.Total != 1050 || u.CacheRead != 768 {
		t.Errorf("单帧全量用量被改动: %+v（期望 prompt=1000 completion=50 total=1050 cache_read=768）", u)
	}
}
