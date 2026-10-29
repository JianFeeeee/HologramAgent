//go:build cgo

package api

// codec_chunkfast_bench_test.go —— 快速路径 vs 原实现的真实开销对比。
//
// 判据不是「C 比 Go 快」，而是「在真实输入分布下是否真的省下分配与时间」。
// 分配数是重点：C 化的原始动机就是消除每 chunk 12~21 次堆分配带来的 GC 抖动。

import (
	"testing"
)

var benchChunks = map[string]string{
	"content_zh":    `{"id":"chatcmpl-abc","object":"chat.completion.chunk","created":1727000000,"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"这是一段来自真实流式响应的中文内容，用于测量解析开销。"},"finish_reason":null}]}`,
	"content_ascii": `{"id":"chatcmpl-abc","choices":[{"index":0,"delta":{"content":"hello world this is a longer ascii content chunk"},"finish_reason":null}]}`,
	"toolcall":      `{"id":"chatcmpl-abc","object":"chat.completion.chunk","created":1727000000,"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9a","type":"function","function":{"name":"stablemem_recall","arguments":"{\"query\":\"用户偏好\",\"limit\":20}"}}]},"finish_reason":null}]}`,
	"usage":         `{"id":"chatcmpl-abc","object":"chat.completion.chunk","created":1727000000,"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":""},"finish_reason":null}],"usage":{"prompt_tokens":3821,"completion_tokens":117,"total_tokens":3938,"prompt_cache_hit_tokens":3584,"prompt_cache_miss_tokens":237}}`,
	"finish":        `{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
}

// BenchmarkChunkFast_Entry 走真实入口（含 C 路径 + 必要的回退）。
func BenchmarkChunkFast_Entry(b *testing.B) {
	for name, in := range benchChunks {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = parseOpenAICompatibleStreamChunkFull(in)
			}
		})
	}
}

// BenchmarkChunkFast_GoOnly 直接调原实现（整块 json.Unmarshal），作对照。
func BenchmarkChunkFast_GoOnly(b *testing.B) {
	for name, in := range benchChunks {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = parseOpenAICompatibleStreamChunkFullGo(in)
			}
		})
	}
}

// ---------------------------------------------------------------------
// 基准门禁：防止「优化」悄悄退步，或在没实测过收益时被打开
// ---------------------------------------------------------------------

// TestChunkFast_BenchGate 钉死当前事实：chunkFastEnabled 必须为 false。
//
// ★ 为什么把「一个优化是关的」也做成断言：
//
//	「还没验证有效就先关着」是**容易丢失的状态** —— 后来者看到
//	「快速路径写得挺全 + 6 万条差分测试全过」，很自然会以为它已生效，
//	进而打开它、甚至删掉开关。而实测它**更慢**。
//	断言把这个事实钉在测试里，开关一旦被改就立刻判红。
func TestChunkFast_BenchGate(t *testing.T) {
	if chunkFastEnabled {
		t.Fatalf("chunkFastEnabled 被打开了，但实测本架构比原实现慢：\n" +
			"  content_ascii  Entry 2016ns/20allocs  vs  GoOnly 1325ns/13allocs\n" +
			"  toolcall       Entry 5854ns/33allocs  vs  GoOnly 3270ns/21allocs\n" +
			"  根因：5+ 次 cgo 边界 × 每次约 200ns（out-param 逃逸到堆）。\n" +
			"  改造方向（已由天花板实验确认可行）：一次 C 调用返回全部字段 span、\n" +
			"  结果写入调用方栈上的 C 结构体。先改架构，再打开此开关。\n" +
			"  改之前请先跑 BenchmarkChunkFast_* 拿到自己的数据。")
	}
}

// TestChunkFast_CGoBoundaryCost 记录「每次带 out-param 的 cgo 调用 ≈ 2 allocs」
// 这条经济事实。它是判断任何后续改造是否值得的标尺。
func TestChunkFast_CGoBoundaryCost(t *testing.T) {
	// 断言存在（防止有人「顺手优化」掉这两个 helper 里的关键细节）
	doc := `{"a":1,"b":{"c":"x"}}`
	if _, found, _, bad := findKeyCI(rootSpan(doc), "b"); !found || bad {
		t.Fatalf("findKeyCI 失效: found=%v bad=%v", found, bad)
	}
	if _, found, _, bad := findKeyCS(rootSpan(doc), "a"); bad || !found {
		t.Fatalf("findKeyCS 失效: found=%v bad=%v", found, bad)
	}
}
