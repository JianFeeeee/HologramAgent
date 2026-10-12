package core

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	agentPkg "github.com/JianFeeeee/HomeAgent/internal/agent"
	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	"github.com/JianFeeeee/HomeAgent/internal/events"
	"github.com/JianFeeeee/HomeAgent/internal/knowledge"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/memory/document"
	"github.com/JianFeeeee/HomeAgent/internal/memory/social"
	"github.com/JianFeeeee/HomeAgent/internal/memory/text"
	"github.com/JianFeeeee/HomeAgent/internal/memory/vector"
	"github.com/JianFeeeee/HomeAgent/internal/plugin"
	"github.com/JianFeeeee/HomeAgent/internal/tracker"
	"github.com/JianFeeeee/HomeAgent/pkg/types"
)

// ContextEvent 和 RelevanceContext 定义在 context.go

// Agent — 单 agent，不区分会话/实例
//
// 并发现状（M3a 起）：所有任务状态只由 **schedulerLoop goroutine** 独占读写，
// 因此不再有保护整轮执行的互斥量——挂起不能持锁（见 docs/zh/input-scheduler-design.md §8.1 I3）。
// 仍需跨 goroutine 保护的是：childMu/llmMu/lastInputMu/noMergeMu 与各子系统自己的锁；
// interceptLoop 只允许触碰 preemptionRequest 与 cancelLLM（经 llmMu）。
type Agent struct {
	id              types.AgentID
	provider        agentAPI.Provider
	providerManager *agentAPI.ProviderManager
	io              *agentIO.IOManager
	memory          *memory.GraphDB
	// graph 是本 agent 的**图记忆共同面**（根 = 同一个 GraphDB；驻留子 = LightMemory）。
	// 整理面仍走 memory 字段（子为 nil ⇒ 既有的 nil 关卡自动禁用整理面）。
	graph GraphMemory

	// kernelSource/parentID/taskPrompt/dataDir：驻留子相关的层级信息（见 AgentConfig）。
	kernelSource string
	parentID     string
	taskPrompt   string
	dataDir      string

	// 驻留子（父侧）：登记表 + 子侧钩子。
	residentMu sync.Mutex
	residents  map[string]*residentChild

	// 子侧：向父发消息（L3）与 contextfull 上报（父侧内核级事件）的钩子。
	notifyParent    func(text string)
	onContextFull   func()
	ctxFullSignaled bool

	// 子侧：inputch 处理表（子持有，父 pull）。
	tableMu        sync.Mutex
	inputchTable   []InputchRecord
	inputchPending *InputchRecord
	currentInputch string
	indexer        *memory.Indexer
	tracker        *tracker.Tracker
	context        *RelevanceContext
	systemPrompt   string
	ctx            context.Context
	cancel         context.CancelFunc

	// 文档记忆（第二层）
	docStore *document.Store

	// 知识库
	knowledge *knowledge.Store

	// 人物特质与关系网
	social *social.SocialStore

	// 文本记忆（原始对话日志）
	textMem *text.Memory

	// 媒体存储句柄已随 CAS 淘汰（块自带负载，见 plan.md）。

	// 人格设定（内容来自启动时载入的人格文件/配置项）
	personality *agentPkg.Personality

	// 人格落库面：首启门禁与 persona_set 工具使用（见 persona.go）。
	// 为 nil 时门禁与工具都静默关闭（例如单测里不接配置的场景）。
	personaStore PersonaStore

	// 被授权的输出通道集合（空 = 完整授权，见 AgentConfig.AllowedOutputs）。
	allowedOutputs []string

	// toolResultWarnTokens 是「单条工具结果过大」的告警阈值（0 = 用默认）。
	// ⚠️ 方案 B 只**统计与报告**，绝不裁剪（见 toolresult_budget.go 的理由）。
	toolResultWarnTokens int
	// toolResultReporter 报告超限；nil 时用 logReporter。
	toolResultReporter toolResultReporter

	// 插件注册表（用于 plgreload）
	pluginReg *plugin.Registry
	pluginDir string

	// 定期心跳蒸馏
	distillInterval time.Duration

	// 三个独立心跳任务间隔
	archiveInterval time.Duration // 冷文档归档
	reviewInterval  time.Duration // 关系复审
	mergeInterval   time.Duration // 实体合并检测

	// 上下文裁剪：活跃上下文最大条数，超出按相关性裁剪
	maxContextSize int

	// overflowStat 统计上下文超页处理（触发次数、裁剪条数、终止次数）。
	// 放 Agent 上而非全局：多实例并存时各自独立（跑分台就要实例隔离）。
	overflowStat struct {
		sync.Mutex
		Checked    int // 判据执行次数（诊断：区分「没触发」与「没执行」）
		Triggered  int // L4 上报次数
		Aborted    int // 裁不出东西的终止次数
		Upstream   int // 上游 ErrContextFull 次数
		LastPruned int // 上次裁剪条数
		LastRatio  float64
		LastEvents int
	}

	// ctxTuning 是上下文预算的可调参数（零值 = 历史默认）。
	//
	// 从配置读入（core.agent.context.*），使不同窗口的实例可以各自调优，
	// 而不是所有实例共用一套写死的 0.8 / 600000 / 1:3。
	ctxTuning ContextTuning

	// 当前请求的输出通道（mutex 保护，process() 内独占）

	// 阶段管道：插件消息流编辑
	stageHost    *StageHost
	eventBus     *events.Bus
	pluginHealth *pluginHealthTracker

	// 自循环输入通道：核心内部任务（记忆消歧、系统维护、子 Agent 通知），
	// 不经过 IO 层。每条消息携带目标输出通道：
	//   "_consolidation_" = 记忆整理（无记忆路径，不写入上下文、不 emit 响应）
	//   其他 = 正常处理（写入上下文、emit 响应到该通道）
	selfInputCh chan selfInputMsg

	// 子任务异步执行
	childMu     sync.Mutex
	childNextID int64
	// childTasks 记录子任务状态：运行中 / 结果 / 是否已交付。
	//
	// 为什么保留结果而不是“读到即删”：完成通知会写进持久上下文
	// （formatMergedTimeline 每轮都重新注入），模型之后还会再查。若读到即删，
	// 第二次查询就得到“不存在或已过期”这个**永久失败信号**——模型据此认为
	// 任务未完成，会无限重试/汇报（实测单轮 35 次工具调用、持续 514 秒）。
	childTasks map[string]*childTaskState
	// childSeq 给完成的任务排个序，用于有界淘汰。
	childSeq int64

	// 输入调度器：就绪队列、任务抽象与快照（见 scheduler.go）。
	// M2 起取代 eventLoop 的隐式 channel 排队。
	sched *scheduler

	// 工具轮次硬上限（0 = 不限）；见 AgentConfig.MaxToolTurns。
	maxToolTurns int
	// offload 是积压任务自动转投的参数（见 offload.go）。
	offload OffloadOptions

	// 进行中的 LLM 请求取消函数，interceptLoop 可调用以在请求中打断
	cancelLLM context.CancelFunc
	llmMu     sync.Mutex

	// 模型思考模式（thinking/reasoning）
	thinkingEnabled bool

	pendingToolBlocks []interface{} // 插件工具通过 SetToolBlocks 注入的多模态块（type agentAPI.ContentBlock），process.go 消费后追加到 tool message

	// 启动时间
	startTime time.Time

	// 当前轮次的非文本媒体数据（图片/音频），供 describe_image 等工具访问
	pendingMedia map[string]interface{}

	// pendingMediaBlocks 累积本轮已捕获的媒体块。
	//
	// 需要缓存而不是当场挂到事件上：媒体在 process() 执行期间被捕获，
	// 而承载它的 ContextEvent 要等 process() 返回后才 Append——此刻还没有 owner。
	// 由 schedulerLoop goroutine 独占读写。
	pendingMediaBlocks []memory.MemoryBlock

	// 当前输入是否为工具提醒/中断（以 system 角色注入，避免被当成用户消息）
	interruptInput bool

	// 非文本输入处理配置
	inputCfg types.InputProcessingConfig

	// noMergeMarkets 记录被标记"禁止合并"的实体对，key="entityA||entityB"（字典序），
	// 每次 reorgGraph 扫描到对应实体对时计数减一，归零后自动移除。
	noMergeMarkers map[string]int
	noMergeMu      sync.Mutex

	// TerminalRegistry 是终端会话与命令历史的权威视图（“内核开，两个插件接”）。
	// 内核订阅自己的事件总线归并而来；WebUI/CLI 经 KernelStatus 读取。
	terminalReg *TerminalRegistry

	// 输入去重：防 webui/GUI 断线重连导致的消息重放
	// key=source+"|"+content, value=上次接收时间；短窗口内同内容丢弃
	lastInput   map[string]time.Time
	lastInputMu sync.Mutex

	// 词嵌入模型，用于实体语义相似度计算
	embedder *memory.StaticEmbedder

	// multimodalSpace 是统一多模态向量空间（可选）。实现可以是内嵌 ONNX，
	// 也可以是外部 API 客户端；两者共享同一套 L0/L2/L3 向量缓存与检索基础设施。
	multimodalSpace vector.MultimodalEmbedder

	// embeddingProvider 是配置里指定的统一向量空间 provider 名；
	// embeddingError 是打开/适配失败的原因（成功时为空）。
	// 二者只用于状态报告：区分「没配」「配了但打不开」「已启用」。
	embeddingProvider string
	embeddingError    string

	// fusionCfg 控制文本路与视觉路的跨模态融合权重，可按模型实测结果配置。
	fusionCfg CrossModalFusionConfig

	// 技能索引提供者：由 skillmgr 插件实现，向 system prompt 注入轻量技能索引
	skillIndex SkillIndexProvider

	// usageLedger 累计**跨调用**的 token 用量与缓存命中。
	//
	// 为何是 Agent 级而不是 TaskFrame 级：「这个会话花了多少、缓存省了多少」
	// 不是单次调用的属性，必须跨轮次、跨任务累积才有意义。
	// 单次用量一直在 StageCtx 与 LLM chain 事件里，缺的正是这个落点。
	usageLedger usageLedger
}

// SkillIndexProvider 提供已加载技能的精炼索引，供 buildSystemPrompt 注入。
// 实现方（skillmgr）需线程安全并快速返回（每次 LLM 调用都会调用）。
type SkillIndexProvider interface {
	// SkillIndex 返回多行文本的技能索引，每行形如 "name vX.Y - description"；
	// 无技能时返回空串。
	SkillIndex() string
}

type AgentConfig struct {
	ID              types.AgentID
	SystemPrompt    string
	Provider        agentAPI.Provider
	ProviderManager *agentAPI.ProviderManager
	IO              *agentIO.IOManager
	Memory          *memory.GraphDB
	// LightMemory 是**轻量内核**的图记忆装配（驻留子用；读 temp∪main，只写 temp）。
	//
	// 给了它就意味着这是轻量内核：`Memory` 必须为 nil，
	// 于是记忆整理面（块/媒体/流水线/整理工具）全部不可达（见 memoryface.go）。
	LightMemory *memory.LightMemory
	Indexer     *memory.Indexer
	Tracker     *tracker.Tracker

	DocStore        *document.Store
	Knowledge       *knowledge.Store
	SocialStore     *social.SocialStore
	TextMemory      *text.Memory
	// 媒体存储句柄已随 CAS 淘汰（块自带负载，见 plan.md）。
	MultimodalSpace vector.MultimodalEmbedder
	// EmbeddingProvider / EmbeddingError 是向量空间的配置身份与打开失败原因，
	// 供 healthcheck_kernel 状态报告区分「未配置 / 打开失败 / 已启用」。
	EmbeddingProvider string
	EmbeddingError    string
	FusionCfg         CrossModalFusionConfig // 跨模态融合权重；零值用默认
	Personality       *agentPkg.Personality
	PersonaStore      PersonaStore // 人格设定的读写面（首启门禁 + persona_set 工具）
	PluginReg         *plugin.Registry
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
	// AllowedOutputs 是本 agent **被授权的输出通道集合**（设计 §4.4 / R2）。
	//
	// nil 或空 = **完整授权**（默认）；非空 = 白名单，只允许列出的输出通道。
	// 父 agent 创建驻留子时用它收窄子的输出能力。
	AllowedOutputs  []string
	PluginDir       string
	DistillInterval time.Duration
	ArchiveInterval time.Duration // 冷文档归档间隔（L2→L3），0 则使用 DistillInterval
	ReviewInterval  time.Duration // 关系复审间隔，0 则使用 DistillInterval
	MergeInterval   time.Duration // 实体合并检测间隔，0 则使用 DistillInterval
	MaxContextSize  int           // 活跃上下文最大条数，超出按相关性裁剪

	// CtxTuning 是上下文预算的可调参数。零值 ⇒ 使用历史默认（与硬编码时代一致）。
	//
	// 为何必须能从配置传进来：这些阈值原本写死在 ComputeTokenBudget 里，
	// 界面改不了、不同窗口的实例也没法各自调优（详见 ContextTuning 的注释）。
	CtxTuning          ContextTuning
	ContextSavePath    string                 // 上下文持久化路径，空则不持久化
	EmbeddingModelPath string                 // 预训练词嵌入模型路径（word2vec 文本格式），空则不使用
	Embedder           *memory.StaticEmbedder // 共享词嵌入实例；nil 时按 EmbeddingModelPath 自建
	StageHost          *StageHost
	EventBus           *events.Bus
	ThinkingEnabled    bool

	SkillIndexProvider SkillIndexProvider

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

	embedder := cfg.Embedder
	if embedder == nil {
		embedder = memory.NewStaticEmbedder(strings.Split(cfg.EmbeddingModelPath, ",")...)
	}
	if cfg.DocStore != nil {
		// TF-IDF 内置为 fallback，无需外部注入
	}
	if cfg.Knowledge != nil {
		cfg.Knowledge.SetVectorizer(embedder)
		cfg.Knowledge.ReindexWithVectorizer(embedder)
	}

	rc := NewRelevanceContext(cfg.ContextSavePath, embedder)
	// 裁剪保留条数：从配置传入（0 时保持 NewRelevanceContext 的历史默认）。
	if cfg.CtxTuning.ProtectedCount > 0 {
		rc.protectedCount = cfg.CtxTuning.ProtectedCount
	}
	if cfg.StageHost != nil {
		rc.SetToolDefLookup(cfg.StageHost.ToolDef)
	}
	if cfg.IO != nil {
		rc.SetChannelDefLookup(cfg.IO.GetInputChannelDef)
	}
	// 注入稠密多模态向量空间（可选）：配置后文档检索、L0 相关性裁剪、
	// 跨模态检索全部共享同一向量空间，取代稀疏 fastText 语义路。
	// 未配置时退化到 TF-IDF/fastText 稀疏检索，保持既有行为。
	if cfg.MultimodalSpace != nil && cfg.MultimodalSpace.Loaded() {
		rc.SetDenseSpace(cfg.MultimodalSpace)
		if cfg.DocStore != nil {
			cfg.DocStore.SetDenseSpace(cfg.MultimodalSpace)
			cfg.DocStore.BuildDenseIndex(cfg.MultimodalSpace)
		}
		// 知识库接入同一多模态空间：媒体作为一等节点参与稠密召回，
		// 于是「按图搜知识」「按文搜含图知识」成立。
		//
		// 稀疏两路（词向量 + TF-IDF）**保持启用**且仍是主召回路径：多模态
		// 空间未配置时知识库行为与此前逐字一致（退化为 0.5/0.5 两路融合）。
		if cfg.Knowledge != nil {
			// 顺序要紧：先接线再重建。ReindexDense 会尝试从 .dense.json 缓存恢复，
			// 恢复不了才重算，最后把结果落盘。若先重建后接线，首次启动算出的
			// 向量会被丢掉而不落盘。
			cfg.Knowledge.SetDenseSpace(cfg.MultimodalSpace)
			// 媒体字节随知识条目自带（CAS 淘汰后）：denseFor 直接读条目目录里的
			// 媒体文件，无需 mediaGet/mediaPut 接线。
			built, skipped := cfg.Knowledge.ReindexDense()
			if built > 0 || skipped > 0 {
				log.Printf("[knowledge] 多模态稠密索引: 新建 %d 跳过 %d（其余命中缓存）", built, skipped)
			}
		}
	}

	a := &Agent{
		id:                cfg.ID,
		startTime:         time.Now(),
		provider:          cfg.Provider,
		providerManager:   cfg.ProviderManager,
		io:                cfg.IO,
		memory:            cfg.Memory,
		graph:             graphMemoryOf(cfg),
		kernelSource:      cfg.KernelSource,
		parentID:          cfg.ParentID,
		taskPrompt:        cfg.TaskPrompt,
		dataDir:           cfg.DataDir,
		indexer:           cfg.Indexer,
		tracker:           cfg.Tracker,
		context:           rc,
		systemPrompt:      cfg.SystemPrompt,
		ctx:               ctx,
		cancel:            cancel,
		docStore:          cfg.DocStore,
		knowledge:         cfg.Knowledge,
		social:            cfg.SocialStore,
		textMem:           cfg.TextMemory,
		// mediaStore 已随 CAS 淘汰：块自带负载。知识层的媒体也已自带字节
		// （见 knowledge 的媒体侧车），不再需要外部 getter/putter 接线。
		personality:       cfg.Personality,
		personaStore:      cfg.PersonaStore,
		allowedOutputs:    cfg.AllowedOutputs,
		pluginReg:         cfg.PluginReg,
		pluginDir:         cfg.PluginDir,
		distillInterval:   cfg.DistillInterval,
		archiveInterval:   cfg.ArchiveInterval,
		reviewInterval:    cfg.ReviewInterval,
		mergeInterval:     cfg.MergeInterval,
		maxContextSize:    cfg.MaxContextSize,
		ctxTuning:         cfg.CtxTuning,
		stageHost:         cfg.StageHost,
		skillIndex:        cfg.SkillIndexProvider,
		eventBus:          cfg.EventBus,
		selfInputCh:       make(chan selfInputMsg, 64),
		childTasks:        make(map[string]*childTaskState),
		sched:             newScheduler(256),
		maxToolTurns:      cfg.MaxToolTurns,
		offload:           cfg.Offload,
		pluginHealth:      newPluginHealthTracker(),
		thinkingEnabled:   cfg.ThinkingEnabled,
		inputCfg:          cfg.InputProcessing,
		embedder:          embedder,
		multimodalSpace:   cfg.MultimodalSpace,
		embeddingProvider: cfg.EmbeddingProvider,
		embeddingError:    cfg.EmbeddingError,
		fusionCfg:         cfg.FusionCfg,
		noMergeMarkers:    make(map[string]int),
		lastInput:         make(map[string]time.Time),
	}

	// 终端权威注册表只归**根 agent**（无 ParentID）。驻留子共用同一事件总线，
	// 若每个子都建一份并订阅，一次工具调用会被 N+1 份重复记账；而终端本就是
	// 内核级设备，不属于任何单个驻留子。
	if cfg.ParentID == "" {
		a.terminalReg = NewTerminalRegistry()
	}

	// 输入路由：inputch 是可分配资源，划给某个 agent 后输入**只**流向那个 agent
	// （设计 §4.1「路由发生在进内核之前」）。io 层不认识 agent，所以在这里把路由器
	// 注入进去：插件注入输入时先问它，被别的 agent 接管就不再进本内核队列。
	if a.io != nil {
		a.io.SetInputRouter(a.routeInputByOwner)
	}
	return a
}

// SetSkillIndexProvider 注入技能索引提供者（skillmgr 插件加载后由 main 接线）。
func (a *Agent) SetSkillIndexProvider(p SkillIndexProvider) { a.skillIndex = p }

func (a *Agent) Start() {
	go a.schedulerLoop()
	go a.interceptLoop()
	go a.distillLoop()
	go a.archiveLoop()
	go a.mergeLoop()
	go a.reviewLoop()
	go a.offloadLoop()
	a.subscribeTerminalRegistry()
	a.reembedStaleMedia()
	a.migrateLegacyGraphMedia()
	log.Printf("[agent] %s started, waiting for IO interrupts", a.id)
}

// subscribeTerminalRegistry 让内核的终端/命令历史权威视图归并事件流。
//
// 内核自己发 EventToolCall（agent 路径），agentcli 发 EventTerminalOutput
// （含生命周期事件）。两者都进这份唯一真相，WebUI/CLI 不再各自推导。
func (a *Agent) subscribeTerminalRegistry() {
	if a.eventBus == nil || a.terminalReg == nil {
		return
	}
	a.eventBus.Subscribe(events.EventToolCall, func(ev *events.Event) {
		a.terminalReg.OnToolCall(ev.Payload)
	})
	a.eventBus.Subscribe(events.EventTerminalOutput, func(ev *events.Event) {
		a.terminalReg.OnTerminalOutput(ev.Payload)
	})
}

func (a *Agent) Stop() {
	// 父退出**必须**销毁全部驻留子（设计 §10 硬约束：子不得比父活得久、不留孤儿）。
	a.StopResidents()
	// 停机前给待办任务补终态。运行中的任务会经 cancel → LLM 失败 → emitResponse
	// 自然拿到终态，但**从未运行**（排队/待处理）与**已挂起**的任务不会有任何人
	// 回它们；带 ResponseCh 的同步注入方（cli / clawhubadapter 均无超时）会永久挂起
	// （设计 §7 I5、§11.3 X2/X4）。必须在 cancel 之前做：cancel 会让调度器直接 return。
	a.drainPendingInterrupts("agent_stopped")
	a.cancel()
}

// drainPendingInterrupts 给排队/待处理/已挂起任务中带同步回执通道的调用方补一条
// skipped 终态（复用 emitSkippedReply：非阻塞写，不对外发 agent_output 事件）。
func (a *Agent) drainPendingInterrupts(reason string) {
	if a.sched == nil {
		return
	}
	pending := a.sched.pendingEvents()
	if len(pending) == 0 {
		return
	}
	for _, evt := range pending {
		a.emitSkippedReply(evt, reason)
	}
	log.Printf("[agent] %s: 停机，%d 条待办任务已补 skipped 终态", a.id, len(pending))
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

// graphMem 返回本 agent 的图记忆**共同面**。
//
// `graph` 显式为 nil 时回落到 `memory` —— 这样"只设 memory 的构造"
// （大量既有测试直接用 Agent 字面量）照常工作，不需要同时维护两个字段。
// 轻量内核则显式设 graph=LightMemory 且 memory=nil：共同面走 LightMemory，
// 整理面因 memory==nil 而全部不可达。
func (a *Agent) graphMem() GraphMemory {
	if a.graph != nil {
		return a.graph
	}
	if a.memory == nil {
		return nil
	}
	return a.memory
}

func (a *Agent) ID() types.AgentID { return a.id }

// IsOutputAllowed 报告某个输出通道是否被授权给本 agent。
//
// 默认（未配置白名单）= **完整授权**；这是"默认完整授权、父可收窄"的落点。
func (a *Agent) IsOutputAllowed(channel string) bool {
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
func (a *Agent) injectSelf(task string) {
	a.injectSelfChannel(selfInputMsg{
		text:    task,
		channel: channelConsolidation,
	})
}

// injectSelfChannel 向自循环通道发送一条带目标通道标志的消息。
// channel == "_consolidation_" 走无记忆整理路径；其他值走正常处理路径。
func (a *Agent) injectSelfChannel(msg selfInputMsg) {
	select {
	case a.selfInputCh <- msg:
	default:
		log.Printf("[agent] self input channel full, dropping task: %s", truncateStr(msg.text, 80))
	}
}
