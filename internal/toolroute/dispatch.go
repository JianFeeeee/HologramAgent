package toolroute

// 内置工具的**分发**（路由）归属 device。
//
// ## 这一步搬的是什么
//
// 原先 `agent/core/toolcall.go` 的 `executeToolCallInner` 自己写了一张
// "工具名前缀 → 族处理器"的 switch。那张表是本模块的第一份事实来源，
// 现在搬到这里：
//
//	device.Dispatch(handlers, tc)  → 选族 → handlers.ExecuteXxx(tc)
//
// 第二阶段再把**族实现**（memory_* / knowledge_* / coldcontext_* …）
// 从 core 搬进 device（依赖改为显式 Deps），路由这一段不用再动。
//
// ## 为什么保留族粒度
//
// 族内工具共享同一批依赖，按族传接口让 device 在不认识 Agent 内部字段的
// 前提下完整接管路由；逐工具传接口会让 30+ 个方法散成一张大表。

import (
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// Dispatch 按工具名选出族处理器并执行。
//
// 返回 (结果文本, 是否属于内置工具)：
//   - handled=false 表示"不是内置工具"，调用方应继续走插件/设备路径。
//     这是关键语义：**device 不吞掉未知工具**，否则插件工具会消失。
func Dispatch(h api.BuiltinToolHandlers, tc api.ToolCall, turnScenes []string) (string, bool) {
	if h == nil {
		return "", false
	}
	switch {
	case tc.Name == "persona_set":
		return h.ExecutePersonaTool(tc), true
	case strings.HasPrefix(tc.Name, "stablemem_"):
		return h.ExecuteMemoryTool(tc, turnScenes), true
	case strings.HasPrefix(tc.Name, "social_"):
		return h.ExecuteSocialTool(tc), true
	case strings.HasPrefix(tc.Name, "knowledge_"):
		return h.ExecuteKnowledgeTool(tc), true
	case strings.HasPrefix(tc.Name, "coldcontext_"):
		// doc_* 是旧前缀（工具已改名），保留兼容：插件/测试仍有旧调用。
		return h.ExecuteColdContextTool(tc), true
	case strings.HasPrefix(tc.Name, "doc_"):
		return h.ExecuteColdContextTool(tc), true
	case strings.HasPrefix(tc.Name, "secret_"):
		return h.ExecuteSecretTool(tc), true
	case strings.HasPrefix(tc.Name, "output_send__") && strings.HasSuffix(tc.Name, "_help"):
		return h.ExecuteOutputSendHelp(tc), true
	case strings.HasPrefix(tc.Name, "output_send__"):
		return h.ExecuteOutputSend(tc), true
	case tc.Name == "output_list_channels":
		return h.ExecuteOutputListChannels(), true
	case tc.Name == "input_channels":
		return h.ExecuteInputChannels(tc), true
	case tc.Name == "resident_agents":
		return h.ExecuteResidentAgents(tc), true
	case tc.Name == "notify_parent":
		return h.ExecuteNotifyParent(tc), true
	case tc.Name == "inputch_note":
		return h.ExecuteInputchNote(tc), true
	case tc.Name == "plgreload":
		return h.ExecutePluginReload(), true
	case tc.Name == "get_plugin_tools":
		name, _ := tc.Arguments["plugin_name"].(string)
		return h.ExecuteGetPluginTools(name), true
	}
	return "", false
}

// ★ 已删除 IsBuiltinTool（2026-10-29）。
//
// 它曾是 Dispatch 之外**第二份**前缀表，注释还写着"与 Dispatch 共用同一套
// 前缀常量" —— 但实际上是手抄的平行清单，两处都要人肉同步。
// 实测：全仓零调用方（唯一的 device.IsBuiltinTool 包装器同样零调用），
// 却在改名时制造了真实故障：改了 Dispatch 的 stablemem_，漏了这份表，
// 于是"看起来在维护事实来源"的代码变成了误导。
//
// 需要判"某名字会不会被处理"时，**直接调 Dispatch** —— 它的
// (string, bool) 第二个返回值就是权威答案。别再引入平行表。
