package io

import (
	"sync"
	"testing"
)

// mockDevice implements Device for testing
type mockDevice struct {
	name           string
	devType        DeviceType
	desc           string
	tools          []ToolDef
	caps           OutputCapability
	startFn        func() error
	stopFn         func() error
	executeFn      func(string, map[string]interface{}) (interface{}, error)
	startCallCount int
	stopCallCount  int
	mu             sync.Mutex
}

func (d *mockDevice) Name() string        { return d.name }
func (d *mockDevice) Type() DeviceType    { return d.devType }
func (d *mockDevice) Description() string { return d.desc }
func (d *mockDevice) Tools() []ToolDef    { return d.tools }
func (d *mockDevice) Start() error {
	d.mu.Lock()
	d.startCallCount++
	d.mu.Unlock()
	if d.startFn != nil {
		return d.startFn()
	}
	return nil
}
func (d *mockDevice) Stop() error {
	d.mu.Lock()
	d.stopCallCount++
	d.mu.Unlock()
	if d.stopFn != nil {
		return d.stopFn()
	}
	return nil
}
func (d *mockDevice) OutputCapabilities() OutputCapability { return d.caps }
func (d *mockDevice) ChannelDef() ChannelDef               { return ChannelDef{} }
func (d *mockDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	if d.executeFn != nil {
		return d.executeFn(tool, args)
	}
	return nil, nil
}

func TestNewIOManager(t *testing.T) {
	m := NewIOManager()
	if m == nil {
		t.Fatal("IOManager should not be nil")
	}
	if m.InputChan() == nil {
		t.Error("InputChan should not be nil")
	}
	if m.OutputChan() == nil {
		t.Error("OutputChan should not be nil")
	}
}

func TestRegisterDevice(t *testing.T) {
	m := NewIOManager()
	dev := &mockDevice{name: "test_dev"}
	if err := m.RegisterDevice(dev); err != nil {
		t.Fatal(err)
	}
	// Duplicate registration
	if err := m.RegisterDevice(dev); err == nil {
		t.Error("expected error for duplicate registration")
	}
}

func TestUnregisterDevice(t *testing.T) {
	m := NewIOManager()
	m.RegisterDevice(&mockDevice{name: "dev1"})
	m.RegisterDevice(&mockDevice{name: "dev2"})

	m.UnregisterDevice("dev1")

	devices := m.ListDevices()
	if len(devices) != 1 {
		t.Errorf("expected 1 device, got %d", len(devices))
	}
	if devices[0].Name() != "dev2" {
		t.Errorf("expected 'dev2', got %q", devices[0].Name())
	}
}

func TestUnregisterDeviceTwice(t *testing.T) {
	m := NewIOManager()
	m.RegisterDevice(&mockDevice{name: "dev1"})
	m.UnregisterDevice("dev1")
	m.UnregisterDevice("dev1") // should not panic
}

func TestInjectTextTo(t *testing.T) {
	m := NewIOManager()
	go func() {
		evt := <-m.InputChan()
		if evt.OutputChannel != "speaker" {
			t.Errorf("expected OutputChannel 'speaker', got %q", evt.OutputChannel)
		}
		content, _ := evt.Payload["content"].(string)
		if content != "say hello" {
			t.Errorf("expected 'say hello', got %q", content)
		}
	}()
	m.InjectTextTo("mic", "speaker", "say hello")
}

func TestInjectTextSyncTo(t *testing.T) {
	m := NewIOManager()
	go func() {
		evt := <-m.InputChan()
		if evt.OutputChannel != "qq" {
			t.Errorf("expected OutputChannel 'qq', got %q", evt.OutputChannel)
		}
		evt.ResponseCh <- &OutputEvent{Done: true}
	}()

	resp := m.InjectTextSyncTo("onebot", "qq", "hi")
	if resp == nil || !resp.Done {
		t.Error("expected Done response")
	}
}

func TestInjectInput(t *testing.T) {
	m := NewIOManager()
	go func() {
		evt := <-m.InputChan()
		if evt.Source != "test_source" {
			t.Errorf("expected source 'test_source', got %q", evt.Source)
		}
		if evt.Type != "text" {
			t.Errorf("expected type 'text', got %q", evt.Type)
		}
		if evt.OutputChannel != "test_source" {
			t.Errorf("expected OutputChannel 'test_source', got %q", evt.OutputChannel)
		}
	}()

	m.InjectInput("test_source", "text", map[string]interface{}{"content": "hello"})
}

func TestInjectInputSync(t *testing.T) {
	m := NewIOManager()
	go func() {
		evt := <-m.InputChan()
		if evt.OutputChannel != "test" {
			t.Errorf("expected OutputChannel 'test', got %q", evt.OutputChannel)
		}
		evt.ResponseCh <- &OutputEvent{RequestID: evt.RequestID, Done: true}
	}()

	resp := m.InjectInputSync("test", "text", map[string]interface{}{"content": "sync"})
	if resp == nil {
		t.Fatal("expected response")
	}
	if !resp.Done {
		t.Error("expected Done=true")
	}
}

func TestInjectInputTo(t *testing.T) {
	m := NewIOManager()
	go func() {
		evt := <-m.InputChan()
		if evt.OutputChannel != "speaker" {
			t.Errorf("expected OutputChannel 'speaker', got %q", evt.OutputChannel)
		}
	}()

	m.InjectInputTo("mic", "speaker", "text", map[string]interface{}{"content": "hello"})
}

func TestInjectInputSyncTo(t *testing.T) {
	m := NewIOManager()
	go func() {
		evt := <-m.InputChan()
		if evt.OutputChannel != "email" {
			t.Errorf("expected OutputChannel 'email', got %q", evt.OutputChannel)
		}
		evt.ResponseCh <- &OutputEvent{Done: true}
	}()

	resp := m.InjectInputSyncTo("plugin", "email", "text", map[string]interface{}{"content": "hi"})
	if resp == nil || !resp.Done {
		t.Error("expected Done response")
	}
}

func TestEmitOutput(t *testing.T) {
	m := NewIOManager()
	go func() {
		evt := <-m.OutputChan()
		if evt.Target != "memory" {
			t.Errorf("expected target 'memory', got %q", evt.Target)
		}
	}()

	m.EmitOutput("memory", "text", map[string]interface{}{"content": "data"})
}

func TestEmitOutputTo(t *testing.T) {
	m := NewIOManager()
	go func() {
		evt := <-m.OutputChan()
		if evt.OutputChannel != "speaker" {
			t.Errorf("expected channel 'speaker', got %q", evt.OutputChannel)
		}
	}()

	m.EmitOutputTo("agent", "speaker", "text", map[string]interface{}{"content": "hello"})
}

func TestGetAllTools(t *testing.T) {
	m := NewIOManager()
	m.RegisterDevice(&mockDevice{
		name: "dev1",
		tools: []ToolDef{
			{Name: "tool1", Description: "first tool"},
		},
	})
	m.RegisterDevice(&mockDevice{
		name: "dev2",
		tools: []ToolDef{
			{Name: "tool2", Description: "second tool"},
			{Name: "tool3", Description: "third tool"},
		},
	})

	tools := m.GetAllTools()
	if len(tools) != 3 {
		t.Errorf("expected 3 tools, got %d", len(tools))
	}
}

func TestExecuteTool(t *testing.T) {
	m := NewIOManager()
	m.RegisterDevice(&mockDevice{
		name: "calc",
		tools: []ToolDef{
			{Name: "add", Description: "addition"},
		},
		executeFn: func(tool string, args map[string]interface{}) (interface{}, error) {
			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)
			return a + b, nil
		},
	})

	result, err := m.ExecuteTool("add", map[string]interface{}{"a": 1.0, "b": 2.0})
	if err != nil {
		t.Fatal(err)
	}
	if result.(float64) != 3.0 {
		t.Errorf("expected 3.0, got %v", result)
	}
}

func TestExecuteToolNotFound(t *testing.T) {
	m := NewIOManager()
	_, err := m.ExecuteTool("nonexistent", nil)
	if err == nil {
		t.Error("expected error for nonexistent tool")
	}
}

func TestListChannels(t *testing.T) {
	m := NewIOManager()
	m.RegisterDevice(&mockDevice{
		name:    "screen",
		caps:    CapText | CapImage,
		devType: DeviceOutput,
	})
	m.RegisterDevice(&mockDevice{
		name:    "mic",
		caps:    0,
		devType: DeviceInput,
	})

	channels := m.ListChannels()
	if len(channels) != 2 {
		t.Errorf("expected 2 channels, got %d", len(channels))
	}
}

func TestGetChannelCapabilities(t *testing.T) {
	m := NewIOManager()
	m.RegisterDevice(&mockDevice{
		name: "speaker",
		caps: CapText | CapAudio,
	})

	caps := m.GetChannelCapabilities("speaker")
	if !caps.Supports(CapText) {
		t.Error("should support text")
	}
	if !caps.Supports(CapAudio) {
		t.Error("should support audio")
	}
	if caps.Supports(CapImage) {
		t.Error("should not support image")
	}

	caps = m.GetChannelCapabilities("nonexistent")
	if caps != 0 {
		t.Errorf("expected 0 capabilities, got %v", caps)
	}
}

func TestStartAll(t *testing.T) {
	m := NewIOManager()
	started := false
	dev := &mockDevice{
		name: "test",
		startFn: func() error {
			started = true
			return nil
		},
	}
	m.RegisterDevice(dev)

	if err := m.StartAll(); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Error("device should have been started")
	}
}

func TestStartAllError(t *testing.T) {
	m := NewIOManager()
	m.RegisterDevice(&mockDevice{
		name: "fail",
		startFn: func() error {
			return nil
		},
	})
	m.RegisterDevice(&mockDevice{
		name: "ok",
		startFn: func() error {
			return nil
		},
	})

	if err := m.StartAll(); err != nil {
		t.Fatal(err)
	}
}

func TestStopAll(t *testing.T) {
	m := NewIOManager()
	stopped := false
	dev := &mockDevice{
		name: "test",
		stopFn: func() error {
			stopped = true
			return nil
		},
	}
	m.RegisterDevice(dev)
	m.StartAll()
	m.StopAll()

	if !stopped {
		t.Error("device should have been stopped")
	}
}

func TestAtomicSwapDevices(t *testing.T) {
	m := NewIOManager()
	m.RegisterDevice(&mockDevice{name: "old1"})
	m.RegisterDevice(&mockDevice{name: "old2"})

	newDevices := map[string]Device{
		"new1": &mockDevice{name: "new1"},
		"new2": &mockDevice{name: "new2"},
	}

	old := m.AtomicSwapDevices(newDevices)
	if len(old) != 2 {
		t.Errorf("expected 2 old devices, got %d", len(old))
	}
	devices := m.ListDevices()
	if len(devices) != 2 {
		t.Errorf("expected 2 devices after swap, got %d", len(devices))
	}
}

func TestInjectText(t *testing.T) {
	m := NewIOManager()
	go func() {
		evt := <-m.InputChan()
		content, _ := evt.Payload["content"].(string)
		if content != "hello world" {
			t.Errorf("expected 'hello world', got %q", content)
		}
		if evt.OutputChannel != "user" {
			t.Errorf("expected OutputChannel 'user', got %q", evt.OutputChannel)
		}
	}()
	m.InjectText("user", "hello world")
}

func TestInjectTextSync(t *testing.T) {
	m := NewIOManager()
	go func() {
		evt := <-m.InputChan()
		evt.ResponseCh <- &OutputEvent{Done: true}
	}()

	resp := m.InjectTextSync("http", "ping")
	if resp == nil || !resp.Done {
		t.Error("expected Done response")
	}
}

func TestEmitText(t *testing.T) {
	m := NewIOManager()
	go func() {
		evt := <-m.OutputChan()
		content, _ := evt.Payload["content"].(string)
		if content != "notification" {
			t.Errorf("expected 'notification', got %q", content)
		}
	}()
	m.EmitText("user", "notification")
}

func TestEmitTextTo(t *testing.T) {
	m := NewIOManager()
	go func() {
		evt := <-m.OutputChan()
		if evt.OutputChannel != "email" {
			t.Errorf("expected 'email', got %q", evt.OutputChannel)
		}
		content, _ := evt.Payload["content"].(string)
		if content != "alert" {
			t.Errorf("expected 'alert', got %q", content)
		}
	}()
	m.EmitTextTo("agent", "email", "alert")
}

func TestOutputCapability(t *testing.T) {
	caps := CapText | CapImage
	if !caps.Supports(CapText) {
		t.Error("CapText should be supported")
	}
	if !caps.Supports(CapImage) {
		t.Error("CapImage should be supported")
	}
	if caps.Supports(CapAudio) {
		t.Error("CapAudio should not be supported")
	}
	if caps.Supports(CapFile) {
		t.Error("CapFile should not be supported")
	}
	if caps.Supports(CapStructured) {
		t.Error("CapStructured should not be supported")
	}
}

func TestOutputCapabilityString(t *testing.T) {
	caps := CapText | CapAudio
	s := caps.String()
	if s != "[text audio]" && s != "[audio text]" {
		t.Errorf("unexpected string: %s", s)
	}

	if OutputCapability(0).String() != "[]" {
		t.Errorf("expected empty, got %s", OutputCapability(0).String())
	}
}

func TestConcurrentAccess(t *testing.T) {
	m := NewIOManager()
	m.RegisterDevice(&mockDevice{name: "dev1"})
	m.RegisterDevice(&mockDevice{name: "dev2"})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.GetAllTools()
			m.ListChannels()
			m.ListDevices()
		}()
	}
	wg.Wait()
}

func TestNextRequestID(t *testing.T) {
	m := NewIOManager()
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := m.nextRequestID()
		if ids[id] {
			t.Errorf("duplicate request ID: %s", id)
		}
		ids[id] = true
	}
}
