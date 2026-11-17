package agenttypes

// run.go —— 一次任务的**值类型契约**（供 kit / base / 各类型模块共享）。
//
// ## 为什么 TaskFrame 必须上移到这一层
//
// 它是 `Hooks.Loop` 的第二个参数。类型模块要实现这个槽位，就必须能命名
// `*TaskFrame`；而它原先声明在 agent 包里 —— 于是"外部包实现 Loop"在
// 类型层面不成立（拆包的第一道墙，与 Step/Outcome 同一原因）。
//
// ## 它是什么
//
// 纯数据：一帧任务在**安全点之间**必须存活的所有状态。
// 不变量：帧不持有任何锁或资源跨越安全点（见 docs/zh/input-scheduler-design.md §8.1）。
//
// ## 这里的字段只增不减的纪律
//
// 它是**跨模块的数据契约**：字段一旦公开就会被各类型的 loop 读写。
// 想删字段要先确认没有类型模块在读它 —— 这也是为什么搬运时一个字段都没动。

import (
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// ToolResultItem 是一条工具结果（进上下文的那一份）。
type ToolResultItem struct {
	// CallID 回指 ToolCall.ID，是 tool_call 与 tool_result 的配对键。
	// 空值表示旧数据（未记录 CallID），渲染时按位置配对（既有行为）。
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name"`
	Output string `json:"output"`
}

// ToolCallItem 是一次工具调用的完整记录（含参数）。
// ToolCallItem 是 model.ToolCallItem 的别名（同一数据在上下文/冷文档两侧使用）。
type ToolCallItem = model.ToolCallItem

// Terminal 是任务的终态种类（每个任务恰有一个终态）。
type Terminal int

const (
	// TerminalNone 任务尚未结束，需进入 run 段。
	TerminalNone Terminal = iota
	// TerminalOK 正常完成（已提交上下文并回执）。
	TerminalOK
	// TerminalError 执行出错（已提交错误响应）。
	TerminalError
	// TerminalStageShortCircuit 被 on_input 阶段短路（响应已发出）。
	TerminalStageShortCircuit
	// TerminalSkipped 未进入执行：解析失败或被去重。
	TerminalSkipped
	// TerminalConsolidation 走记忆整理专用路径，已处理完毕。
	TerminalConsolidation
	// TerminalSuspended 被抢占挂起，等待恢复。
	TerminalSuspended
)

// Frame 承载一个任务在安全点之间必须存活的所有状态。
//
// ★ 字段与搬运前的 `agent.TaskFrame` **逐字一致**（一个都没动、一个都没删）：
// 它是跨模块数据契约，改字段要同时确认各类型的 loop。
type Frame struct {
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
	TurnScene memory.TurnScene
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
