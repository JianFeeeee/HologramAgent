package core

// 设备类工具的**授权闸**：设备指令类工具走的是工具面，而 AllowedOutputs 只作用于
// output_send__<通道> —— 不补闸的话"授权"对指令类完全无效（驻留子拿到
// device_ctl_cmdrun 就能指挥任意设备）。这里按目标设备的通道名 device/<id> 查同一道闸。

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// registerFakeDevice 注册一台假设备，带一个"需要 device_id 的指令类工具"和一个无参枚举工具。
func registerFakeDevice(t *testing.T, a *Agent, name string, called *[]string) {
	t.Helper()
	dev := &mockOutputDevice{
		name: name,
		caps: agentIO.CapStructured,
		tools: []agentIO.ToolDef{
			{Name: "device_ctl_cmdrun", Description: "在设备上执行命令"},
			{Name: "devicedetect", Description: "枚举设备"},
		},
		toolFn: func(tool string, args map[string]interface{}) (interface{}, error) {
			if called != nil {
				*called = append(*called, tool)
			}
			return "ok:" + tool, nil
		},
	}
	if err := a.io.RegisterDevice(dev); err != nil {
		t.Fatalf("注册测试设备 %s 失败: %v", name, err)
	}
}

func deviceToolCall(name string, args map[string]interface{}) agentAPI.ToolCall {
	return agentAPI.ToolCall{ID: "call_1", Name: name, Arguments: args}
}

// 完整授权（根 agent 默认）：设备指令工具照常可用。
func TestDeviceToolAuth_RootHasFullGrant(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	var called []string
	registerFakeDevice(t, a, "devicectl", &called)

	got := a.executeToolCall(deviceToolCall("device_ctl_cmdrun", map[string]interface{}{
		"device_id": "pc-1", "command": "ls",
	}), "cli")
	if !strings.Contains(got, "ok:device_ctl_cmdrun") {
		t.Fatalf("根 agent 应可指挥任意设备，实际: %s", got)
	}
}

// 收窄授权（驻留子）：只授权了 device/ok-1，指挥别的设备必须被拒，且**不落到设备**。
func TestDeviceToolAuth_NarrowedGrantRefusesOtherDevice(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.allowedOutputs = []string{"device/ok-1"}
	var called []string
	registerFakeDevice(t, a, "devicectl", &called)

	got := a.executeToolCall(deviceToolCall("device_ctl_cmdrun", map[string]interface{}{
		"device_id": "other-2", "command": "rm -rf /",
	}), "cli")
	if !strings.Contains(got, "未授权") {
		t.Fatalf("未授权设备应被拒，实际: %s", got)
	}
	if len(called) != 0 {
		t.Fatalf("被拒的调用不得落到设备，实际执行了 %v", called)
	}

	// 已授权的设备照常可用
	got = a.executeToolCall(deviceToolCall("device_ctl_cmdrun", map[string]interface{}{
		"device_id": "ok-1", "command": "ls",
	}), "cli")
	if !strings.Contains(got, "ok:device_ctl_cmdrun") {
		t.Fatalf("已授权设备应可用，实际: %s", got)
	}
}

// 无参枚举类（devicedetect）不受闸门影响：它不指向具体设备。
func TestDeviceToolAuth_EnumerationNotGated(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.allowedOutputs = []string{"device/ok-1"}
	var called []string
	registerFakeDevice(t, a, "devicectl", &called)

	got := a.executeToolCall(deviceToolCall("devicedetect", map[string]interface{}{}), "cli")
	if !strings.Contains(got, "ok:devicedetect") {
		t.Fatalf("枚举类工具不应被设备授权闸拦，实际: %s", got)
	}
}
