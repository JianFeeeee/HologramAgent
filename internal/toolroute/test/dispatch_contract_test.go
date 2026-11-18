package test

// internal/toolroute 的黑盒判据：Dispatch(handlers, ToolCall) → (输出, handled)。
// 只调导出函数，不读源码、不碰内部 switch 以外的任何状态。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/toolroute"
)

// recordingHandlers 记录每个族处理器被调用的次数。
type recordingHandlers struct {
	calls map[string]int
}

func newHandlers() *recordingHandlers {
	return &recordingHandlers{calls: map[string]int{}}
}
func (h *recordingHandlers) hit(name string) string {
	h.calls[name]++
	return "out:" + name
}
func (h *recordingHandlers) ExecutePersonaTool(tc api.ToolCall) string { return h.hit("persona") }
func (h *recordingHandlers) ExecuteMemoryTool(tc api.ToolCall, s []string) string {
	return h.hit("memory")
}
func (h *recordingHandlers) ExecuteSocialTool(tc api.ToolCall) string { return h.hit("social") }
func (h *recordingHandlers) ExecuteColdContextTool(tc api.ToolCall) string {
	return h.hit("coldcontext")
}
func (h *recordingHandlers) ExecuteSecretTool(tc api.ToolCall) string { return h.hit("secret") }
func (h *recordingHandlers) ExecuteOutputSendHelp(tc api.ToolCall) string {
	return h.hit("output_help")
}
func (h *recordingHandlers) ExecuteOutputSend(tc api.ToolCall) string { return h.hit("output_send") }
func (h *recordingHandlers) ExecuteOutputListChannels() string        { return h.hit("output_list") }
func (h *recordingHandlers) ExecuteInputChannels(tc api.ToolCall) string {
	return h.hit("input_channels")
}
func (h *recordingHandlers) ExecutePluginReload() string { return h.hit("plgreload") }
func (h *recordingHandlers) ExecuteGetPluginTools(name string) string {
	return h.hit("get_plugin_tools:" + name)
}

// TestDispatchRoutesEachPrefix：每种工具前缀路由到对应族处理器。
func TestDispatchRoutesEachPrefix(t *testing.T) {
	cases := []struct {
		name, family string
	}{
		{"persona_set", "persona"},
		{"stablemem_commit", "memory"},
		{"social_note", "social"},
		{"coldcontext_query", "coldcontext"},
		{"doc_query", "coldcontext"}, // 旧前缀兼容
		{"secret_get", "secret"},
		{"output_send__qq", "output_send"},
		{"output_send__qq_help", "output_help"},
		{"output_list_channels", "output_list"},
		{"input_channels", "input_channels"},
		{"plgreload", "plgreload"},
	}
	for _, c := range cases {
		h := newHandlers()
		out, handled := toolroute.Dispatch(h, api.ToolCall{Name: c.name}, nil)
		if !handled {
			t.Fatalf("★ %q 未被认作内置工具", c.name)
		}
		if out == "" || h.calls[c.family] != 1 {
			t.Fatalf("★ %q 路由错：out=%q calls=%v", c.name, out, h.calls)
		}
	}
}

// TestDispatchGetPluginToolsExtractsArg：plugin_name 参数必须传给处理器。
func TestDispatchGetPluginToolsExtractsArg(t *testing.T) {
	h := newHandlers()
	out, handled := toolroute.Dispatch(h, api.ToolCall{
		Name: "get_plugin_tools", Arguments: map[string]interface{}{"plugin_name": "qq"},
	}, nil)
	if !handled || out != "out:get_plugin_tools:qq" {
		t.Fatalf("★ plugin_name 未透传：handled=%v out=%q", handled, out)
	}
}

// TestDispatchUnknownIsNotSwallowed：未知工具 handled=false（插件工具不能消失）。
func TestDispatchUnknownIsNotSwallowed(t *testing.T) {
	h := newHandlers()
	out, handled := toolroute.Dispatch(h, api.ToolCall{Name: "my_plugin_custom_tool"}, nil)
	if handled || out != "" {
		t.Fatalf("★ 未知工具被吞掉：handled=%v out=%q", handled, out)
	}
	if len(h.calls) != 0 {
		t.Fatalf("★ 未知工具却调了处理器：%v", h.calls)
	}
}

// TestDispatchNilHandlers：nil handlers 不得 panic。
func TestDispatchNilHandlers(t *testing.T) {
	if _, handled := toolroute.Dispatch(nil, api.ToolCall{Name: "stablemem_commit"}, nil); handled {
		t.Fatal("★ nil handlers 却报告 handled")
	}
}

// TestSubAgentToolsLiveInRegistry：子 agent 管理面的工具**不在 device 路由表里**。
//
// 它们由各自的注册者登记到 registry（谁拥有那个能力，谁注册那个工具），
// 分发走 registry.DispatchTool。本判据钉住"device 不许把它们吞回去" ——
// 一旦有人把 `resident_agents` / `rt_children` 之类塞回 Dispatch，
// 模型看得到的两处事实就会再次分叉（这正是 2026-11-18 修掉的形态）。
func TestSubAgentToolsLiveInRegistry(t *testing.T) {
	for _, name := range []string{
		"admin_child", "admin_child_help", "admin_child_list",
		"spawn_child", "child_result", "notify_parent", "inputch_note",
		"rt_children", "rt_transcript", "resident_agents",
	} {
		h := newHandlers()
		if _, handled := toolroute.Dispatch(h, api.ToolCall{Name: name}, nil); handled {
			t.Fatalf("★ %q 被 device 路由表接管了 —— 它应当由 registry 分发", name)
		}
	}
}
