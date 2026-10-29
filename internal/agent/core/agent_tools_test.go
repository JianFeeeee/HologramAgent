package core

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

type mockOutputDevice struct {
	name   string
	caps   agentIO.OutputCapability
	tools  []agentIO.ToolDef
	toolFn func(string, map[string]interface{}) (interface{}, error)
}

func (d *mockOutputDevice) Name() string                                 { return d.name }
func (d *mockOutputDevice) Type() agentIO.DeviceType                     { return agentIO.DeviceOutput }
func (d *mockOutputDevice) Description() string                          { return "mock " + d.name }
func (d *mockOutputDevice) Tools() []agentIO.ToolDef                     { return d.tools }
func (d *mockOutputDevice) Start() error                                 { return nil }
func (d *mockOutputDevice) Stop() error                                  { return nil }
func (d *mockOutputDevice) OutputCapabilities() agentIO.OutputCapability { return d.caps }
func (d *mockOutputDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{} }
func (d *mockOutputDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	if d.toolFn != nil {
		return d.toolFn(tool, args)
	}
	return nil, nil
}

func TestExecuteOutputListChannels(t *testing.T) {
	io := agentIO.NewIOManager()
	dev := &mockOutputDevice{
		name: "speaker",
		caps: agentIO.CapText | agentIO.CapAudio,
	}
	io.RegisterDevice(dev)

	a := &Agent{io: io}
	result := a.executeOutputListChannels()
	if result == "" || result == "没有可用通道" {
		t.Errorf("expected channel list, got: %s", result)
	}
}

func TestExecuteOutputListChannelsEmpty(t *testing.T) {
	io := agentIO.NewIOManager()
	a := &Agent{io: io}
	result := a.executeOutputListChannels()
	if result != "没有可用通道" {
		t.Errorf("expected '没有可用通道', got: %s", result)
	}
}

func TestExecuteOutputSendTool(t *testing.T) {
	io := agentIO.NewIOManager()
	io.RegisterDevice(&mockOutputDevice{
		name: "screen",
		caps: agentIO.CapText,
	})

	a := &Agent{io: io}
	tc := agentAPI.ToolCall{Name: "output_send__screen", Arguments: map[string]interface{}{
		"payload": "hello world",
		"type":    "text",
	}}
	result := a.executeOutputSendTool(tc)
	if result != "ok" {
		t.Errorf("expected ok, got: %s", result)
	}
}

func TestExecuteOutputSendToolEmptyContent(t *testing.T) {
	io := agentIO.NewIOManager()
	a := &Agent{io: io}
	tc := agentAPI.ToolCall{Name: "output_send__screen", Arguments: map[string]interface{}{}}
	result := a.executeOutputSendTool(tc)
	if result == "" || strings.Contains(result, "已通过") {
		t.Errorf("expected error for empty content, got: %s", result)
	}
}

func TestExecuteOutputSendToolChannelNotExist(t *testing.T) {
	io := agentIO.NewIOManager()
	a := &Agent{io: io}
	tc := agentAPI.ToolCall{Name: "output_send__nonexistent", Arguments: map[string]interface{}{
		"payload": "hello",
		"type":    "text",
	}}
	result := a.executeOutputSendTool(tc)
	if !strings.Contains(result, "不存在") && !strings.Contains(result, "不可用") {
		t.Errorf("expected error for nonexistent channel, got: %s", result)
	}
}

func TestExecuteOutputSendToolNoTextCap(t *testing.T) {
	io := agentIO.NewIOManager()
	io.RegisterDevice(&mockOutputDevice{
		name: "camera",
		caps: agentIO.CapImage,
	})

	a := &Agent{io: io}
	tc := agentAPI.ToolCall{Name: "output_send__camera", Arguments: map[string]interface{}{
		"payload": "hello",
		"type":    "text",
	}}
	result := a.executeOutputSendTool(tc)
	if strings.Contains(result, "已通过") {
		t.Errorf("should reject channel without text capability")
	}
}

func TestBuildToolDefsOutputToolsWithDevice(t *testing.T) {
	io := agentIO.NewIOManager()
	io.RegisterDevice(&mockOutputDevice{
		name: "screen",
		caps: agentIO.CapText,
	})
	a := &Agent{io: io, knowledge: nil, coldcontextStore: nil, pluginReg: nil}
	tools := a.buildToolDefs()

	foundSend := false
	foundHelp := false
	for _, td := range tools {
		m, ok := td.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		switch name {
		case "output_send__screen":
			foundSend = true
		case "output_send__screen_help":
			foundHelp = true
		}
	}
	if !foundSend {
		t.Error("output_send__screen should be in tools when device registered")
	}
	if !foundHelp {
		t.Error("output_send__screen_help should be in tools when device registered")
	}
}

func TestGetAllToolsEmpty(t *testing.T) {
	io := agentIO.NewIOManager()
	a := &Agent{io: io}
	tools := a.buildToolDefs()
	if len(tools) < 1 {
		t.Errorf("expected at least 1 tool, got %d", len(tools))
	}
}
