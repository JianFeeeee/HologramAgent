package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalizeGateway(t *testing.T) {
	cases := map[string]string{
		// 已是完整端点：原样
		"ws://devices.localhost:8080/api/v1/device/ws": "ws://devices.localhost:8080/api/v1/device/ws",
		"wss://devices.example.com/api/v1/device/ws":   "wss://devices.example.com/api/v1/device/ws",
		// 只有 ws 根：补路径
		"ws://127.0.0.1:9890":       "ws://127.0.0.1:9890/api/v1/device/ws",
		"ws://devices.example.com/": "ws://devices.example.com/api/v1/device/ws",
		// 裸 host:port：补 scheme + 路径（旧行为）
		"127.0.0.1:9890":           "ws://127.0.0.1:9890/api/v1/device/ws",
		"devices.example.com:8080": "ws://devices.example.com:8080/api/v1/device/ws",
		// http(s) → ws(s)
		"http://127.0.0.1:9890":       "ws://127.0.0.1:9890/api/v1/device/ws",
		"https://devices.example.com": "wss://devices.example.com/api/v1/device/ws",
		// ★ 子域地址不得被改写（这正是改造后的正确形态）
		"devices.example.com": "ws://devices.example.com/api/v1/device/ws",
		// 空
		"":   "",
		"  ": "",
	}
	for in, want := range cases {
		if got := normalizeGateway(in); got != want {
			t.Errorf("normalizeGateway(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 发现端点返回权威地址时，必须采用它（而不是自己拼门户同源地址）。
func TestDiscoverGatewayUsesServerAnswer(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-API-Key")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"available":  true,
			"url":        "ws://devices.localhost:8080/api/v1/device/ws",
			"url_portal": "ws://127.0.0.1:8080/api/v1/device/ws",
			"auth":       "none",
		})
	}))
	defer srv.Close()

	url, err := discoverGateway(srv.URL, "PORTAL-KEY", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// ★ 必须优先门户同源形态：waiter 走系统解析器，*.localhost 解析不到
	if url != "ws://127.0.0.1:8080/api/v1/device/ws" {
		t.Errorf("未优先采用门户同源形态: %q", url)
	}
	if gotPath != "/api/v1/device/gateway" {
		t.Errorf("发现路径不对: %q", gotPath)
	}
	if gotKey != "PORTAL-KEY" {
		t.Errorf("未带门户凭证: %q", gotKey)
	}
}

// 用户填的是完整网关地址时，也要能正确截到门户根再问。
func TestDiscoverGatewayFromFullEndpointInput(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(map[string]interface{}{
			"available": true, "url": "ws://devices.localhost/api/v1/device/ws",
		})
	}))
	defer srv.Close()

	// 输入形态：完整旧端点（含 /api/v1/device/ws）与 ws:// 前缀
	for _, in := range []string{
		srv.URL + "/api/v1/device/ws",
		"ws://" + srv.Listener.Addr().String() + "/api/v1/device/ws",
	} {
		gotPath = ""
		if _, err := discoverGateway(in, "k", 3*time.Second); err != nil {
			t.Errorf("输入 %q 应成功: %v", in, err)
			continue
		}
		if gotPath != "/api/v1/device/gateway" {
			t.Errorf("输入 %q 未截到门户根，实际路径 %q", in, gotPath)
		}
	}
}

// 服务端明确报告不可用 → 必须返回错误（调用方据此回退），而不是给个连不上的 URL。
func TestDiscoverGatewayUnavailableReportsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"available": false,
			"reason":    "本实例没有声明设备网关反代",
		})
	}))
	defer srv.Close()

	if _, err := discoverGateway(srv.URL, "k", 3*time.Second); err == nil {
		t.Error("服务端报告不可用时应返回错误")
	}
}

// 老版本 HologramAgent 没有该端点（404）→ 返回错误而不是 panic/空成功。
func TestDiscoverGatewayOldServerFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := discoverGateway(srv.URL, "k", 3*time.Second); err == nil {
		t.Error("404 应返回错误，让调用方回退到自配地址")
	}
	// 空地址快速失败
	if _, err := discoverGateway("", "k", 3*time.Second); err == nil {
		t.Error("空门户地址应返回错误")
	}
}

// 老版本只给 url（无 url_portal）时，仍必须能用 —— 退回子域形态。
func TestDiscoverGatewayFallsBackToSubdomainForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"available": true,
			"url":       "ws://devices.example.com/api/v1/device/ws",
		})
	}))
	defer srv.Close()
	got, err := discoverGateway(srv.URL, "k", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ws://devices.example.com/api/v1/device/ws" {
		t.Errorf("无 url_portal 时应退回 url，实际 %q", got)
	}
}

// 两者都没有 → 明确报错，而不是返回空串让调用方拿着空地址去连。
func TestDiscoverGatewayNoURLReportsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"available": true})
	}))
	defer srv.Close()
	if _, err := discoverGateway(srv.URL, "k", 3*time.Second); err == nil {
		t.Error("两个形态都缺时应报错")
	}
}
