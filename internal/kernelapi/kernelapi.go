// Package kernelapi 是内核能力的**纯接口层**（叶子包）。
//
// ## 它解决什么（用户裁定 2026-11-17）
//
// > 注意解耦的目的是让其他模块在不 import 实现的前提下通过 import 接口进行调用
//
// > 现在内核明明提供了挂载公共类与共享数据的接口，为什么不通过接口解耦而是层层嵌合？
//
// 在此之前，任何想用记忆/安全/插件/知识能力的模块都必须 import 那一个
// **实现包**（`internal/memory`、`internal/security`、`internal/plugin`…），
// 于是 agent 子包在编译期被绑死在实现上，也无法被"像插件那样装配"。
//
// 现在：实现包把能力以**接口**形式挂到内核挂载点（`kstate.State.Provide`），
// 消费方 `Require(设施名) → 断言成本包接口`，编译期不再依赖任何实现。
//
// ## 依赖方向（编译器强制）
//
//	kernelapi → stdlib + kernelapi/model + agent/api + agent/io
//	实现包   → kernelapi（产出者）
//	消费方   → kernelapi（接口）+ kstate（挂载点）
//
// ## 接口按「消费方实际用到的方法集」定义，不是生产方的导出面
//
// 这是刻意的：接口越窄，实现越自由，也越不容易把实现的内部形状泄漏成契约。
// 每加一个方法都要有真实消费方；没有消费方的能力留在实现包里。
package kernelapi

import (
	"context"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// ───────────────────────── 设施名（挂载点上的键） ─────────────────────────
//
// ★ 这些名字是**模块间的协议**：提供方与消费方必须逐字一致。
// 之所以放在本包而不是 kernelmod：本包是叶子，任何消费方都能 import；
// 而 kernelmod 会拉进 kernelboot（agent 子包不许依赖它）。
// kernelmod/facilities.go 保留同名常量并转发到这里（对外 SDK 契约不变）。
const (
	// FacConfig 是配置中心（config 模块提供）。
	FacConfig = "config"
	// FacKernelIO 是 IO 抽象层（kernel-io 模块提供，类型 IO）。
	FacKernelIO = "kernel-io"
	// FacIO 是 IO 抽象层的**具名能力**（与 FacKernelIO 同值语义，供 Require 使用）。
	FacIO = "kernel-io"
	// FacEventBus 是事件总线的**代理面**（安全模块注册；消费方唯一的取用入口）。
	FacEventBus = "eventbus"
	// FacGraphOpener 是图记忆的**装配面**（memory 模块提供，类型模块用它开子视图）。
	FacGraphOpener = "memory.opener"
	// FacEventDispatch 是**投递驱动者的注入点**（安全模块提供，
	// 供运行期事件循环把 Dispatcher 挂上裸总线）。
	FacEventDispatch = "eventbus.dispatch"
	// FacEventBusRaw 是**裸总线**（kernel-io 模块创建；只有 security 取它，
	// 用来挂闸门并把它挡在消费方可达面之外）。消费方**不得**使用本设施名。
	//
	// ★ 名字本身就是一条纪律：能取到 Raw 的只有装配侧的 security。
	FacEventBusRaw = "eventbus.raw"
	// FacMemory 是图记忆（memory 模块提供，类型 GraphMemory）。
	FacMemory = "memory"
	// FacMemoryIndexer 是记忆索引器（memory 模块提供）。
	FacMemoryIndexer = "memory.indexer"
	// FacMemorySocial 是人物关系网（memory 模块提供）。
	FacMemorySocial = "memory.social"
	// FacMemoryDistiller 是蒸馏器（memory 模块提供，可暂停/恢复）。
	FacMemoryDistiller = "memory.distiller"
	// FacEmbedder 是共享词嵌入（memory 模块提供）。
	FacEmbedder = "memory.embedder"
	// FacLua 是 Lua 协议适配 VM（lua 模块提供）。
	FacLua = "lua"
	// FacSupervisor 是代理生命周期守护（supervisor 模块提供）。
	FacSupervisor = "supervisor"
	// FacTracker 是变更追踪（tracker 模块提供）。
	FacTracker = "tracker"
	// FacTextMem 是文本记忆（textmem 模块提供）。
	FacTextMem = "textmem"
	// FacLLM 是多源 provider 管理（llm 模块提供）。
	FacLLM = "llm"
	// FacLLMDefault 是默认 provider（llm 模块提供）。
	FacLLMDefault = "llm.default"
	// FacLLMClient 是 **LLM 设施**（重试/fallback/记账/活动源/分类错误，§3.2.1）。
	//
	// ★ 与 FacLLM（多源管理器）不同：管理器是"有哪些源"的**事实**，
	//   本设施是"怎么用它们"的**策略**。基座只持本设施的句柄。
	FacLLMClient = "llm.client"
	// FacDocStore 是文档记忆（docmem 模块提供）。
	FacDocStore = "docmem"
	// FacMultimodal 是多模态向量空间（multimodal 模块提供）。
	FacMultimodal = "multimodal"
	// FacMultimodalStatus 是多模态空间的启用状态（provider 名 / 失败原因）。
	FacMultimodalStatus = "multimodal.status"
	// FacONNX 是内嵌 ONNX 运行时（onnx 模块提供）。
	FacONNX = "onnx"
	// FacStageHost 是阶段管道 + 工具注册表（stage-host 模块提供）。
	FacStageHost = "stagehost"
	// FacPromptSections 是插件声明的系统提示词段（stage-host 模块提供）。
	FacPromptSections = "promptsec"
	// FacPluginRegistry 是插件注册表（stage-host 模块提供）。
	FacPluginRegistry = "plugin.registry"
	// FacPluginCatalog 是插件元数据缓存（plugin-host 模块提供）。
	FacPluginCatalog = "plugin.catalog"
	// FacPluginScheduler 是插件拉起调度器（plugin-host 模块提供）。
	FacPluginScheduler = "plugin.scheduler"
	// FacPluginHostReady 标记插件调度已完成（plugin-host 模块提供）。
	FacPluginHostReady = "plugin.host-ready"
	// FacSecurity 是安全防火墙运行时（security 模块提供）。
	FacSecurity = "security"
	// FacDevice 是设备/工具数据面（device 模块提供）。
	FacDevice = "device"
	// FacUpdate 是更新子系统的状态面（update 模块提供）。
	FacUpdate = "update"
	// FacUpdateCapabilities 是本机升级能力快照（update 模块提供）。
	FacUpdateCapabilities = "update.capabilities"
	// FacAgent 是主 agent（agent 模块提供）。
	FacAgent = "agent"
	// FacAgentRegistry 是 **agent 记录表**（唯一事实来源，见 model.AgentRecord）。
	//
	// ★ 与 FacAgent（主 agent 实例）不同：登记表描述"这台内核下有哪些 agent
	//   以及父为它们声明了什么"，与"主 agent 是谁"无关。安全模块只读它。
	FacAgentRegistry = "agent.registry"
	// FacAgentIndex 是登记表的**只读视图**（安全模块/基座渲染用）。
	FacAgentIndex = "agent.index"
	// FacSyncBus 是内核就绪状态总线（kernelboot 提供）。
	FacSyncBus = "kernel.syncbus"
	// FacMigrationPreflight 是数据目录迁移体检（migration 模块提供，可空）。
	FacMigrationPreflight = "migration.preflight"
	// FacBaseAPIKey 是网关基础 key（llm 模块提供）。
	FacBaseAPIKey = "llm.base-api-key"
	// FacDataDir 是数据目录（Group 0 提供，与 ks.DataDir 同值；给只读接口用）。
	FacDataDir = "kernel.datadir"
)

// ───────────────────────── 图记忆 ─────────────────────────

// GraphMemory 是任意 agent 都能用的图记忆面。
//
// 实现者：`*memory.GraphDB`（根 agent）与 `*memory.LightMemory`（驻留子：读
// temp∪main、只写 temp）。
type GraphMemory interface {
	// Recall 按关键词/种子实体召回。
	Recall(keywords []string, seedEntities []string, depth int, sessionFilter string) (*model.RecallResult, error)
	// RecallSorted 是可指定呈现顺序的召回（相关性 / 时间倒序）。
	RecallSorted(keywords []string, seedEntities []string, depth int, sessionFilter, fingerprint string, mode model.SortMode) (*model.RecallResult, error)

	// Commit 写入三元组，返回（旧表行数, 关系数, 错误）。
	Commit(triples []model.Triple, sessionID string, turnID int) (int, int, error)
	// CommitReady 写入三元组并返回**本次真正写入的条数**与句子块 ID。
	CommitReady(triples []model.Triple, sessionID string, turnID int) (sentenceIDs map[string]string, written int, err error)

	// RecallBlocks 按向量相似度召回块。
	RecallBlocks(q model.BlockRecallQuery) ([]model.BlockHit, error)
	// RecallBlocksGuarded 在召回前做拒答判定与时间仲裁。
	RecallBlocksGuarded(q model.BlockRecallQuery, query string) ([]model.FusedHit, *model.AbstainReason, model.ArbitrationResult, error)

	// BlockByIDPrefix 按块 ID 前缀查块（前缀歧义时 ok=false）。
	BlockByIDPrefix(prefix string) (model.MemoryBlock, bool, error)
	// BlocksForNode 查某个节点持有的块。
	BlocksForNode(nodeKind, nodeID string) ([]model.MemoryBlock, error)
	// BlockData 读块负载（媒体字节）。
	BlockData(blockID string) ([]byte, error)

	// MemoryBlockCount / MemoryEdgeCount 是库规模（诊断与"是否真写入"判据）。
	MemoryBlockCount() (int, error)
	MemoryEdgeCount() (int, error)

	// Purge 按条件删除（mode = soft / hard）。
	Purge(criteria map[string]string, mode string) (int, error)
	// FindRelations 查关系。
	FindRelations(subject, relationType, object string) ([]model.Relation, error)
	// Introspect 返回库内部统计（诊断面）。
	Introspect() (map[string]interface{}, error)
	// DeleteEntity 删除实体。
	DeleteEntity(name string) (model.DeleteResult, error)
	// MergeEntities 合并两个实体。
	MergeEntities(sourceName, targetName string) (int, error)
	// MigrateLegacyMediaEntities 把旧媒体实体迁移成原生块。
	MigrateLegacyMediaEntities(resolve LegacyMediaResolver) (blocks, entities int, err error)

	// EnterSceneWithHint 走「主动声明 + 被动涌现」两条路得到本轮场景。
	EnterSceneWithHint(sig model.Situation, declaredKeys []string) (model.TurnScene, error)
	// DecaySceneRefs 按半衰期衰减场景引用。
	DecaySceneRefs(halfLife time.Duration, floor float64) (int, error)
	// DedupeScenes 合并重复场景键。
	DedupeScenes() (int, error)
	// CleanupOrphanedSentences 清理孤立句子。
	CleanupOrphanedSentences() (int, error)
	// ClearSentenceID 断开关系边与原句的关联。
	ClearSentenceID(edgeID int64) error
	// AddMemoryBlockEdge 建块之间的结构边。
	AddMemoryBlockEdge(sourceKind, sourceID, targetKind, targetID, edgeType string) error
	// PutMemoryBlocks 写入块。
	PutMemoryBlocks(blocks []model.MemoryBlock) error
	// PutDocumentNode 建文档节点。
	PutDocumentNode(id, summary string) error
	// ScenesOfRelation 查关系挂载的场景。
	ScenesOfRelation(relationID int64) ([]string, error)
	// SetBlockVector 写块向量。
	SetBlockVector(blockID string, vec []float64, fp string) error
	// StaleBlockVectors 列出向量与当前空间不一致的块。
	StaleBlockVectors(currentFP string) ([]model.MemoryBlock, error)
	// TagSceneDocument 把文档挂到场景上。
	TagSceneDocument(sceneKey, docID string) error

	// Path 返回库文件路径（父 agent 据此派生子的 temp 路径）。
	Path() string
}

// LegacyMediaResolver 把"旧媒体实体的短 digest"解析成块（迁移用）。
type LegacyMediaResolver func(shortDigest string) (model.MemoryBlock, bool)

// ───────────────────────── 记忆索引 / 文本记忆 / 社交 ─────────────────────────

// MemoryIndexer 是记忆索引器（构建注入上下文与工具提示）。
type MemoryIndexer interface {
	BuildContextInScene(userInput string, scenes []string) *model.InjectedContext
	BuildToolPrompt() string
	FormatContext(ctx *model.InjectedContext) string
	GetToolDefinitions() []map[string]interface{}
	MarkRecalled(names ...string)
	Sync() error
	// SetDenseSpace 是装配期接线（多模态空间就绪后注入）。
	SetDenseSpace(space MultimodalSpace)
}

// TextMemory 是原始对话日志（JSONL，可滚动）。
type TextMemory interface {
	Append(evt model.TextEvent) error
	PurgeByFilter(match func(model.TextEvent) bool) (int, error)
	ReplaceByFilter(match func(model.TextEvent) bool, replace func(model.TextEvent) model.TextEvent) (int, error)
	// FileCount 返回当前滚动文件数（状态面用）。
	FileCount() int
	Stop()
}

// SocialStore 是人物特质与关系网。
type SocialStore interface {
	GetPerson(name string) (*model.PersonProfile, error)
	SetTrait(name, trait, value string) error
	AddRelation(personA, relation, personB string) error
	GetNetwork(name string, depth int) ([]*model.PersonProfile, error)
	// ListPersons 列出全部人名（状态面用）。
	ListPersons() ([]string, error)
}

// Distiller 是记忆蒸馏流水线（可暂停/恢复，供伪关机）。
type Distiller interface {
	Pause() error
	Resume() error
}

// ───────────────────────── 文档记忆 / 知识库 ─────────────────────────

// ChannelCleaner 是「给某个来源取一个清洗器」的函数类型（可 nil）。
//
// ★ 声明点在本包，实现包（coldcontext）以别名转发 —— 这样两边的
// 方法签名逐字相同，接口断言才能通过。
type ChannelCleaner func(source string) func(string) string

// DocStore 是 L2 文档记忆。
type DocStore interface {
	Insert(doc *model.ColdDoc) error
	Peek(text string, topK int) []*model.ColdDoc
	QueryScored(text string, topK int) []model.ColdHit
	FindColdContexts(maxAge time.Duration, minAccess int) []*model.ColdDoc
	NoteArchiveFailure(id, reason string) int
	Remove(id string)
	Dirty() bool
	Reindex()
	Stats() map[string]interface{}
	Stop()
	// ContextToColdContext 把 L1 事件归档成 L2 文档。
	//
	// ★ 参数用 model 值类型 + 裸函数：接口包不能 import coldcontext 实现包。
	ContextToColdContext(source string, entries []model.ContextEntry, embedder any,
		cleanFn func(string) string, toolCleanFn func(name, output string) string,
		channelCleaner ChannelCleaner) (*model.ColdDoc, error)
	SetDenseSpace(space MultimodalSpace)
	BuildDenseIndex(space MultimodalSpace)
	// SetSecretStore 注入变量库（落盘前脱敏用）。
	SetSecretStore(store SecretStore)
}

// Knowledge 是知识库（文档树 + 检索 + 导入）。

// ───────────────────────── LLM ─────────────────────────

// LLMProvider 是模型 provider（与 agent/api 的契约逐字一致）。
type LLMProvider = agentAPI.Provider

// LLMManager 是多源 provider 管理面（按消费方实际用到的方法集）。
type LLMManager interface {
	Get(name string) LLMProvider
	Default() LLMProvider
	List() []string
	OrderedProviders() []LLMProvider
	ResolveForModel(model string) []LLMProvider
	IsAvailable(name string) bool
	MarkUnavailable(name string)
	ResetAvailability(name string)
	ReportStatus(name string, statusCode int)
	ProviderCount() int
	SetDefault(name string) error
	SupportsVision(name string) bool
	SupportsAudio(name string) bool
	QuickChat(ctx context.Context, prompt string) (*agentAPI.CompletionResponse, error)
}

// MultimodalSpace 是统一多模态向量空间（内嵌 ONNX 或外部 HTTP 服务均可）。
//
// 消费方只经本接口调：具体是哪种后端属于实现细节。
type MultimodalSpace interface {
	VectorizeDense(text string) ([]float64, error)
	EmbedImageDense(img []byte, mime string) ([]float64, error)
	Fingerprint() string
	Dim() int
	Loaded() bool
	Close()
}

// ───────────────────────── 事件总线 ─────────────────────────

// EventBus 是内核事件总线的**消费面**（2026-11-17 第⑥次裁定）。
//
// ★ 它是**安全模块注册的代理面**，不是裸总线的形状：
//
//	设施表 `FacEventBus` 上放的就是它，裸总线退到 `FacEventBusRaw`
//	（只有 security 取，用来劫持与挡在表外）。
//
// ★ 方法集刻意与总线的既有 API **同形**（Publish / Subscribe 同签名、同语义），
//
//	于是调用点的写法一行不用改 —— 换的是实现，不是调用约定。
//
// 契约要点：
//
//	· 发布/订阅都要带**主体身份**（空身份 = 内核内部）
//	· 授权与出入向脱敏由代理在**发布时刻**完成；被拒必须能被调用方观察到
//	· 投递（投给谁、按什么顺序）由事件循环推动，不属于本接口的职责
type EventBus interface {
	// Publish 发布一条事件（内核身份）。
	Publish(evt *model.Event)
	// PublishAs 以指定主体身份发布；被拒返回错误（不能静默丢弃）。
	PublishAs(p model.Publisher, evt *model.Event) error
	// Subscribe 以内核身份订阅，返回退订函数。
	Subscribe(eventType model.EventType, handler model.Handler) func()
	// SubscribeAs 以指定主体身份订阅；被拒返回错误与空退订函数。
	SubscribeAs(p model.Publisher, eventType model.EventType, handler model.Handler) (func(), error)
}

// EventDispatcher 是**投递驱动者**（由事件循环实现）。
//
// ★ 为什么放在接口层：安全模块持有裸总线，而"投递由谁推动"是**运行期**
//
//	事件循环的决定，不是安全模块的决定。所以安全模块只提供**注入点**
//	（EventDispatchSink），事件循环经它把自己的 Dispatcher 挂进去 ——
//	裸总线始终不出 security 的手。
type EventDispatcher interface {
	// Dispatch 把一次**已过闸、已脱敏**的事件投给这批订阅者。
	//
	// 传进来的 evt/handlers 已经是可以被看见的东西：授权与脱敏都在
	// 发布时刻完成了。实现只负责"什么时候投、按什么顺序投"。
	Dispatch(evt *model.Event, handlers []model.Handler)
}

// EventDispatchSink 是**投递驱动者的注入点**（安全模块提供）。
//
// 只有运行期的事件循环会调它一次；消费方拿不到（也拿不到裸总线）。
type EventDispatchSink interface {
	SetDispatcher(d EventDispatcher)
}

// GraphMemoryOpener 是**图记忆的装配面**：打开只读视图与轻量实例。
//
// ★ 为什么需要它（2026-11-17 拆包）：驻留子与 rtchild 要 `OpenGraphDBReadOnly`
//   - `NewLightMemory` 来装配自己的记忆面。那两个是**实现构造**，agent 不该
//     直接调 —— 但也不能没有。于是由 memory 模块把这两个构造挂到设施表，
//     类型模块经 `Require` 取（这样"谁来构造"是装配的事，不是基座的事）。
//
// ★ 返回的是 `GraphMemory`/`any` 而不是具体类型：消费方只需要"能 Recall/Commit
//
//	的图记忆面"与"可关闭句柄"，不需要知道它是 LightMemory 还是 GraphDB。
type GraphMemoryOpener interface {
	// OpenReadOnly 按已存在的主库路径开一个**结构上写不进**的只读视图。
	OpenReadOnly(mainPath string) (GraphMemory, error)
	// OpenLight 用只读主库 + temp 路径构造轻量实例（读 temp∪main、只写 temp）。
	//
	// 返回值是 `any` 而不是 GraphMemory：LightMemory 的方法集与全量 GraphDB
	// 不同（它只实现 Recall/Commit 那一面），强行统一成同一接口就得给
	// GraphMemory 加它没有的方法 —— 那是把实现的形状泄漏成契约。
	// 消费方（驻留子/rt 的装配点）自己断言成它需要的窄面。
	OpenLight(mainRO GraphMemory, tempPath string, allowWrite bool) (any, error)
	// Close 关闭一个由本接口打开的句柄（幂等）。
	Close(m GraphMemory) error
}

// ───────────────────────── 安全闸门 ─────────────────────────

// SecurityGate 是横在 Core 与全部外部模块之间的**数据面闸门**。
//
// 它不是"某个能力"，而是边界：出向（Core → 外部）与入向（外部 → Core）
// 都要经它审查。消费方只依赖本接口，不 import security 实现包。
//
// ★ 方法集按**消费方实际用到**的（agent 侧的 security_profile.go / toolcall.go /
//
//	toolguard.go / memory_scrub.go / log_scrub.go）定义，不是生产方的导出面。
type SecurityGate interface {
	// Enabled 报告闸门是否真的生效（配置关闭时为 false 但仍是透传）。
	Enabled() bool
	// ForAgent 取某 agent 的策略执行视图（幂等）。
	ForAgent(agentID string, isChild bool) SecurityScope
	// ReleaseAgent 释放某 agent 的视图（停机/重建时）。
	ReleaseAgent(agentID string)
	// BeginRound 按事实开一轮（AgentID + 有无父 + 载荷），由 security 自己分类。
	BeginRound(in model.RoundInput) (SecurityRound, error)
	// DeclareChildPolicy 由父为子声明工具白/黑名单。
	DeclareChildPolicy(childID string, allow, deny []string) error
	// RevokeChildPolicy 撤销子策略。
	RevokeChildPolicy(childID string)
	// CheckPath / CheckNetwork 是路径与网络策略判定。
	CheckPath(direction, path string) model.Decision
	CheckNetwork() model.Decision
	// ScanContentForAgent 做内容审查，返回命中、是否阻断、替换文本。
	ScanContentForAgent(agentID string, isChild bool, ins model.Inspection, text string) ([]model.Finding, bool, string)
	// ReleaseOutputs 撤销授权声明（销毁时调，防同名重建继承旧授权）。
	ReleaseOutputs(agentID string)
	// DeclareOutputs 登记"这个 agent 被授权了哪些输出通道"（授权事实由父声明）。
	//
	// ★ 为什么它属于安全模块（用户裁定 2026-11-17：「设备是否授权不应该
	//   安全模块负责吗？」）：设备/通道授权是**横在 core 与外部之间**的
	//   边界判定，与工具策略、路径策略、内容审查同一类。授权事实由 agent
	//   提供（谁被委派了什么），**判定**由安全模块做。
	//
	// 空/nil = 完整授权（默认）。重复声明覆盖（子被缩小授权后重建是常规操作）。
	DeclareOutputs(agentID string, outputs []string)
	// CheckOutput 判定某 agent 能否使用某个输出通道（含 `device/<id>` 形态）。
	//
	// 未装配安全模块时调用方应视为放行 —— 但那是"未装配"，不是"授权通过"。
	CheckOutput(agentID string, channel string) model.Decision
	// ClampPriority 把注入方声明的中断级别**收到允许的范围**。
	//
	// ★ 这是一个**授权**判定，不是调度判定（用户裁定 2026-11-18）：
	//   L4 是"只有编译期内置插件可声明"的级别；外部插件/普通来源拿到它
	//   就能抢占全部执行且父无从分辨来源。所以夹取必须发生在**注入路径**
	//   （闸门这一侧），而不是让每个 agent 在投递时各夹一次 ——
	//   那样任何 agent 收到的 L4 都不可信。
	//
	// 语义：
	//   特权来源（内置插件 / 父 agent）⇒ 原样返回
	//   其余来源                      ⇒ 夹到 L3（LevelInteractive）
	//   未知字符串                    ⇒ 返回 fallback（"L1"），呼叫方据此降级
	ClampPriority(declared string, privileged bool) string
	// Store 返回变量库（明文 ↔ 别名）。
	Store() SecretStore
}

// SecurityScope 是单个 agent 的策略执行视图。
type SecurityScope interface {
	BeginRound(in model.RoundInput) (SecurityRound, error)
	SetLongtimeProfile(name string) error
	LongtimeProfile() string
	SessionProfile() string
	EffectiveProfile() string
	CheckTool(tool string) model.Decision
}

// SecurityRound 是一轮策略句柄。
type SecurityRound interface {
	Principal() string
	Profile() string
	EffectiveProfile() string
	CheckTool(name string) model.Decision
	End()
	CheckContent(ins model.Inspection, text string) ([]model.Finding, bool)
}

// SecretStore 是变量库（明文的唯一落盘处）。
type SecretStore interface {
	// Replace 把明文换成别名，返回替换后的文本与逐条记录。
	//
	// detect nil 表示用实现侧的内置模式库 —— 消费方**不该**为"用哪些模式"
	// import security 实现包（规则是实现细节，不是调用方的选择项）。
	Replace(text, where string, detect []model.SecretPattern) (string, []model.Evt)
	// Fill 把别名回填成明文。
	Fill(text string) (string, int, []string)
	// FillMap 对参数表做别名回填。
	FillMap(args map[string]interface{}) (int, []string)
	// Put 登记一条别名。
	Put(alias, plain, source, note string) (merged bool, err error)
	// Submit 由 agent 主动登记一条凭据，返回别名。
	Submit(plain, note string) (alias string, err error)
	// Forget 按别名删除。
	Forget(alias string) error
	// ErrUnknownAlias 报告"别名不存在"（哨兵，调用方用于给模型可执行提示）。
	ErrUnknownAlias() error
	// List 列出别名元信息（不含明文）。
	List() []model.SecretInfo
}

// ───────────────────────── 插件 ─────────────────────────

// PluginHost 是插件注册表（生命周期 + 状态面）。
type PluginHost interface {
	List() []string
	IsBuiltinPlugin(name string) bool
	AutoRestartEnabled(name string) bool
	Reload(dir string) (string, error)
	ReloadOne(name string) error
	ListLoadedPlugins() []string
	LoadOneDir(dir, name string) error
	LoadSkipping(dir string, skip map[string]bool) error
	SetLoadAllowlist(names []string)
}

// ───────────────────────── 工具 / 阶段 ─────────────────────────

// StageHost 是工具注册表与阶段管道（插件声明的执行面）。
//
// 它同时是"工具定义来源"与"工具执行入口"：工具执行必须始终经过 agent 的
// 完整执行链（schema 校验 → 授权闸 → 超时 → panic 恢复 → 脱敏），
// 本接口只提供**管道**，不替代那条链。
type StageHost interface {
	// ToolDef 取单个工具定义（nil = 不存在）。
	ToolDef(name string) interface{}
	GetToolDefs() []interface{}
	ConcurrencySafeOf(name string) (safe, found bool)
	NoMemoryOf(name string) (v, found bool)
	HasTool(name string) bool
	ToolPlugin(name string) string
	UnregisterPluginTools(pluginName string)
	ToolDefCleaner(name string) func(string) string
	ToolCount() int
	// RunStage 执行某个阶段的处理器（错误已汇总到 StageContext）。
	RunStage(stage interface{}, ctx interface{})
}

// ───────────────────────── 变更追踪 ─────────────────────────

// Tracker 是工具执行前后的文件变更追踪。
type Tracker interface {
	PreActionView(action string) *model.TrackedChange
	PostActionView(action string) *model.TrackedChange
	// MergeDir 返回变更记录的落盘目录（状态面用）。
	MergeDir() string
}

// ───────────────────────── 守护 / Lua / 同步总线 ─────────────────────────

// Supervisor 是插件与子进程的生命周期守护。
type Supervisor interface {
	StopAll()
	List() []interface{}
}

// LuaVM 是协议适配脚本 VM。
type LuaVM interface {
	Stop()
}

// SyncBus 是内核就绪状态总线（提供方读面，消费方用于等待设施就绪）。
type SyncBus interface {
	MarkReady(facility string)
	IsReady(facility string) bool
	Known(facility string) bool
	KnownFacilities() []string
	AllReady(facilities []string) bool
	Missing(facilities []string) []string
	Snapshot() map[string]bool
	ReadyInOrder() []string
	KernelComplete() bool
	OnUpdate(fn func(SyncState))
}

// SyncState 是就绪状态的只读视图（与具体总线实现解耦）。
type SyncState interface {
	IsReady(facility string) bool
	Known(facility string) bool
	KnownFacilities() []string
	AllReady(facilities []string) bool
	Missing(facilities []string) []string
	Snapshot() map[string]bool
	ReadyInOrder() []string
	KernelComplete() bool
}

// ───────────────────────── IO / 设备数据面 ─────────────────────────

// IO 是内核 IO 抽象层（输入事件、输出设备、工具执行）。
//
// ★ 它是**别名**而不是新接口：`internal/agent/io` 本身就是零依赖叶子包
// （只 import stdlib），所以这里不需要再抽一层（多一层只会多一份漂移风险）。
type IO = agentIO.IOManager

// DeviceSurface / DeviceMounter 是设备数据面的契约（声明在 agent/api，
// 因为那边的类型必须先于 agent 存在，避免 import 环）。
type DeviceSurface = agentAPI.DeviceSurface
type DeviceMounter = agentAPI.DeviceMounter
type ToolHost = agentAPI.ToolHost

// ───────────────────────── 取用辅助 ─────────────────────────

// Provider 是"能按设施名取实现"的最小面（kstate.State 满足它）。
//
// ★ 刻意用接口而不是直接 import kstate：本包是叶子，不能依赖挂载点；
// 而消费方本来就已经 import kstate，传进来即可。
type Provider interface {
	Require(name string) (any, bool)
}

// Lookup 取出并断言成某个能力接口。
//
// 用法：
//
//	gate, ok := kernelapi.Lookup[kernelapi.SecurityGate](ks, kernelapi.FacSecurity)
//
// 返回 ok=false 表示该设施未装配 —— 调用方必须显式处理（**不许静默降级**）。
func Lookup[T any](p Provider, name string) (T, bool) {
	var zero T
	if p == nil {
		return zero, false
	}
	impl, ok := p.Require(name)
	if !ok || impl == nil {
		return zero, false
	}
	typed, ok := impl.(T)
	return typed, ok
}

// MultimodalStatus 是多模态向量空间的启用状态（provider 名 / 打开失败原因）。
//
// 放在本包而不是实现包里：它是**拷问装配结果**的只读值，消费方（状态面、
// healthcheck）只需要这三个字段，不该为它们 import multimodal 实现包。
type MultimodalStatus struct {
	Provider string
	Error    string
}

// PromptSections 是插件声明的系统提示词段注册表（读面）。
type PromptSections interface {
	Snapshot() []model.PromptSection
}

// ───────────────────────── 装配期取值辅助 ─────────────────────────
//
// 下面这些是**具名设施的窄取值器**：装配模块（kernelmod/*）用它取到
// 自己依赖的能力，缺了就返回 nil（由调用方响亮报错）。
//
// 它们存在只是为了省去每个 mod.go 里重复的"Require + 断言"三行样板；
// 判据在 `ok==false` 时必须失败（装配契约），不许当默认值用。

// ───────────────────────── agent 登记（C+D，§3.2.2） ─────────────────────────

// AgentIndex 是 agent 记录的**只读视图**（叶子层，安全模块与基座都用它）。
//
// ## 为什么只读接口放这里
//
// 安全模块**不能** import `internal/agent/agent`（那是把墙焊到基座上），
// 而它需要知道"这个 agent 被授权了什么"。所以：
//
//	internal/kernelapi/model   AgentFacts / AgentRecord（纯数据）
//	internal/kernelapi         AgentIndex（只读接口，本类型）
//	内核（Group 0 之后的装配）  AgentRegistry 实现，Provide(FacAgentIndex, …)
//	kernelmod/security         Requires(FacAgentIndex)，Runtime 持只读句柄
//
// ## 契约（改语义就是改安全行为）
//
//   - **fail-open**：记录缺失（裸 agent / 单测）或 AllowedOutputs 为空 ⇒ 放行。
//     fail-open 是存量契约：`device_id` 缺失、agent 未声明授权，改造前都是放行。
//   - **deny 优先**：全局 policy deny 先判，再判父声明的 allow，顺序不变。
//   - **profiles 校验不变**：未声明的档位名照样**响亮报错**。
type AgentIndex interface {
	// Facts 返回某 agent 的父声明事实（不存在 ⇒ ok=false）。
	Facts(agentID string) (model.AgentFacts, bool)
	// Present 报告某 agent 是否有记录（存活或已知）。
	Present(agentID string) bool
}

// AgentRegistry 是 agent 记录的**写面**（只有实例生命周期写它）。
type AgentRegistry interface {
	AgentIndex
	// Register 登记一个新 agent（覆盖同 ID 的旧记录）。
	Register(rec model.AgentRecord) error
	// Update 就地改一条记录（fn 返回后落库）。
	Update(agentID string, fn func(*model.AgentRecord))
	// Remove 删除一条记录（**唯一**的清理点）。
	Remove(agentID string)
	// Records 返回全部记录（状态面与持久化用）。
	Records() []model.AgentRecord
}

// AgentRegistryOf 从闸门取出记录表的**写面**（未装配 ⇒ nil）。
//
// ★ 为什么挂在闸门上而不是单独注入：记录表与安全模块是**同一组装配事实**
//
//	（记录描述"谁被授权了什么"，闸门执行判定）。让 agent 只接一个字段，
//	就不会出现"登记表接了、闸门没接"这种半装状态。
//
// 语义：nil = 该部署没有记录表（裸 agent / 裁剪版内核）⇒ 调用方跳过登记，
// 判定侧 fail-open（与改造前逐字一致）。
func AgentRegistryOf(g SecurityGate) AgentRegistry {
	if g == nil {
		return nil
	}
	if p, ok := g.(interface{ Registry() AgentRegistry }); ok {
		return p.Registry()
	}
	return nil
}
