package agent

// agentstate.go —— **类型专属状态的存放处**（拆包的结构基础，2026-11-17）。
//
// ## 问题
//
// `Agent` 结构原先同时挂着四类 agent 的状态：
//
//	主 agent     记忆整理面 / 媒体 / 插件 / 预算统计（约 32 个字段）
//	rt            rtSlot / rtMaxTools / 归档留档
//	驻留子        inputch 处理表 / contextfull 标记 / 向父汇报的回调
//	工作式子      childTasks / 序号
//
// 于是读代码时无法回答"这个字段是谁的"，也无法把一个类型整块搬走。
//
// ## 解法：按类型分组持有，基座只留一个入口
//
//	Agent.sub  —— 一张**分区表**，每个类型一格；用不到就是 nil
//
// 基座因此只有 1 个字段而不是 88 个。各类型模块（以及基座里那些**只服务
// 某个类型**的方法）经 `a.rt()` / `a.resident()` / `a.worker()` 取自己那一格。
//
// ## 为什么不是"直接搬进类型模块的结构体"
//
// 那些方法仍在基座包里（例如 `rtLoop` 要读 `rtSlot`）。把它们连方法一起搬
// 是下一步（类型成包）；在那之前，状态先按类型归位 —— 这样搬的时候
// **只需改接收者**，字段已经不再散落。
//
// ## 为什么格子是 nil 而不是零值
//
// "这个 agent 不是 rtchild" 与 "它是 rtchild 但还没开始一轮" 是两件事。
// nil 表达前者（装配错误能响亮报出来），零值会把两者混成同一个。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
	"sync"
	"time"

	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// rtState 是 rtchild 专属状态。
type rtState struct {
	// slot 是唯一输入入口（单槽覆盖：最新输入赢，旧输入被丢弃）。
	slot *rtSlot
	// maxTools 是单轮最多几批工具调用（0 = 不限）。
	maxTools int
}

// rtArchiveState 是父侧的 rt 留档（已回收实例的上下文交接数据）。
//
// ★ 它在**父**上，不在 rtchild 上：留档的意义是"回收后父按需取回原文"，
// 实例本身已经不存在了。
type rtArchiveState struct {
	mu     sync.Mutex
	closed map[string][]agentAPI.Message
}

// residentState 是驻留子专属状态。
type residentState struct {
	// notifyParent 是"子 → 父"的主动汇报回调（L3 中断）。
	notifyParent func(text string)
	// onContextFull 是 contextfull → 父侧内核级事件（L4）。
	onContextFull func()

	// ctxFullMu / ctxFullSignaled 保证 contextfull 只上报一次（防抖动刷屏）。
	ctxFullMu       sync.Mutex
	ctxFullSignaled bool

	// tableMu 保护处理表（子持有，父 pull）。
	tableMu        sync.Mutex
	table          []InputchRecord
	pending        *InputchRecord
	currentInputch string
}

// workerState 是工作式子（spawn_child）专属状态。
type workerState struct {
	mu     sync.Mutex
	nextID int64
	seq    int64
	tasks  map[string]*agenttypes.ChildTaskState
}

// mainState 是**主 agent 专属状态**（记忆整理面 / 媒体 / 插件 / 预算统计）。
//
// ★ 为什么连记忆与插件的句柄都放这里：它们不是"内核设施"，而是**主 agent
// 用它做记忆整理**的那一份装配（蒸馏/归档/合并/复审/记忆块/媒体桥）。
// 驻留子与 rtchild 是轻量内核，它们的记忆面只有 Recall/Commit（见 memoryface）。
//
// 把这些放基座，等于让每个 agent 实例都挂着一整套它用不到的实现句柄。
type mainState struct {
	// 记忆整理面（主图库 + 索引器 + 文档/知识/人物/文本记忆）
	indexer          kernelapi.MemoryIndexer
	tracker          kernelapi.Tracker
	coldcontextStore kernelapi.DocStore
	social           kernelapi.SocialStore
	textMem          kernelapi.TextMemory
	// 嵌入与多模态空间
	embedder          Embedder
	multimodalSpace   kernelapi.MultimodalSpace
	embeddingProvider string
	embeddingError    string
	fusionCfg         CrossModalFusionConfig
	// 插件面
	pluginReg    kernelapi.PluginHost
	pluginDir    string
	stageHost    *kit.StageHostType
	pluginHealth *kit.PluginHealthTracker
	// 上下文调优与维护节奏
	distillInterval time.Duration
	archiveInterval time.Duration
	reviewInterval  time.Duration
	mergeInterval   time.Duration
	maxContextSize  int
	ctxTuning       kit.ContextTuning
	offload         OffloadOptions
	inputCfg        types.InputProcessingConfig
	// 媒体与人格
	personality        *agentPkg.Personality
	personaStore       PersonaStore
	pendingMedia       map[string]interface{}
	pendingMediaBlocks []memory.MemoryBlock
	pendingToolBlocks  []interface{}
	noMergeMarkers     map[string]int
	noMergeMu          sync.Mutex
	// 工具结果预算的记账（方案 B：只统计不裁剪）
	toolResultWarnTokens int
	toolResultReporter   toolResultReporter
	// 设备数据面（device 模块挂载的那一份）
	device   agentAPI.DeviceSurface
	deviceMu sync.RWMutex
	// 内置工具处理器（路由交给 device，这里只是族实现）
	builtinHandlers agentAPI.BuiltinToolHandlers
	// 开始时间与思考开关
	startTime       time.Time
	thinkingEnabled bool
}

// subStates 是**类型分区表**（见文件头）。
type subStates struct {
	rt       *rtState
	archive  *rtArchiveState
	resident *residentState
	worker   *workerState
	main     *mainState
}

// ensure 返回分区表（`New` 里已建好；这里只是 nil 安全的兜底）。
func (a *Agent) ensureSub() *subStates {
	if a == nil {
		return nil
	}
	if a.sub == nil {
		a.sub = &subStates{}
	}
	return a.sub
}

// rt 取 rt 状态（不是 rtchild 时返回 nil）。
func (a *Agent) rt() *rtState {
	if a == nil || a.sub == nil {
		return nil
	}
	return a.sub.rt
}

// mkRT 为本次装配创建 rt 状态（rtchild 构造时调）。
func (a *Agent) mkRT(slot *rtSlot, maxTools int) {
	s := a.ensureSub()
	s.rt = &rtState{slot: slot, maxTools: maxTools}
}

// archive 取父侧的 rt 留档（不是 rt 父时返回 nil）。
func (a *Agent) ArchiveOf() *rtArchiveState {
	if a == nil || a.sub == nil {
		return nil
	}
	return a.sub.archive
}

// mkArchive 创建留档区（父第一次拉起 rt 时调）。
func (a *Agent) MkArchive() *rtArchiveState {
	s := a.ensureSub()
	if s.archive == nil {
		s.archive = &rtArchiveState{closed: map[string][]agentAPI.Message{}}
	}
	return s.archive
}

// resident 取驻留子状态（不是驻留子时返回 nil）。
func (a *Agent) Resident() *residentState {
	if a == nil || a.sub == nil {
		return nil
	}
	return a.sub.resident
}

// mkResident 为本次装配创建驻留子状态（驻留子构造时调）。
func (a *Agent) mkResident(notifyParent func(string), onContextFull func()) *residentState {
	s := a.ensureSub()
	s.resident = &residentState{notifyParent: notifyParent, onContextFull: onContextFull}
	return s.resident
}

// worker 取工作式子状态（不是工作式子父时返回 nil）。
func (a *Agent) WorkerStateOf() *workerState {
	if a == nil || a.sub == nil {
		return nil
	}
	return a.sub.worker
}

// mkWorker 创建工作式子状态（父第一次 spawn_child 时调）。
//
// ★ 调用方持锁：并发 spawn 时不能各建一张表（两张表会丢一半任务）。
func (a *Agent) MkWorker() *workerState {
	s := a.ensureSub()
	if s.worker == nil {
		s.worker = &workerState{tasks: map[string]*agenttypes.ChildTaskState{}}
	}
	return s.worker
}

// mainState 取主 agent 状态（**惰性创建**，零值可用）。
//
// ★ 为什么惰性而不是 nil：这些字段的读写散在 15 个文件的 300 处，
// 逐个判 nil 会把"这不是主 agent"这个装配事实变成 300 个分支。
// 语义边界改由**谁填了这些字段**表达（只有主 agent 的装配点会填），
// 而基座一行代码都不认识"主 agent"。
func (a *Agent) MainState() *mainState {
	if a == nil {
		return nil
	}
	s := a.ensureSub()
	if s == nil {
		return nil
	}
	if s.main == nil {
		s.main = &mainState{}
	}
	return s.main
}

// mkMain 创建主 agent状态（主 agent 构造时调）。
func (a *Agent) MkMain() *mainState {
	s := a.ensureSub()
	if s.main == nil {
		s.main = &mainState{}
	}
	return s.main
}
