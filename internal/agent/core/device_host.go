package core

// Agent 侧的 **device 数据面适配层**。
//
// ## 为什么适配层在 core 而不是让 device 直接操作 Agent
//
// 工具执行必须**始终**经过 Agent 的完整执行链：
//
//	参数 schema 校验 → 工具授权闸 / 设备授权闸 → 超时与取消
//	→ panic 恢复 → 明文脱敏（出向 + 入向）→ ToolResult 成败判定
//
// 这些是**执行安全**，与敏感信息安全（security 模块）是两件事，
// 都不能被对方顶替（见 plan.md §五）。所以 device 只拿到窄接口
// （api.ToolHost），拿不到 Agent 内部字段，也就绕过不了这条链。

import (
	"sort"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// ToolDefinitions 实现 api.ToolHost：返回当前可见的工具定义。
//
// ★ 复用 buildToolDefs 的同一批生成逻辑 —— 两处各写一份必然漂移
// （历史上就出现过"模型看得到但插件查不到"的两套语义）。
func (a *Agent) ToolDefinitions() []api.ToolDefinition {
	if a == nil {
		return nil
	}
	raw := a.buildToolDefs()
	out := make([]api.ToolDefinition, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := fn["description"].(string)
		out = append(out, api.ToolDefinition{
			Name:        name,
			Description: desc,
			// 并发资格与超时**不在这里重算**：它们由 argvalidate.go 的
			// batchRunnable / resolveToolTimeout 统一判定（单一事实来源）。
			ParallelSafe: a.toolParallelSafe(name),
			Timeout:      "",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ExecuteTool 实现 api.ToolHost：走完整执行链执行一次工具调用。
//
// 通道用内置标记：工具执行本身不产出"用户可见输出"，
// 而记忆写入等场景需要一个稳定的来源标识（见 defaultChannelForBuiltin）。
func (a *Agent) ExecuteTool(name string, args map[string]interface{}) (string, error) {
	if a == nil {
		return "", errNoAgentForDevice
	}
	return a.executeToolCall(api.ToolCall{
		ID:        "device_" + name,
		Name:      name,
		Arguments: args,
	}, a.defaultChannelForBuiltin()), nil
}

// MountDevice 挂载 device 数据面。
//
// 单向依赖：core 只认识低层接口（api.DeviceSurface），不认识
// kernelmod/device 的具体类型 —— 否则 core ↔ kernelboot ↔ device 会成环。
//
// 幂等：重复挂载只覆盖（装配期可能因重建 Agent 而重挂）。
func (a *Agent) MountDevice(d api.DeviceSurface) {
	if a == nil {
		return
	}
	// 挂载同时接上路由适配器：device 由此拿到"选族后调谁"。
	a.builtinHandlers = builtinHandlers{a: a}
	a.deviceMu.Lock()
	a.device = d
	a.deviceMu.Unlock()
}

// Device 返回已挂载的数据面（未挂载时为 nil）。
func (a *Agent) Device() api.DeviceSurface {
	if a == nil {
		return nil
	}
	a.deviceMu.RLock()
	defer a.deviceMu.RUnlock()
	return a.device
}

// errNoAgentForDevice 表示 device 调用时 Agent 未装配（装配顺序错误）。
var errNoAgentForDevice = &deviceHostErr{"device: agent 未装配"}

type deviceHostErr struct{ msg string }

func (e *deviceHostErr) Error() string { return e.msg }

// builtinHandlers 把 Agent 的族处理器暴露成 api.BuiltinToolHandlers。
//
// 为什么用适配器而不是把 Agent 的方法全部导出：
//   - 导出只会让"谁都能调内置工具"，绕过 executeToolCall 的授权闸
//   - 适配器只在 core 内部构造，然后交给 device 的路由层
type builtinHandlers struct{ a *Agent }

func (h builtinHandlers) ExecutePersonaTool(tc api.ToolCall) string {
	return h.a.executePersonaTool(tc)
}
func (h builtinHandlers) ExecuteMemoryTool(tc api.ToolCall, scenes []string) string {
	return h.a.executeMemoryTool(tc, scenes)
}
func (h builtinHandlers) ExecuteSocialTool(tc api.ToolCall) string {
	return h.a.executeSocialTool(tc)
}
func (h builtinHandlers) ExecuteKnowledgeTool(tc api.ToolCall) string {
	return h.a.executeKnowledgeTool(tc)
}
func (h builtinHandlers) ExecuteColdContextTool(tc api.ToolCall) string {
	return h.a.executeDocTool(tc)
}
func (h builtinHandlers) ExecuteSecretTool(tc api.ToolCall) string {
	return h.a.executeSecretTool(tc)
}
func (h builtinHandlers) ExecuteOutputSendHelp(tc api.ToolCall) string {
	return h.a.executeOutputSendHelp(tc)
}
func (h builtinHandlers) ExecuteOutputSend(tc api.ToolCall) string {
	return h.a.executeOutputSendTool(tc)
}
func (h builtinHandlers) ExecuteOutputListChannels() string {
	return h.a.executeOutputListChannels()
}
func (h builtinHandlers) ExecuteInputChannels(tc api.ToolCall) string {
	return h.a.executeInputChannels(tc)
}
func (h builtinHandlers) ExecuteResidentAgents(tc api.ToolCall) string {
	return h.a.executeResidentAgents(tc)
}
func (h builtinHandlers) ExecuteNotifyParent(tc api.ToolCall) string {
	return h.a.executeNotifyParent(tc)
}
func (h builtinHandlers) ExecuteInputchNote(tc api.ToolCall) string {
	return h.a.executeInputchNote(tc)
}
func (h builtinHandlers) ExecutePluginReload() string {
	return h.a.executePluginReload()
}
func (h builtinHandlers) ExecuteGetPluginTools(pluginName string) string {
	return h.a.executeGetPluginTools(pluginName)
}
