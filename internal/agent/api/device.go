package api

// Device 数据面的**公共契约**（低层包，避免 import 环）。
//
// ## 为什么这些类型必须住在这里
//
// 依赖图上有两个方向的需求：
//
//	internal/kernelmod/device → kernelboot → internal/agent/core
//	internal/agent/core       → 需要满足 device 的 ToolHost 接口
//
// 如果接口定义在 kernelmod/device，agent/core 就得 import 它 ⇒ **循环**。
// 所以契约下沉到 agent/api（core 早已依赖它，而它不依赖任何内核模块）。

// ToolDefinition 是工具定义的数据面投影（与具体 schema 表示解耦）。
type ToolDefinition struct {
	Name        string
	Description string
	// ParallelSafe 声明该工具可与同批其他工具并发执行。
	ParallelSafe bool
	// Timeout 是声明的超时（空 = 用内核默认）。
	Timeout string
}

// ToolHost 是 device 需要 agent 提供的那部分能力。
//
// ★ 用接口而不是具体类型：第 2 步把工具实现迁进 device 时，agent 只需
// 继续满足这个接口，双方都不必互相 import。
type ToolHost interface {
	// ToolDefinitions 返回当前 agent 可见的工具定义（含运行期门控）。
	ToolDefinitions() []ToolDefinition
	// ExecuteTool 执行一个工具调用（已含参数校验、授权闸、超时、脱敏）。
	ExecuteTool(name string, args map[string]interface{}) (string, error)
}

// DeviceSurface 是 device 数据面暴露给内核的最小面（KernelState.Device 的类型）。
//
// 只放"别的模块真的要读"的东西：诊断与状态面用工具清单与数量；
// 执行只经 Agent（安全闸门在那边），**不**从这里暴露任意执行入口。
type DeviceSurface interface {
	Tools() []ToolDefinition
	ToolCount() int
}

// BuiltinToolHandlers 是**内置工具的族处理器**（每个族一个方法）。
//
// ## 为什么用族的粒度而不是逐工具
//
// 族内工具共享同一批依赖（memory 族要 graph、knowledge 族要 store、
// coldcontext 族要 docStore…）。按族传接口，device 的分发层就能
// 在不认识 Agent 内部字段的前提下完整接管**路由**：
//
//	device.Dispatch(tc) → 选族 → handler.ExecuteMemoryTool(tc)
//
// 第二阶段再把族实现搬进 device（那时依赖改为显式 Deps 结构体），
// 路由这一段不用再改。
type BuiltinToolHandlers interface {
	ExecutePersonaTool(tc ToolCall) string
	ExecuteMemoryTool(tc ToolCall, turnScenes []string) string
	ExecuteSocialTool(tc ToolCall) string
	ExecuteKnowledgeTool(tc ToolCall) string
	ExecuteColdContextTool(tc ToolCall) string
	ExecuteSecretTool(tc ToolCall) string
	ExecuteOutputSendHelp(tc ToolCall) string
	ExecuteOutputSend(tc ToolCall) string
	ExecuteOutputListChannels() string
	ExecuteInputChannels(tc ToolCall) string
	ExecuteResidentAgents(tc ToolCall) string
	ExecuteNotifyParent(tc ToolCall) string
	ExecuteInputchNote(tc ToolCall) string
	ExecutePluginReload() string
	ExecuteGetPluginTools(pluginName string) string
}
