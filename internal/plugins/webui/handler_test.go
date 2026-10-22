package webui

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentCore "github.com/JianFeeeee/HologramAgent/internal/agent/core"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/internal/supervisor"
	"github.com/JianFeeeee/HologramAgent/internal/tracker"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

func testSDK(cfg sdk.SDKConfig) *sdk.PluginSDK {
	if cfg.EventBus == nil {
		cfg.EventBus = events.NewBus()
	}
	return sdk.New("webui", cfg)
}

func newTestHandler(t *testing.T) (*Handler, *supervisor.Daemon) {
	t.Helper()
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	sup := supervisor.New(cfg)
	sup.Start()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Config:     sdk.NewConfig(cfg),
	})
	return NewHandler(s), sup
}

func seedWebUIConfig(cfgReg *internalConfig.ConfigRegistry) {
	webuiCfg := cfgReg.PluginConfig("webui")
	webuiCfg.RegisterDef(internalConfig.ConfigDef{Key: "api_key", Default: ""})
	webuiCfg.RegisterDef(internalConfig.ConfigDef{Key: "username", Default: "admin"})
	webuiCfg.RegisterDef(internalConfig.ConfigDef{Key: "password", Default: ""})
	webuiCfg.RegisterDef(internalConfig.ConfigDef{Key: "session_ttl_hours", Default: "24"})
	webuiCfg.Set("api_key", "test-api-key")
	webuiCfg.Set("username", "admin")
	webuiCfg.Set("password", "secret-pass")
	webuiCfg.Set("session_ttl_hours", "24")
}

func TestAuthMiddleware(t *testing.T) {
	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)

	sup := supervisor.New(&types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	})
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Settings:   sdk.NewSettings("webui", cfgReg),
		Config:     sdk.NewConfig(&types.Config{}),
	})
	h := NewHandler(s)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	t.Run("api_requires_auth", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", w.Code)
		}
	})

	t.Run("api_key_allows_access", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		req.Header.Set("X-API-Key", "test-api-key")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})

	t.Run("login_sets_session_cookie", func(t *testing.T) {
		body := `{"username":"admin","password":"secret-pass"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if len(w.Result().Cookies()) == 0 {
			t.Fatal("expected session cookie")
		}
	})

	t.Run("root_redirects_to_login_without_session", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Fatalf("expected 302, got %d", w.Code)
		}
	})
}

func TestHandleStatus(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	w := httptest.NewRecorder()
	h.handleStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "running" {
		t.Errorf("expected running, got %v", resp["status"])
	}
}

func TestHandleStatusMethodNotAllowed(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/status", nil)
	w := httptest.NewRecorder()
	h.handleStatus(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleAgents(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()
	sup.RegisterAgent("test_agent")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	w := httptest.NewRecorder()
	h.handleAgents(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	agents, ok := resp["agents"].([]interface{})
	if !ok || len(agents) == 0 {
		t.Error("expected agents list")
	}
}

func TestHandleAgentByID(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()
	sup.RegisterAgent("my_agent")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/my_agent", nil)
	w := httptest.NewRecorder()
	h.handleAgentByID(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["id"] != "my_agent" {
		t.Errorf("expected my_agent, got %v", resp["id"])
	}
}

func TestHandleAgentByIDNotFound(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/nonexistent", nil)
	w := httptest.NewRecorder()
	h.handleAgentByID(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleKnowledgeSearch(t *testing.T) {
	ks := knowledge.NewStore(t.TempDir())
	ks.Start()
	ks.Add("test_doc", "this is test content for searching")

	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	sup := supervisor.New(cfg)
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Knowledge:  sdk.NewKnowledge(ks),
		Config:     sdk.NewConfig(cfg),
	})
	h := NewHandler(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge?q=test", nil)
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	results, ok := resp["results"].([]interface{})
	if !ok || len(results) == 0 {
		t.Error("expected search results")
	}
}

func TestHandleKnowledgeCreate(t *testing.T) {
	ks := knowledge.NewStore(t.TempDir())
	ks.Start()

	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	sup := supervisor.New(cfg)
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Knowledge:  sdk.NewKnowledge(ks),
		Config:     sdk.NewConfig(cfg),
	})
	h := NewHandler(s)

	body := `{"name":"new_doc","content":"fresh content"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", w.Code)
	}
}

func TestHandleKnowledgeUnavailable(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge?q=test", nil)
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleMemoryUnavailable(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory?q=test", nil)
	w := httptest.NewRecorder()
	h.handleMemory(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleTrackerNotAvailable(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tracker", nil)
	w := httptest.NewRecorder()
	h.handleTracker(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleTrackerStats(t *testing.T) {
	tr := tracker.NewTracker(t.TempDir(), t.TempDir())

	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	}
	sup := supervisor.New(cfg)
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Tracker:    tr,
		Config:     sdk.NewConfig(cfg),
	})
	h := NewHandler(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tracker", nil)
	w := httptest.NewRecorder()
	h.handleTracker(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleOpenAICompletionsNoMessages(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	body := `{"model":"test"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleOpenAICompletions(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleOpenAICompletionsLastMsgNotUser(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	body := `{"messages":[{"role":"assistant","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleOpenAICompletions(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleOpenAICompletionsMethodNotAllowed(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	h.handleOpenAICompletions(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleStaticServesHTML(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.handleStatic(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "HologramAgent Dashboard") {
		t.Error("expected dashboard HTML")
	}
}

func TestHandleStaticNotFound(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	w := httptest.NewRecorder()
	h.handleStatic(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleConfigGet(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	w := httptest.NewRecorder()
	h.handleConfig(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestRegisterRoutes(t *testing.T) {
	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)

	sup := supervisor.New(&types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	})
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Settings:   sdk.NewSettings("webui", cfgReg),
		Config:     sdk.NewConfig(&types.Config{}),
	})
	h := NewHandler(s)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	tests := []struct {
		path   string
		method string
		code   int
	}{
		{"/api/v1/status", http.MethodGet, http.StatusOK},
		{"/api/v1/agents", http.MethodGet, http.StatusOK},
		{"/api/v1/config", http.MethodGet, http.StatusOK},
		{"/api/v1/network", http.MethodGet, http.StatusOK},
		{"/", http.MethodGet, http.StatusFound},
		{"/api/v1/memory", http.MethodGet, http.StatusServiceUnavailable},
		{"/api/v1/knowledge", http.MethodGet, http.StatusServiceUnavailable},
		{"/api/v1/tracker", http.MethodGet, http.StatusServiceUnavailable},
		{"/api/v1/adapters", http.MethodGet, http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		if strings.HasPrefix(tt.path, "/api/v1/") {
			req.Header.Set("X-API-Key", "test-api-key")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != tt.code {
			t.Errorf("%s %s: expected %d, got %d", tt.method, tt.path, tt.code, w.Code)
		}
	}
}

func TestHandleAdapterByIDNotFound(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/adapters/nonexistent", nil)
	w := httptest.NewRecorder()
	h.handleAdapterByID(w, req)

	// Returns 503 when lua VM is not available
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleAdaptersUnavailable(t *testing.T) {
	h, sup := newTestHandler(t)
	defer sup.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/adapters", nil)
	w := httptest.NewRecorder()
	h.handleAdapters(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// === 全流程集成测试：ConfigRegistry → WebUI → HTTP API ===

func TestSettingsAPIFlow(t *testing.T) {
	cfgReg := internalConfig.NewConfigRegistry("")
	cfgReg.Register("core.llm.model", "deepseek-v4-flash")
	cfgReg.Register("core.llm.base_url", "https://api.deepseek.com")
	cfgReg.Register("webui.listen_addr", ":8080")
	cfgReg.Register("plugin.qq.access_token", "secret123")

	sup := supervisor.New(&types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	})
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Settings:   sdk.NewSettings("webui", cfgReg),
		Config:     sdk.NewConfig(&types.Config{}),
	})
	h := NewHandler(s)

	t.Run("GET_settings_lists_keys_and_plugins", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
		w := httptest.NewRecorder()
		h.handleSettings(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		settings, ok := resp["settings"].(map[string]interface{})
		if !ok {
			t.Fatal("settings not a map")
		}
		if v, _ := settings["core.llm.model"].(string); v != "deepseek-v4-flash" {
			t.Fatalf("expected deepseek-v4-flash, got %v", settings["core.llm.model"])
		}

		plugins, ok := resp["plugins"].([]interface{})
		if !ok || len(plugins) == 0 {
			t.Fatal("expected plugins list")
		}
		if plugins[0] != "core" {
			t.Fatalf("expected first plugin 'core', got %v", plugins[0])
		}
	})

	t.Run("GET_settings_with_prefix", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings?prefix=webui", nil)
		w := httptest.NewRecorder()
		h.handleSettings(w, req)

		var resp map[string]interface{}
		json.NewDecoder(w.Body).Decode(&resp)
		settings := resp["settings"].(map[string]interface{})

		if _, ok := settings["webui.listen_addr"]; !ok {
			t.Fatal("expected webui.listen_addr in filtered results")
		}
		if _, ok := settings["core.llm.model"]; ok {
			t.Fatal("core.llm.model should not be in webui filtered results")
		}
	})

	t.Run("PUT_settings_updates_value", func(t *testing.T) {
		body := `{"key":"core.llm.model","value":"gpt-4"}`
		req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.handleSettings(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		val, err := cfgReg.Get("core.llm.model")
		if err != nil {
			t.Fatalf("Get error: %v", err)
		}
		if v, _ := val.(string); v != "gpt-4" {
			t.Fatalf("expected gpt-4, got %v", val)
		}
	})

	t.Run("PUT_settings_invalid_body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader("not json"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.handleSettings(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})

	t.Run("handleStatic_returns_webui_html", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		h.handleStatic(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		body, _ := io.ReadAll(w.Body)
		html := string(body)
		if !strings.Contains(html, "settings-layout") {
			t.Fatal("HTML should contain settings-layout class")
		}
		if !strings.Contains(html, "settings-tabs") {
			t.Fatal("HTML should contain settings-tabs class")
		}
		if !strings.Contains(html, "saveSetting") {
			t.Fatal("HTML should contain saveSetting JS function")
		}
		if !strings.Contains(html, `api("/settings"`) {
			t.Fatal("HTML should call api('/settings')")
		}
	})

	t.Run("settings_not_available_without_registry", func(t *testing.T) {
		s2 := testSDK(sdk.SDKConfig{
			Supervisor: supervisor.NewSDKAdapter(sup),
			Config:     sdk.NewConfig(&types.Config{}),
		})
		h2 := NewHandler(s2)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
		w := httptest.NewRecorder()
		h2.handleSettings(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", w.Code)
		}
	})
}

func TestSettingsWithPluginRegistry(t *testing.T) {
	cfgReg := internalConfig.NewConfigRegistry("")
	cfgReg.Register("core.test.key", "value")
	cfgReg.Register("plugin.testplug.apikey", "abc123")

	sup := supervisor.New(&types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	})
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Settings:   sdk.NewSettings("webui", cfgReg),
		Config:     sdk.NewConfig(&types.Config{}),
	})
	h := NewHandler(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	w := httptest.NewRecorder()
	h.handleSettings(w, req)

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)

	plugins, _ := resp["plugins"].([]interface{})
	foundCore := false
	for _, p := range plugins {
		if p == "core" {
			foundCore = true
			break
		}
	}
	if !foundCore {
		t.Fatal("expected 'core' in plugins list")
	}
}

// === 端到端测试：Handler + IOManager + Agent + HTTP ===

type echoProvider struct{ name string }

func (p *echoProvider) Name() string          { return p.name }
func (p *echoProvider) MaxContextTokens() int { return 8192 }
func (p *echoProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	content := "echo: " + lastUserContent(req.Messages)
	return &agentAPI.CompletionResponse{Content: content, FinishReason: "stop"}, nil
}
func (p *echoProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	// 流契约：chunk 发送完毕后必须 close(channel) 标识流结束（与
	// LuaAdaptedProvider.ChatStream 的 defer close(ch) 一致）；
	// accumulateStream 以 channel 关闭为终止条件，Done 只是 finish_reason 载体。
	// 内容与 Chat() 保持一致，保证端到端断言在流式/非流式两条路径下等价。
	ch := make(chan agentAPI.StreamChunk, 1)
	content := "echo: " + lastUserContent(req.Messages)
	ch <- agentAPI.StreamChunk{Content: content, Done: true, FinishReason: "stop"}
	close(ch)
	return ch, nil
}

func lastUserContent(msgs []agentAPI.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

func init() {
	// 避免测试时自动输出
}

func TestHandleCompletionsEndToEnd(t *testing.T) {
	iom := agentIO.NewIOManager()

	// 启动一个最小 Agent，使用 echoProvider（不调真实 LLM）
	memDB, err := memory.NewGraphDB(t.TempDir() + "/graph.db")
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	defer memDB.Close()

	pm := agentAPI.NewProviderManager()
	pm.Register("echo", &echoProvider{name: "echo"})

	agent := agentCore.New(agentCore.AgentConfig{
		ID:              "test",
		SystemPrompt:    "你是测试助手",
		Provider:        &echoProvider{name: "echo"},
		ProviderManager: pm,
		IO:              iom,
		Memory:          memDB,
		Indexer:         nil,
		ContextSavePath: "",
	})
	agent.Start()
	defer agent.Stop()

	sup := supervisor.New(&types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	})
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		IOManager:  iom,
		Config:     sdk.NewConfig(&types.Config{}),
	})
	h := NewHandler(s)

	t.Run("POST_chat_completions_returns_echo", func(t *testing.T) {
		body := `{"model":"test","messages":[{"role":"user","content":"你好"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		h.handleOpenAICompletions(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		choices, ok := resp["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			t.Fatal("expected choices")
		}
		msg, ok := choices[0].(map[string]interface{})["message"].(map[string]interface{})
		if !ok {
			t.Fatal("expected message")
		}
		if msg["content"] != "echo: 你好" {
			t.Fatalf("expected 'echo: 你好', got '%v'", msg["content"])
		}
	})

	t.Run("POST_chat_completions_no_iom_returns_503", func(t *testing.T) {
		s2 := testSDK(sdk.SDKConfig{
			Supervisor: supervisor.NewSDKAdapter(sup),
		})
		h2 := NewHandler(s2)
		body := `{"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h2.handleOpenAICompletions(w, req)

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", w.Code)
		}
	})

	t.Run("POST_chat_completions_400_on_no_messages", func(t *testing.T) {
		body := `{"model":"test"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.handleOpenAICompletions(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})

	t.Run("POST_chat_completions_400_on_non_user_last_msg", func(t *testing.T) {
		body := `{"messages":[{"role":"assistant","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.handleOpenAICompletions(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})
}

// ===== client_msg_id 去重测试（防 GUI 断线重连消息重放）=====

func TestHandleChatClientMsgIDDedup(t *testing.T) {
	iom := agentIO.NewIOManager()

	memDB, err := memory.NewGraphDB(t.TempDir() + "/graph.db")
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	defer memDB.Close()

	pm := agentAPI.NewProviderManager()
	pm.Register("echo", &echoProvider{name: "echo"})

	agent := agentCore.New(agentCore.AgentConfig{
		ID:              "test",
		SystemPrompt:    "你是测试助手",
		Provider:        &echoProvider{name: "echo"},
		ProviderManager: pm,
		IO:              iom,
		Memory:          memDB,
	})
	agent.Start()
	defer agent.Stop()

	sup := supervisor.New(&types.Config{
		Daemon: types.DaemonConfig{
			CheckInterval:     time.Minute,
			HeartbeatInterval: 30 * time.Second,
		},
	})
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		IOManager:  iom,
		Config:     sdk.NewConfig(&types.Config{}),
	})
	h := NewHandler(s)

	t.Run("same_client_msg_id_replay_returns_cached_response", func(t *testing.T) {
		body := `{"message":"你好","client_msg_id":"msg-abc-123"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.handleChat(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("first request: expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var first map[string]interface{}
		json.NewDecoder(w.Body).Decode(&first)
		if first["response"] != "echo: 你好" {
			t.Fatalf("expected echo response, got %v", first["response"])
		}

		// 同 ID 重放：应直接复用首次结果，不重复注入 agent
		req2 := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(body))
		w2 := httptest.NewRecorder()
		h.handleChat(w2, req2)
		if w2.Code != http.StatusOK {
			t.Fatalf("replay: expected 200, got %d: %s", w2.Code, w2.Body.String())
		}
		var second map[string]interface{}
		json.NewDecoder(w2.Body).Decode(&second)
		if second["response"] != "echo: 你好" {
			t.Fatalf("replay expected same response, got %v", second["response"])
		}
		if second["deduplicated"] != true {
			t.Fatalf("replay expected deduplicated=true, got %v", second["deduplicated"])
		}
	})

	t.Run("different_client_msg_id_processed_normally", func(t *testing.T) {
		body := `{"message":"第二条","client_msg_id":"msg-def-456"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.handleChat(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["deduplicated"] == true {
			t.Fatal("new msg id should not be deduplicated")
		}
	})

	t.Run("no_client_msg_id_backward_compatible", func(t *testing.T) {
		body := `{"message":"旧客户端消息"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.handleChat(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// ===== agent 核心层内容级去重测试 =====

func TestAgentDuplicateInputDedup(t *testing.T) {
	iom := agentIO.NewIOManager()

	memDB, err := memory.NewGraphDB(t.TempDir() + "/graph.db")
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	defer memDB.Close()

	pm := agentAPI.NewProviderManager()
	pm.Register("echo", &echoProvider{name: "echo"})

	agent := agentCore.New(agentCore.AgentConfig{
		ID:              "test",
		SystemPrompt:    "你是测试助手",
		Provider:        &echoProvider{name: "echo"},
		ProviderManager: pm,
		IO:              iom,
		Memory:          memDB,
	})
	agent.Start()
	defer agent.Stop()

	// 直接验证 isDuplicateInput 行为
	if agent.IsDuplicateInput("webui", "重复消息") {
		t.Fatal("first input should not be duplicate")
	}
	if !agent.IsDuplicateInput("webui", "重复消息") {
		t.Fatal("immediate same-content same-source should be duplicate")
	}
	if agent.IsDuplicateInput("webui", "不同消息") {
		t.Fatal("different content should not be duplicate")
	}
	if agent.IsDuplicateInput("qq", "重复消息") {
		t.Fatal("different source should not be duplicate")
	}
}

// TestSettingsNoCrossPluginLeak 锁住设置接口的两类泄漏：
//
//  1. meta 不得把别家插件的 def 复制进来（曾经 28 插件 × ~186 def = 5208 条，
//     96% 重复），也不得出现 plugin.<a>.plugin.<b>.<key> 这种幻影键——
//     按幻影键写回会落到错误插件的配置表里。
//  2. chathistory 是内部数据（生产实测 5.2MB），不得出现在设置响应里，
//     也不得经设置接口写入。
func TestSettingsNoCrossPluginLeak(t *testing.T) {
	cfgReg := internalConfig.NewConfigRegistry("")
	cfgReg.RegisterDef(internalConfig.ConfigDef{Key: "core.agent.max_tool_turns", Default: "10"})

	webuiCfg := cfgReg.PluginConfig("webui")
	webuiCfg.RegisterDef(internalConfig.ConfigDef{Key: "addr", Default: ":8080"})
	// 隔离聊天记录文件：不设的话会落到 os.TempDir()/webui_chat_history.json，
	// 与其它用例串味（迁移用例会把自己的数据写进去）。
	webuiCfg.RegisterDef(internalConfig.ConfigDef{Key: "history_file", Default: ""})
	if err := webuiCfg.Set("history_file", filepath.Join(t.TempDir(), "chat.json")); err != nil {
		t.Fatalf("set history_file: %v", err)
	}
	webuiCfg.Set("addr", ":8080")
	webuiCfg.Set("chathistory", `[{"role":"assistant","content":"secret blob"}]`)

	qqCfg := cfgReg.PluginConfig("qq")
	qqCfg.RegisterDef(internalConfig.ConfigDef{Key: "access_token", Default: ""})
	qqCfg.Set("access_token", "qq-token")

	sup := supervisor.New(&types.Config{Daemon: types.DaemonConfig{
		CheckInterval: time.Minute, HeartbeatInterval: 30 * time.Second,
	}})
	sup.Start()
	defer sup.Shutdown()

	s := testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Settings:   sdk.NewSettings("webui", cfgReg),
		Config:     sdk.NewConfig(&types.Config{}),
	})
	h := NewHandler(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	w := httptest.NewRecorder()
	h.handleSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Settings map[string]interface{}            `json:"settings"`
		Meta     map[string]map[string]interface{} `json:"meta"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// 1) meta 里每个键只能出现一次 "plugin." 前缀，且内容与键同源
	for k, v := range resp.Meta {
		if strings.Count(k, "plugin.") > 1 {
			t.Fatalf("meta 出现幻影键：%q", k)
		}
		if inner, ok := v["key"].(string); ok && strings.HasPrefix(inner, "plugin.") {
			t.Fatalf("meta[%q].key 带命名空间前缀（会造成双重前缀）：%q", k, inner)
		}
	}
	// 2) webui 不能看到 qq 的 def，反之亦然
	if _, ok := resp.Meta["plugin.webui.addr"]; !ok {
		t.Fatalf("meta 缺少 plugin.webui.addr：%v", resp.Meta)
	}
	if _, ok := resp.Meta["plugin.webui.access_token"]; ok {
		t.Fatal("meta 里出现了别家插件的 def：plugin.webui.access_token")
	}
	if _, ok := resp.Meta["plugin.qq.access_token"]; !ok {
		t.Fatal("meta 缺少 plugin.qq.access_token")
	}
	if _, ok := resp.Meta["plugin.qq.addr"]; ok {
		t.Fatal("meta 里出现了别家插件的 def：plugin.qq.addr")
	}
	// 3) 内部数据不进设置面
	if _, ok := resp.Settings["plugin.webui.chathistory"]; ok {
		t.Fatal("settings 泄露了 chathistory 内部数据")
	}
	if _, ok := resp.Meta["plugin.webui.chathistory"]; ok {
		t.Fatal("meta 泄露了 chathistory")
	}
	if _, ok := resp.Settings["plugin.qq.access_token"]; !ok {
		t.Fatal("普通插件配置项应照常返回")
	}

	// 4) 内部数据也不可经设置接口写入
	body := `{"key":"plugin.webui.chathistory","value":"tampered"}`
	preq := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	preq.Header.Set("Content-Type", "application/json")
	pw := httptest.NewRecorder()
	h.handleSettings(pw, preq)
	if pw.Code != http.StatusBadRequest {
		t.Fatalf("写内部键应被拒（400），实际 %d", pw.Code)
	}
	// 迁移已把这条记录搬去独立文件并从配置表移除；若仍在，则必须是原值（未被改写）
	if got, _ := webuiCfg.Get("chathistory"); got != nil && got != `[{"role":"assistant","content":"secret blob"}]` {
		t.Fatalf("内部数据被改写：%v", got)
	}
	// 记录本身必须没有丢：迁移后的文件里应能找到它
	msgs := h.history.Load()
	if len(msgs) != 1 || msgs[0].Content != "secret blob" {
		t.Fatalf("迁移后记录不应丢失，实际 %+v（文件 %s）", msgs, h.history.Path())
	}
}

// TestListenOverrideAndBindFailure 钉住两个曾经静默的缺陷：
//
//  1. CLI --webui / webui.listen_addr 的覆盖必须真的生效（优先级高于插件 settings["addr"]）。
//     修复前内核只在插件设置键为空时才写，而 REGISTERDEF 建表时写的是默认 :8080，
//     覆盖因此永远是死配置。
//  2. 端口被占时 Start 必须返回错误。修复前监听在后台 goroutine 里做，
//     Start 永远返回 nil，WebUI 静默死亡而插件仍被当成加载成功。
func TestListenOverrideAndBindFailure(t *testing.T) {
	// 取一个确定空闲的地址
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)
	// 插件设置里故意放一个别的地址，用来证明覆盖的优先级
	cfgReg.PluginConfig("webui").Set("addr", "127.0.0.1:1")

	s := testSDK(sdk.SDKConfig{
		Settings:  sdk.NewSettings("webui", cfgReg),
		Config:    sdk.NewConfig(&types.Config{}),
		EventBus:  events.NewBus(),
		IOManager: agentIO.NewIOManager(),
	})

	SetListenOverride(addr)
	defer SetListenOverride("")

	p1 := New("webui")
	if err := p1.Start(s); err != nil {
		t.Fatalf("Start with override: %v", err)
	}
	defer p1.Stop()

	// 覆盖值必须真的在监听
	cli := &http.Client{Timeout: 2 * time.Second}
	resp, err := cli.Get("http://" + addr + "/login")
	if err != nil {
		t.Fatalf("覆盖地址未监听（%s）：%v", addr, err)
	}
	resp.Body.Close()

	// 同一地址再来一个插件实例 → 必须同步报错
	p2 := New("webui")
	err = p2.Start(s)
	if err == nil {
		p2.Stop()
		t.Fatal("端口被占用时 Start 应返回错误，而不是静默成功")
	}
	if !strings.Contains(err.Error(), "监听") {
		t.Fatalf("错误信息应说明监听失败，实际：%v", err)
	}
}

func TestResolveListenAddrPrecedence(t *testing.T) {
	SetListenOverride("")
	defer SetListenOverride("")
	if got := resolveListenAddr(""); got != ":8080" {
		t.Fatalf("默认应为 :8080，得到 %q", got)
	}
	if got := resolveListenAddr(":9001"); got != ":9001" {
		t.Fatalf("插件设置应生效，得到 %q", got)
	}
	SetListenOverride("127.0.0.1:9002")
	if got := resolveListenAddr(":9001"); got != "127.0.0.1:9002" {
		t.Fatalf("覆盖值应优先，得到 %q", got)
	}
}

// TestResolveHistoryFile 钉住聊天记录路径的解析规则：
// 插件设置优先、相对路径按 data 目录解析、绝对路径原样、留空走默认。
func TestResolveHistoryFile(t *testing.T) {
	cases := []struct{ setting, dataDir, want string }{
		{"", "/data", "/data/webui_chat_history.json"},
		{"chat.json", "/data", "/data/chat.json"},
		{"sub/chat.json", "/data", "/data/sub/chat.json"},
		{"/mnt/ssd/chat.json", "/data", "/mnt/ssd/chat.json"},
		{"  ", "/data", "/data/webui_chat_history.json"},
	}
	for _, c := range cases {
		if got := resolveHistoryFile(c.setting, c.dataDir); got != c.want {
			t.Errorf("resolveHistoryFile(%q, %q) = %q, want %q", c.setting, c.dataDir, got, c.want)
		}
	}
	// data 目录未知时不得落到进程 CWD（测试/嵌入场景会污染工作目录）
	if got := resolveHistoryFile("", ""); filepath.Dir(got) != strings.TrimRight(os.TempDir(), "/") {
		t.Errorf("data 目录未知时应落到临时目录，实际 %q", got)
	}
}

// TestHistoryStoreMigratesFromConfig 钉住从「配置项存整段记录」到「独立文件」的迁移：
// 记录必须完好搬到文件、老配置项必须从配置表消失（它正是 config.db 膨胀与设置接口
// 大响应的来源），且第二次加载不再重复迁移。
func TestHistoryStoreMigratesFromConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "chat.json")

	cfgReg := internalConfig.NewConfigRegistry("")
	webuiCfg := cfgReg.PluginConfig("webui")
	webuiCfg.RegisterDef(internalConfig.ConfigDef{Key: "chathistory", Default: ""})
	legacy := []ChatMsg{
		{Role: "user", Content: "老记录 1", Time: "2026-01-01T00:00:00Z"},
		{Role: "assistant", Content: "老记录 2", Time: "2026-01-01T00:00:01Z"},
	}
	b, _ := json.Marshal(legacy)
	webuiCfg.Set("chathistory", string(b))

	settings := sdk.NewSettings("webui", cfgReg)
	hs := newHistoryStore(file)

	got := hs.LoadWithMigration(settings)
	if len(got) != 2 || got[0].Content != "老记录 1" || got[1].Content != "老记录 2" {
		t.Fatalf("迁移后应拿到 2 条老记录，实际 %+v", got)
	}
	// 文件已落盘
	onDisk := newHistoryStore(file).Load()
	if len(onDisk) != 2 {
		t.Fatalf("记录应写入 %s，实际 %+v", file, onDisk)
	}
	// 老配置项必须消失（否则 config.db 里那 5MB 还在）
	if v, _ := webuiCfg.Get("chathistory"); v != nil {
		t.Fatalf("迁移后老配置项应被删除，实际仍为 %v", v)
	}
	// 二次加载：直接读文件，不重复迁移
	hs2 := newHistoryStore(file)
	if again := hs2.LoadWithMigration(settings); len(again) != 2 {
		t.Fatalf("二次加载应仍为 2 条，实际 %+v", again)
	}
	// 文件损坏时按空历史处理，不得 panic
	if err := os.WriteFile(file, []byte("{不是 JSON"), 0644); err != nil {
		t.Fatal(err)
	}
	if broken := newHistoryStore(file).Load(); len(broken) != 0 {
		t.Fatalf("损坏文件应按空历史处理，实际 %+v", broken)
	}
}

// TestHistoryStoreSaveIsAtomicAndRoundTrips 钉住原子写：不留 .tmp、内容可回读。
func TestHistoryStoreSaveIsAtomicAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sub", "chat.json") // 目录不存在，Save 需自建
	hs := newHistoryStore(file)
	msgs := []ChatMsg{{Role: "user", Content: "你好", Time: "2026-01-01T00:00:00Z"}}
	if err := hs.Save(msgs); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(file + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("不应残留 %s.tmp", file)
	}
	got := hs.Load()
	if len(got) != 1 || got[0].Content != "你好" {
		t.Fatalf("回读不一致：%+v", got)
	}
}

// TestChatPersistenceIsThrottled 钉住聊天记录写盘节流：
// 连续变更不得每条都整段重写文件，但 Close 前必须把最后一次落盘（否则丢对话）。
func TestChatPersistenceIsThrottled(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "chat.json")

	cfgReg := internalConfig.NewConfigRegistry("")
	webuiCfg := cfgReg.PluginConfig("webui")
	webuiCfg.RegisterDef(internalConfig.ConfigDef{Key: "history_file", Default: ""})
	webuiCfg.Set("history_file", file)

	s := testSDK(sdk.SDKConfig{
		Settings: sdk.NewSettings("webui", cfgReg),
		Config:   sdk.NewConfig(&types.Config{}),
		EventBus: events.NewBus(),
	})
	h := NewHandler(s)
	if h.history.Path() != file {
		t.Fatalf("history 路径应为 %s，实际 %s", file, h.history.Path())
	}

	// 模拟一轮对话里的连续变更（用户消息 + 多个工具事件 + 收尾）
	base := time.Now()
	for i := 0; i < 20; i++ {
		h.chatMu.Lock()
		h.chatHistory = append(h.chatHistory, ChatMsg{
			Role: "assistant", Content: "消息", Time: base.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
		})
		h.persistChatLocked()
		h.chatMu.Unlock()
	}
	// 节流窗口内不应落盘
	if _, err := os.Stat(file); err == nil {
		t.Fatal("节流窗口（3s）内不应已经写盘")
	}

	// Close 必须把最后一次变更落下去
	h.Close()
	msgs := newHistoryStore(file).Load()
	if len(msgs) != 20 {
		t.Fatalf("Close 后应有 20 条记录，实际 %d", len(msgs))
	}
	// Close 幂等
	h.Close()
}

// TestDashboardAssetsSplit 钉住前端的拆分方式：
// 外壳 dashboard.html 只留两个占位符，样式与脚本分别在 dashboard.css / dashboard.js，
// 启动时组装回**与拆分前逐字节一致**的页面（拆分当日已用 git 里的原文件比对通过）。
func TestDashboardAssetsSplit(t *testing.T) {
	shell, err := dashboardFS.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("读 dashboard.html: %v", err)
	}
	for _, ph := range []string{"{{DASHBOARD_CSS}}", "{{DASHBOARD_JS}}"} {
		if !strings.Contains(string(shell), ph) {
			t.Fatalf("外壳里应保留占位符 %s", ph)
		}
	}
	// 样式/脚本不能又塞回外壳（否则拆分名存实亡）
	if strings.Contains(string(shell), "--sakura-300") {
		t.Fatal("dashboard.html 里仍内联着样式，拆分未生效")
	}
	if strings.Contains(string(shell), "function saveSetting") {
		t.Fatal("dashboard.html 里仍内联着脚本，拆分未生效")
	}
	css, err := dashboardFS.ReadFile("dashboard.css")
	if err != nil || len(css) == 0 {
		t.Fatalf("读 dashboard.css: %v (%d 字节)", err, len(css))
	}
	js, err := dashboardFS.ReadFile("dashboard.js")
	if err != nil || len(js) == 0 {
		t.Fatalf("读 dashboard.js: %v (%d 字节)", err, len(js))
	}
	// 组装结果：占位符必须都被替换掉，且样式/脚本内容都在里面
	if strings.Contains(dashboardHTML, "{{DASHBOARD_") {
		t.Fatal("组装后的页面仍残留占位符")
	}
	if !strings.Contains(dashboardHTML, "--sakura-300") {
		t.Fatal("组装后的页面缺样式")
	}
	if !strings.Contains(dashboardHTML, "function saveSetting") {
		t.Fatal("组装后的页面缺脚本")
	}
}

// TestStagePipelineToolCellShowsLatestOnly 钉住总览「阶段管道」里工具格的展示口径：
// 只显示**最新一条**工具/输出调用（单行视口 + 滑入动画），而不是把本轮每一次
// 都追加成 chip —— 后者在几十次工具调用的一轮里会把这一格撑成长条，
// 反而看不出「现在正在调什么」。
//
// 前端没有 JS 测试运行器，这里按仓库既有做法（见 TestDashboardAssetsSplit）
// 直接对产物做字符串钉桩：若日后有人把这条口径改回「不断追加」，用例会红。
func TestStagePipelineToolCellShowsLatestOnly(t *testing.T) {
	js, err := dashboardFS.ReadFile("dashboard.js")
	if err != nil {
		t.Fatalf("读 dashboard.js: %v", err)
	}
	css, err := dashboardFS.ReadFile("dashboard.css")
	if err != nil {
		t.Fatalf("读 dashboard.css: %v", err)
	}
	sjs, scss := string(js), string(css)
	for _, want := range []string{
		"items[items.length - 1]", // 工具格只取末条（最新）
		"rt-pipe-scroll",          // 单行滚动视口
		"rtFlashLatestTool",       // 新调用到来时重放滑入动画
		"rt-scroll-count",         // 右侧本轮累计次数
	} {
		if !strings.Contains(sjs, want) {
			t.Errorf("dashboard.js 缺 %q：工具格应只滚动展示最新一条调用", want)
		}
	}
	for _, want := range []string{".rt-pipe-scroll", "@keyframes rt-chip-scroll-in"} {
		if !strings.Contains(scss, want) {
			t.Errorf("dashboard.css 缺 %q：工具格的滚动视口/滑入动画样式", want)
		}
	}
}

// TestChatHistoryDefaultIsPaged 钉住 /chat/history 的默认页大小。
//
// 原先缺省 limit=0 表示"不限制"，于是任何不带 limit 的调用每次都拿到完整聊天记录
// （生产实测 ~5MB；本地 126 条 635KB）——这正是"每次都在发完整聊天记录"的来源。
// 现在默认只回一页；想整取必须显式 limit=0。
func TestChatHistoryDefaultIsPaged(t *testing.T) {
	h, _ := newTestHandler(t)
	// 清空可能从临时目录共享文件加载进来的历史，让用例只依赖自己播的数据
	h.chatMu.Lock()
	h.chatHistory = nil
	for i := 0; i < maxChatHistory; i++ {
		h.chatHistory = append(h.chatHistory, ChatMsg{Role: "user", Content: "消息", Time: "t"})
	}
	h.chatMu.Unlock()

	get := func(q string) map[string]interface{} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/history"+q, nil)
		w := httptest.NewRecorder()
		h.handleChatHistory(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %q: %d", q, w.Code)
		}
		var out map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return out
	}
	// 不带 limit → 只回一页，且明确告知还有更早的
	def := get("")
	if n := len(def["messages"].([]interface{})); n != defaultChatHistoryLimit {
		t.Fatalf("默认应回 %d 条，实际 %d", defaultChatHistoryLimit, n)
	}
	if def["has_more"] != true {
		t.Fatal("还有更早内容时 has_more 应为 true")
	}
	// 显式 limit=0 → 整取（逃生口）：返回条数应等于 total
	all := get("?limit=0")
	total := int(all["total"].(float64))
	if n := len(all["messages"].([]interface{})); n != total {
		t.Fatalf("limit=0 应整取 total=%d 条，实际 %d", total, n)
	}
	// 显式分页仍然照旧
	page := get("?limit=5")
	if n := len(page["messages"].([]interface{})); n != 5 {
		t.Fatalf("limit=5 应回 5 条，实际 %d", n)
	}
}

// TestChatHistoryIncrementalAfterCursor 钉住增量游标 API（前端轮询只拿增量、
// 不做整块重建的根据）。
//
// 三条不变量：
//   - after=<seq> 只回 seq 更大的消息，按 seq 升序；
//   - 响应带 last_seq，续取不重不漏；
//   - 一批超过 limit 时返回**最旧的一批**并把 last_seq 停在返回的最后一条
//     （若返回最新一批，被挤掉的旧几条就永远追不回来了）。
func TestChatHistoryIncrementalAfterCursor(t *testing.T) {
	h, _ := newTestHandler(t)
	h.chatMu.Lock()
	h.chatHistory = nil
	h.chatSeq = 0
	for i := 0; i < 5; i++ {
		h.chatHistory = append(h.chatHistory, ChatMsg{Seq: h.bumpSeqLocked(), Role: "user", Content: "m", Time: "t"})
	}
	h.chatMu.Unlock()

	get := func(q string) map[string]interface{} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/history"+q, nil)
		w := httptest.NewRecorder()
		h.handleChatHistory(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %q: %d", q, w.Code)
		}
		var out map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return out
	}

	first := get("?after=0&limit=2")
	if n := len(first["messages"].([]interface{})); n != 2 {
		t.Fatalf("after=0&limit=2 应回 2 条，实际 %d", n)
	}
	if got := first["last_seq"].(float64); got != 2 {
		t.Fatalf("截断时 last_seq 必须停在返回的最后一条（2），实际 %v", got)
	}

	second := get("?after=2&limit=2")
	if got := second["last_seq"].(float64); got != 4 {
		t.Fatalf("续取 last_seq 应为 4，实际 %v", got)
	}
	third := get("?after=4&limit=2")
	if n := len(third["messages"].([]interface{})); n != 1 {
		t.Fatalf("after=4 应只剩 1 条，实际 %d", n)
	}
	if got := third["last_seq"].(float64); got != 5 {
		t.Fatalf("追平后 last_seq 应为 5，实际 %v", got)
	}

	none := get("?after=5")
	if n := len(none["messages"].([]interface{})); n != 0 {
		t.Fatalf("无新增应回 0 条，实际 %d", n)
	}
	if got := none["last_seq"].(float64); got != 5 {
		t.Fatalf("无新增时 last_seq 应保持传入值 5，实际 %v", got)
	}

	// 非数字游标 → 400（而不是静默当成 0 全量重投）
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/history?after=abc", nil)
	w := httptest.NewRecorder()
	h.handleChatHistory(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 after 应为 400，实际 %d", w.Code)
	}
}

// fakeStatus 是给 /runtime 用的最小内核状态桩。
type fakeStatus struct{ ks *sdk.KernelStatus }

func (f fakeStatus) GetKernelStatus() *sdk.KernelStatus { return f.ks }

// TestRuntimeEndpoint 钉住运行态小接口的形状：
// 只回运行态三件事（调度器/驻留子/通道），且体积远小于 /kernel ——
// 前端靠它做秒级刷新，字段一丢图就画不出来。
func TestRuntimeEndpoint(t *testing.T) {
	ks := &sdk.KernelStatus{
		Uptime: "1m",
		Scheduler: sdk.SchedulerStatus{
			ReadyQueueDepth:   2,
			PendingInterrupts: 1,
			SuspendStack:      1,
			MaxSuspendDepth:   4,
			InterruptQueues:   [5]int{0, 0, 0, 0, 1},
			SuspendFrames:     []sdk.SchedulerFrame{{Task: sdk.SchedulerTask{ID: 9, Level: 3, Kind: "input"}}},
		},
		Residents: []sdk.ResidentStatus{{ID: "r1", State: "running", Rounds: 3}},
		Channels:  []sdk.ChannelInfo{{Name: "webui", Type: "1", Direction: "out", Ready: true, OutputCaps: 7, CapsText: "[text file image]"}},
		// inputch 的登记与归属：必须带出**驻留子划走的那条**（owner=子 id）——
		// 只回设备能力（channels）就无法回答「这条输入归谁」，驻留子的通道分配
		// 在界面上完全不可见（本次修的缺口）。
		InputChannels: []sdk.InputChannelInfo{
			{Name: "qq", Plugin: "qq", Capacity: 32, Output: "qq"},
			{Name: "timer", Plugin: "timer", Owner: "r1", Capacity: 8},
		},
	}
	s := testSDK(sdk.SDKConfig{
		Settings: sdk.NewSettings("webui", internalConfig.NewConfigRegistry("")),
		Config:   sdk.NewConfig(&types.Config{}),
		Status:   fakeStatus{ks: ks},
	})
	h := NewHandler(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runtime", nil)
	w := httptest.NewRecorder()
	h.handleRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var out struct {
		Scheduler     sdk.SchedulerStatus    `json:"scheduler"`
		Residents     []sdk.ResidentStatus   `json:"residents"`
		Channels      []sdk.ChannelInfo      `json:"channels"`
		InputChannels []sdk.InputChannelInfo `json:"input_channels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Scheduler.ReadyQueueDepth != 2 || out.Scheduler.PendingInterrupts != 1 {
		t.Fatalf("调度器字段丢失：%+v", out.Scheduler)
	}
	if out.Scheduler.InterruptQueues[4] != 1 {
		t.Fatalf("四级队列深度丢失：%v", out.Scheduler.InterruptQueues)
	}
	if len(out.Scheduler.SuspendFrames) != 1 || out.Scheduler.SuspendFrames[0].Task.ID != 9 {
		t.Fatalf("中断栈帧丢失：%+v", out.Scheduler.SuspendFrames)
	}
	if len(out.Residents) != 1 || out.Residents[0].ID != "r1" {
		t.Fatalf("驻留子丢失：%+v", out.Residents)
	}
	if len(out.Channels) != 1 || out.Channels[0].Direction != "out" {
		t.Fatalf("通道方向丢失：%+v", out.Channels)
	}
	// 通道分配：既要带归属，也要带**驻留子划走的那条**
	if len(out.InputChannels) != 2 {
		t.Fatalf("input_channels 应回 2 条，实际 %+v", out.InputChannels)
	}
	var childCh, rootCh *sdk.InputChannelInfo
	for i := range out.InputChannels {
		switch out.InputChannels[i].Owner {
		case "":
			rootCh = &out.InputChannels[i]
		case "r1":
			childCh = &out.InputChannels[i]
		}
	}
	if rootCh == nil || rootCh.Name != "qq" || rootCh.Capacity != 32 || rootCh.Output != "qq" {
		t.Fatalf("根 agent 的 inputch 归属/容量/回程丢失：%+v", rootCh)
	}
	if childCh == nil || childCh.Name != "timer" || childCh.Capacity != 8 {
		t.Fatalf("驻留子划走的 inputch 未带出（这正是本次修的缺口）：%+v", childCh)
	}
	// 只回运行态：不该把 tools/plugins 这类大块带上
	if strings.Contains(w.Body.String(), "\"tools\"") || strings.Contains(w.Body.String(), "\"plugins\"") {
		t.Fatal("/runtime 不应携带 tools/plugins（那是 /kernel 的内容）")
	}
	// 没有内核状态时给 503，而不是空对象
	s2 := testSDK(sdk.SDKConfig{Settings: sdk.NewSettings("webui", internalConfig.NewConfigRegistry(""))})
	h2 := NewHandler(s2)
	w2 := httptest.NewRecorder()
	h2.handleRuntime(w2, httptest.NewRequest(http.MethodGet, "/api/v1/runtime", nil))
	if w2.Code != http.StatusServiceUnavailable {
		t.Fatalf("无状态源应回 503，实际 %d", w2.Code)
	}
}
