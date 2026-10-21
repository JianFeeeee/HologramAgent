//go:build linux || windows

package agentcli

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	sdkpub "github.com/JianFeeeee/homeagentsdk/sdk"
)

type toolCapture struct {
	handlers map[string]sdk.ToolHandler
	defs     map[string]sdk.ToolDef
}

func newToolCapture() *toolCapture {
	return &toolCapture{
		handlers: make(map[string]sdk.ToolHandler),
		defs:     make(map[string]sdk.ToolDef),
	}
}

func (tc *toolCapture) RegisterTool(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
	tc.handlers[name] = handler
	tc.defs[name] = def
	return nil
}
func (tc *toolCapture) RegisterStage(stage sdk.Stage, handler sdk.StageHandler) {}
func (tc *toolCapture) RegisterAPI(name string) error                           { return nil }

func setupPlugin() (*Plugin, *toolCapture, error) {
	p := New("agentcli")
	tc := newToolCapture()
	sdk := sdk.New("agentcli", sdk.SDKConfig{
		RegTool:  tc.RegisterTool,
		RegStage: tc.RegisterStage,
		RegAPI:   tc.RegisterAPI,
		Settings: sdk.NewSettings("agentcli", nil),
	})
	if err := p.Start(sdk); err != nil {
		return nil, nil, err
	}
	return p, tc, nil
}

func TestKeyMapping(t *testing.T) {
	tests := []struct {
		key      string
		expected []byte
	}{
		{"enter", []byte{0x0D}},
		{"tab", []byte{0x09}},
		{"escape", []byte{0x1B}},
		{"esc", []byte{0x1B}},
		{"backspace", []byte{0x7F}},
		{"delete", []byte{0x1B, 0x5B, 0x33, 0x7E}},
		{"home", []byte{0x1B, 0x5B, 0x48}},
		{"end", []byte{0x1B, 0x5B, 0x46}},
		{"up", []byte{0x1B, 0x5B, 0x41}},
		{"down", []byte{0x1B, 0x5B, 0x42}},
		{"left", []byte{0x1B, 0x5B, 0x44}},
		{"right", []byte{0x1B, 0x5B, 0x43}},
		{"page_up", []byte{0x1B, 0x5B, 0x35, 0x7E}},
		{"page_down", []byte{0x1B, 0x5B, 0x36, 0x7E}},
		{"ctrl_a", []byte{0x01}},
		{"ctrl_z", []byte{0x1A}},
		{"alt_a", []byte{0x1B, 'a'}},
		{"alt_z", []byte{0x1B, 'z'}},
		{"f1", []byte{0x1B, 0x5B, 0x50}},
		{"f4", []byte{0x1B, 0x5B, 0x53}},
		{"f12", []byte{0x1B, 0x5B, 0x32, 0x34, 0x7E}},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			result, err := mapKey(tt.key)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytesEqual(result, tt.expected) {
				t.Fatalf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestKeyMappingInvalid(t *testing.T) {
	_, err := mapKey("unknown_key")
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListEmpty(t *testing.T) {
	p, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	handler := tc.handlers["terminal_list"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
	if resp["count"].(float64) != 0 {
		t.Fatalf("expected count 0, got %v", resp["count"])
	}
}

func TestCreateAndCloseTerminal(t *testing.T) {
	p, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	createHandler := tc.handlers["terminal_create"]
	result, err := createHandler(map[string]interface{}{
		"command": "echo hello",
		"timeout": "10s",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var createResp map[string]interface{}
	json.Unmarshal(data, &createResp)

	if v, ok := createResp["error"]; ok {
		t.Skipf("PTY not available in this environment: %v", v)
	}
	if createResp["status"] != "created" {
		t.Fatalf("expected status created, got %v", createResp["status"])
	}

	id := createResp["id"].(string)
	if id == "" {
		t.Fatal("expected non-empty terminal id")
	}

	// Give the terminal a moment to output
	time.Sleep(200 * time.Millisecond)

	// Read output
	readHandler := tc.handlers["terminal_read"]
	result, err = readHandler(map[string]interface{}{
		"id":    id,
		"clear": true,
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ = json.Marshal(result)
	var readResp map[string]interface{}
	json.Unmarshal(data, &readResp)
	if readResp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", readResp["status"])
	}

	// Close
	closeHandler := tc.handlers["terminal_close"]
	result, err = closeHandler(map[string]interface{}{
		"id": id,
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ = json.Marshal(result)
	var closeResp map[string]interface{}
	json.Unmarshal(data, &closeResp)
	if closeResp["status"] != "closed" {
		t.Fatalf("expected status closed, got %v", closeResp["status"])
	}
}

func TestCreateTerminalMissingArgs(t *testing.T) {
	p, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	handler := tc.handlers["terminal_create"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if v, ok := resp["error"]; ok {
		t.Skipf("PTY not available in this environment: %v", v)
	}
	if resp["status"] != "created" {
		t.Fatalf("expected status created, got %v", resp["status"])
	}
	id := resp["id"].(string)
	// 经注册的 handler 关闭（handler 内部会带上 sdk）；
	// 直接调 p.handleClose 需自备 sdk 参数。
	closeHandler := tc.handlers["terminal_close"]
	if _, err := closeHandler(map[string]interface{}{"id": id}); err != nil {
		t.Fatal(err)
	}
}

func TestWriteToNonexistentTerminal(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["terminal_write"]
	result, err := handler(map[string]interface{}{
		"id":    "nonexistent",
		"input": "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error for nonexistent terminal")
	}
}

func TestReadNonexistentTerminal(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["terminal_read"]
	result, err := handler(map[string]interface{}{
		"id": "nonexistent",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error for nonexistent terminal")
	}
}

func TestResizeNonexistentTerminal(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["terminal_resize"]
	result, err := handler(map[string]interface{}{
		"id":   "nonexistent",
		"rows": float64(40),
		"cols": float64(120),
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error for nonexistent terminal")
	}
}

func TestCloseNonexistentTerminal(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["terminal_close"]
	result, err := handler(map[string]interface{}{
		"id": "nonexistent",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error for nonexistent terminal")
	}
}

func TestTerminalWriteRequiresId(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["terminal_write"]
	result, err := handler(map[string]interface{}{
		"input": "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error when id is missing")
	}
}

func TestTerminalWriteRequiresContent(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["terminal_write"]
	result, err := handler(map[string]interface{}{
		"id": "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if _, ok := resp["error"]; !ok {
		t.Fatal("expected error when both input and key are missing")
	}
}

func TestToolsRegistered(t *testing.T) {
	p, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	expectedTools := []string{
		"terminal_create",
		"terminal_write",
		"terminal_read",
		"terminal_resize",
		"terminal_close",
		"terminal_list",
	}

	for _, name := range expectedTools {
		if _, ok := tc.handlers[name]; !ok {
			t.Errorf("tool %s not registered", name)
		}
	}
}

// ——— Phase 6: 通知节流测试（mock 终端 + 捕获注入） ———

type injectCapture struct {
	mu    sync.Mutex
	texts []string
}

func (c *injectCapture) InjectInterruptText(source, channel, text string) {
	c.mu.Lock()
	c.texts = append(c.texts, text)
	c.mu.Unlock()
}
func (c *injectCapture) InjectText(source, channel, text string) {
	c.mu.Lock()
	c.texts = append(c.texts, text)
	c.mu.Unlock()
}
func (c *injectCapture) InjectTextNoMemory(source, channel, text string) {
	c.mu.Lock()
	c.texts = append(c.texts, text)
	c.mu.Unlock()
}

func (c *injectCapture) SetToolBlocks(blocks []sdkpub.ContentBlock) {
	// 测试桩：忽略多模态块
}
func (c *injectCapture) InjectInputSync(source, channel, text string) string { return "" }

// 三个带媒体的注入方法同样记录文本：本测试只关心「注入了什么话」，
// 媒体块的转发在 core 的 injectedBlocks 测试里覆盖。
func (c *injectCapture) InjectInputMedia(source, channel, text string, blocks []sdkpub.ContentBlock) {
	c.mu.Lock()
	c.texts = append(c.texts, text)
	c.mu.Unlock()
}

func (c *injectCapture) InjectInputMediaSync(source, channel, text string, blocks []sdkpub.ContentBlock) string {
	return ""
}

func (c *injectCapture) InjectInterruptMedia(source, channel, text string, blocks []sdkpub.ContentBlock) {
	c.mu.Lock()
	c.texts = append(c.texts, text)
	c.mu.Unlock()
}

// ---- 带 InjectOptions 的注入（1.2.0）：同样只记文本 ----

func (c *injectCapture) InjectTextOpts(source, channel, text string, opts sdkpub.InjectOptions) {
	c.InjectText(source, channel, text)
}
func (c *injectCapture) InjectInterruptTextOpts(source, channel, text string, opts sdkpub.InjectOptions) {
	c.InjectInterruptText(source, channel, text)
}
func (c *injectCapture) InjectInputSyncOpts(source, channel, text string, opts sdkpub.InjectOptions) string {
	return ""
}
func (c *injectCapture) InjectInputMediaOpts(source, channel, text string, blocks []sdkpub.ContentBlock, opts sdkpub.InjectOptions) {
	c.InjectInputMedia(source, channel, text, blocks)
}
func (c *injectCapture) InjectInputMediaSyncOpts(source, channel, text string, blocks []sdkpub.ContentBlock, opts sdkpub.InjectOptions) string {
	return ""
}
func (c *injectCapture) InjectInterruptMediaOpts(source, channel, text string, blocks []sdkpub.ContentBlock, opts sdkpub.InjectOptions) {
	c.InjectInterruptMedia(source, channel, text, blocks)
}

func (c *injectCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.texts))
	copy(out, c.texts)
	return out
}

// mockTerm 可控输出流的假终端：Read 从 data chan 取数据，可模拟进程退出/读取错误
type mockTerm struct {
	mu      sync.Mutex
	data    chan []byte
	running bool
	err     error
}

func newMockTerm() *mockTerm {
	return &mockTerm{data: make(chan []byte, 16), running: true}
}

func (m *mockTerm) Read(buf []byte) (int, error) {
	for {
		m.mu.Lock()
		err := m.err
		running := m.running
		m.mu.Unlock()
		if err != nil {
			return 0, err
		}
		if !running {
			return 0, fmt.Errorf("process exited")
		}
		select {
		case data, ok := <-m.data:
			if !ok {
				return 0, fmt.Errorf("closed")
			}
			n := copy(buf, data)
			return n, nil
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (m *mockTerm) WriteString(s string) (int, error) { return len(s), nil }
func (m *mockTerm) Resize(rows, cols uint16) error    { return nil }
func (m *mockTerm) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}
func (m *mockTerm) Kill() error  { return nil }
func (m *mockTerm) Close() error { return nil }

func (m *mockTerm) push(data []byte) {
	m.data <- data
}

func (m *mockTerm) setRunning(v bool) {
	m.mu.Lock()
	m.running = v
	m.mu.Unlock()
}

func (m *mockTerm) setErr(err error) {
	m.mu.Lock()
	m.err = err
	m.mu.Unlock()
}

func newTestSession(term ptyTerm) *TerminalSession {
	return &TerminalSession{
		id:        "t1",
		session:   term,
		createdAt: time.Now(),
		timeout:   10 * time.Minute,
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
	}
}

func startReadLoop(p *Plugin, s *sdk.PluginSDK, t *TerminalSession) {
	p.wg.Add(1)
	go p.readLoop(t, s)
}

func waitInjected(c *injectCapture, substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, text := range c.snapshot() {
			if strings.Contains(text, substr) {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// Phase 6: 持续吐进度时，通知频率显著低于 500ms/条（节流生效）
func TestReadLoopNotifyThrottle(t *testing.T) {
	p := New("agentcli")
	p.notifyBytes = 2048
	p.notifyInterval = 2 * time.Second

	capture := &injectCapture{}
	sdkInst := sdk.New("agentcli", sdk.SDKConfig{
		RegTool:  newToolCapture().RegisterTool,
		RegStage: func(sdk.Stage, sdk.StageHandler) {},
		RegAPI:   func(string) error { return nil },
		Settings: sdk.NewSettings("agentcli", nil),
	})
	sdkInst.SetIOInjector(capture)

	term := newMockTerm()
	ts := newTestSession(term)
	startReadLoop(p, sdkInst, ts)

	if !waitInjected(capture, "已启动", 2*time.Second) {
		t.Fatal("expected startup notification")
	}

	// 持续以 100B/50ms(=2KB/s) 吐进度 3 秒
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		chunk := make([]byte, 100)
		for i := range chunk {
			chunk[i] = 'x'
		}
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				term.push(chunk)
			}
		}
	}()

	time.Sleep(3 * time.Second)
	close(stop)

	notifies := 0
	for _, text := range capture.snapshot() {
		if strings.Contains(text, "有新输出") {
			notifies++
		}
	}
	// 3 秒持续输出，500ms/条 的旧行为应有 6 条；节流后 ≤3 条
	if notifies > 3 {
		t.Errorf("notify throttle ineffective: %d notifies in 3s (expected <=3)", notifies)
	}
	if notifies == 0 {
		t.Error("expected at least one output notification")
	}

	close(ts.stopCh)
	<-ts.done
}

// Phase 6: 进程退出 → 立即通知（两条路径：PTY Read 返回 EOF 走"读取结束"，
// 或 reader 阻塞时顶部 terminalRunning 检测走"进程已退出"）
func TestReadLoopNotifyOnExit(t *testing.T) {
	p := New("agentcli")
	p.notifyBytes = 2048
	p.notifyInterval = 2 * time.Second

	capture := &injectCapture{}
	sdkInst := sdk.New("agentcli", sdk.SDKConfig{
		RegTool:  newToolCapture().RegisterTool,
		RegStage: func(sdk.Stage, sdk.StageHandler) {},
		RegAPI:   func(string) error { return nil },
		Settings: sdk.NewSettings("agentcli", nil),
	})
	sdkInst.SetIOInjector(capture)

	term := newMockTerm()
	ts := newTestSession(term)
	startReadLoop(p, sdkInst, ts)

	if !waitInjected(capture, "已启动", 2*time.Second) {
		t.Fatal("expected startup notification")
	}

	term.setRunning(false)
	gotExit := waitInjected(capture, "进程已退出", 2*time.Second)
	gotReadEnd := waitInjected(capture, "读取结束", time.Second)
	if !gotExit && !gotReadEnd {
		t.Error("expected immediate notification on process exit (either 进程已退出 or 读取结束)")
	}
	close(ts.stopCh)
}

// Phase 6: 读取错误/EOF → 立即通知
func TestReadLoopNotifyOnReadError(t *testing.T) {
	p := New("agentcli")
	p.notifyBytes = 2048
	p.notifyInterval = 2 * time.Second

	capture := &injectCapture{}
	sdkInst := sdk.New("agentcli", sdk.SDKConfig{
		RegTool:  newToolCapture().RegisterTool,
		RegStage: func(sdk.Stage, sdk.StageHandler) {},
		RegAPI:   func(string) error { return nil },
		Settings: sdk.NewSettings("agentcli", nil),
	})
	sdkInst.SetIOInjector(capture)

	term := newMockTerm()
	ts := newTestSession(term)
	startReadLoop(p, sdkInst, ts)

	if !waitInjected(capture, "已启动", 2*time.Second) {
		t.Fatal("expected startup notification")
	}

	term.setErr(fmt.Errorf("read timeout"))
	if !waitInjected(capture, "读取结束", 3*time.Second) {
		t.Error("expected immediate notification on read error")
	}
	close(ts.stopCh)
	<-ts.done
}
