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
	sdkpub "github.com/JianFeeeee/hologramagentsdk/sdk"
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
	// reads 记录 Read 被调用次数，供 waitReadLoopReady 判断 readLoop 已起来。
	// 不用固定 sleep：机器慢时 sleep 不够、快时白等。
	reads int
}

// readCalls 返回 Read 被调用次数（并发安全）。
func (m *mockTerm) readCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reads
}

func newMockTerm() *mockTerm {
	return &mockTerm{data: make(chan []byte, 16), running: true}
}

func (m *mockTerm) Read(buf []byte) (int, error) {
	m.mu.Lock()
	m.reads++
	m.mu.Unlock()
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

// Kill 模拟真实 PTY 的行为：把一份 **buffer 里的输出 + EIO** 交给 Read。
//
// 两点必须还原，否则测不出线上的回声：
//
//  1. Read 返回错误（真实 PTY 被 Kill 后读端立刻 `read /dev/ptmx:
//     input/output error`）。旧版这里是空操作，于是"主动关闭"在测试里
//     从不产生回声，判据永远绿（假绿）。
//  2. **已产出的数据先于错误到达**。真实内核里 PTY 缓冲中残留的输出
//     会先被读走，EIO 随后才到——这一点决定了 readLoop 的 select
//     在"stopCh 已关"与"readCh 有数据/错误"之间如何竞争。
func (m *mockTerm) Kill() error {
	m.mu.Lock()
	m.running = false
	if m.err == nil {
		m.err = fmt.Errorf("read /dev/ptmx: input/output error")
	}
	m.mu.Unlock()
	return nil
}
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

// waitReadLoopReady 等 readLoop 真正进入工作状态。
//
// ★ 为什么需要它：原测试用「已启动」注入当同步点（等那条通知出现再继续）。
// 但 2026-10-25 起该注入已移除（它是"同一条信息告知两次"，每条都会唤醒
// 一整轮推理）。同步点因此需要换成不依赖注入的方式。
//
// 这里用「mockTerm 的 Read 至少被调用过一次」作为信号：readLoop 的第一件事
// 就是 Read，比固定 sleep 可靠，也不引入对业务通知的依赖。
func waitReadLoopReady(t *testing.T, term *mockTerm) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if term.readCalls() > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("readLoop 未在 2s 内进入读取状态")
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

	// 等 readLoop 真正起来（不再依赖"已启动"注入：那已被移除，
	// 理由见 plugin.go 中"不注入终端已启动通知"的说明）
	waitReadLoopReady(t, term)

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

	// 等 readLoop 真正起来（不再依赖"已启动"注入：那已被移除，
	// 理由见 plugin.go 中"不注入终端已启动通知"的说明）
	waitReadLoopReady(t, term)

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

	// 等 readLoop 真正起来（不再依赖"已启动"注入：那已被移除，
	// 理由见 plugin.go 中"不注入终端已启动通知"的说明）
	waitReadLoopReady(t, term)

	term.setErr(fmt.Errorf("read timeout"))
	if !waitInjected(capture, "读取结束", 3*time.Second) {
		t.Error("expected immediate notification on read error")
	}
	close(ts.stopCh)
	<-ts.done
}

// 主动关闭终端后**不得**再注入任何收尾状态。
//
// 这是用户报的线上问题（2026-10-25）：
//
//	关闭终端 → agent 收到「[终端 term_6 读取结束: read /dev/ptmx: input/output error]」
//	→ 又收到「[终端 term_6 中的进程已退出]」
//	→ 而 terminal_list 此时已是 0 个会话
//
// 后果不只是刷屏：每条注入都会唤醒一轮推理，agent 只能一遍遍解释
// "这是旧终端的回声，不用管它"。而且它**永远无法自证清白**——
// 会话列表已经是空的，它拿不到任何证据说明这条消息是历史遗留。
//
// 根因：主动 Close() 会 ①关 stopCh ②Kill 进程，而 ③PTY 读端随即返回
// 「input/output error」、④terminalRunning 变 false。旧实现在这两处
// **无条件注入**，把"我们自己关掉的"当成"进程自己结束了"上报。
//
// 判据：Close 之后只允许**关闭前**已发生的注入存在，不得新增。
// 变异：删掉 isClosing 判断 ⇒ 本判据变红。
func TestClose_NoEchoNotificationsAfterClose(t *testing.T) {
	p := New("agentcli")
	p.notifyBytes = 1 << 20 // 抬高阈值，避免缓冲通知混进来干扰判据
	capture := &injectCapture{}
	sdkInst := sdk.New("agentcli", sdk.SDKConfig{
		RegTool:  newToolCapture().RegisterTool,
		RegStage: func(sdk.Stage, sdk.StageHandler) {},
		RegAPI:   func(string) error { return nil },
		Settings: sdk.NewSettings("agentcli", nil),
	})
	sdkInst.SetIOInjector(capture)

	mterm := newMockTerm()
	ts := newTestSession(mterm)
	startReadLoop(p, sdkInst, ts)

	// 等 readLoop 真正起来（不再依赖"已启动"注入：那已被移除，
	// 理由见 plugin.go 中"不注入终端已启动通知"的说明）
	waitReadLoopReady(t, mterm)
	before := len(capture.snapshot())

	// 主动关闭：模拟 close 工具路径（关 stopCh + 杀进程 + 释放 PTY）。
	//
	// 注意：Close() 内部会 <-t.done 等 readLoop 退出，所以它返回时
	// readLoop 已经走完——那正是回声注入发生的时机，判据必须等到它之后。
	ts.Close()

	// 给 readLoop 充分时间走完所有收尾分支（含 r.err 与 terminalRunning）。
	// 用固定等待而不是"等到某条消息"——我们要断言的是**什么都没有**。
	time.Sleep(600 * time.Millisecond)

	after := capture.snapshot()
	if len(after) != before {
		t.Fatalf("主动关闭后不应新增注入，实际多了 %d 条：\n%s",
			len(after)-before, strings.Join(after[before:], "\n"))
	}
	t.Logf("关闭前后注入数：%d（无回声）", len(after))
	// 明确点出两种最典型的回声，便于失败时一眼看出原因。
	for _, s := range after[before:] {
		if strings.Contains(s, "读取结束") || strings.Contains(s, "进程已退出") ||
			strings.Contains(s, "命令已执行结束") {
			t.Fatalf("关闭后出现了收尾回声：%s", s)
		}
	}
}

// 反向判据：进程**自己**结束时必须有通知。
//
// 这条守住修复不过度——不能因为要消除回声，把正常的上报也一起关掉。
// 变异：把 isClosing 判断写成无条件 return ⇒ 本判据变红。
func TestProcessExit_StillNotifies(t *testing.T) {
	p := New("agentcli")
	p.notifyBytes = 1 << 20
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

	// 等 readLoop 真正起来（不再依赖"已启动"注入：那已被移除，
	// 理由见 plugin.go 中"不注入终端已启动通知"的说明）
	waitReadLoopReady(t, term)

	// 进程自己退出（**不是** Close）：mockTerm 返回 process exited。
	term.mu.Lock()
	term.running = false
	term.mu.Unlock()

	got := waitInjected(capture, "进程已退出", 3*time.Second) ||
		waitInjected(capture, "命令已执行结束", 3*time.Second) ||
		waitInjected(capture, "读取结束", 3*time.Second)
	if !got {
		t.Fatal("进程自己结束必须有通知（修复不能把正常上报一起关掉）")
	}
}

// 竞态版回声判据：Close() 会让 stopCh 与 Read 错误**同时**就绪，
// 而 readLoop 的 select 在两者都就绪时**随机**选一个分支。
//
// 所以「关闭后不注入回声」不能只在"select 恰好选中 stopCh"时成立——
// 必须在**任何**顺序下都成立。上面那条判据只跑一次，抓不到这个随机性
// （实测：把 isClosing 改成恒 false 仍然绿）。
//
// 这里连开关闭 20 次，只要有一次出现回声就算失败。
func TestClose_NoEcho_Repeated(t *testing.T) {
	for i := 0; i < 20; i++ {
		p := New("agentcli")
		p.notifyBytes = 1 << 20
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
		waitReadLoopReady(t, term)
		before := len(capture.snapshot())

		// ★ 关键：先在 PTY 缓冲里**留下未读数据**，再关闭。
		//
		// 线上关闭终端时 PTY 缓冲里通常还有未读输出（提示符、命令尾部）。
		// 这时 readLoop 的 select 在 `<-t.stopCh` 与 `<-readCh` 之间是
		// **随机**选择——选到 readCh 就会先处理数据，随后紧跟着拿到
		// EIO（因为 Kill 后读端立刻报错），于是走进 r.err 分支。
		// 若这里不预置数据，select 几乎总是选中 stopCh，回声永远复现不出来
		// （这就是我第一版判据显示"全绿"的原因）。
		term.push([]byte("leftover output" + "\r\n"))
		time.Sleep(30 * time.Millisecond)

		ts.Close()
		time.Sleep(120 * time.Millisecond)

		after := capture.snapshot()
		for _, msg := range after[before:] {
			if strings.Contains(msg, "读取结束") || strings.Contains(msg, "进程已退出") ||
				strings.Contains(msg, "命令已执行结束") {
				t.Fatalf("第 %d 次关闭后出现回声（select 随机选中了 r.err 分支）: %s", i, msg)
			}
		}
	}
}

// 关闭终端后，session 必须**立刻**从表里消失。
//
// 这是"关闭终端后反复注入过期状态"的**直接**成因（2026-10-25 线上）：
//
//	close 工具 → session.Close() → readLoop 从 <-t.stopCh 静默 return
//	           → 但**没删 sessions 条目**
//	→ cleanupLoop 每 30s 扫到它，发现 !terminalRunning
//	→ 又注入一条「[终端 term_6 中的进程已退出]」
//	→ 而 terminal_list 已经是 0 个会话
//
// 所以用户看到的不是"一次回声"，而是**每 30 秒一次、永不停止**的
// 过期状态注入——每条都会唤醒一轮推理，agent 只能反复解释"这是旧回声"。
//
// 判据：Close() 返回后 sessions 里不得再有它。
// 变异：把 removeSession 调用删掉 ⇒ 本判据变红。
func TestClose_RemovesSessionFromTable(t *testing.T) {
	p := New("agentcli")
	p.notifyBytes = 1 << 20
	capture := &injectCapture{}
	sdkInst := sdk.New("agentcli", sdk.SDKConfig{
		RegTool:  newToolCapture().RegisterTool,
		RegStage: func(sdk.Stage, sdk.StageHandler) {},
		RegAPI:   func(string) error { return nil },
		Settings: sdk.NewSettings("agentcli", nil),
	})
	sdkInst.SetIOInjector(capture)

	mterm2 := newMockTerm()
	ts := newTestSession(mterm2)
	ts.id = "term_leak"
	p.mu.Lock()
	p.sessions = map[string]*TerminalSession{"term_leak": ts}
	p.mu.Unlock()

	startReadLoop(p, sdkInst, ts)
	// 等 readLoop 真正起来（不再依赖"已启动"注入：那已被移除，
	// 理由见 plugin.go 中"不注入终端已启动通知"的说明）
	waitReadLoopReady(t, mterm2)

	ts.Close() // 内部会 <-t.done，等 readLoop 走完

	p.mu.Lock()
	_, still := p.sessions["term_leak"]
	remaining := len(p.sessions)
	p.mu.Unlock()

	if still {
		t.Fatalf("关闭后 session 仍留在表里（%d 个）——cleanupLoop 会每 30s "+
			"把它当成「进程已退出」再注入一次，形成持续回声", remaining)
	}
}

// cleanupLoop 对**已关闭**的终端不得再注入通知。
//
// 这是上一条的补强：即使将来某条路径又漏删了表，清理路径也必须靠
// isClosing 兜住，不能把"我们自己关掉的"当成"进程自己结束了"上报。
func TestCleanup_DoesNotNotifyForClosedTerminal(t *testing.T) {
	p := New("agentcli")
	p.notifyBytes = 1 << 20
	capture := &injectCapture{}

	term := newMockTerm()
	ts := newTestSession(term)
	ts.id = "term_closed"
	ts.closed = true // 已被 close 工具关闭（但故意留在表里）

	if !p.isClosing(ts) {
		t.Fatal("isClosing 应识别 closed 标记")
	}

	// 模拟「漏删表」的坏状态：关闭了但还在 sessions 里。
	p.sessions = map[string]*TerminalSession{"term_closed": ts}
	term.setRunning(false)

	// 走一遍 cleanupLoop 的判定逻辑（与生产同一段代码路径的判据部分）。
	p.mu.Lock()
	var wouldNotify []string
	for id, t := range p.sessions {
		if !p.isClosing(t) && !terminalRunning(t) {
			wouldNotify = append(wouldNotify, id)
		}
	}
	p.mu.Unlock()

	if len(wouldNotify) > 0 {
		t.Fatalf("已关闭的终端不应被 cleanupLoop 当作「进程已退出」上报：%v", wouldNotify)
	}
	_ = capture
}

// ── 2026-10-25 新增：禁止「自造状态回声」的判据 ──────────────────
//
// 背景（真机事故）：agent 通过 terminal_create 建终端后，插件注入
// 「[终端 X 已启动]」；超时后又注入「[终端 X 已超时关闭]」。
// 这两条都是**插件自己造成、且 agent 已能从工具返回值推导**的状态，
// 而每条注入都会唤醒一整轮 LLM 推理。实测后果：agent 反复向用户
// 解释"终端怎么又没了"，真正要做的活一件没做。
//
// 下面两条是**反向判据**：断言这些注入**不再出现**。没有它们，
// 后人（或我自己）很容易为了"让 agent 感知"把注入加回来，而那时
// 上面那些 waitReadLoopReady 的测试依然全绿 —— 缺陷会静默复发。

// 判据一：建终端不得产生「已启动」注入（terminal_create 返回值已足够）。
func TestNoStartupNotificationInjected(t *testing.T) {
	p := New("agentcli")
	p.notifyBytes = 1 << 20 // 不触发缓冲通知，隔离变量

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
	waitReadLoopReady(t, term)

	// 给足时间让任何潜在注入发生
	time.Sleep(150 * time.Millisecond)

	for _, text := range capture.snapshot() {
		if strings.Contains(text, "已启动") {
			t.Fatalf("不得注入「已启动」（terminal_create 返回值已告知）: %q", text)
		}
	}

	close(ts.stopCh)
	<-ts.done
}

// 判据二：超时关闭不得注入（创建时 timeout 已告知）。
func TestNoExpiredNotificationInjected(t *testing.T) {
	p := New("agentcli")
	p.notifyBytes = 1 << 20

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
	// 设为已超时：readLoop 下一轮循环就会走 IsExpired 分支
	ts.mu.Lock()
	ts.timeout = time.Nanosecond
	ts.createdAt = time.Now().Add(-time.Hour)
	ts.mu.Unlock()

	startReadLoop(p, sdkInst, ts)
	waitReadLoopReady(t, term)

	// 等 removeSession 真的发生（说明走了超时分支）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		_, still := p.sessions[ts.id]
		p.mu.Unlock()
		if !still {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	for _, text := range capture.snapshot() {
		if strings.Contains(text, "已超时关闭") {
			t.Fatalf("不得注入「已超时关闭」（创建时 timeout 已告知）: %q", text)
		}
	}
}
