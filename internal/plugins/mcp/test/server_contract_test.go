package test

// mcp 的黑盒判据：httptest 提供真实 HTTP JSON-RPC 输入，
// 通过公开 NewSSETransport/NewServer/ListTools/CallTool 观察输出。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugins/mcp"
)

func rpcServer(t *testing.T, handler func(method string, body []byte) (status int, ct string, resp string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct{ Method string }
		_ = json.Unmarshal(body, &req)
		st, ct, resp := handler(req.Method, body)
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(st)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T, url string) *mcp.Server {
	t.Helper()
	s := mcp.NewServer("contract-mcp", mcp.NewSSETransport(url))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestListToolsThroughHTTP(t *testing.T) {
	srv := rpcServer(t, func(method string, body []byte) (int, string, string) {
		if method != "tools/list" {
			t.Errorf("method = %q，期望 tools/list", method)
		}
		return 200, "application/json", `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"read_file","description":"读取文件"},{"name":"write_file","description":"写文件"}]}}`
	})
	tools, err := newClient(t, srv.URL).ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "read_file" || tools[1].Name != "write_file" {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestCallToolSuccessAndEmptyContentHandling(t *testing.T) {
	srv := rpcServer(t, func(method string, body []byte) (int, string, string) {
		if method != "tools/call" {
			t.Errorf("method = %q，期望 tools/call", method)
		}
		return 200, "application/json", `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"world"}],"isError":false}}`
	})
	out, err := newClient(t, srv.URL).CallTool("greet", map[string]interface{}{"x": 1})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out != "world" {
		t.Fatalf("out = %q", out)
	}

	emptySrv := rpcServer(t, func(method string, body []byte) (int, string, string) {
		return 200, "application/json", `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`
	})
	if _, err := newClient(t, emptySrv.URL).CallTool("x", nil); err == nil || !strings.Contains(err.Error(), "未返回任何文本") {
		t.Fatalf("空内容未被判失败：%v", err)
	}
}

func TestCallToolSurfacesIsErrorAsFailure(t *testing.T) {
	srv := rpcServer(t, func(method string, body []byte) (int, string, string) {
		return 200, "application/json", `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"id is required"}],"isError":true}}`
	})
	_, err := newClient(t, srv.URL).CallTool("x", nil)
	if err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Fatalf("isError 未被判失败：%v", err)
	}
}

func TestCallToolSurfacesRPCError(t *testing.T) {
	srv := rpcServer(t, func(method string, body []byte) (int, string, string) {
		return 200, "application/json", `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"bad params"}}`
	})
	_, err := newClient(t, srv.URL).CallTool("x", nil)
	if err == nil || !strings.Contains(err.Error(), "bad params") {
		t.Fatalf("RPC error 未被透出：%v", err)
	}
}

func TestCallToolSurfacesHTTPError(t *testing.T) {
	srv := rpcServer(t, func(method string, body []byte) (int, string, string) {
		return 500, "text/plain", "boom"
	})
	_, err := newClient(t, srv.URL).CallTool("x", nil)
	if err == nil || !strings.Contains(err.Error(), "http status 500") {
		t.Fatalf("HTTP 错误未被透出：%v", err)
	}
}

func TestListToolsParseSSEStreamFrame(t *testing.T) {
	srv := rpcServer(t, func(method string, body []byte) (int, string, string) {
		frame := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"sse_tool","description":"d"}]}}`
		return 200, "text/event-stream", "event: message\ndata: " + frame + "\n\n"
	})
	tools, err := newClient(t, srv.URL).ListTools()
	if err != nil {
		t.Fatalf("SSE LL: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "sse_tool" {
		t.Fatalf("tools=%+v", tools)
	}
}
