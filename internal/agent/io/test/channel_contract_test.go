package test

// internal/agent/io 的模块级黑盒判据：IOManager 的输入注入 → 可观察事件，
// 设备注册/查询/生命周期，以及能力标志语义。

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

type contractDevice struct {
	name     string
	started  int
	stopped  int
	startErr error
}

func (d *contractDevice) Name() string              { return d.name }
func (d *contractDevice) Type() io.DeviceType       { return io.DeviceIO }
func (d *contractDevice) ChannelDef() io.ChannelDef { return io.ChannelDef{} }
func (d *contractDevice) Description() string       { return "contract device" }
func (d *contractDevice) Tools() []io.ToolDef       { return nil }
func (d *contractDevice) Execute(string, map[string]interface{}) (interface{}, error) {
	return nil, nil
}
func (d *contractDevice) Start() error                            { d.started++; return d.startErr }
func (d *contractDevice) Stop() error                             { d.stopped++; return nil }
func (d *contractDevice) OutputCapabilities() io.OutputCapability { return io.CapText }

func receiveInput(t *testing.T, m *io.IOManager) *io.InputEvent {
	t.Helper()
	select {
	case evt := <-m.InputChan():
		return evt
	case <-time.After(2 * time.Second):
		t.Fatal("injected input never reached the module's input queue")
		return nil
	}
}

// TestInjectTextDeliversInputEvent：注入文本必须成为一条可观察的输入事件。
func TestInjectTextDeliversInputEvent(t *testing.T) {
	m := io.NewIOManager()
	m.InjectText("qq", "你好")
	evt := receiveInput(t, m)
	if evt.Source != "qq" || evt.OutputChannel != "qq" {
		t.Fatalf("event routing = %+v", evt)
	}
	if text, _ := evt.Payload["content"].(string); text != "你好" {
		t.Fatalf("payload text = %v", evt.Payload)
	}
	if evt.RequestID == "" {
		t.Fatal("injected event has no request id")
	}
}

// TestInjectInputToRoutesToRequestedChannel：显式输出通道必须被保留。
func TestInjectInputToRoutesToRequestedChannel(t *testing.T) {
	m := io.NewIOManager()
	m.InjectInputTo("qq", "webui", "message", map[string]interface{}{"text": "hi"})
	evt := receiveInput(t, m)
	if evt.Source != "qq" || evt.OutputChannel != "webui" {
		t.Fatalf("routing = %+v", evt)
	}
}

// TestSyncInjectionWaitsForResponse：同步注入必须把响应交回调用方。
func TestSyncInjectionWaitsForResponse(t *testing.T) {
	m := io.NewIOManager()
	go func() {
		evt := <-m.InputChan()
		if evt.ResponseCh != nil {
			evt.ResponseCh <- &io.OutputEvent{Payload: map[string]interface{}{"content": "answer"}}
		}
	}()
	got := m.InjectTextSync("qq", "问题")
	if got == nil {
		t.Fatal("sync injection returned nil")
	}
	if c, _ := got.Payload["content"].(string); c != "answer" {
		t.Fatalf("response payload = %v", got.Payload)
	}
}

// TestDeviceRegistrationLifecycle：注册 → 查询 → StartAll/StopAll 真实生效。
func TestDeviceRegistrationLifecycle(t *testing.T) {
	m := io.NewIOManager()
	dev := &contractDevice{name: "contract-dev"}
	if err := m.RegisterDevice(dev); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	if got := m.GetDevice("contract-dev"); got == nil || got.Name() != "contract-dev" {
		t.Fatalf("GetDevice = %v", got)
	}
	if err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	m.StopAll()
	if dev.started != 1 || dev.stopped != 1 {
		t.Fatalf("device lifecycle started=%d stopped=%d", dev.started, dev.stopped)
	}
}

// TestDuplicateDeviceRejected：重复注册必须报错，不静默覆盖。
func TestDuplicateDeviceRejected(t *testing.T) {
	m := io.NewIOManager()
	if err := m.RegisterDevice(&contractDevice{name: "dup"}); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterDevice(&contractDevice{name: "dup"}); err == nil {
		t.Fatal("duplicate device name accepted")
	}
}

// TestStartAllPropagatesDeviceFailure：设备启动失败必须向上传播。
func TestStartAllPropagatesDeviceFailure(t *testing.T) {
	m := io.NewIOManager()
	dev := &contractDevice{name: "bad", startErr: errors.New("boom")}
	if err := m.RegisterDevice(dev); err != nil {
		t.Fatal(err)
	}
	err := m.StartAll()
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("StartAll error = %v", err)
	}
}

// TestSupportsSingleAndCombinedFlags：位标志的 Supports 语义（任一位命中）。
func TestSupportsSingleAndCombinedFlags(t *testing.T) {
	c := io.CapText | io.CapAudio
	if !c.Supports(io.CapText) || !c.Supports(io.CapAudio) {
		t.Fatal("set capability reported as unsupported")
	}
	if c.Supports(io.CapImage) {
		t.Fatal("unset capability reported as supported")
	}
	if !io.CapText.Supports(io.CapText | io.CapImage) {
		t.Fatal("any-bit semantics broken")
	}
	if io.CapText.Supports(io.CapImage) {
		t.Fatal("disjoint capability reported supported")
	}
}

// TestCapabilityStringListsFlags：String 列出已置位能力名。
func TestCapabilityStringListsFlags(t *testing.T) {
	s := (io.CapText | io.CapImage).String()
	if !strings.Contains(s, "text") || !strings.Contains(s, "image") {
		t.Fatalf("String() = %q", s)
	}
	if strings.Contains(s, "audio") {
		t.Fatalf("String() = %q contains unset capability", s)
	}
}
