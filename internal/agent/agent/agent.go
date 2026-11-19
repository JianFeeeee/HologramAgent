package agent

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"

	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent/child"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	agentProvider "github.com/JianFeeeee/HologramAgent/internal/agent/provider"
	"github.com/JianFeeeee/HologramAgent/internal/artifacts"
	"github.com/JianFeeeee/HologramAgent/internal/kernelloop"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// ContextEvent 和 RelevanceContext 定义在 context.go

// Agent — 单 agent，不区分会话/实例
//
// 并发现状（M3a 起）：所有任务状态只由 **schedulerLoop goroutine** 独占读写，
// 因此不再有保护整轮执行的互斥量——挂起不能持锁（见 docs/zh/input-scheduler-design.md §8.1 I3）。
// 仍需跨 goroutine 保护的是：childMu/llmMu/lastInputMu/noMergeMu 与各子系统自己的锁；
// interceptLoop 只允许触碰 preemptionRequest 与 cancelLLM（经 llmMu）。
type Agent struct {
	// logIdentity 提供日志身份标签（`[main]` / `[resident:<id>]` / `[rt:<id>]`）。
	//
	// ★ 主 agent 没有 child.Child（它是根），但工具执行日志是**共用代码路径**
	// （toolcall.go 的 executeToolCallOutcome 三条路都经过它）。标签推导收在
	// child.LogIdentity 里，这里嵌入一份，让主 agent 与子 agent 用同一个入口。
	logIdentity child.LogIdentity

	id              types.AgentID
	provider        agentAPI.Provider
	providerManager *agentProvider.ProviderManager
	// preferredProvider 是本 agent 优先使用的模型源名（"" = 总默认源）。
	// 语义见 AgentConfig.PreferredProvider：优先而非独占。
	preferredProvider string
	io                *agentIO.IOManager
	// graph 是本 agent 的**图记忆共同面**（根 = 同一个 GraphDB；驻留子 = LightMemory）。
	// 整理面仍走 memory 字段（子为 nil ⇒ 既有的 nil 关卡自动禁用整理面）。
	graph GraphMemory

	// kernelSource/parentID/taskPrompt/dataDir：驻留子相关的层级信息（见 AgentConfig）。
	kernelSource string
	parentID     string
	taskPrompt   string
	dataDir      string

	// ★ 原先是 `isRealtime` 字段 → `hooks.IsRealtime` 槽位 → 现在两者都没有：
	// rt 的身份由它自己的包知道（它注册自己的 Loop 与槽位），
	// 基座不留标记，更不按标记分支。
	//
	// 与通道侧的 CapRT 仍是两个不同结构：

	// children 是**全部类型**的子 agent 的唯一登记表（重构 R1 起）。
	//
	// ## 为什么只有一张表
	//
	// 此前 residents 与 rtChildren 各自一张表 + 一把锁，每加一种类型就
	// 多一张表、一把锁、一段路由分支。实测新增一种子 agent 要改 18 个生产
	// 文件，并已因复制而漂移过一次（StopRTChildren 漏注销入站登记）。
	//
	// 现在类型差异下沉到 child.Entry / 各类型的 Dispose，骨架
	// （单数/复数停止、撤销策略、注销入站）只写一遍。
	children child.Registry

	// rt 的实例登记已并入 children（统一登记表）。
	//
	// 这里只剩**已回收实例的上下文留档**：它不是登记表的一部分
	// （实例已经不在了），而是"推信号 + 拉状态"交接协议的拉侧数据源。
	//
	// ★ 键是调用方（模型）给的 id，所以同名复用会撞键。串线的真因在**读侧**
	// （RTTranscript 曾"先查留档、命中即返回"），现在按"在跑实例优先"。
	// 刻意**不**在 Spawn 时删留档：提前删掉等于让"推了信号却拉不到正文"。
	// sub 是**类型分区表**（见 agentstate.go）：rt / 驻留子 / 工作式子
	// 各自的运行状态按格存放，基座只留这一个字段。
	sub *subStates

	// impl 是**类型实现体的回指**（用户裁定 2026-11-17：完整模块化）。
	//
	// 基座只存 any，不认识它是谁 —— 于是主 agent 的全部实现（记忆整理/媒体/
	// 插件/工具族）都可以搬出基座包，基座也就不再 import 那 16 个实现包。
	impl any

	// callParent 是**父为子安装**的上行投递函数（子 → 父的唯一通路）。
	//
	// ★ 由父在创建子时装入（InstallCallParent），因为只有父知道自己的
	//   IOManager 与那条入站 inputch 名。子若自己拼，历史上出过两次故障：
	//   rt 拼成 "rt/"+id 导致路由永远查不到；子在自己的 io 上注入导致
	//   消息进子自己的队列（父收不到）。
	callParent CallParentFunc

	// instances 是**内核侧的实例管理器**（管理本内核下所有子 agent）。
	//
	// ★ 挂在根 agent 上，但语义上不属于它：创建/销毁/注入/恢复都是
	//   实例管理，主 agent 只是调用方（用户裁定 2026-11-18：
	//   「创建子 agent 执行在主 agent？不应该在内核吗？」）。
	instances *InstanceManager

	// cfg 是**归一化后的**构造参数。类型模块的初始化回调要用它
	// （例如归档间隔 ≤0 时继承蒸馏间隔 —— 归一化只该有一处）。
	cfg AgentConfig
	// contextMgr 是**类型的**上下文实现（§6.2 #1：基座零持有实现体）。
	//
	// ★ 这不是"基座持有实现"：它是装配期一次性写入的**句柄**，语义上属于
	//   类型（主 agent 在 Initialize 里 new 出来挂进来）。基座的代码只读它的
	//   能力面（条数/落盘/工具结果），不参与它的任何策略。
	ctxHolder *RelevanceContext

	// ★ context 实现不在这里：它是**主 agent 的类型私有实现**（§6.2 #1）。
	// 基座通过 Hooks.ContextManager 取它（类型在 Initialize 里装上自己的那份）。
	systemPrompt string
	ctx          context.Context
	cancel       context.CancelFunc

	// 文档记忆（第二层）

	// 知识库

	// 人物特质与关系网

	// 文本记忆（原始对话日志）

	// 媒体存储句柄已随 CAS 淘汰（块自带负载）。

	// 人格设定（内容来自启动时载入的人格文件/配置项）

	// 人格落库面：首启门禁与 persona_set 工具使用（见 persona.go）。
	// 为 nil 时门禁与工具都静默关闭（例如单测里不接配置的场景）。

	// 被授权的输出通道集合（空 = 完整授权，见 AgentConfig.AllowedOutputs）。
	allowedOutputs []string

	// toolAllow / toolDeny 是**父为本 agent 声明的工具白/黑名单**。
	//
	// ★ 从 `residentChild` 提到基座（用户裁定 2026-11-18）：
	//   **渲染**发生在 `*Agent` 上（工具表按 agent 生成），而子的载体原先
	//   拿不到自己的白名单 ⇒ 模型看得见父禁用的工具，执行时才被拒。
	//
	// 语义（与安全模块的闸门一致）：deny 优先；allow 非空 ⇒ 只允许命中项；
	// 两者都空 ⇒ 不额外限制（沿用全局策略）。
	toolAllow []string
	toolDeny  []string
	// toolAllowSet / toolDenySet 是切分后的匹配形式（支持 "cmd_*" 通配）。
	toolAllowSet []string
	toolDenySet  []string

	// 插件注册表（用于 plgreload）

	// 定期心跳蒸馏

	// 三个独立心跳任务间隔

	// 上下文裁剪：活跃上下文最大条数，超出按相关性裁剪

	// ctxTuning 是上下文预算的可调参数（零值 = 历史默认）。
	//
	// 从配置读入（core.agent.context.*），使不同窗口的实例可以各自调优，
	// 而不是所有实例共用一套写死的 0.8 / 600000 / 1:3。

	// 当前请求的输出通道（mutex 保护，process() 内独占）

	// 阶段管道：插件消息流编辑
	// ★ 原先这里是 `eventBus *events.Bus`（裸总线）。
	//
	// 用户裁定（2026-11-17）：「事件总线是被安全模块代理的吧，原始总线只被
	// 安全模块持有」。⇒ Agent 不再持有裸总线，只持有一个**发布代理**
	// （见 agent_eventbus.go）：带本 agent 身份、过安全闸门、只能 Publish。
	publisher agentEventBus

	// 自循环输入通道：核心内部任务（记忆消歧、系统维护、子 Agent 通知），
	// 不经过 IO 层。每条消息携带目标输出通道：
	//   "_consolidation_" = 记忆整理（无记忆路径，不写入上下文、不 emit 响应）
	//   其他 = 正常处理（写入上下文、emit 响应到该通道）
	selfInputCh chan selfInputMsg

	// 输入调度器：就绪队列、任务抽象与快照（见 scheduler.go）。
	// M2 起取代 eventLoop 的隐式 channel 排队。
	// sched 见 Agent 的构造：主 agent 与驻留子各自一份 *standard.Scheduler。
	sched *Scheduler

	// 工具轮次硬上限（0 = 不限）；见 AgentConfig.MaxToolTurns。
	maxToolTurns int
	// offload 是积压任务自动转投的参数（见 offload.go）。

	// 进行中的 LLM 请求取消函数，interceptLoop 可调用以在请求中打断
	llm         kernelapi.LLMClient
	extractor   artifacts.Extractor
	norm        artifacts.Norm
	memOps      kernelapi.MemoryOps
	graphOpener kernelapi.GraphMemoryOpener
	cancelLLM   context.CancelFunc
	llmMu       sync.Mutex

	// 进行中的**工具调用**取消函数集合（2026-10-31 加）。
	//
	// ❗为何需要它：`cancelCurrentLLM` 只能停 LLM 流。而工具可能是长阻塞的
	// （实测：`browser_start` 被模型要求 30m、被硬上限夹到 10m）。
	// 那种情况下用户按「停止」什么都不发生 —— 日志里只有
	// `stop requested ... (queued=1 will be short-circuited at pre-action)`，
	// 而当前轮仍卡在工具里直到自己超时。
	//
	// ❗为何是**集合**而不是单个槽：同一轮可并发执行多个工具
	// （批内全部声明 ParallelSafe 时），单槽会互相覆盖，且先完成的那个
	// 会把别人的取消函数清掉。
	//
	// ❗为何只给「停止」用、不给中断抢占用：工具执行期间不取消是用户
	// 裁定（R5）—— 副作用不可回滚，中途掤断会把「决定执行但未执行」
	// 变成尴尬状态。所以 `cancelRunningTools` 只在 stop 分支调，
	// 不在 `requestPreempt` 分支调（见 eventloop.go）。
	cancelTools map[uint64]context.CancelFunc
	toolSeq     uint64
	toolMu      sync.Mutex

	// 模型思考模式（thinking/reasoning）

	// 启动时间

	// 当前轮次的非文本媒体数据（图片/音频），供 describe_image 等工具访问

	// pendingMediaBlocks 累积本轮已捕获的媒体块。
	//

	// 当前输入是否为工具提醒/中断（以 system 角色注入，避免被当成用户消息）
	interruptInput bool

	// 非文本输入处理配置

	// noMergeMarkets 记录被标记"禁止合并"的实体对，key="entityA||entityB"（字典序），
	// 每次 reorgGraph 扫描到对应实体对时计数减一，归零后自动移除。

	// 输入去重：防 webui/GUI 断线重连导致的消息重放
	// key=source+"|"+content, value=上次接收时间；短窗口内同内容丢弃
	lastInput   map[string]time.Time
	lastInputMu sync.Mutex

	// 词嵌入模型，用于实体语义相似度计算

	// multimodalSpace 是统一多模态向量空间（可选）。实现可以是内嵌 ONNX，
	// 也可以是外部 API 客户端；两者共享同一套 L0/L2/L3 向量缓存与检索基础设施。

	// fusionCfg 控制文本路与视觉路的跨模态融合权重，可按模型实测结果配置。

	// device 是挂载上来的工具/通道数据面（kernelmod/device 产出）。
	// 类型是低层接口：core 不认识具体模块，避免 core ↔ kernelboot 成环。
	// deviceMu 只保护 device 字段（挂载是装配期动作，读取可能来自状态面）。
	// ★ 刻意不复用整轮执行的锁：Agent 按设计**没有**保护整轮的互斥量
	//（挂起不能持锁，见 input-scheduler-design §8.1 I3）。

	// builtinHandlers 是交给 device 路由层的族处理器适配器（装配期构造一次）。

	// securityScopeView / securityScopeOnce 是本 agent 的**策略执行视图**
	// （由 gate 按 agentID 持有，见 security_profile.go 的 securityScope）。
	//
	// ★ 为什么是 per-agent 而不是全局：轮次档位（sessionProfile）是该 agent
	// 独有的状态。放在共享单例上，主 agent 的 guest 会被驻留子的 owner 覆盖
	// —— 实测可复现（两者同进程并发）。
	// agents 是内核装配的 **agent 记录表**（可能为 nil ⇒ 独立使用）。
	agents kernelapi.AgentRegistry

	securityScopeView kernelapi.SecurityScope
	securityScopeOnce sync.Once

	// ★ 原先这里有 `security *security.Runtime` 字段，已删除。
	//
	// 用户裁定（2026-11-17）：「安全模块是**强制的透明层**」。

	// kit.Ledger 累计**跨调用**的 token 用量与缓存命中。
	//
	// 为何是 Agent 级而不是 TaskFrame 级：「这个会话花了多少、缓存省了多少」
	// 不是单次调用的属性，必须跨轮次、跨任务累积才有意义。
	// 单次用量一直在 StageCtx 与 LLM chain 事件里，缺的正是这个落点。
	usageLedger kit.Ledger

	// statusSecMu / statusSections 是**类型注册的状态段**（见 statussection.go）。
	//
	// ★ 为什么不是字段：`KernelStatus` 那 20 个字段里 9 个生产下恒空、
	//   3 个装的是类型专属结构。谁持有数据谁注册，基座只按 key 收集。
	statusSecMu    sync.RWMutex
	statusSections map[string]StatusSection

	// hooks 是**类型行为注册表**（用户裁定：各类型通过回调注册自己的行为）。
	//
	// ★ 基座**不认识自己是哪一类 agent**：它只调这些钩子。
	//   装配期由类型模块（main / resident / rtchild）通过 SetHooks 挂上；
	//   未挂必填槽位是装配错误（见 Hooks.Validate）。
	hooks Hooks

	// periodic 是**类型注册**的定时任务（用户裁定：基座暴露定时器回调接口，
	// 而不是在 Start 里硬编码 distill/archive/merge/review/offload 五个循环）。
	//
	// 装配期由类型模块调用 RegisterPeriodic 填充；Start 统一拉起。
	// 基座自己**不注册任何任务** —— 它不知道有谁会用它。
	periodic []PeriodicTask
}

type AgentConfig struct {
	ID              types.AgentID
	SystemPrompt    string
	Provider        agentAPI.Provider
	ProviderManager *agentProvider.ProviderManager
	// PreferredProvider 是本 agent **优先使用**的模型源名（空 = 用总默认源）。
	//
	// ## 为什么需要它（用户 2026-10-30 要求）
	//
	// 「要求支持为主 agent、子 agent、驻留式子 agent、rt agent 可各自配置模型源，
	//   **默认使用总模型源（default）**」
	//
	// 之前四类 agent 共用 providerManager 的同一套顺序：谁都没法单独指定。
	// 但它们的负载完全不同 —— 主 agent 要强推理、rtchild 要极低首字延迟、
	// 驻留子要长时间挂着省成本。用同一个源对三者都是妥协。
	//
	// ## 语义：**优先，不是独占**
	//
	// 它把该源排到候选链**最前面**，其余源仍作为回退（与原有多源回退语义一致）。
	// 硬锁定成唯一源会让"这个源挂了"直接变成"这个 agent 完全不工作"，
	// 而回退链正是本仓为此建立的机制。
	//
	// 名字不存在时**响亮回退到默认源并记日志**（不静默）：配置拼错要能看见。
	PreferredProvider string
	// LLM 是**内核提供的 LLM 设施**（§3.2.1）：重试 / fallback / 可用性标记 /
	// 活动源 / 分类错误全在它内部完成。
	//
	// ## 为什么基座只拿一个句柄
	//
	// 改造前基座自己写回退链（`callLLMWithFallback` 120 行）、自己重试、
	// 自己切活动源（`a.provider = fbProvider`）。于是：
	//   · 同一套语义在四个地方各写一遍（主 / rt / spawn / resident）
	//   · 基座知道"有两个源"这件事 —— 它本该只知道"我要一次补全"
	//   · "现在用哪个源"有两个来源（manager 的排序 + 基座字段）
	//
	// nil = 独立使用（单测）：基座回落到"单源直调 Provider"，
	// 行为与改造前一致（没有回退链可走）。
	LLM kernelapi.LLMClient
	// Extractor 是**三元组提取器**（契约在 internal/artifacts，实现在 nlp）。
	//
	// ★ 为什么经配置注入而不是让基座 import nlp：那是带 ONNX 的实现包
	//   （判据 §8 第 3 条）。基座只调 `Extract(text) []Triple` 一件。
	//
	// nil ⇒ 归档时跳过通用提取（只写文档元数据三元组）。
	Extractor artifacts.Extractor
	// Norm 是**归一化判定**（实体名能否落库 / 是否噪声，实现在 memory）。
	//
	// 零值语义是"放行"：未装配时多写几条噪声（可清理），而不是让归档
	// 一条关系都建不起来（静默失效）。
	Norm artifacts.Norm
	// DepParser 返回依存句法分析器的**只读事实**（状态面用）。
	//
	// ★ 与 Extractor 同理：实现是 `internal/nlp`（带 ONNX），基座只读事实。
	// nil ⇒ 状态面报"未启用"。
	DepParser func() (enabled bool, modelPath string)
	// MemoryOps 是记忆侧的**纯函数能力面**（契约在 kernelapi，实现在 memory）。
	//
	// ★ 为什么它必须存在：基座要用 CleanText / ExtractKeywords / MediaBlockID /
	//   SortMode / Situation，而那些函数今天住在 `internal/memory`
	//   —— 一个带 SQLite 的实现包（判据 §8 第 3 条）。
	//
	// nil ⇒ 各调用点按零值安全处理（见 memoryops_face.go）。
	MemoryOps kernelapi.MemoryOps
	// GraphOpener 是**图记忆的装配面**（关库/开轻量实例）。
	//
	// ★ 为什么它也必须注入：驻留子与 rt 要开"主库只读视图 + temp 实例"，
	//   而那是 `memory.OpenGraphDBReadOnly` / `memory.NewLightMemory`
	//   —— 实现构造（判据 §8 第 3 条）。
	//
	// nil ⇒ 子 agent 起不来（装配错误，构造期响亮报错而不是静默无记忆）。
	GraphOpener kernelapi.GraphMemoryOpener
	IO          *agentIO.IOManager
	Memory      kernelapi.GraphMemory
	// LightMemory 是**轻量内核**的图记忆装配（驻留子用；读 temp∪main，只写 temp）。
	//
	// 给了它就意味着这是轻量内核：`Memory` 必须为 nil，
	// 于是记忆整理面（块/媒体/流水线/整理工具）全部不可达（见 memoryface.go）。
	LightMemory kernelapi.GraphMemory
	Indexer     kernelapi.MemoryIndexer
	Tracker     kernelapi.Tracker

	DocStore    kernelapi.DocStore
	SocialStore kernelapi.SocialStore
	TextMemory  kernelapi.TextMemory
	// 媒体存储句柄已随 CAS 淘汰（块自带负载）。
	MultimodalSpace kernelapi.MultimodalSpace
	// EmbeddingProvider / EmbeddingError 是向量空间的配置身份与打开失败原因，
	// 供 healthcheck_kernel 状态报告区分「未配置 / 打开失败 / 已启用」。
	EmbeddingProvider string
	EmbeddingError    string
	FusionCfg         CrossModalFusionConfig // 跨模态融合权重；零值用默认
	Personality       *agentPkg.Personality
	PersonaStore      PersonaStore // 人格设定的读写面（首启门禁 + persona_set 工具）
	PluginReg         kernelapi.PluginHost
	// KernelSource 是本 agent 的"上级"（驻留子的父）。
	//
	// 设计 §6.1：某个 agent 的 L4 只属于它的**内核** —— 根 agent 的内核是内核自身与
	// 内核级插件；驻留子的内核是**父 agent**。因此子的 KernelSource = 父 ⇒ 只有父
	// 能在子的阶梯上产生 L4（父的"发送消息"）。
	KernelSource string
	// ParentID 是父 agent 的 id（空 = 根 agent）。子用它判断自己是不是驻留子。
	ParentID string
	// DataDir 是本 agent 的数据目录；创建驻留子时用它派生 temp 图记忆路径。
	DataDir string
	// TaskPrompt 是在固定提示词之上注入的**任务提示词**（驻留子创建时给定）。
	TaskPrompt string
	// AllowedTools / DenyTools 是**父为本 agent 声明的工具白/黑名单**。
	//
	// ★ 渲染要读 ⇒ 数据必须在基座（原先是 `residentChild` 的字段，渲染侧拿不到）。
	// 语义与安全模块的主体策略一致：deny 优先，allow 非空则只允许命中项。
	AllowedTools []string
	DenyTools    []string

	// AllowedOutputs 是本 agent **被授权的输出通道集合**（设计 §4.4 / R2）。
	//
	// nil 或空 = **完整授权**（默认）；非空 = 白名单，只允许列出的输出通道。
	// 父 agent 创建驻留子时用它收窄子的输出能力。
	AllowedOutputs []string
	// Agents 是 **agent 记录表**的写面（§3.2.2 唯一事实来源）。
	//
	// ## 为什么它必须进 AgentConfig
	//
	// 记录表承载父为子声明的全部事实（输出授权 / 工具策略 / 长期档位）与
	// 重建所需参数。它由内核装配（`kernelmod/agentreg`）并在这里传入 ——
	// 于是"登记"与"判定"（安全模块读同一张表）看到的是同一份数据。
	//
	// nil = 独立使用（单测 / 裁剪版内核）：实例管理器会自建一张私有表，
	// 行为与"内核提供一张表"完全一致（同一个实现，不是第二套代码）。
	Agents kernelapi.AgentRegistry

	// Security 是安全防火墙的**数据面闸门**（`kernelapi.SecurityGate`）。
	//
	// ## 用户裁定（2026-11-17）：「安全模块是强制的透明层」
	//
	// 强制 ⇒ 装配点必须给（给不出来就别构造 agent）；透明 ⇒ 它的类型不进
	// agent 的结构体，只经接口调用。
	//
	// ## 为什么装配期字段而不是事后 Set
	//
	// 这个字段曾经只能靠 `SetSecurity()` 注入，而生产装配链里**没有任何
	// 地方调用它** —— 唯一调用方是 4 条测试。于是安全判定在生产中恒走
	// nil 分支、恒放行，而策略表本身完全正确：这是「机制 ≠ 接线」的教科书
	// 样本。修复必须同时改三处：图里声明依赖、构造期传入、装配级判据。
	//
	// ## 子 agent 必须继承
	//
	// 驻留子 / rtchild 各自 `New(AgentConfig{...})`，不继承就等于子 agent
	// 完全没有闸门（而它们恰是"能指挥设备、能写记忆"的高风险执行体）。
	Security kernelapi.SecurityGate

	// IsRealtime 标记本 agent 是 **rtchild**（实时子 agent，见 rtchild.go）。
	//
	// 置 true 后：
	//   · 只有它能被划入带 CapRT 的 rt 通道（Agent.AssignRealtime 校验）；
	//   · 打断不走四级中断阶梯，而走单槽覆盖。
	//
	// ★ 这不是权限位，而是**类型标记**：另外两类子 agent 的设计目标函数
	//   与 rt 相反（公平 vs 最新者胜），混用会两边都坏。
	IsRealtime      bool
	PluginDir       string
	DistillInterval time.Duration
	ArchiveInterval time.Duration // 冷文档归档间隔（L2→L3），0 则使用 DistillInterval
	ReviewInterval  time.Duration // 关系复审间隔，0 则使用 DistillInterval
	MergeInterval   time.Duration // 实体合并检测间隔，0 则使用 DistillInterval
	MaxContextSize  int           // 活跃上下文最大条数，超出按相关性裁剪

	// CtxTuning 是上下文预算的可调参数。零值 ⇒ 使用历史默认（与硬编码时代一致）。
	//
	// 为何必须能从配置传进来：这些阈值原本写死在 ComputeTokenBudget 里，
	// 界面改不了、不同窗口的实例也没法各自调优（详见 kit.ContextTuning 的注释）。
	CtxTuning          kit.ContextTuning
	ContextSavePath    string // 上下文持久化路径，空则不持久化
	EmbeddingModelPath string // 预训练词嵌入模型路径（word2vec 文本格式），空则不使用
	Embedder           Embedder
	StageHost          *kit.StageHostType
	// EventBus 是**发布代理的注入源**（见 agent_eventbus.go）。
	//
	// ★ 用户裁定：原始总线只被安全模块持有 ⇒ agent 不存裸总线，
	//   构造期把它包成带身份的发布代理（`publisher`）。
	EventBus        kernelapi.EventBus
	ThinkingEnabled bool

	InputProcessing types.InputProcessingConfig // 非文本输入处理配置

	// MaxToolTurns 是单个任务允许的工具轮次上限（0 = 不限）。
	// 设计文档 D6：主循环必须有硬上限，否则模型不停调用就永不完结。
	MaxToolTurns int

	// Offload 是「积压任务自动转投给驻留子」的参数（见 offload.go）。
	//
	// 默认关闭（Enabled=false）：它让**内核替父做决策**，是设计 §7
	//「决策在父的模型手里」的刻意例外，因此必须由部署方显式打开。
	Offload OffloadOptions
}

func New(cfg AgentConfig) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	if cfg.DistillInterval <= 0 {
		cfg.DistillInterval = 30 * time.Minute
	}
	if cfg.ArchiveInterval <= 0 {
		cfg.ArchiveInterval = cfg.DistillInterval
	}
	if cfg.ReviewInterval <= 0 {
		cfg.ReviewInterval = cfg.DistillInterval
	}
	if cfg.MergeInterval <= 0 {
		cfg.MergeInterval = cfg.DistillInterval
	}
	if cfg.MaxContextSize <= 0 {
		cfg.MaxContextSize = 30
	}

	a := &Agent{
		id:                cfg.ID,
		provider:          cfg.Provider,
		llm:               cfg.LLM,
		extractor:         cfg.Extractor,
		norm:              cfg.Norm,
		memOps:            cfg.MemoryOps,
		graphOpener:       cfg.GraphOpener,
		preferredProvider: cfg.PreferredProvider,
		providerManager:   cfg.ProviderManager,
		io:                cfg.IO,
		graph:             graphMemoryOf(cfg),
		kernelSource:      cfg.KernelSource,
		parentID:          cfg.ParentID,
		taskPrompt:        cfg.TaskPrompt,
		dataDir:           cfg.DataDir,
		logIdentity:       agentLogIdentity(cfg),
		systemPrompt:      cfg.SystemPrompt,
		ctx:               ctx,
		cancel:            cancel,
		// mediaStore 已随 CAS 淘汰：块自带负载。知识层的媒体也已自带字节
		// （见 knowledge 的媒体侧车），不再需要外部 getter/putter 接线。
		allowedOutputs: cfg.AllowedOutputs,
		toolAllow:      cfg.AllowedTools,
		toolDeny:       cfg.DenyTools,
		toolAllowSet:   splitToolPatterns(cfg.AllowedTools),
		toolDenySet:    splitToolPatterns(cfg.DenyTools),
		selfInputCh:    make(chan selfInputMsg, 64),
		maxToolTurns:   cfg.MaxToolTurns,
		// ★ IsRealtime 现在是**钩子**（装配期由 rt 模块注册），不是字段。
		//   这里只把 cfg 的意图带过去，保持构造点签名不变。
		cfg:       cfg,
		agents:    cfg.Agents,
		hooks:     Hooks{},
		publisher: agentEventBus{bus: cfg.EventBus, who: string(cfg.ID)},
		lastInput: make(map[string]time.Time),
	}

	// ★ 授权事实声明给**安全模块**（用户裁定 2026-11-17）。
	//   agent 只提供事实（谁被委派了什么），判定由安全模块做。
	//
	// ★ 2026-11-18（§3.2.2）：事实现在落在 **agent 记录表**上
	//   （`AgentRecord.Facts`），安全模块判定时读它。所以这里先登记记录：
	//   一条记录同时承载输出授权、工具策略、长期档位 —— 清理点只剩 Remove。
	//
	// 无记录表（裸 agent 单测）⇒ 跳过：判定侧 fail-open，行为与改造前一致。
	if reg := a.agentsRegistry(); reg != nil {
		rec := model.AgentRecord{
			ID:        string(a.id),
			ParentID:  a.cfg.ParentID,
			Lifecycle: "running",
			Arg:       map[string]interface{}{},
			Facts: model.AgentFacts{
				AllowedOutputs: a.allowedOutputs,
				AllowedTools:   a.cfg.AllowedTools,
				DenyTools:      a.cfg.DenyTools,
			},
		}
		if err := reg.Register(rec); err != nil {
			log.Printf("[agent] register agent record %q failed: %v", a.id, err)
		}
	}
	if g := a.Gate(); g != nil {
		g.DeclareOutputs(string(a.id), a.allowedOutputs)
	}

	// 变量库注入：context 的落盘前脱敏要用**本 agent** 的变量库，
	// 而不是进程级单例（见 RelevanceContext.SecretStore 的注释）。
	if gate := a.Gate(); gate != nil {
		if st := gate.Store(); st != nil {
			if a.contextMgr() != nil {
				a.contextMgr().SetSecretStore(st)
			}
			// ProviderManager 是**全进程共享**的一份（主/子/驻留子同源），
			// 出向装饰要在它 Register 时生效，所以注入也必须到它身上。
			if a.providerManager != nil {
				a.providerManager.SetSecretStore(st)
			}
		}
	}

	// ★ typed-nil 归一化（经典陷阱，实测已复现）：
	//   接口字段装的是"具体类型的 nil 指针"时，接口值本身**不是 nil**
	//   ⇒ `if a.pluginReg != nil` 会放行，随后方法内部解引用空指针 panic。
	//   这里把每一处 typed-nil 显式折成真 nil。

	// 输入路由：inputch 是可分配资源，划给某个 agent 后输入**只**流向那个 agent
	// （设计 §4.1「路由发生在进内核之前」）。io 层不认识 agent，所以在这里把路由器
	// 注入进去：插件注入输入时先问它，被别的 agent 接管就不再进本内核队列。
	if a.io != nil {
		a.io.SetInputRouter(a.RouteInputByOwner)
		// ★ 注入侧的**授权夹取**（L4→L3）：判据在安全模块，接线在这里。
		//
		// 这是"任何 agent 收到的 L4 都可信"的实现点：所有注入必经 io，
		// 而夹取发生在事件进内核之前。未装配闸门 ⇒ 不夹（与"安全未加载
		// ⇒ 恒放行"同口径）。
		a.io.SetPriorityClamp(a.clampPriority)
	}
	return a
}

func (a *Agent) Start() {
	// ★ 向**内核事件循环**登记本 agent 的输入投递口：此后按 inputch 归属
	//   路由的输入由循环推动到它这里（用户裁定「处理输入事件路由」归 eventloop）。
	//   循环未装配（单测/独立工具）时这一步是 no-op，行为与改造前一致。
	if l := kernelloop.Current(); l != nil {
		l.RegisterInputSink(string(a.id), &inputSink{a: a})
	}
	// ★ 安全运行态**响亮**显现（未装配即 unguarded，不能沉默）。
	a.logPosture()
	// ★ "security" 段由基座注册：它是纯内核事实（与任何 agent 类型无关）。
	a.RegisterStatusSection(StatusSection{Key: "security", Payload: func() any {
		return a.securitySectionValue()
	}})

	// 跑起来是**类型**的事（它注册 Runtime.Start）；基座只负责通用登记。
	if r := a.hooks.Runtime; r.Start != nil {
		r.Start(a)
	}
	a.Logf("%s started, waiting for IO interrupts", a.id)
}

func (a *Agent) Stop() {
	// 父退出**必须**销毁全部驻留子（设计 §10 硬约束：子不得比父活得久、不留孤儿）。
	if a.hooks.Runtime.Stop != nil {
		a.hooks.Runtime.Stop(a)
	}
	// 释放本 agent 的策略执行视图（2026-10-31）。
	//
	// 视图里存着 **longtimeProfile**（跨轮保留的档位）。不释放的话，
	// 同名 agent 重建会继承上一代的 long-time 档 —— 那时可能是
	// "上一个任务留下的访客权限"被新一代默默沿用。
	// 与主体策略撤销（revokeChildPolicy）同一纪律。
	// ★ 从内核循环撤销投递口登记：同名重建是常规操作，残留会让输入
	//   投到一个已销毁的 agent 上（悬空引用）。
	if l := kernelloop.Current(); l != nil {
		l.UnregisterInputSink(string(a.id))
	}
	a.ReleaseSecurityScope()
	// ★ 删除记录 ⇒ 一次性清掉全部父声明事实（输出授权 / 工具策略 / 长期档位）。
	//   这是 §3.2.2 的收益：原先四处清理点（ReleaseOutputs / ReleaseAgent /
	//   RevokeChildPolicy / forget）合并成这一处。漏一处就是残留（同名重建
	//   继承上一代授权是**提权**），现在从结构上不可能漏。
	if reg := a.agentsRegistry(); reg != nil {
		reg.Remove(string(a.id))
	}
	// 停机前给待办任务补终态。运行中的任务会经 cancel → LLM 失败 → emitResponse
	// 自然拿到终态，但**从未运行**（排队/待处理）与**已挂起**的任务不会有任何人
	// 回它们；带 ResponseCh 的同步注入方（cli / clawhubadapter 均无超时）会永久挂起
	// （设计 §7 I5、§11.3 X2/X4）。必须在 cancel 之前做：cancel 会让调度器直接 return。
	//
	// ★ 这是**通用**收尾（任何有队列的类型都要它），所以留在基座；
	//   类型回调里不再重复做同一件事。
	a.drainPendingInterrupts("agent_stopped")
	a.cancel()
}

// skipped 终态（复用 emitSkippedReply：非阻塞写，不对外发 agent_output 事件）。
func (a *Agent) drainPendingInterrupts(reason string) {
	if a.sched == nil {
		return
	}
	pending := a.sched.PendingEvents()
	if len(pending) == 0 {
		return
	}
	for _, evt := range pending {
		a.EmitSkippedReply(evt, reason)
	}
	a.Logf("%s: shutting down, filled skipped terminal state for %d pending task(s)", a.id, len(pending))
}

// graphMemoryOf 决定本 agent 的图记忆共同面实现。
//
//   - 轻量内核（给了 LightMemory）：用 LightMemory，**整理面保持 nil**；
//   - 完整内核：直接用主图库（*memory.GraphDB 天然满足 GraphMemory）。
func graphMemoryOf(cfg AgentConfig) GraphMemory {
	if cfg.LightMemory != nil {
		return cfg.LightMemory
	}
	if cfg.Memory == nil {
		return nil
	}
	return cfg.Memory
}

// graphMem 返回本 agent 的图记忆**共同面**（七公共面第 2 项：可选记忆）。
//
// ★ 唯一事实源：`graph` 槽位。它可以是 GraphDB（主）、LightMemory（驻留/rt），
//
//	也可以是 nil（工作式子 / 独立工具）—— 用户原话「图记忆是可选配置」。
func (a *Agent) GraphMem() GraphMemory {
	if a == nil {
		return nil
	}
	return a.graph
}

// graphDB 返回**完整**图记忆面（整理面：蒸馏/归档/合并/场景衰减…）。
//
// 只有真正的图库满足它（LightMemory 不满足 ⇒ 驻留子的整理面全部不可达，
// 这是设计意图：轻量内核不该有那些代码路径）。
func (a *Agent) GraphDB() kernelapi.GraphMemory {
	if a == nil {
		return nil
	}
	if cfg, ok := a.graph.(graphFace); ok {
		return cfg.g
	}
	if full, ok := a.graph.(kernelapi.GraphMemory); ok {
		return full
	}
	return nil
}

// agentLogIdentity 由 AgentConfig 推导日志身份。
//
// 三类执行者的判定依据（与 child.Kind 同源）：
//
//	IsRealtime       ⇒ rt（rtchild：单槽覆盖、丢现场）
//	ParentID 非空    ⇒ resident（驻留子 / 转投驻留子）
//	其余             ⇒ 主 agent（根）
func agentLogIdentity(cfg AgentConfig) child.LogIdentity {
	switch {
	case cfg.IsRealtime:
		return child.LogIdentity{Kind: child.KindRT, ID: string(cfg.ID)}
	case cfg.ParentID != "":
		return child.LogIdentity{Kind: child.KindResident, ID: string(cfg.ID)}
	}
	return child.LogIdentity{Kind: "", ID: ""}
}

// Logf 写一行带身份标签的日志。
//
// ★ 身份由基类（child.LogIdentity）自动补，**调用方不需要手动带标签**：
//
//	a.Logf("tool %s result: %s", name, res)
//	⇒ [resident:res_3] tool files_read result: ...
//	⇒ [rt:rt_1]        tool files_read result: ...
//	⇒ [main]           tool files_read result: ...
//
// 之前每个调用点都要 `a.Logf("...", ...)`，
// 新加一处忘了补就又没有身份，而且没人会发现 —— 那正是本仓反复栽的
// "改一处漏一处"。收进方法后，写成 a.Logf 就必然有身份。
func (a *Agent) Logf(format string, args ...any) {
	if a == nil {
		(&child.LogIdentity{}).Logf(format, args...)
		return
	}
	a.logIdentity.Logf(format, args...)
}

func (a *Agent) ID() types.AgentID { return a.id }

// isChildAgent 报告本 agent 是不是某个父下的子（**层级数据事实**）。
//
// ★ 它不是槽位、不是类型结论：就是"有没有父"这个构造期事实
// （用户裁定 2026-11-18：IsChild 是数据事实，不该占行为钩子）。
func (a *Agent) isChildAgent() bool {
	return a != nil && a.cfg.ParentID != ""
}

// IsOutputAllowed 是**渲染判据**：决定某个输出通道要不要出现在工具表里。
//
// ## 它只用于渲染（用户裁定 2026-11-18）
//
//	范围（父声明的白名单） → **渲染**：进不进工具表 → 基座（数据在 agent）
//	闸门（CheckOutput）    → **调用**：过不过     → 安全模块
//
// 这两个判据必须分开：
//
//	· 渲染侧必须**照旧过滤**，即使安全模块未装配 —— 否则模型会看见父明确
//	  禁用的通道，再在执行时被拒（模型白算一轮，用户看到"工具调用失败"）。
//	· 调用侧必须**恒放行**（未装配 = 没有闸门），而不是靠白名单兜底。
//
// ⇒ 本方法**不再被任何执行路径调用**（`output.go` / `toolcall.go` 三处
//
//	改走 `a.Gate().CheckOutput(...)`），只剩 `tooldefs.go` 的渲染过滤。
func (a *Agent) IsOutputAllowed(channel string) bool {
	// ★ 判定在**安全模块**（用户裁定 2026-11-17：「设备是否授权不应该安全
	//   模块负责吗？」）。agent 只提供事实（`allowedOutputs` 在构造期由父
	//   声明给安全模块），不再自己比较字段 —— 否则同一个判定有两份实现。
	//
	// ★ 无 ID 的 agent（判据里的匿名构造）无法在安全模块的按-agentID 表里
	//   定位，因此退回本地事实比较 —— 否则会落到"未声明 ⇒ 放行"，把
	//   白名单判据变成假绿（实测：`TestOutputAllowlist` 先撞这个）。
	if g := a.Gate(); g != nil && a.id != "" {
		return g.CheckOutput(string(a.id), channel).Allowed
	}
	// 未装配闸门 / 无身份：退回本地事实，与改造前行为一致。
	if len(a.allowedOutputs) == 0 {
		return true
	}
	for _, c := range a.allowedOutputs {
		if c == channel {
			return true
		}
	}
	return false
}

// ResolveOutputTarget 解析输出通道的投递目标（agent + inputch）。
//
// ok=false 表示该输出通道由传输层（device 通道，如 qq/webui）自行处理。
func (a *Agent) ResolveOutputTarget(channel string) (agentIO.OutputTarget, bool) {
	if a.io == nil || a.io.ChannelRegistry() == nil {
		return agentIO.OutputTarget{}, false
	}
	return a.io.ChannelRegistry().ResolveOutputTarget(channel)
}

// isDuplicateInput 判断是否为短窗口内的重复输入（防 webui/GUI 断线重连消息重放）。
// key=source+"|"+content；窗口内重复返回 true 并刷新时间戳（持续轰炸时保持拦截）。
const duplicateInputWindow = 10 * time.Second

func (a *Agent) isDuplicateInput(source, content string) bool {
	a.lastInputMu.Lock()
	defer a.lastInputMu.Unlock()
	now := time.Now()
	key := source + "|" + content
	if last, ok := a.lastInput[key]; ok && now.Sub(last) < duplicateInputWindow {
		a.lastInput[key] = now
		return true
	}
	a.lastInput[key] = now
	// 顺带清理过期项，防止 map 无限增长
	for k, t := range a.lastInput {
		if now.Sub(t) > duplicateInputWindow {
			delete(a.lastInput, k)
		}
	}
	return false
}

// IsDuplicateInput 导出包装，供测试验证去重行为。
func (a *Agent) IsDuplicateInput(source, content string) bool {
	return a.isDuplicateInput(source, content)
}

// SelfInputChan 返回自循环输入通道（只读，供内部测试验证）
func (a *Agent) SelfInputChan() <-chan selfInputMsg {
	return a.selfInputCh
}

// injectSelf 向自循环通道发送记忆整理类内部任务（无记忆路径）。
// 线程安全，不阻塞发送者（通道缓冲 64）。
func (a *Agent) InjectSelf(task string) {
	a.InjectSelfChannel(selfInputMsg{
		Text:    task,
		Channel: channelConsolidation,
	})
}

// injectSelfChannel 向自循环通道发送一条带目标通道标志的消息。
// channel == "_consolidation_" 走无记忆整理路径；其他值走正常处理路径。
func (a *Agent) InjectSelfChannel(msg selfInputMsg) {
	select {
	case a.selfInputCh <- msg:
	default:
		a.Logf("self input channel full, dropping task: %s", truncateStr(msg.Text, 80))
	}
}

// SetHooks 挂上本类型的**行为回调**（装配期调用一次；用户裁定的机制）。
//
// 用法（类型模块自己的构造点）：
//
//	base := agent.NewBase(cfg)
//	base.SetHooks(agent.Hooks{
//	    ContextBudget: main.ContextBudget,       // 上下文管理
//	    Loop:          kit.Loop,            // agent loop
//	    GraphMemory:   db,                       // 可选记忆
//	    SecurityGate:  Gate,
//	})
//	if err := base.Validate(); err != nil { … }   // 缺槽位 ⇒ 装配期失败
//
// ★ 它是**覆盖**（不是合并）：类型工厂一次挂齐自己的整套回调。装配点若在
// 工厂之后再调一次 `SetHooks`，会把工厂已挂的槽位（含安全闸、上下文策略、
// stage）**全部清空** —— 实测 `kernelmod/agent` 就犯过这个错：工厂填满后
// 又 `SetHooks(Hooks{Loop: LoopStandard})`，其余槽位全归零。
//
// 正确用法只有一个：**类型工厂里**挂齐，装配点不再碰它。
func (a *Agent) SetHooks(h Hooks) {
	if a == nil {
		return
	}
	a.hooks = h
}

// Validate 校验必填槽位（装配期在 SetHooks 之后调用）。
func (a *Agent) Validate() error {
	if a == nil {
		return &MissingHookError{Agent: "", Slot: "agent", Hint: "agent 为 nil"}
	}
	return a.hooks.Validate(string(a.id))
}
