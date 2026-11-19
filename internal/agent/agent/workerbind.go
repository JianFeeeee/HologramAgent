package agent

import agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"

// workerbind.go —— 工作式子类型的**装配注入口**（唯一的跨包点）。
//
// ## 为什么需要它（而不是基座直接 import worker）
//
// 依赖必须是 `worker → 基座`（类型模块依赖基座），单向。基座 import worker
// 会成环。而"工具表分派"发生在基座（工具表是基座渲染的），所以执行体
// 必须由 **worker 包在 init() 里注进来**。
//
//	worker 包 init()：agent.BindWorker(spawnChild, childResult, executeLLMTool)
//	基座工具分派：调绑定的那三个函数
//
// 这不是"映射层"：它只有三个函数指针，且是**类型注册**的同一手法
// （与 `Hooks` / `registry.Descriptor` 一致）。加一种新类型时，
// 新包自己 Bind，基座零改动。

// workerBind 是工作式子类型注入的三个执行体。
type workerBind struct {
	spawnChild     func(a *Agent, tc agentAPI.ToolCall, parentChannel string) string
	childResult    func(a *Agent, tc agentAPI.ToolCall) string
	executeLLMTool func(a *Agent, tc agentAPI.ToolCall) string
}

var workerImpl workerBind

// BindWorker 由 worker 包在 init() 里调用（只调一次）。
func BindWorker(spawn func(a *Agent, tc agentAPI.ToolCall, ch string) string,
	childRes func(a *Agent, tc agentAPI.ToolCall) string,
	llmTool func(a *Agent, tc agentAPI.ToolCall) string) {
	workerImpl = workerBind{spawnChild: spawn, childResult: childRes, executeLLMTool: llmTool}
}

func spawnChildFn(a *Agent, tc agentAPI.ToolCall, ch string) string {
	if workerImpl.spawnChild == nil {
		return "工作式子类型未注册（装配缺少 worker 包）"
	}
	return workerImpl.spawnChild(a, tc, ch)
}

func childResultFn(a *Agent, tc agentAPI.ToolCall) string {
	if workerImpl.childResult == nil {
		return "工作式子类型未注册（装配缺少 worker 包）"
	}
	return workerImpl.childResult(a, tc)
}

func executeLLMToolFn(a *Agent, tc agentAPI.ToolCall) string {
	if workerImpl.executeLLMTool == nil {
		return "工作式子类型未注册（装配缺少 worker 包）"
	}
	return workerImpl.executeLLMTool(a, tc)
}
