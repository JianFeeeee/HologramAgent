//go:build linux || windows

package agentcli

import (
	"testing"
	"time"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// Stop() 必须在有活跃终端时也能返回：它先 p.wg.Wait() 再 Close 终端，
// 而 readLoop 自己也记在 p.wg 上且不监听 p.stopCh —— 若 readLoop 只在
// t.stopCh 上阻塞，p.wg.Wait() 会永远等下去（死锁）。
func TestStopWithActiveTerminal(t *testing.T) {
	p := New("agentcli")
	sdkInst := sdk.New("agentcli", sdk.SDKConfig{
		RegTool:  func(string, sdk.ToolDef, sdk.ToolHandler) error { return nil },
		RegStage: func(sdk.Stage, sdk.StageHandler) {},
		RegAPI:   func(string) error { return nil },
		Settings: sdk.NewSettings("agentcli", nil),
	})
	sdkInst.SetIOInjector(&injectCapture{})
	if err := p.Start(sdkInst); err != nil {
		t.Fatal(err)
	}

	term := newMockTerm()
	ts := newTestSession(term)
	ts.command = "sleep 999"
	p.mu.Lock()
	p.sessions[ts.id] = ts
	p.mu.Unlock()
	startReadLoop(p, sdkInst, ts)

	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	go func() { p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() deadlocked with an active terminal")
	}
}

// Stop 必须幂等：重复调用不能 panic（close 已关闭的 channel 会 panic）。
func TestStopIsIdempotent(t *testing.T) {
	p := New("agentcli")
	sdkInst := sdk.New("agentcli", sdk.SDKConfig{
		RegTool:  func(string, sdk.ToolDef, sdk.ToolHandler) error { return nil },
		RegStage: func(sdk.Stage, sdk.StageHandler) {},
		RegAPI:   func(string) error { return nil },
		Settings: sdk.NewSettings("agentcli", nil),
	})
	sdkInst.SetIOInjector(&injectCapture{})
	if err := p.Start(sdkInst); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop() panicked on repeated call: %v", r)
		}
	}()
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
}
