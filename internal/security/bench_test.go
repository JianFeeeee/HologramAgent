package security

// 热路径基准 —— 安全模块挂在每次 LLM 调用与每次工具调用上，
// 它的开销会被**乘上**这两个频次。所以必须知道数量级。
//
//	go test -bench . -benchmem ./internal/security/
//
// 关注点：
//   · Replace（出向）：每条消息扫一遍模式库 —— 未命中时应当很便宜
//   · Fill（入向）：每个工具参数调用一次
//   · FillMap（入向）：工具参数通常是嵌套 map
//   · Alias：HMAC 派生（每次拦截都要算）

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func benchStore(b *testing.B) *Store {
	b.Helper()
	dir := b.TempDir()
	s := New(Options{
		Path:    dir + "/v.json",
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	})
	if err := s.Load(); err != nil {
		b.Fatal(err)
	}
	return s
}

// ★ 最重要的一个：**未命中**时的代价（绝大多数消息不含凭据）。
//
// 若这条与"命中时"一个量级，说明模式库每次都在做昂贵的全量扫描 ——
// 那是随消息数线性放大的开销。
func BenchmarkReplaceNoMatch(b *testing.B) {
	s := benchStore(b)
	text := strings.Repeat("这是一段普通的对话内容，讨论架构与实现细节。", 20)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if out, evts := s.Replace(text, "bench", Patterns); len(evts) != 0 {
			b.Fatal("不该命中")
		} else {
			_ = out
		}
	}
}

// 命中时的代价（含 HMAC 派生 + 落盘）。
func BenchmarkReplaceWithMatch(b *testing.B) {
	s := benchStore(b)
	text := "token=ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 请使用"
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, evts := s.Replace(text, "bench", Patterns)
		if len(evts) == 0 {
			b.Fatal("应命中")
		}
	}
}

// Fill：工具参数回填。
func BenchmarkFill(b *testing.B) {
	s := benchStore(b)
	alias, _ := s.Submit("bench-secret-value-123456", "")
	text := "Authorization: token @{" + alias + "}"
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = s.Fill(text)
	}
}

// FillMap：真实工具参数（嵌套）。
func BenchmarkFillMap(b *testing.B) {
	s := benchStore(b)
	alias, _ := s.Submit("bench-secret-value-123456", "")
	args := map[string]interface{}{
		"url": "https://x:@" + alias + "@host/r",
		"nested": map[string]interface{}{
			"token": "@{" + alias + "}",
			"list":  []interface{}{"a", "@{" + alias + "}"},
		},
		"n": 42,
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.FillMap(args)
	}
}

// Alias：HMAC 派生（拦截路径上的固定成本）。
func BenchmarkAlias(b *testing.B) {
	s := benchStore(b)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := s.Alias("some-plain-secret"); err != nil {
			b.Fatal(err)
		}
	}
}

// 并发下的吞吐：模拟多 agent 同时出向。
func BenchmarkReplaceParallel(b *testing.B) {
	s := benchStore(b)
	text := strings.Repeat("普通对话内容，没有凭据。", 10)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = s.Replace(text, "bench", Patterns)
		}
	})
}

// ── provider 装饰器的端到端开销（经真实请求结构） ──

// BenchmarkProviderScrub 走真实的 api 装饰器路径。
//
// 放在 security 包里会形成 import 环，所以这里只模拟它的形状：
// 逐条消息调 Replace。真实开销 = 消息条数 × Replace。
func BenchmarkProviderScrubShape(b *testing.B) {
	s := benchStore(b)
	msgs := make([]string, 20)
	for i := range msgs {
		msgs[i] = fmt.Sprintf("第 %d 条消息：讨论实现细节，无凭据。%s", i,
			strings.Repeat("内容。", 30))
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, m := range msgs {
			_, _ = s.Replace(m, "llm_out", Patterns)
		}
	}
	_ = context.Background
}
