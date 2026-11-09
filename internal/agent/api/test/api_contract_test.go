package test

// internal/agent/api 的黑盒判据：只调导出函数/方法，输入 → 可观察输出。
// 不读源码、不碰 m.status、不用 ForTest。

import (
	"context"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// ─────────── 纯函数：输入 → 输出 ───────────

// TestEstimateTokensIsMonotonic：更长的文本不得得到更少的 token。
func TestEstimateTokensIsMonotonic(t *testing.T) {
	short := api.EstimateTokens("你好")
	long := api.EstimateTokens("你好" + strings.Repeat("这是一段用于估算长度的中文文本", 20))
	if short <= 0 {
		t.Fatalf("★ EstimateTokens(短文本) = %d，期望 > 0", short)
	}
	if long <= short {
		t.Fatalf("★ 长文本估算 %d 未超过短文本 %d", long, short)
	}
}

// TestTruncateByTokensRespectsBudget：截断结果不得超过预算。
func TestTruncateByTokensRespectsBudget(t *testing.T) {
	src := strings.Repeat("中文内容ABC ", 200)
	for _, budget := range []int{8, 32, 128} {
		got := api.TruncateByTokens(src, budget)
		if n := api.EstimateTokens(got); n > budget+1 {
			t.Fatalf("★ budget=%d 截断后估算 %d，超预算", budget, n)
		}
	}
}

// TestModelContextWindowAlwaysPositive：任何模型名都给出可用的正数窗口。
func TestModelContextWindowAlwaysPositive(t *testing.T) {
	for _, m := range []string{"deepseek-chat", "qwen3-vl", "完全未知的模型", ""} {
		if w := api.ModelContextWindow(m); w <= 0 {
			t.Fatalf("★ ModelContextWindow(%q) = %d，期望 > 0", m, w)
		}
	}
}

// TestIsValidSourceConfig：四字段 + URL scheme 的输入→输出真值表。
func TestIsValidSourceConfig(t *testing.T) {
	cases := []struct {
		name, url, model, adapter string
		want                      bool
	}{
		{"deepseek", "https://api.deepseek.com", "deepseek-chat", "openai", true},
		{"local", "http://127.0.0.1:8081/v1", "AUTO", "openai", true},
		{"", "https://x", "m", "a", false},
		{"n", "ftp://x", "m", "a", false},
		{"n", "https://x", "", "a", false},
		{"n", "https://x", "nil", "a", false},
		{"n", "https://x", "m", "<nil>", false},
	}
	for _, c := range cases {
		if got := api.IsValidSourceConfig(c.name, c.url, c.model, c.adapter); got != c.want {
			t.Errorf("★ IsValidSourceConfig(%q,%q,%q,%q)=%v，期望 %v",
				c.name, c.url, c.model, c.adapter, got, c.want)
		}
	}
}

// ─────────── ProviderManager：输入 → 可观察输出 ───────────

type stubProvider struct{ name string }

func (s *stubProvider) Name() string          { return s.name }
func (s *stubProvider) MaxContextTokens() int { return 8192 }
func (s *stubProvider) Chat(ctx context.Context, req *api.CompletionRequest) (*api.CompletionResponse, error) {
	return &api.CompletionResponse{Content: "ok"}, nil
}
func (s *stubProvider) ChatStream(ctx context.Context, req *api.CompletionRequest) (<-chan api.StreamChunk, error) {
	return nil, context.Canceled
}

// TestRegisterGetListDefault：注册顺序、按名取回、默认源。
func TestRegisterGetListDefault(t *testing.T) {
	m := api.NewProviderManager()
	m.Register("first", &stubProvider{name: "first"})
	m.Register("second", &stubProvider{name: "second"})
	if got := m.ProviderCount(); got != 2 {
		t.Fatalf("★ ProviderCount = %d，期望 2", got)
	}
	if got := m.List(); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("★ List = %v，期望注册顺序", got)
	}
	if p := m.Get("second"); p == nil || p.Name() != "second" {
		t.Fatalf("★ Get(\"second\") = %v", p)
	}
	if p := m.Default(); p == nil || p.Name() != "first" {
		t.Fatalf("★ Default = %v，期望第一个注册的源", p)
	}
	if err := m.SetDefault("second"); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	if p := m.Default(); p.Name() != "second" {
		t.Fatalf("★ SetDefault 后 Default = %q，期望 second", p.Name())
	}
	if err := m.SetDefault("missing"); err == nil {
		t.Fatal("★ SetDefault 不存在的源应报错")
	}
}

// TestAvailabilityStateMachine：MarkUnavailable → IsAvailable=false →
// ResetAvailability → 恢复。这是 provider 熔断的可观察效果。
func TestAvailabilityStateMachine(t *testing.T) {
	m := api.NewProviderManager()
	m.Register("p", &stubProvider{name: "p"})
	if !m.IsAvailable("p") {
		t.Fatal("★ 新注册源不可用")
	}
	m.MarkUnavailable("p")
	if m.IsAvailable("p") {
		t.Fatal("★ MarkUnavailable 后仍报告可用 —— 熔断没生效")
	}
	m.ResetAvailability("p")
	if !m.IsAvailable("p") {
		t.Fatal("★ ResetAvailability 后仍报告不可用")
	}
}

// TestProviderErrorCarriesStatusAndMessage：错误对象是对外输出。
func TestProviderErrorCarriesStatusAndMessage(t *testing.T) {
	e := &api.ProviderError{StatusCode: 429, Message: "rate limit", Kind: api.ErrTransient}
	if e.Error() != "rate limit" {
		t.Fatalf("★ Error() = %q", e.Error())
	}
	if e.StatusCode != 429 || e.Kind != api.ErrTransient {
		t.Fatalf("★ 状态码/类别丢失：%d / %v", e.StatusCode, e.Kind)
	}
}
