//go:build linux || windows

package agentcli

import (
	"sync"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// eventCapture 订阅事件总线，收下 terminal_output 事件供断言。
type eventCapture struct {
	mu   sync.Mutex
	outs []map[string]interface{}
}

func (c *eventCapture) add(ev *events.Event) {
	c.mu.Lock()
	c.outs = append(c.outs, ev.Payload)
	c.mu.Unlock()
}

// outputFor 汇总某个终端已发布的全部 output 片段。
func (c *eventCapture) outputFor(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var s string
	for _, p := range c.outs {
		if p["terminal_id"] == id {
			if o, _ := p["output"].(string); o != "" {
				s += o
			}
		}
	}
	return s
}

func (c *eventCapture) statesFor(id string) []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []bool
	for _, p := range c.outs {
		if p["terminal_id"] == id {
			r, _ := p["running"].(bool)
			out = append(out, r)
		}
	}
	return out
}

// 短命令（在首个 200ms ticker 之前就结束）的输出必须在退出时补推，
// 否则只留在 buf 里、永远发不出 terminal_output 事件，
// 内核权威视图（以及 WebUI/CLI 的 /terminals）output 恒为空。
func TestReadLoopFlushesOutputOnExit(t *testing.T) {
	p := New("agentcli")
	bus := events.NewBus()
	capture := &eventCapture{}
	bus.Subscribe(events.EventTerminalOutput, capture.add)

	sdkInst := sdk.New("agentcli", sdk.SDKConfig{
		RegTool:  func(string, sdk.ToolDef, sdk.ToolHandler) error { return nil },
		RegStage: func(sdk.Stage, sdk.StageHandler) {},
		RegAPI:   func(string) error { return nil },
		Settings: sdk.NewSettings("agentcli", nil),
		EventBus: bus,
	})
	sdkInst.SetIOInjector(&injectCapture{})

	term := newMockTerm()
	ts := newTestSession(term)
	startReadLoop(p, sdkInst, ts)

	// 推入输出后立刻让进程退出——远早于 200ms ticker。
	term.push([]byte("HELLO_KERNEL_REGISTRY\n"))
	time.Sleep(50 * time.Millisecond)
	term.setRunning(false)

	select {
	case <-ts.done:
	case <-time.After(3 * time.Second):
		t.Fatal("readLoop did not exit")
	}

	if got := capture.outputFor(ts.id); got != "HELLO_KERNEL_REGISTRY\n" {
		t.Fatalf("output on exit = %q, want %q", got, "HELLO_KERNEL_REGISTRY\n")
	}
	// 停止状态也要报到（readLoop 退出时 emitTermState(false)）。
	states := capture.statesFor(ts.id)
	if len(states) == 0 || states[len(states)-1] {
		t.Fatalf("expected trailing running=false, got %v", states)
	}
}
