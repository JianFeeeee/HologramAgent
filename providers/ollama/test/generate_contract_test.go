package test

// providers/ollama 的黑盒判据：New(cfg) → Generate → Response 输出。
// 用 httptest 提供真实 HTTP 输入，不 mock client、不碰 p.endpoint。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JianFeeeee/HologramAgent/pkg/generation"
	ollama "github.com/JianFeeeee/HologramAgent/providers/ollama"
)

func newProvider(t *testing.T, endpoint string) *ollama.Provider {
	t.Helper()
	p, err := ollama.New(generation.Config{Options: map[string]string{
		"endpoint": endpoint, "model": "contract-model",
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// TestGenerateReturnsText：200 + {"response":...} → Response.Text。
func TestGenerateReturnsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":"生成结果","done_reason":"stop"}`))
	}))
	defer srv.Close()

	out, err := newProvider(t, srv.URL).Generate(context.Background(), generation.Request{Prompt: "你好"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out.Text != "生成结果" {
		t.Fatalf("★ Text = %q，期望 生成结果", out.Text)
	}
	if out.Truncated {
		t.Fatal("★ done_reason=stop 却报告 Truncated")
	}
}

// TestGenerateMarksTruncated：done_reason=length → Truncated=true。
func TestGenerateMarksTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"response":"半截","done_reason":"length"}`))
	}))
	defer srv.Close()

	out, err := newProvider(t, srv.URL).Generate(context.Background(), generation.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Truncated {
		t.Fatal("★ done_reason=length 未标记 Truncated")
	}
}

// TestGenerateHTTPError：非 200 → 报错。
func TestGenerateHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	if _, err := newProvider(t, srv.URL).Generate(context.Background(), generation.Request{}); err == nil {
		t.Fatal("★ 5xx 却没报错")
	}
}

// TestGenerateRejectsInvalidJSONSchema：非 JSON 的 schema 必须报错。
func TestGenerateRejectsInvalidJSONSchema(t *testing.T) {
	p, err := ollama.New(generation.Config{Options: map[string]string{"model": "m"}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Generate(context.Background(), generation.Request{JSONSchema: "not-json"}); err == nil {
		t.Fatal("★ 非法 JSONSchema 被接受")
	}
}

// TestNewFallsBackToDefaultModel：空 model 走默认值，构造仍可用。
func TestNewFallsBackToDefaultModel(t *testing.T) {
	p, err := ollama.New(generation.Config{Options: map[string]string{"model": ""}})
	if err != nil {
		t.Fatalf("★ 空 model 应回退默认值，实际报错：%v", err)
	}
	defer p.Close()
	if got := p.Info().Model; got == "" {
		t.Fatal("★ 默认模型名为空")
	}
}
