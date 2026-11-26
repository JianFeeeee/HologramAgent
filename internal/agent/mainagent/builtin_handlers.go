package mainagent

// builtin_handlers.go —— 本类型实现 `api.BuiltinToolHandlers`（工具**族处理器**）。
//
// ## 为什么必须有这一层（2026-11-20 实测回归）
//
// 内核执行设施的 `dispatch` 按序问四处：
//
//	① registry.DispatchTool    （名字 + 执行体都在登记表里）
//	② toolroute.Dispatch       （**族处理器**，经 host.BuiltinTools()）
//	③ stageHost.ExecuteToolCtx （插件工具）
//	④ io.ExecuteToolCtx        （通道/设备工具）
//
// ② 那一层此前是**死的**：`SetBuiltinTools` 全仓零调用 ⇒ `BuiltinTools()`
// 恒 nil ⇒ 所有"族处理器"形态的工具全部不可执行。
//
// 实测（真二进制 /tmp/lt6, 2026-11-20）：
//
//	output_list_channels → 兜底报错「不存在或未注册」
//	llm_list_sources     → 同上
//	secret_list          → 同上
//	get_plugin_tools     → 同上
//
// 而 `output_send__<通道>` 更严重：它会话里**真正用来对用户说话**的那条路
// （异步通道如 qq/wechat 必须显式 output_send，纯文本会被丢弃），
// 同样落空 ⇒ 用户永远收不到回复，而模型以为自己答过了。
//
// ## 分工（用户裁定 2026-11-19 的判据）
//
// 「这个工具读的是不是本类型的私有状态？」
//
//	是 ⇒ 本类型自己执行（`ownsTool` 分岔，见 toolroute.go），**不走本文件**
//	否 ⇒ 内核设施 → 本文件转发回宿主（执行体在基座 `*agent.Agent` 上）
//
// 本文件只做**转发**：不判定、不清洗、不重试 —— 那些都在执行链里。

import (
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// builtinHandlers 把基座的执行面适配成 `api.BuiltinToolHandlers`。
//
// ★ 私有族（memory/social/coldcontext/persona）**也**在这里接上，
//
//	因为 `toolroute` 的表把它们算作"内置族"。但正常路径不会走到这里：
//	本类型的 `ownsTool` 会先把它们自己执行掉（它持有那些私有句柄）。
//	这里接上只是为了"如果它们被交给内核，不会静默变成'不存在'"。
type builtinHandlers struct{ a *Module }

func (h builtinHandlers) ExecutePersonaTool(tc agentAPI.ToolCall) string {
	return h.a.executePersonaTool(tc)
}

func (h builtinHandlers) ExecuteMemoryTool(tc agentAPI.ToolCall, turnScenes []string) string {
	return h.a.executeMemoryTool(tc, turnScenes)
}

func (h builtinHandlers) ExecuteSocialTool(tc agentAPI.ToolCall) string {
	return h.a.executeSocialTool(tc)
}

func (h builtinHandlers) ExecuteColdContextTool(tc agentAPI.ToolCall) string {
	return h.a.executeDocTool(tc)
}

func (h builtinHandlers) ExecuteLLMTool(tc agentAPI.ToolCall) string {
	return h.a.a.ExecuteLLMTool(tc)
}

func (h builtinHandlers) ExecuteSecretTool(tc agentAPI.ToolCall) string {
	return h.a.a.ExecuteSecretTool(tc)
}

func (h builtinHandlers) ExecuteOutputSendHelp(tc agentAPI.ToolCall) string {
	return h.a.a.ExecuteOutputSendHelp(tc)
}

func (h builtinHandlers) ExecuteOutputSend(tc agentAPI.ToolCall) string {
	return h.a.a.ExecuteOutputSendTool(tc)
}

func (h builtinHandlers) ExecuteOutputListChannels() string {
	return h.a.a.ExecuteOutputListChannels()
}

func (h builtinHandlers) ExecuteInputChannels(tc agentAPI.ToolCall) string {
	return h.a.a.ExecuteInputChannels(tc)
}

func (h builtinHandlers) ExecutePluginReload() string {
	return h.a.a.ExecutePluginReload()
}

func (h builtinHandlers) ExecuteGetPluginTools(pluginName string) string {
	return h.a.a.ExecuteGetPluginTools(pluginName)
}

// installBuiltinHandlers 把族处理器交给基座（**必须调用**，不调用则
// 内核执行设施的 ② 那一路是死的）。
func (m *Module) installBuiltinHandlers() {
	if m == nil || m.a == nil {
		return
	}
	m.a.SetBuiltinTools(builtinHandlers{a: m})
}
