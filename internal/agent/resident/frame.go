package resident

// frame.go —— **驻留子自己的**任务帧（docs §0.3 铁律）。
//
// ★ 原先用 `agenttypes.Frame`（公共值层）。按「agent 间是隔离实例，
//   任何情况不允许共享」，帧归**用它的那个类型**：
//   主 agent 有 `mainagent.TaskFrame`，驻留子有本文件这份。
//   两份今天字段相同 —— 那是现状；将来分叉时各改各的。

import (
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	rctx "github.com/JianFeeeee/HologramAgent/internal/agent/resident/ctx"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// ToolResultItem 是进上下文的那份工具结果（声明点在本包 ctx）。
type ToolResultItem = rctx.ToolResultItem

type ResidentFrame struct {
	Input    string
	StageCtx *sdk.StageContext

	// 跨轮次状态
	Msgs        []agentAPI.Message
	Tools       []interface{}
	ToolsUsed   []string
	ToolResults []ToolResultItem
	// ToolCalls 是每一次工具调用的完整记录（含参数），与 ToolResults 通过
	// CallID 配对；两者同步追加，不各写一份。
	ToolCalls          []ToolCallItem
	Turn               int
	LastBatchReplyOnly bool

	// OverflowRecover 是本帧内的超页恢复次数（按帧而非按 agent 计：
	// 要防的是"一次裁剪没搞定 ⇒ 裁剪→复原→又超页→再裁剪"这个**单帧内**的循环）。
	OverflowRecover int

	// EventID 是因果链标识（中断/转投要把来源带过去）。
	EventID string

	// 本批工具的执行状态（串行路径用）
	PendingTools []agentAPI.ToolCall
	ToolIdx      int
	// ReplyOnly 表示本批只回一句话（不再调工具）。
	ReplyOnly bool
	// ContentOnce 让"第一段正文"只发一次（避免重复刷标题）。
	ContentOnce bool

	CurTool       agentAPI.ToolCall
	CurToolPlugin string
	CurResult     string
	CurRaw        interface{}

	// ToolCtxs 是**每个工具各一份**的 StageContext（阶段 2c）。
	//
	// 为何必须拆：原先 f.StageCtx 是单槽，批内每个工具都覆写；并行下
	// N 个 goroutine 同写一个 ctx = 数据竞争，且 after_toolcall 可能读到
	// **别的工具**的结果。拆分后每个工具只写自己那份。
	ToolCtxs []sdk.StageContext

	// OversizeTools / OversizeToolNames 记录本任务「过大工具结果」的次数与工具名。
	// ⚠️ 结果**未被裁剪**（方案 B），这里只是记账。
	OversizeTools     int
	OversizeToolNames []string

	// AssistantMsgIdx 是本批 assistant(tool_calls) 消息在 Msgs 中的下标，
	// -1 表示尚未写入。
	AssistantMsgIdx int

	// BatchAttachments / BatchRecall 是本批**积压**的附加消息，只在整批
	// tool 消息全部落完之后才追加（否则上游会 400：assistant(tool_calls)
	// 之后必须紧接 n 条 role=tool）。
	BatchAttachments []agentAPI.Message
	BatchRecall      string

	// Resp 是最近一次带工具调用的 LLM 响应（纯文本收尾那轮不写，见 TurnReasoning）。
	Resp *agentAPI.CompletionResponse
	// TurnReasoning 是本次任务里**最后一次** LLM 响应的思考内容。
	// 不能靠 Resp.ReasoningContent：纯文本收尾那轮的 Resp 是 nil 或旧响应，
	// 拿它写 L1 会把上一批的思维链冒名顶替成本轮的。
	TurnReasoning string
	// Scene 是本轮**涌现**出来的场景键；SceneDone 标记是否已解析（一轮只解析一次）。
	Scene     string
	TurnScene model.TurnScene
	SceneDone bool

	Step     Step
	Response string
	Err      error
	// turnUsage 是本任务内全部 LLM 调用的用量合计。
	TurnUsage agentAPI.TokenUsage

	// Evt / CleanInput / IsInterrupt / StartedAt / Terminal / OutputChannel
	Evt           *agentIO.InputEvent
	CleanInput    string
	IsInterrupt   bool
	StartedAt     time.Time
	Terminal      Terminal
	OutputChannel string
	// PrefixLen 是被挂起任务的前缀长度（恢复时用于重定基）。
	PrefixLen int
	// InputBlocks 是本轮输入携带的多模态块；重建前缀时要重新挂回。
	InputBlocks []agentAPI.ContentBlock
}

// ToolCallItem 是一次工具调用的完整记录（声明点在 model）。
type ToolCallItem = model.ToolCallItem

// Terminal 是本类型任务的终态种类。
type Terminal int

const (
	TerminalNone Terminal = iota
	TerminalOK
	TerminalError
	TerminalStageShortCircuit
	TerminalSkipped
	TerminalConsolidation
	TerminalSuspended
)

// Step 是**驻留子自己**状态机的游标。
//
// 具体有哪些 step、怎么转移，是**本类型**的 agentloop 决定的事。
type Step int

const (
	// StepPrepare 构建消息与工具表、应用中断标记、跑 pre_action 阶段。
	StepPrepare Step = iota
	// StepLLM 轮次顶部（中断/占位）+ LLM 调用 + post_action。
	StepLLM
	// StepToolBatch 并发执行整批工具（仅当全批可并发时）。
	StepToolBatch
	// StepToolBegin 取本批下一个工具，跑 before_toolcall。
	StepToolBegin
	// StepToolExec 执行工具（**临界区**：副作用不可回滚）。
	StepToolExec
	// StepToolAfter after_toolcall、裁剪、消息与事件组装。
	StepToolAfter
	// StepTurnEnd 收尾本批并进入下一轮。
	StepTurnEnd
)

// Outcome 是**驻留子自己**任一 step 的终态。
type Outcome int

const (
	// OutcomeContinue 继续执行下一个 step（游标可能停在原地以表达"重跑"）。
	OutcomeContinue Outcome = iota
	// OutcomeDone 任务成功结束，响应在帧里。
	OutcomeDone
	// OutcomeFailed 任务失败结束，错误在帧里。
	OutcomeFailed
	// OutcomeSuspended 任务在安全点被抢占挂起，帧已保存。
	OutcomeSuspended
)

// 本包内部短名（既有调用点逐字不变）。
const (
	stepPrepare   = StepPrepare
	stepLLM       = StepLLM
	stepToolBatch = StepToolBatch
	stepToolBegin = StepToolBegin
	stepToolExec  = StepToolExec
	stepToolAfter = StepToolAfter
	stepTurnEnd   = StepTurnEnd

	outcomeContinue  = OutcomeContinue
	outcomeDone      = OutcomeDone
	outcomeFailed    = OutcomeFailed
	outcomeSuspended = OutcomeSuspended
)
