package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ===== 模拟数据 =====

type Status struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	StartedAt string `json:"startedAt"`
	Uptime    int64  `json:"uptime"`
}

type Kernel struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Status   string `json:"status"`
}

type Setting struct {
	Settings        map[string]interface{} `json:"settings"`
	Meta            map[string]interface{} `json:"meta"`
	Plugins         []string               `json:"plugins"`
	PluginMeta      map[string]interface{} `json:"plugin_meta"`
	DisabledPlugins []string               `json:"disabled_plugins"`
}

type Plugin struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Enabled     bool   `json:"enabled"`
	Builtin     bool   `json:"builtin"`
}

type PluginInfo struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Version     string       `json:"version"`
	Enabled     bool         `json:"enabled"`
	Builtin     bool         `json:"builtin"`
	Tools       []PluginTool `json:"tools"`
}

type PluginTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type Adapter struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type MemoryItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Time    string `json:"time"`
}

type Device struct {
	DeviceID   string   `json:"device_id"`
	Name       string   `json:"name"`
	Authorized bool     `json:"authorized"`
	Online     bool     `json:"online"`
	Caps       []string `json:"caps"`
}

// ===== SSE 管理器 =====

type SSEManager struct {
	mu      sync.RWMutex
	clients map[chan string]bool
}

func NewSSEManager() *SSEManager {
	return &SSEManager{clients: make(map[chan string]bool)}
}

func (m *SSEManager) Add(ch chan string) {
	m.mu.Lock()
	m.clients[ch] = true
	m.mu.Unlock()
}

func (m *SSEManager) Remove(ch chan string) {
	m.mu.Lock()
	delete(m.clients, ch)
	m.mu.Unlock()
}

func (m *SSEManager) Broadcast(eventType, data string) {
	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for ch := range m.clients {
		select {
		case ch <- msg:
		default:
		}
	}
}

// ===== HTTP 处理器 =====

type MockServer struct {
	startedAt time.Time
	sse       *SSEManager
	mu        sync.Mutex
	plugins   []Plugin
	adapters  []Adapter
	devices   []Device
	settings  map[string]interface{}
}

func NewMockServer() *MockServer {
	now := time.Now()
	return &MockServer{
		startedAt: now,
		sse:       NewSSEManager(),
		plugins: []Plugin{
			{Name: "core", Description: "核心插件", Version: "1.0.0", Enabled: true, Builtin: true},
			{Name: "remotedevice", Description: "远程设备管理", Version: "0.9.0", Enabled: true, Builtin: true},
			{Name: "webui", Description: "Web 用户界面", Version: "0.9.0", Enabled: true, Builtin: true},
			{Name: "knowledge", Description: "知识库管理", Version: "0.5.0", Enabled: true, Builtin: false},
		},
		adapters: []Adapter{
			{Name: "openai", Type: "llm", Enabled: true},
			{Name: "siliconflow", Type: "llm", Enabled: true},
		},
		devices: []Device{
			{DeviceID: "gui-test-local", Name: "GUI 测试设备", Authorized: true, Online: true, Caps: []string{"status", "cmdrun", "deviceinfo"}},
		},
		settings: map[string]interface{}{
			"language": "zh-CN",
			"theme":    "dark",
		},
	}
}

// 中间件：CORS + API Key 校验
func (s *MockServer) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization, Cookie")

		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}

		// API Key 校验（可选）
		// apiKey := r.Header.Get("X-API-Key")
		// if apiKey == "" {
		// 	http.Error(w, "unauthorized", 401)
		// 	return
		// }

		next.ServeHTTP(w, r)
	})
}

func (s *MockServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, Status{
		Status:    "running",
		Version:   "0.9.0",
		StartedAt: s.startedAt.Format(time.RFC3339),
		Uptime:    int64(time.Since(s.startedAt).Seconds()),
	})
}

func (s *MockServer) handleKernel(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, Kernel{
		Model:    "sensenova-6.8-flash-lite",
		Provider: "siliconflow",
		Status:   "ready",
	})
}

func (s *MockServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var updates map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&updates); err == nil {
			s.mu.Lock()
			for k, v := range updates {
				s.settings[k] = v
			}
			s.mu.Unlock()
		}
		writeJSON(w, map[string]string{"status": "saved"})
		return
	}

	writeJSON(w, Setting{
		Settings: s.settings,
		Meta: map[string]interface{}{
			"version": "0.9.0",
			"build":   "mock-20260823",
		},
		Plugins: []string{"core", "remotedevice", "webui", "knowledge"},
		PluginMeta: map[string]interface{}{
			"core":         map[string]interface{}{"version": "1.0.0"},
			"remotedevice": map[string]interface{}{"version": "0.9.0"},
			"webui":        map[string]interface{}{"version": "0.9.0"},
			"knowledge":    map[string]interface{}{"version": "0.5.0"},
		},
		DisabledPlugins: []string{},
	})
}

func (s *MockServer) handlePlugins(w http.ResponseWriter, r *http.Request) {
	// 获取路径中的插件名
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/plugins")
	path = strings.TrimSuffix(path, "/")

	if path == "/reload" && r.Method == "POST" {
		writeJSON(w, map[string]string{"status": "reloaded"})
		return
	}

	if path == "" && r.Method == "GET" {
		writeJSON(w, s.plugins)
		return
	}

	if path == "" && r.Method == "POST" {
		writeJSON(w, map[string]string{"status": "installed"})
		return
	}

	// /api/v1/plugins/:name
	if strings.Contains(path, "/") {
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		if len(parts) >= 1 {
			name := parts[0]
			if len(parts) >= 2 {
				action := parts[1]
				if action == "disable" && r.Method == "POST" {
					s.mu.Lock()
					for i := range s.plugins {
						if s.plugins[i].Name == name {
							s.plugins[i].Enabled = false
						}
					}
					s.mu.Unlock()
					writeJSON(w, map[string]string{"status": "disabled"})
					return
				}
				if action == "enable" && r.Method == "POST" {
					s.mu.Lock()
					for i := range s.plugins {
						if s.plugins[i].Name == name {
							s.plugins[i].Enabled = true
						}
					}
					s.mu.Unlock()
					writeJSON(w, map[string]string{"status": "enabled"})
					return
				}
			}

			// GET /api/v1/plugins/:name
			writeJSON(w, PluginInfo{
				Name:        name,
				Description: name + " 插件描述",
				Version:     "0.9.0",
				Enabled:     true,
				Builtin:     true,
				Tools: []PluginTool{
					{Name: name + "_tool1", Description: name + " 工具1"},
					{Name: name + "_tool2", Description: name + " 工具2"},
				},
			})
			return
		}
	}

	http.NotFound(w, r)
}

func (s *MockServer) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, []ChatMessage{
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "你好！我是 HologramAgent，有什么可以帮你的？"},
		{Role: "user", Content: "测试消息"},
		{Role: "assistant", Content: "这是模拟后端的测试回复，GUI 连接正常 ✅"},
	})
}

func (s *MockServer) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		// 模拟后端接收消息，通过 SSE 推流
		go func() {
			time.Sleep(500 * time.Millisecond)

			// agent_start
			s.sse.Broadcast("agent_output", `{"type":"agent_start","payload":{"agent":"mock"}}`)

			time.Sleep(300 * time.Millisecond)

			// tool_call
			s.sse.Broadcast("agent_output", `{"type":"tool_call","payload":{"tool":"mock_tool","args":{},"id":"call_001"}}`)

			time.Sleep(500 * time.Millisecond)

			// channel_output
			s.sse.Broadcast("agent_output", `{"type":"channel_output","payload":{"kind":"channel_output","channel":"mock","content":"这是一条来自模拟后端的测试回复。\n\n- 模拟后端状态: running\n- 版本: 0.9.0\n- 连接测试: ✅ 成功\n\nGUI 所有功能验证正常！"}}`)

			time.Sleep(300 * time.Millisecond)

			// agent_end
			s.sse.Broadcast("agent_output", `{"type":"agent_end","payload":{"agent":"mock"}}`)
		}()

		writeJSON(w, map[string]string{"status": "queued", "id": "mock_" + time.Now().Format("150405")})
		return
	}
	http.Error(w, "method not allowed", 405)
}

func (s *MockServer) handleChatEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 100)
	s.sse.Add(ch)
	defer s.sse.Remove(ch)

	// 发送初始连接成功事件
	fmt.Fprintf(w, "event: connected\ndata: {\"status\":\"connected\"}\n\n")
	w.(http.Flusher).Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			fmt.Fprint(w, msg)
			w.(http.Flusher).Flush()
		}
	}
}

func (s *MockServer) handleMemoryGraph(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"nodes": []map[string]interface{}{
			{"id": "1", "label": "HologramAgent", "group": "system"},
			{"id": "2", "label": "GUI 测试", "group": "user"},
		},
		"edges": []map[string]interface{}{
			{"from": "1", "to": "2", "label": "connected"},
		},
	})
}

func (s *MockServer) handleMemory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, []MemoryItem{
		{ID: "m1", Content: "这是模拟内存中的测试数据", Time: time.Now().Format(time.RFC3339)},
	})
}

func (s *MockServer) handleMemoryContext(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"context": "模拟上下文：用户正在测试 GUI 功能",
		"items":   []MemoryItem{},
	})
}

func (s *MockServer) handleKnowledge(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		writeJSON(w, map[string]string{"status": "saved"})
		return
	}
	writeJSON(w, []map[string]interface{}{
		{"id": "k1", "title": "模拟知识条目1", "content": "这是模拟知识库的测试内容"},
		{"id": "k2", "title": "模拟知识条目2", "content": "GUI 功能验证测试数据"},
	})
}

func (s *MockServer) handleTerminals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, []map[string]interface{}{
		{"id": "t1", "name": "终端 1", "status": "running"},
		{"id": "t2", "name": "终端 2", "status": "idle"},
	})
}

func (s *MockServer) handleCmdHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, []map[string]interface{}{
		{"cmd": "echo hello", "time": time.Now().Add(-5 * time.Minute).Format(time.RFC3339)},
		{"cmd": "ls -la", "time": time.Now().Add(-10 * time.Minute).Format(time.RFC3339)},
	})
}

func (s *MockServer) handleDevices(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/device")
	path = strings.TrimSuffix(path, "/")

	switch {
	case path == "/online" || path == "":
		writeJSON(w, s.devices)
	case path == "/auth" && r.Method == "POST":
		var req struct {
			DeviceID   string `json:"device_id"`
			Authorized bool   `json:"authorize"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		for i := range s.devices {
			if s.devices[i].DeviceID == req.DeviceID {
				s.devices[i].Authorized = req.Authorized
			}
		}
		s.mu.Unlock()
		writeJSON(w, map[string]interface{}{
			"authorized": true,
			"device_id":  req.DeviceID,
		})
	case path == "/push" && r.Method == "POST":
		writeJSON(w, map[string]string{"status": "pushed"})
	default:
		http.NotFound(w, r)
	}
}

func (s *MockServer) handleAdapters(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/adapters")
	path = strings.TrimSuffix(path, "/")

	switch {
	case path == "" && r.Method == "GET":
		writeJSON(w, s.adapters)
	case path == "" && r.Method == "POST":
		var a Adapter
		if err := json.NewDecoder(r.Body).Decode(&a); err == nil {
			s.mu.Lock()
			s.adapters = append(s.adapters, a)
			s.mu.Unlock()
		}
		writeJSON(w, map[string]string{"status": "added"})
	case strings.Count(path, "/") == 1 && r.Method == "DELETE":
		name := strings.TrimPrefix(path, "/")
		s.mu.Lock()
		for i := range s.adapters {
			if s.adapters[i].Name == name {
				s.adapters = append(s.adapters[:i], s.adapters[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		writeJSON(w, map[string]string{"status": "deleted"})
	default:
		http.NotFound(w, r)
	}
}

func (s *MockServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// 简单返回 400，GUI 的 main.js 会尝试连接设备桥 WS
	// 这里只验证 HTTP 路由可达
	http.Error(w, "WebSocket upgrade required (mock server)", 400)
}

// ===== 路由注册 =====

func (s *MockServer) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/status", s.handleStatus)
	mux.HandleFunc("/api/v1/kernel", s.handleKernel)
	mux.HandleFunc("/api/v1/settings", s.handleSettings)
	mux.HandleFunc("/api/v1/plugins", s.handlePlugins)
	mux.HandleFunc("/api/v1/plugins/", s.handlePlugins)
	mux.HandleFunc("/api/v1/chat/history", s.handleChatHistory)
	mux.HandleFunc("/api/v1/chat", s.handleChat)
	mux.HandleFunc("/api/v1/chat/events", s.handleChatEvents)
	mux.HandleFunc("/api/v1/memory/graph", s.handleMemoryGraph)
	mux.HandleFunc("/api/v1/memory", s.handleMemory)
	mux.HandleFunc("/api/v1/memory/context", s.handleMemoryContext)
	mux.HandleFunc("/api/v1/knowledge", s.handleKnowledge)
	mux.HandleFunc("/api/v1/terminals", s.handleTerminals)
	mux.HandleFunc("/api/v1/cmd/history", s.handleCmdHistory)
	mux.HandleFunc("/api/v1/device", s.handleDevices)
	mux.HandleFunc("/api/v1/device/", s.handleDevices)
	mux.HandleFunc("/api/v1/adapters", s.handleAdapters)
	mux.HandleFunc("/api/v1/adapters/", s.handleAdapters)
	mux.HandleFunc("/api/v1/device/ws", s.handleDeviceWS)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func main() {
	server := NewMockServer()
	mux := http.NewServeMux()
	server.registerRoutes(mux)

	addr := ":9099"
	log.Printf("=== HologramAgent Mock Server ====")
	log.Printf("listening on: http://0.0.0.0%s", addr)
	log.Printf("API base path: http://0.0.0.0%s/api/v1/", addr)
	log.Printf("SSE endpoint: http://0.0.0.0%s/api/v1/chat/events", addr)
	log.Printf("device bridge WS: ws://0.0.0.0%s/api/v1/device/ws", addr)
	log.Printf("==============================")
	log.Fatal(http.ListenAndServe(addr, server.middleware(mux)))
}
