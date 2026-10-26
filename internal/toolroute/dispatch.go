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
	case strings.HasPrefix(tc.Name, "memory_"):
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

// IsBuiltinTool 报告某名字是否会由 Dispatch 处理（供诊断与提示词使用）。
//
// ★ 与 Dispatch 共用同一套前缀常量，避免"两套语义"（历史上出现过
// "模型看得到但插件查不到"的漂移）。
func IsBuiltinTool(name string) bool {
	switch {
	case name == "persona_set", name == "output_list_channels", name == "input_channels",
		name == "resident_agents", name == "notify_parent", name == "inputch_note",
		name == "plgreload", name == "get_plugin_tools":
		return true
	}
	for _, p := range []string{"memory_", "social_", "knowledge_", "coldcontext_", "doc_", "secret_", "output_send__"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
