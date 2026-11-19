package agent

// aliases.go —— 基座侧的**值类型别名**（声明点统一在 agenttypes / model）。
//
// 拆包后各类型模块都要命名这些类型，所以声明点上移；本包做别名，
// 既有调用点逐字不变（类型恒等）。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// channelConsolidation 见 agenttypes.ChannelConsolidation。
const channelConsolidation = agenttypes.ChannelConsolidation

type (
	// 上下文（L1）与工具结果的纯数据。
	ContextEvent    = agenttypes.ContextEvent
	CompactPlan     = agenttypes.CompactPlan
	ColdRecallPlan  = agenttypes.ColdRecallPlan
	ToolResultStore = agenttypes.ToolResultStore
	ChildTaskState  = agenttypes.ChildTaskState
	ToolResultItem  = agenttypes.ToolResultItem
	ToolCallItem    = agenttypes.ToolCallItem
	_               = agenttypes.SelfInputMsg
	MemoryBlock     = model.MemoryBlock

	// 任务终态与自循环输入。
	TaskTerminal = agenttypes.Terminal
	Outcome      = agenttypes.Outcome
	SelfInputMsg = agenttypes.SelfInputMsg

	// selfInputMsg 是包内别名（既有调用点逐字不变）。
	selfInputMsg = agenttypes.SelfInputMsg
)

const (
	OutcomeContinue  = agenttypes.OutcomeContinue
	OutcomeDone      = agenttypes.OutcomeDone
	OutcomeFailed    = agenttypes.OutcomeFailed
	OutcomeSuspended = agenttypes.OutcomeSuspended

	TerminalNone              = agenttypes.TerminalNone
	TerminalOK                = agenttypes.TerminalOK
	TerminalError             = agenttypes.TerminalError
	TerminalStageShortCircuit = agenttypes.TerminalStageShortCircuit
	TerminalSkipped           = agenttypes.TerminalSkipped
	TerminalConsolidation     = agenttypes.TerminalConsolidation
	TerminalSuspended         = agenttypes.TerminalSuspended
)
