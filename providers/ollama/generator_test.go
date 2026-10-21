package ollamagen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/pkg/generation"
)

// 测试不依赖真模型：起一个本地 httptest server 模拟 Ollama，
// 断言 Go 侧发出的请求形态（schema 透传、线程参数、think:false）与解析正确。

func newTestProvider(t *testing.T, handler http.HandlerFunc) (*Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p, err := New(generation.Config{Options: map[string]string{
		"endpoint": srv.URL + "/api/generate",
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.Close)
	return p, srv
}

// ★ 关键契约：JSONSchema 必须原样透传成 Ollama 的 format 字段。
// 实测证明这是 1.7b 零幻觉的唯一机制——丢了它模型就输出聊天腔。
func TestGenerate_透传Schema到Format(t *testing.T) {
	schema := `{"type":"object","properties":{"fields":{"type":"array"}}}`
	var gotFormat map[string]any
	var gotThink bool
	var gotThread int = -1

	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if f, ok := req["format"].(map[string]any); ok {
			gotFormat = f
		}
		gotThink, _ = req["think"].(bool)
		if opts, ok := req["options"].(map[string]any); ok {
			if v, ok := opts["num_thread"].(float64); ok {
				gotThread = int(v)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": `{"fields":[]}`, "done": true, "done_reason": "stop",
		})
	})

	resp, err := p.Generate(context.Background(), generation.Request{
		Prompt: "测试", MaxTokens: 64, JSONSchema: schema,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// schema 作为对象透传（json.RawMessage），不是字符串包装
	if gotFormat == nil {
		t.Error("format 未透传：服务端未收到 format 字段")
	} else if _, ok := gotFormat["properties"]; !ok {
		t.Errorf("format 应保留 schema 的 properties 键，实际 %v", gotFormat)
	}
	if gotThink {
		t.Errorf("think 应为 false（实测不关思考 1.7b 会先输出大段思考再答案）")
	}
	if gotThread != defaultThreads {
		t.Errorf("num_thread 应默认 %d，实际 %v", defaultThreads, gotThread)
	}
	if resp.Text != `{"fields":[]}` {
		t.Errorf("Text=%q", resp.Text)
	}
	if resp.Truncated {
		t.Errorf("stop 不应报 Truncated")
	}
}

// 非法 schema 必须在本地拒绝，而不是发出去让服务端报含糊的错。
func TestGenerate_非法Schema本地拒绝(t *testing.T) {
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("不应发出请求")
	})
	_, err := p.Generate(context.Background(), generation.Request{
		Prompt: "x", JSONSchema: "{not json",
	})
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("应报 schema 非法，实际: %v", err)
	}
}

// HTTP 错误要带状态码和响应片段，不能只给 "request failed"。
func TestGenerate_HTTP错误带响应体(t *testing.T) {
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"model busy"}`))
	})
	_, err := p.Generate(context.Background(), generation.Request{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "model busy") {
		t.Fatalf("错误信息应含状态码与响应体，实际: %v", err)
	}
}

// 服务端报 error 字段时必须转发给调用方。
func TestGenerate_服务端Error字段(t *testing.T) {
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "model not found"})
	})
	_, err := p.Generate(context.Background(), generation.Request{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("应转发服务端 error，实际: %v", err)
	}
}

// done_reason=length 必须映射成 Truncated，让调用方对截断的结构化输出起疑。
func TestGenerate_截断标记(t *testing.T) {
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": `{"fields":[{"name":"a","value":"1"},{"name":"b","va`,
			"done":     true, "done_reason": "length",
		})
	})
	resp, err := p.Generate(context.Background(), generation.Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !resp.Truncated {
		t.Fatal("done_reason=length 应标记 Truncated")
	}
}

// SPI 注册表联通：名字必须能从注册表里 Open 出来（核心的引用方式）。
func TestProvider通过SPI注册表打开(t *testing.T) {
	if _, err := generation.Open("__不存在__", generation.Config{}); err == nil {
		t.Fatal("未知 provider 应报错")
	}
	// ollama 注册本身不建连接（New 不 ping），可以安全 Open
	p, err := generation.Open("ollama", generation.Config{Options: map[string]string{
		"model": "test-model",
	}})
	if err != nil {
		t.Fatalf("Open(ollama): %v", err)
	}
	defer p.Close()
	if info := p.Info(); info.Model != "test-model" || !info.SupportsJSONSchema {
		t.Errorf("Info=%+v", info)
	}
}

// 未设/空白 model 回退到默认模型，而不是构造出一个空模型名的 provider。
func TestNew_空模型名回退默认(t *testing.T) {
	p, err := New(generation.Config{Options: map[string]string{"model": "  "}})
	if err != nil {
		t.Fatalf("空白 model 应回退默认而非失败: %v", err)
	}
	defer p.Close()
	if p.Info().Model != defaultModel {
		t.Errorf("应回退默认 %q，实际 %q", defaultModel, p.Info().Model)
	}
}
