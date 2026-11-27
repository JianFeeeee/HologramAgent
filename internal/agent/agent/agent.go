package agent

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent/child"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	agentProvider "github.com/JianFeeeee/HologramAgent/internal/agent/provider"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
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
	io              *agentIO.IOManager

	// kernelSource/parentID/taskPrompt/dataDir：驻留子相关的层级信息（见 AgentConfig）。
	kernelSource string
	parentID     string
	// preferredProvider / systemPrompt 是**父声明**（装配期 SetXxx 写入，
	// 运行期只读）。不进 AgentConfig：它不是"配置中心里那条键"（§2）。
	preferredProvider string
	systemPrompt      string
	taskPrompt        string
	dataDir           string
	// workDir 是**内核分配给本实例的工作数据目录**（`<data>/agents/<type>/<id>/`）。
	//
	// ★ 由内核派生（见 InstanceManager.WorkDirOf），类型只读不拼。
	workDir string

	// ★ 能力面在这里（`agentCap`）—— 一个位掩码，不是"是不是 rt"的布尔。
	// 基座不认识任何具体能力名，只做位与比较。
	//
	// 与通道侧的 `OutputCapability` 是**同一个位类型**（两侧对称比较）：

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
	// stageHooks 是**基座持有**的阶段钩子槽位（类型注册自己的行为）。
	//
	// ★ 与 `stageHost` 的分工：
	//
	//	stageHost   插件声明的阶段处理器（外部能力：cli/webui/qq 这些）
	//	stageHooks  类型注册的阶段行为（主 agent 的记忆/渲染，驻留子的整窗策略）
	//
	//   RunStage 会**先跑类型钩子、再转插件面**：类型的上下文哲学优先，
	//   插件看到的是已处理过的现场。
	stageHooks stageHookTable

	// stageHost 是**基座持有**的阶段管道与插件工具表（装配期由内核注入）。
	//
	// ★ 为什么基座持有：工具表渲染、参数 schema 校验、并发判据、阶段执行
	//   都要查它。它是**内核设施**（FacStageHost），不是某个类型的状态。
	stageHost *kit.StageHostType

	// toolReg 是**插件工具登记簿**（独立设施，与阶段管道分开，2026-11-25 拆）。
	//
	// ★ 为什么基座持有：公有面渲染、参数 schema 校验、并发判据都要查它。
	//   它是内核设施（FacToolReg），不是某个类型的状态。
	toolReg *kit.ToolRegistry

	// rtOut 是 rt 输出通道状态（基座持有，类型只交文本）。
	//
	// ★ 通道由**内核 device 抽象**管理：类型调 EmitRTText 交文本，
	//   不知道通道是谁（用户裁定 2026-11-25）。
	rtOut rtOutputState

	// cfgStore 是内核注入的配置管理器句柄（`Config().Cfg`）。
	cfgStore kernelapi.ConfigStore

	// ★ 这里**没有**上下文句柄：上下文是 agentloop 的，归类型持有
	//   （`mainagent.Module` 自己的字段）。基座既不需要读它、也不想知道它的形状 ——
	//   它拿到的永远是**类型渲染好的完整上下文**（`[]Message` + `[]ToolDef`）。
	ctx    context.Context
	cancel context.CancelFunc

	// 文档记忆（第二层）

	// 知识库

	// 人物特质与关系网

	// 文本记忆（原始对话日志）

	// 媒体存储句柄已随 CAS 淘汰（块自带负载）。

	// 人格设定（内容来自启动时载入的人格文件/配置项）

	// 人格落库面：首启门禁与 persona_set 工具使用（见 persona.go）。
	// 为 nil 时门禁与工具都静默关闭（例如单测里不接配置的场景）。

	// 被授权的输出通道集合（空 = 完整授权，见 AgentConfig.AllowedOutputs）。
	// agentCap 是本实例的能力面（装配期一次，读面只读它）。
	agentCap       agentIO.OutputCapability
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

	// 自循环输入通道：核心内部任务（记忆消歧、系统维护、子 Agent 通知），
	// 不经过 IO 层。每条消息携带目标输出通道：
	//   "_consolidation_" = 记忆整理（无记忆路径，不写入上下文、不 emit 响应）
	//   其他 = 正常处理（写入上下文、emit 响应到该通道）
	selfInputCh chan SelfInputMsg

	// 输入调度器：就绪队列、任务抽象与快照（见 scheduler.go）。
	// M2 起取代 eventLoop 的隐式 channel 排队。
	// sched 是**类型交给内核的调度器句柄**（实例仍归类型持有）。
	//
	// ★ 基座不驱动它、不决策 —— 只用它做三件内核必须能到达的事：
	//   投内核级中断（L4）、停机时为排队任务补终态、暴露统计。
	//   `schedMu` 保护的是"类型交出/替换实例"与"并发读"这一对。

	// offload 是积压任务自动转投的参数（见 offload.go）。

	// 进行中的 LLM 请求取消函数，interceptLoop 可调用以在请求中打断
	llm       kernelapi.LLMClient
	extractor artifacts.Extractor
	norm      artifacts.Norm
	memOps    kernelapi.MemoryOps
	// embedder 是**稀疏词嵌入**（fastText 词向量 + jieba）。零状态之外的一个
	// 只读内存表，全进程一份 —— 装配点从 `FacEmbedder` 取，这里只按窄接口持用。
	embedder Embedder
	// multimodal 是**稠密多模态向量空间**（可选载入）。provider 未配置/打开失败
	// ⇒ 为 nil，所有依赖它的路径（跨模态召回、块向量重算）明确降级。
	multimodal kernelapi.MultimodalSpace
	// semanticMu 保护下面这些"装配期挂载、运行期读取"的字段。
	semanticMu sync.RWMutex
	// toolExec 是内核工具执行设施（`kernelmod/toolexec` 的实例）。
	toolExec ToolExecutor
	// gate 是安全防火墙的数据面闸门。
	gate kernelapi.SecurityGate
	// pluginDir 是插件目录（plgreload 用）。
	pluginDir string
	// ── 类型交出的面（MountTypeFaces 写，基座只读）──
	toolDefs     ToolDefsFace
	contextStats ContextStats
	depParser    func() (enabled bool, modelPath string)
	// ── 主 agent 私有值（MountMainValues 写，基座不解释语义）──
	fusionCfg    any
	personality  any
	personaStore any
	cancelLLM    context.CancelFunc
	llmMu        sync.Mutex

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

	// device 是**内核持有的设备数据面**（用户裁定：与 toolcall 同类 ——
	// 基座接受请求、内核统一调用、经防火墙）。它**不是**任何类型的私有状态。
	device   agentAPI.DeviceSurface
	deviceMu sync.RWMutex

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

	// startOnce 保证 `Start()` 只生效一次。
	//
	// ★ 为什么基座自己需这个守卫（2026-11-26 实测缺陷）：
	//   有两个装配点曾各自调一次 `Start`——`kernelmod/agent` 的 `buildAgent`
	//   （装配层）与 `cmd/homed/runtime.go`（运行期起点）。后果是
	//   `hooks.Runtime.Start` 跑两遍，而 mainagent 在那里 `go SchedulerLoop()`
	//   且**没有幂等守卫** ⇒ 同一主 agent 两个调度循环并发。
	//
	//   删除多余的装配点是根因修法；本守卫是**防守**：类型侧的运行期
	//   启动（那些 `go ...Loop`）不该由基座去要求每个类型自己写守卫——
	//   "改一处漏一处"的形态就在这里。
	startOnce sync.Once

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
	ID types.AgentID
	// ProviderManager 是内核的源表句柄。**类型不许读它** —— 一律走
	// `CompleteLLM` / `StreamLLM` / `LLMSources` / `ActiveLLMSource`。
	//
	// 保留字段的用途是装配侧（变量库注入、子实例继承同一张表）。
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
	// Extractor 是**三元组提取器**（契约在 internal/artifacts，实现在 nlp）。
	//
	// ★ 为什么经配置注入而不是让基座 import nlp：那是带 ONNX 的实现包
	//   （判据 §8 第 3 条）。基座只调 `Extract(text) []Triple` 一件。
	//
	// nil ⇒ 归档时跳过通用提取（只写文档元数据三元组）。
	// Norm 是**归一化判定**（实体名能否落库 / 是否噪声，实现在 memory）。
	//
	// 零值语义是"放行"：未装配时多写几条噪声（可清理），而不是让归档
	// 一条关系都建不起来（静默失效）。
	// DepParser 返回依存句法分析器的**只读事实**（状态面用）。
	//
	// ★ 与 Extractor 同理：实现是 `internal/nlp`（带 ONNX），基座只读事实。
	// nil ⇒ 状态面报"未启用"。
	// MemoryOps 是记忆侧的**纯函数能力面**（契约在 kernelapi，实现在 memory）。
	//
	// ★ 为什么它必须存在：基座要用 CleanText / ExtractKeywords / MediaBlockID /
	//   SortMode / Situation，而那些函数今天住在 `internal/memory`
	//   —— 一个带 SQLite 的实现包（判据 §8 第 3 条）。
	//
	// nil ⇒ 各调用点按零值安全处理（见 memoryops_face.go）。
	// 媒体存储句柄已随 CAS 淘汰（块自带负载）。
	// EmbeddingProvider / EmbeddingError 是向量空间的配置身份与打开失败原因，
	// 供 healthcheck_kernel 状态报告区分「未配置 / 打开失败 / 已启用」。
	// KernelSource 是本 agent 的"上级"（驻留子的父）。
	//
	// 设计 §6.1：某个 agent 的 L4 只属于它的**内核** —— 根 agent 的内核是内核自身与
	// 内核级插件；驻留子的内核是**父 agent**。因此子的 KernelSource = 父 ⇒ 只有父
	// 能在子的阶梯上产生 L4（父的"发送消息"）。
	KernelSource string
	// TypeName 是本实例的**类型名**（类型表里那一项）。
	//
	// ★ 它由内核在造实例时填（`NewInstance`），基座只读它 —— 不再用
	//   `ParentID != ""` 去**猜**自己是什么类型（那是硬编码
	//   三种类型；插件注册的新类型永远猜不出来）。
	TypeName string
	// ParentID 是父 agent 的 id（空 = 根 agent）。
	ParentID string
	// DataDir 是**内核**的数据根目录（`cfg.Daemon.DataDir`）。
	//
	// ★ 它是内核的，不是本 agent 的：本 agent 自己的可写目录是 WorkDir。
	// 留它是因为部分类型要访问内核级设施（图库锚点、知识库），但**任何类型
	// 都不得从它拼自己的子目录**（那是 `WorkDir` 的职责）。
	DataDir string
	// WorkDir 是本实例的**工作数据目录**（内核派生并管理）。
	//
	// 目录布局 = `<DataDir>/agents/<Type>/<ID>/`，由 `InstanceManager` 在
	// 登记实例时分配并落进 `AgentRecord.WorkDir`（于是重启能算回同一位置）。
	//
	// ★ 类型把**所有**运行数据写在这里（上下文、图库、临时文件），
	// 永远不自己 `filepath.Join(dataDir, "residents", id, ...)` ——
	// 那会让"加一种类型"变成"再发明一套目录规则"。
	WorkDir string
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

	// AgentCap 是本实例**能承接的通道能力位**（能力面，docs §8.1.1）。
	//
	// ★ 它是**数据声明**，不是行为位：基座与内核只做位掩码比较，
	//   不认识 "rt" 这个词（具体能力名由 io 包定义）。加第三种能力
	//   （音频 / 图像 / 结构化）时，本字段与基座代码都不用改。
	AgentCap agentIO.OutputCapability
	// ★ DistillInterval / ArchiveInterval / ReviewInterval / MergeInterval /
	//   MaxContextSize / EmbeddingModelPath / InputProcessing / ThinkingEnabled
	//   已全部迁走（2026-11-20）：
	//     · 间隔与上限 → `a.DistillInterval()` 等**现读**方法（`core.agent.*`）
	//     · 词嵌入模型路径 → 内核设施 `FacEmbedder`（`kernelmod/multimodal`）
	//     · 非文本输入处理 → 各调用点 `a.Cfg()` 现读

	// Cfg 是**配置管理器**（内核注入）。各 agent 经它读写配置 —— 配置是内核
	// 持有的事实，不是 agent 的私有状态（用户裁定）。
	//
	// ★ 未注入 ⇒ 各读取点走各自的默认值（独立工具/单测），行为与改造前一致。
	Cfg kernelapi.ConfigStore

	// ToolExecutor 是**内核的工具执行设施**（路由 / 授权闸 / 并发 / 清洗）。
	//
	// ★ 基座只转发：类型提交批请求，拿回同序结果。它不认识"哪个工具该调"。

	// ToolDefs 是**类型注册的工具清单渲染面**（只读闭包）。
	//
	// ## 为什么是闭包而不是基座自己渲染
	//
	// 渲染归类型（用户裁定 2026-11-19）：模型看到的那份清单包含人格段、
	// 插件段、工具族的并行/串行位、output_send__ 的通道枚举……基座不认识
	// 其中任何一项，也不该认识。
	//
	// 基座只在 `api.ToolHost` 契约上转一次（`ToolDefinitions`），
	// 供 device 等内核设施读"当前可见工具"。
	//
	// nil ⇒ 本类型不对外暴露工具清单（返回空，不伪造）。
	// ToolExec 是**内核工具执行设施**（`kernelmod/toolexec` 的实例）。
	//
	// ★ 由**装配侧**注入（`kernelmod/agent`），不是基座自己构造：
	//   设施需要基座的能力（io/stageHost/安全闸），基座若反过来 import 设施
	//   会成环。注入点只有一处，缺它由 `RunToolBatch` 响亮报错。
	// ContextStats 是**类型的上下文**的只读统计面（条数 / 水位）。

	// ★ 已删 Offload（2026-11-20）：自动转投是**主 agent 自己的**决策参数，
	//   归 `mainagent.values.go`。基座不认识"转投"这件事。
}

func New(cfg AgentConfig) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	// ★ 时间/条数类配置**不再在这里归一化**：它们是配置项透传
	// （docs §2），改配置不重启就生效。归一化逻辑搬到 `a.DistillInterval()`
	// 等现读方法里 —— 那里是唯一的"默认值推导"落点。

	a := &Agent{
		id: cfg.ID,

		// extractor / norm / memOps 是**语义计算能力**（docs §2 结论 4：
		// AgentConfig 不许放设施句柄）⇒ 由装配点在 New 之后 MountSemantic 挂载。
		kernelSource: cfg.KernelSource,
		parentID:     cfg.ParentID,
		taskPrompt:   cfg.TaskPrompt,
		dataDir:      cfg.DataDir,
		workDir:      cfg.WorkDir,
		logIdentity:  agentLogIdentity(cfg),
		ctx:          ctx,
		cancel:       cancel,
		// mediaStore 已随 CAS 淘汰：块自带负载。知识层的媒体也已自带字节
		// （见 knowledge 的媒体侧车），不再需要外部 getter/putter 接线。
		agentCap:       cfg.AgentCap,
		allowedOutputs: cfg.AllowedOutputs,
		toolAllow:      cfg.AllowedTools,
		toolDeny:       cfg.DenyTools,
		toolAllowSet:   splitToolPatterns(cfg.AllowedTools),
		toolDenySet:    splitToolPatterns(cfg.DenyTools),
		selfInputCh:    make(chan SelfInputMsg, 64),
		cfg:            cfg,
		cfgStore:       cfg.Cfg,
		hooks:          Hooks{},
		lastInput:      make(map[string]time.Time),
	}

	// ★ 阶段管道与插件工具表：装配期注入（内核的 FacStageHost）。

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

	// 变量库注入 ProviderManager（**全进程共享**的一份：主/子/驻留子同源）。
	//
	// ★ 上下文侧的注入由**类型**自己接（它持有上下文与变量库槽位）——
	//   基座没有上下文句柄，也不该有。
	if gate := a.Gate(); gate != nil {
		if st := gate.Store(); st != nil {
			if a.providerManager != nil {
				a.providerManager.SetSecretStore(st)
			}
		}
	}

	// ★ typed-nil 归一化（经典陷阱，实测已复现）：
	//   接口字段装的是"具体类型的 nil 指针"时，接口值本身**不是 nil**
	//   ⇒ `if a.pluginReg != nil` 会放行，随后方法内部解引用空指针 panic。
	//   这里把每一处 typed-nil 显式折成真 nil。

	// ★ 实例管理器：**内核空间**的东西，基座自己在构造时装上（2026-11-20）。
	//
	// 它不是某个类型的能力：主 agent 只是"恰好是被选中的那个"，
	// 换一个类型当主 agent，它照样要能管子 agent。
	// 装在这里而不是类型的 Initialize 里 —— 类型够不到内核表。
	if a.instances == nil {
		a.instances = NewInstanceManager(a)
	}

	// 输入路由：inputch 是可分配资源，划给某个 agent 后输入**只**流向那个 agent
	// （设计 §4.1「路由发生在进内核之前」）。io 层不认识 agent，所以在这里把路由器
	// 注入进去：插件注入输入时先问它，被别的 agent 接管就不再进本内核队列。
	a.wireIO()
	return a
}

// wireIO 把本 agent 的**输入路由**与**优先级夹取**接到 IO 面上。
//
// ★ 为什么单独一个方法（2026-11-20）：IO 现在经 `MountSurface` 挂载，
// 而不是 `AgentConfig.IO`。挂载点必须自己完成这段接线 —— 否则
// "通道归属路由"与"L4 夹取"两件事在装配后静默失效（判据当场抓到：
// 悬空通道的输入没有进 stash，直接落给父了）。
//
// 幂等：重复挂载只覆盖（装配期可能重挂）。
func (a *Agent) wireIO() {
	if a == nil || a.io == nil {
		return
	}
	a.io.SetInputRouter(a.RouteInputByOwner)
	// ★ 注入侧的**授权夹取**（L4→L3）：判据在安全模块，接线在这里。
	//
	// 这是"任何 agent 收到的 L4 都可信"的实现点：所有注入必经 io，
	// 而夹取发生在事件进内核之前。未装配闸门 ⇒ 不夹（与"安全未加载
	// ⇒ 恒放行"同口径）。
	a.io.SetPriorityClamp(a.clampPriority)
}

func (a *Agent) Start() {
	a.startOnce.Do(a.startOnceBody)
}

// startOnceBody 是 Start 的正文（只跑一次，见 startOnce 字段的注释）。
func (a *Agent) startOnceBody() {
	// ★ 向**内核事件循环**登记本 agent 的输入投递口：此后按 inputch 归属
	//   路由的输入由循环推动到它这里（用户裁定「处理输入事件路由」归 eventloop）。
	//   循环未装配（单测/独立工具）时这一步是 no-op，行为与改造前一致。
	// ★ 登记**投递入口**（基座假设：注册的 agent 会恰当承接 inputch 的内容，
	//   并按需要调用 outputch）。内核按归属路由到它，它把输入放进本 agent 的 io ——
	//   `priority` 标签随事件走，分拣由各类型自己的 agentloop 解释。
	if l := kernelloop.Current(); l != nil {
		l.RegisterInputSink(string(a.id), &inputSink{a: a})
	}
	// ★ 安全运行态**响亮**显现（未装配即 unguarded，不能沉默）。
	a.logPosture()
	// ★ 基座注册的三个段（纯内核事实，与任何 agent 类型无关）：
	//
	//	"security"  安全运行态（guarded / unguarded）
	//	"runtime"   Go runtime 统计 —— 主 agent 挂了它照样存在
	//	"llm"       Provider* 是 AgentConfig 公有字段 —— 每个类型都有
	//
	// ★★ 注册是**装配期动作**，只能在这里做（2026-11-20 实测缺陷）。
	//
	// 原先 "runtime" / "llm" 两段是在 `GetKernelStatus()` 里注册的 ——
	// 那是**高频读路径**（cli /status 15s 轮询、webui 面板、healthcheck）。
	// 后果：每读一次就往共享段表写一次（读路径改共享状态）、每读一次分配
	// 两个闭包、并且每次写触发一条告警日志（实测 15s 两条，一天 11520 行）。
	//
	// 载荷是**求值函数**：读取时现取当下事实（provider 名与源数量会变）。
	a.RegisterStatusSection(StatusSection{Key: "security", Payload: func() any {
		return a.securitySectionValue()
	}})
	a.RegisterStatusSection(StatusSection{Key: "runtime", Payload: func() any {
		return NewRuntimeSection()
	}})
	a.RegisterStatusSection(StatusSection{Key: "llm", Payload: func() any {
		name, n := "", 0
		if a.providerManager != nil {
			n = len(a.providerManager.List())
		}
		if a.provider != nil {
			name = a.provider.Name()
		}
		return NewLLMSection(name, n)
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
	// ★ 注销本实例的输入投递口：**基座自己登记的东西自己撤**。
	//   这不是"内核侧清理"，是本实例在 kernelloop 里的那张登记 ——
	//   同名重建是常规操作，残留会让输入投到已销毁的实例（悬空引用）。
	if l := kernelloop.Current(); l != nil {
		l.UnregisterInputSink(string(a.id))
	}
	// ★ 内核侧的三件事**不在这里**（2026-11-25，docs §10「生命周期全部内核执行」）：
	//
	//	① 注销输入投递口     InstanceManager / Children().StopOne 的销毁链做
	//	② 释放安全策略视图   同上
	//	③ 从记录表删事实     同上（记录表是内核数据，类型/基座都不该各自清）
	//
	// 基座 `Stop` 只做本实例的事：停自己的运行期、补自己的队列终态、取消 context。
	// 把①~③留在这里的后果是"谁负责清理"有两个答案 —— 实测已出现
	// 同名重建漏撤销安全视图（继承上一代 guest 档 = 提权）。
	// 停机前给待办任务补终态。运行中的任务会经 cancel → LLM 失败 → emitResponse
	// 自然拿到终态，但**从未运行**（排队/待处理）与**已挂起**的任务不会有任何人
	// 回它们；带 ResponseCh 的同步注入方（cli / clawhubadapter 均无超时）会永久挂起
	// （设计 §7 I5、§11.3 X2/X4）。必须在 cancel 之前做：cancel 会让调度器直接 return。
	//
	// ★ 停机补终态：基座**不持有**队列（docs §0.3），所以由类型交出的
	//   一个钩子完成。没交（rt/worker 没有队列）⇒ 跳过。
	if a.hooks.Runtime.DrainTerminal != nil {
		a.hooks.Runtime.DrainTerminal(a, "agent_stopped")
	}
	a.cancel()
}

// graphMemoryOf 决定本 agent 的图记忆共同面实现。
//
// ★ 只有**主 agent** 有图库（`AgentConfig.Memory`）。子 agent 不再持有任何
//
//	图记忆实现 —— 它们的记忆能力来自主 agent 注册的 `graph_recall` /
//	`graph_commit` 工具（docs §6.4）。
//
// agentLogIdentity 由 `AgentConfig.TypeName` 得出日志身份。
//
// ★ 这里**不再猜类型**（2026-11-20 修）。
//
// 原先按两个布尔推断：
//
//	ParentID 非空    ⇒ resident
//	其余             ⇒ 主 agent
//
// 那是把**三种内置类型**写死进基座：
//
//	· 插件经 SDK 注册的新类型永远猜不出来（一律落到"其余"）
//	· rt 被选为主 agent 时仍被判成 KindRT
//	· 加一种类型就要回来改这个 switch（正是"基座认识类型"的证据）
//
// 现在类型名由内核在造实例时填进 `AgentConfig.TypeName`，基座只读它。
func agentLogIdentity(cfg AgentConfig) child.LogIdentity {
	kind := child.Kind(cfg.TypeName)
	if kind == "" {
		// 未填类型名 ⇒ 装配错误（契约要求内核必须填）。
		// 不静默给个笼统标签：那会让"某类型没有身份"永远查不出来。
		kind = "<untyped>"
	}
	return child.LogIdentity{Kind: kind, ID: string(cfg.ID)}
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
// IsChildAgent 报告本 agent 是否是子 agent（层级事实）。
func (a *Agent) IsChildAgent() bool {
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

// IsDuplicateInput 判断是否为短窗口内的重复输入（防 webui/GUI 断线重连消息重放）。
// key=source+"|"+content；窗口内重复返回 true 并刷新时间戳（持续轰炸时保持拦截）。
const duplicateInputWindow = 10 * time.Second

// IsDuplicateInput 判定这条输入是否是重放的重复（webui 断线重连会重放）。
func (a *Agent) IsDuplicateInput(source, content string) bool {
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

// SelfInputChan 返回自循环输入通道（只读，供内部测试验证）
func (a *Agent) SelfInputChan() <-chan SelfInputMsg {
	return a.selfInputCh
}

// injectSelf 向自循环通道发送记忆整理类内部任务（无记忆路径）。
// 线程安全，不阻塞发送者（通道缓冲 64）。
func (a *Agent) InjectSelf(task string) {
	a.InjectSelfChannel(SelfInputMsg{
		Text:    task,
		Channel: agentIO.ChannelConsolidation,
	})
}

// injectSelfChannel 向自循环通道发送一条带目标通道标志的消息。
// channel == "_consolidation_" 走无记忆整理路径；其他值走正常处理路径。
func (a *Agent) InjectSelfChannel(msg SelfInputMsg) {
	select {
	case a.selfInputCh <- msg:
	default:
		a.Logf("self input channel full, dropping task: %s", TruncateStr(msg.Text, 80))
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

// LogIdentityTag 返回本 agent 的日志身份标签（`[main]` / `[resident:<id>]`）。
func (a *Agent) LogIdentityTag() string { return a.logIdentity.LogTag() }

// AgentCapabilities 返回本 agent 声明的能力面（通道划入的 agent 侧判据）。
//
// ★ 它是**装配期声明的数据事实**（`AgentConfig.AgentCap`），不是类型分支：
//
//	基座不按它分支，只有通道划拨校验与状态面读它。
func (a *Agent) AgentCapabilities() agentIO.OutputCapability {
	if a == nil {
		return 0
	}
	return a.agentCap
}

// ── AgentHandle：执行链需要的**实例句柄**（见 kernelmod/device 的同名接口） ──
//
// 只有执行链替代不了的三样：身份、取消登记、交回基座工具。
// 安全闸 / io / 插件工具表 / 工具声明全部由 device 从设施自取。

// AgentID 实现 device.AgentHandle。
func (a *Agent) AgentID() string {
	if a == nil {
		return ""
	}
	return string(a.id)
}

// IsChild 实现 device.AgentHandle。
func (a *Agent) IsChild() bool { return a.IsChildAgent() }

// DialTool 实现 device.AgentHandle：调**基座登记的工具**。
//
// 这些工具的执行体就是基座自己的方法（`admin_child` 族 / 内核族
// `llm_*` / `secret_*` / `output_*` / `plgreload` / `get_plugin_tools` /
// `input_channels`），要读写本实例字段 —— 设施替代不了。
func (a *Agent) DialTool(ctx context.Context, call ToolCall) (string, bool) {
	if a == nil {
		return "", false
	}
	return registry.DispatchTool(registry.ToolCtx{Host: a}, agentAPI.ToolCall{
		ID: call.ID, Name: call.Name, Arguments: call.Arguments,
	})
}

// toolExecutor 返回工具执行设施。
//
// ## 为什么可以在这里直接构造（不走挂载点）
//
// 设施是**无状态纯逻辑**：它不持有任何 agent/内核数据，宿主每次随
// 实例句柄传入（见 kernelmod/device 的注释）。所以"1 份实例"与
// "每次构造"在行为上等价，而后者让基座**不依赖装配顺序**
// （agent 早于 toolexec 模块构造，运行期取挂载点会有一段 nil 窗口 ——
// 实测：判据与独立工具全在那个窗口里跑）。
//
// ★ 依赖方向：`agent` import `kernelmod/toolexec` 是**成环**的
//
//	（toolexec 通过 hostFace 需要 agent 的能力）。所以设施实例由
//	**装配侧**通过 `AgentConfig.ToolExec` 注入（`kernelmod/agent` 装它）。
//	未注入 ⇒ 响亮失败（见 RunToolBatch）。
func (a *Agent) toolExecutor() ToolExecutor {
	if a == nil {
		return nil
	}
	// ★ 未注入 ⇒ nil，由 `RunToolBatch` 响亮报错。
	//
	// 这里刻意**不**提供"内建兜底实现"：那就是第二套执行链，而两套链
	// 必然漂移（本仓已有实测教训）。需要执行工具的**判据与独立工具**应当
	// 自己注入设施（import `kernelmod/toolexec` 是合法方向：测试可以 import 任何）。
	a.semanticMu.RLock()
	injected := a.toolExec
	a.semanticMu.RUnlock()
	if injected != nil {
		return injected
	}
	// ★ 没被显式注入 ⇒ 向**内核按具名设施要**（2026-11-26 压测实测缺陷）。
	//
	// 前情：装配点只给**根实例**调了一次 `SetToolExec`，而子实例
	// （resident / rtchild / worker）由实例管理器造，**没有那一次调用** ⇒
	// `a.toolExec` 恒 nil ⇒ 子 agent 一调工具就报「工具执行设施未装配」。
	//
	// 压测现场：三个驻留子被派活后，9 次工具调用全部未执行，而主 agent
	// 一切正常 —— 症状是"子 agent 看起来在工作、实际什么工具都调不动"。
	//
	// 工具执行设施是**内核组件**（`kernelmod/toolexec` 一份实例，服务全部
	// agent，实例句柄随调用传入）。各类型 agent 对内核应一致，所以取用
	// 点是"按设施名要"，不是"装配点逐个塞" —— 后者正是漏掉子实例的原因。
	return ToolExecOf()
}

// SetToolExec 挂载工具执行设施（装配期一次）。
func (a *Agent) SetToolExec(t ToolExecutor) {
	if a == nil || t == nil {
		return
	}
	a.semanticMu.Lock()
	a.toolExec = t
	a.semanticMu.Unlock()
}
