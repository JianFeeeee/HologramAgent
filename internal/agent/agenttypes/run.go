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
	"context"
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

// OverflowKind 是超页处置的**触发形态**。
//
// ★ 为什么需要它（用户 2026-11-18：「overflow 应该是一个预定义的钩子，
// 各个子 agent 注册这个钩子来定义各自的 overflow 行为」）：
//
// 同一个钩子会被**两个不同时机**调用，而两者的正确处置不同：
//
//	OverflowLocal   发请求前的水位判定 / 收到上游 context_full 之后
//	                ⇒ 策略裁剪型发空中断，让裁剪在任务边界发生
//	OverflowResume  被挂起任务恢复、前缀被重建之后
//	                ⇒ **必须保护本帧自己的现场**（f.Msgs[PrefixLen:]）后裁剪，
//	                  因为那批工具轮产物是"这个任务刚做完的事"，
//	                  归档掉它等于恢复出一个自相矛盾的任务
//
// 把"时机"作为参数传进钩子，比让基座写两套分支好：类型模块一处实现、
// 一处决策，基座只剩转发。
type OverflowKind int

const (
	// OverflowLocal 是常规入口（发请求前的水位判定 / 上游报错）。
	OverflowLocal OverflowKind = iota
	// OverflowResume 是任务恢复后的重新装配入口。
	OverflowResume

	// OverflowAir 是**类型自己投给自己的空中断**被调度时的入口。
	//
	// 与上面两者同一格钩子：主 agent 超页时投一条无正文自输入，
	// 该任务被调度时回到这里 —— 于是"抢占"与"裁剪"仍是同一条策略的两步，
	// 而不是基座替它发中断（用户裁定：「空中断由内核发是妥协」）。
	OverflowAir
)

// OverflowRequest 是一次超页处置请求（钩子的入参）。
type OverflowRequest struct {
	// Kind 说明这是哪个时机触发的。
	Kind OverflowKind
	// Query 是相关性基准。**由基座推导、类型决定是否采信**：
	// OverflowLocal 时调用方给的可能是空串（由策略自己用最后一条用户输入兜底）；
	// OverflowResume 时基座已按"本帧最新上下文"推导好。
	Query string
	// Force 表示上游已**明确**报过 context_full（判定已被外部短路）。
	Force bool
}

// ToolCall 是 model 的工具调用值（别名：agent/api 与 model 同源）。
type ToolCall = agentAPI.ToolCall

// BatchOpts 是一批工具执行的可变参数（声明点在这里：类型模块要传它）。
type BatchOpts struct {
	// Ctx 是本次批量的执行作用域（rt 传整轮 roundCtx，其余传自身 ctx）。
	//
	// BatchOpts is the request options passed to the kernel tool boundary.
	// 各类型注册的实现必须能接受"这一批属于哪个作用域"——rt 的取消域是
	// 整轮，主 agent 是自身 ctx。让 runner 自己猜（读 a.ctx）会让 rt 的
	// 取消域失效（实测会退化成"打断不了"）。
	Ctx context.Context
	// Channel 是本次批量要回的通道（进 StageContext.Extra，插件据此判权限）。
	Channel string
	// Scenes 是本轮的场景键（rt / 工作式子传 nil）。
	Scenes []string
	// StageCtx 是基底 StageContext；内部按 idx 拆分（Extra 逐份复制）。
	StageCtx *sdk.StageContext
	// NoBatch 为 true 时强制串行（工作式子历史上是逐条串行）。
	NoBatch bool
	// ToolFilter 可在执行前改写/拒绝单个工具（工作式子用它禁输出工具）。
	//
	// 返回非空字符串表示"拒绝并以此作为结果"，不回落到真实执行。
	ToolFilter func(tc ToolCall) string
}

// BatchResult 是单条工具的执行结果（与入参**同序**返回）。
type BatchResult struct {
	CallID string
	Name   string
	Text   string
	Raw    interface{}
	Denied bool
}
