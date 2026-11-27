package sdk

// agenttype.go —— **子 agent 类型注册**的中立契约（内置插件用）。
//
// ## 用户裁定（2026-11-19）
//
// > 「注意 sdk 暴露出去的是抽象接口即插件只是把对应的回调与每个事件对应的
// >    处理函数注册进去作为一个参数结构体传给 sdk 接口，由 plghost 通过
// >    eventbus 推到内核，内核再按照契约组装，而不是直接引用 agent 包」
//
// > 「内置插件通过 sdk 接口可以注册自己插件对应的特化 agentloop 与相关处理」
//
// > 「主 agent 是可被替换的它和子 agent 在界面层上看并无不同，只不过内核
// >    按照配置选择它作为主 agent 拉起，并将通道默认划分给它」
//
// ## 分层
//
//	插件          只填 AgentTypeDef（全是中立类型 + 回调）
//	  ↓  s.AgentTypes().Register(def)
//	plghost       打包成事件载荷（真 Go 函数值，同进程才能带）
//	  ↓  EventBus "agent.type.register"
//	security      事件策略闸门（只有内置插件能发）
//	  ↓
//	内核          按 Def.Spec 组装成 registry.Descriptor
//	  ↓
//	registry      唯一那张类型表 → admin_child / 实例管理器 / 持久化
//
// ★ **插件不认识 `*agent.Agent` / `*agent.TaskFrame`**：那两个类型是内核实现。
//   插件看到的是本文件的中立接口（AgentRuntime / AgentFrame）。
//   适配在内核侧做一次（`kernelmod/agenttype`），不在插件里。

// ───────────────────────── 注册面 ─────────────────────────

// AgentTypeRegistrar 是插件侧的注册口（由内核注入到 PluginSDK）。
//
// 返回 error = 拒绝（同名已注册 / 不是内置插件 / 声明不完整）。
// **不静默成功**：静默成功会让"插件以为注册了、实际没有"变成查不出的谜。
type AgentTypeRegistrar func(def AgentTypeDef) error

// AgentTypeUnregistrar 撤销一个类型（仍有存活实例 ⇒ 拒绝）。
type AgentTypeUnregistrar func(typeName string) error

// ───────────────────────── 声明面 ─────────────────────────

// AgentTypeDef 是一个子 agent 类型的**完整声明**。
//
// 插件填它、交给 SDK，之后什么都不用管：实例的构造、生命周期、
// 调度器、消息落库全由内核按 `Spec` 组装。
type AgentTypeDef struct {
	// Type 是类型名（唯一键，也是 `admin_child` 的 `type` 参数取值）。
	Type string
	// Description 是一句话说明（进 admin_child 的 type 枚举）。
	Description string

	// NeedsInputCh / NeedsOutputCh 声明创建时的通道要求。
	//
	//	驻留子  至少一个 inputch（不划通道它收不到任何输入）
	//	rt      inputch + outputch **成对**
	//	worker  两个都不要（一次性执行体）
	NeedsInputCh  bool
	NeedsOutputCh bool

	// Actions 是类型独有的**管理动作**（`admin_child(type, action)` 用）。
	//
	// ★ 通用动作（list/create/inspect/send/destroy/restart/destroy_all）
	//   由内核统一实现，**不要**在这里声明。
	Actions []AgentTypeAction

	// ── 三个回调：插件的实现面（全是中立签名）──
	Loop  AgentLoopFunc
	Admin AgentAdminFunc
	Help  AgentHelpFunc

	// Spec 声明"这类 agent 用哪些零件"——内核按它组装，插件不碰构造细节。
	Spec AgentTypeSpec
}

// AgentTypeAction 是一个管理动作的声明（渲染 schema + 分发用）。
type AgentTypeAction struct {
	Name        string
	Description string
	// Args 是 JSON Schema properties（可为空）。
	Args map[string]interface{}
	// Required 是必填参数名。
	Required []string
}

// AgentTypeSpec 声明这类 agent 的**零件组合**。
//
// ★ 为什么是"声明"而不是"插件自己 new"：造实例要碰 `IO` / `GraphOpener` /
//
//	`Security` / `child.Registry` —— 那些是内核实现。插件只声明要什么，
//	内核负责按声明装好。
type AgentTypeSpec struct {
	// Scheduler 声明输入调度形态。
	Scheduler SchedulerKind
	// Context 声明上下文策略。
	Context ContextKind
	// Memory 声明记忆形态。
	Memory MemoryKind
	// ToolSide 声明工具可见侧别（"parent" / "child"，空 = "parent"）。
	ToolSide string
	// DenyTools 是**额外禁止**的工具（前缀匹配，如 "output_send__"）。
	DenyTools []string
	// Capabilities 声明本类型**能承接的通道能力**（如 "text"、"rt"、"audio"）。
	//
	// ★ 它是**中立字符串**（插件不依赖内核的位类型），内核按名字映射到位；
	//   空 = 只要求 "text"（所有类型的基础能力）。
	//   能力面决定"哪些通道能划进来"—— 与权限面（AllowedOutputs）是两件事。
	Capabilities []string
}

// SchedulerKind 是输入调度形态。
type SchedulerKind string

const (
	// SchedulerNone：无队列，单槽覆盖（最新者胜）。rt 用这个。
	SchedulerNone SchedulerKind = "none"
	// SchedulerStandard：标准队列 + 四级抢占 + 挂起恢复。主 / 驻留用这个。
	SchedulerStandard SchedulerKind = "standard"
)

// ContextKind 是上下文策略形态。
type ContextKind string

const (
	// ContextNone：不持有上下文（一次性执行体，worker 用这个）。
	ContextNone ContextKind = "none"
	// ContextWindow：整窗不裁，撞窗口上报父（驻留 / rt 用这个）。
	ContextWindow ContextKind = "window"
	// ContextRelevance：按相关性裁剪（主 agent 用这个）。
	ContextRelevance ContextKind = "relevance"
)

// MemoryKind 是记忆形态。
type MemoryKind string

const (
	// MemoryNone：没有记忆（worker）。
	MemoryNone MemoryKind = "none"
	// MemoryLight：轻量记忆（受限只读主库 + 自己的 temp 实例）。
	MemoryLight MemoryKind = "light"
	// MemoryFull：完整图记忆（主 agent）。
	MemoryFull MemoryKind = "full"
)

// ───────────────────────── 回调签名（中立，不含内核类型）─────────────────────────

// AgentLoopFunc 驱动**一帧**任务到终态。
//
// 内核每帧调一次；返回 `AgentOutcomeContinue` 表示"还要下一帧"，
// `AgentOutcomeDone` / `AgentOutcomeSuspended` 表示本帧结束。
//
// ★ 这是**插件特化 agentloop**的落点：它决定这类 agent 怎么思考
//
//	（怎么拼提示词、什么时候停、工具结果怎么处理）。
//	它**不**决定"发几条 LLM 调用"——那是内核并发机制（见 target-architecture §5）。
type AgentLoopFunc func(rt AgentRuntime, fr AgentFrame) AgentOutcome

// AgentAdminFunc 处理**类型独有**的管理动作。
//
// 通用动作（list/create/inspect/send/destroy/restart）由内核处理，不会到这里。
type AgentAdminFunc func(rt AgentRuntime, action string, arg map[string]interface{}) (string, error)

// AgentHelpFunc 返回 `admin_child_help(type)` 的正文。
type AgentHelpFunc func(rt AgentRuntime) string

// ───────────────────────── 消息与 LLM 的中立面 ─────────────────────────

// Message 是一条对话消息（openai-compatible 形态）。
//
// ★ 与内核的 `agentAPI.Message` 同构但不共享类型：插件不认识内核实现。
//
//	适配在内核侧（`kernelmod/agenttype`）做一次字段拷贝。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls 只在 role=assistant 且本轮要调工具时非空。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 只在 role=tool 时非空（对应哪个调用的结果）。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// CompletionRequest 是一次 LLM 调用的请求（中立面）。
type CompletionRequest struct {
	Messages  []Message
	MaxTokens int
	// ToolChoice 是 "auto" / "none" / 具体工具名。
	ToolChoice string
	// DisableThinking 让上游不输出 reasoning（默认跟随实例配置）。
	DisableThinking bool
}

// CompletionResponse 是一次 LLM 调用的响应（中立面）。
type CompletionResponse struct {
	Content      string
	Reasoning    string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        map[string]int
}

// ───────────────────────── 运行时视图（插件能碰的东西）─────────────────────────

// AgentRuntime 是插件在回调里能碰到的**全部内核能力**。
//
// ★ 刻意小：只有 LLM / 工具 / 事件 / 日志四类。插件不该碰 IO 通道、
//
//	实例管理器、安全闸的原始策略 —— 那些归内核。
type AgentRuntime interface {
	// ID 返回本实例的标识。
	ID() string
	// IsChild 报告本实例是否为子 agent。
	IsChild() bool

	// Complete 跑一次**阻塞**的 LLM 调用（插件等它回来）。
	//
	// ★ 不做并发、不做重试、不做 fallback：那些在内核的 LLM 设施里，
	//   agentloop 是阻塞流水线（target-architecture §5.2）。
	Complete(req *CompletionRequest) (*CompletionResponse, error)

	// RunTools 提交一批工具调用，拿回**同序**结果。
	//
	// 结果是 `ToolResult`（含 Text/Raw/Denied + 工具声明），
	// 工具声明里的标记（NoMemory 等）由插件的 Loop 自己消费。
	RunTools(calls []ToolCall) []ToolResult

	// Publish 发一条内核事件（订阅方是 cli / webui 等）。
	Publish(eventType string, payload map[string]interface{})
	// Logf 记日志（带本实例的身份前缀）。
	Logf(format string, args ...interface{})
}

// AgentFrame 是一帧任务的**中立视图**。
//
// ★ 只暴露线性流程要用的东西：输入、消息、当前工具批、步进。
//
//	主 agent 那种 7 阶段可抢占状态机是**它自己的复杂度**，不外包给插件；
//	插件要更复杂的行为，改 `Spec`（内核组装），而不是在 Loop 里重写内核机制。
type AgentFrame interface {
	// Input 是本帧的输入文本。
	Input() string
	// Messages 返回当前消息序列的**副本**（插件改动无效，用 AppendMessage）。
	Messages() []Message
	// AppendMessage 追加一条消息。
	AppendMessage(m Message)
	// SetResponse 设置本帧要回给通道的文本。
	SetResponse(text string)
	// Response 返回已设置的回复（空 = 未设置）。
	Response() string

	// PendingTools 返回本帧待执行的工具调用（内核填好，插件只读）。
	PendingTools() []ToolCall
	// AppendToolResult 把一条工具结果并入上下文（内核负责落消息）。
	AppendToolResult(callID, name, text string)

	// Turn 返回本帧已用的轮次（从 0 起）。
	Turn() int
	// SetTurn 设置轮次（插件自增）。
	SetTurn(n int)

	// Marks 返回本帧输入携带的**原始标记**（`priority`/`stop`/`interrupt`/…）。
	//
	// ★ 这是**数据透传**，不是语义：内核不解释任何键，插件在自己的 Loop 里
	//   决定怎么用（例如 `priority=="L4"` 表示"本插件希望立即打断"，
	//   而"立即打断"具体是丢弃当前轮还是别的，由插件自己的 Loop 定义）。
	//   返回副本，插件改动无效。
	Marks() map[string]interface{}
}

// AgentOutcome 是一次 Loop 调用的结果。
type AgentOutcome string

const (
	// AgentOutcomeDone 本帧正常结束。
	AgentOutcomeDone AgentOutcome = "done"
	// AgentOutcomeContinue 还要下一帧（还有工具要跑 / 还没说完）。
	AgentOutcomeContinue AgentOutcome = "continue"
	// AgentOutcomeSuspended 本帧被抢占，现场已保存（内核负责栈）。
	AgentOutcomeSuspended AgentOutcome = "suspended"
	// AgentOutcomeFailed 本帧失败（错误信息在 Frame 里）。
	AgentOutcomeFailed AgentOutcome = "failed"
)
