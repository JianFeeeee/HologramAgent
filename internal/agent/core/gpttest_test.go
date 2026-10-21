package core

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func TestExecuteGetPluginTools(tmp *testing.T) {
	sh := NewStageHost()
	sh.RegisterTool("weather_current", sdk.ToolDef{Name: "weather_current", Plugin: "weather", Description: "当前天气", Parameters: map[string]interface{}{"type": "object"}}, nil)
	sh.RegisterTool("weather_forecast", sdk.ToolDef{Name: "weather_forecast", Plugin: "weather", Description: "天气预报"}, nil)
	sh.RegisterTool("device_ctl_cmdrun", sdk.ToolDef{Name: "device_ctl_cmdrun", Plugin: "remotedevice", Description: "下发命令"}, nil)

	a := New(AgentConfig{ID: "t", StageHost: sh})

	out := a.executeGetPluginTools("weather")
	if !strings.Contains(out, "weather_current") || !strings.Contains(out, "weather_forecast") {
		tmp.Fatalf("weather tools missing:\n%s", out)
	}
	if strings.Contains(out, "device_ctl_cmdrun") {
		tmp.Fatalf("should not contain other plugin:\n%s", out)
	}
	all := a.executeGetPluginTools("")
	if !strings.Contains(all, "weather_current") || !strings.Contains(all, "device_ctl_cmdrun") {
		tmp.Fatalf("all tools missing:\n%s", all)
	}
	none := a.executeGetPluginTools("nope")
	if !strings.Contains(none, "没有可用的工具") {
		tmp.Fatalf("unknown plugin msg:\n%s", none)
	}
}
