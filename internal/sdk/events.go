package sdk

import "github.com/JianFeeeee/HologramAgent/internal/events"

// 事件类型/事件类型别名：插件经 SDK 订阅/发布内核事件，
// 无需直接 import internal/events（内核事件总线仅通过 SDK 暴露）。

type EventType = events.EventType
type Event = events.Event

const (
	EventRawInput       = events.EventRawInput
	EventAgentOutput    = events.EventAgentOutput
	EventAgentLLMChain  = events.EventAgentLLMChain
	EventToolCall       = events.EventToolCall
	EventReasoning      = events.EventReasoning
	EventStage          = events.EventStage
	EventSystem         = events.EventSystem
	EventTerminalOutput = events.EventTerminalOutput
	EventAll            = events.EventAll

	// 流式增量事件（token 级）：核心 process() 流式化后每收到一个增量块发布。
	// 客户端可选订做真逐 token 渲染；聚合事件仍照常发布，旧订阅者不受影响。
	EventReasoningDelta = events.EventReasoningDelta
	EventContentDelta   = events.EventContentDelta

	// EventMemoryAccess 是图记忆的真实读写事件（哪个块被读/写/删/合）。
	//
	// 星图靠它做「反馈」：此前只能拿 tool_call 的工具名猜节点，而工具名是英文、
	// 实体名是中文概念（实测命中 0），只能退化成点亮一批固定节点。
	EventMemoryAccess = events.EventMemoryAccess
)
