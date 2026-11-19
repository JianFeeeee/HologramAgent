package agent

// install.go —— 类型模块把自己的实现**装进**基座的四条入口。
//
// ## 为什么是"安装"而不是"基座自己 new"
//
// 内核持有基座 ⇒ 基座是唯一的运行载体；但"用哪种上下文 / 有没有调度器 /
// 哪些专属服务"是**类型**的决定。所以基座只提供安装点，具体实现在
// 类型的 Initialize 回调里装进来（`RuntimeHooks.Initialize`）。
//
// 未安装 ⇒ 基座保持 nil，所以"这不是主 agent"不靠类型分支表达，
// 而是靠"没有人给它装过"这个装配事实表达。

import (
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
)

// InstallContext 装入本类型的上下文实现（主：策略裁剪型；整窗型另装）。
func (a *Agent) InstallContext(c *RelevanceContext) {
	if a == nil {
		return
	}
	a.ctxHolder = c
}

// InstallScheduler 装入本类型的输入调度器（无队列的类型不装）。
func (a *Agent) InstallScheduler() {
	if a == nil {
		return
	}
	if a.sched == nil {
		a.sched = newScheduler(256)
	}
}

// InstallMainState 装入主 agent 的专属服务面。
//
// ★ 这是**主 agent 的**装配（记忆整理/媒体/插件/预算统计/设备）。
// 驻留子、rtchild、工作式子不调它 ⇒ 它们连一个用不到的句柄都不持有。
func (a *Agent) InstallMainState(cfg AgentConfig) {
	if a == nil {
		return
	}
	if cfg.Memory == nil && cfg.Indexer == nil && cfg.DocStore == nil &&
		cfg.PluginReg == nil && cfg.StageHost == nil {
		return
	}
	m := a.MkMain()
	m.indexer = cfg.Indexer
	m.tracker = cfg.Tracker
	m.coldcontextStore = cfg.DocStore
	m.social = cfg.SocialStore
	m.textMem = cfg.TextMemory
	m.pluginReg = cfg.PluginReg
	m.pluginDir = cfg.PluginDir
	m.stageHost = cfg.StageHost
	m.embedder = cfg.Embedder
	m.multimodalSpace = cfg.MultimodalSpace
	m.embeddingProvider = cfg.EmbeddingProvider
	m.embeddingError = cfg.EmbeddingError
	m.fusionCfg = cfg.FusionCfg
	m.personality = cfg.Personality
	m.personaStore = cfg.PersonaStore
	m.distillInterval = cfg.DistillInterval
	m.archiveInterval = cfg.ArchiveInterval
	m.reviewInterval = cfg.ReviewInterval
	m.mergeInterval = cfg.MergeInterval
	m.maxContextSize = cfg.MaxContextSize
	m.ctxTuning = cfg.CtxTuning
	m.offload = cfg.Offload
	m.inputCfg = cfg.InputProcessing
	m.thinkingEnabled = cfg.ThinkingEnabled
	m.startTime = time.Now()
	m.pluginHealth = kit.NewPluginHealthTracker()
	m.noMergeMarkers = make(map[string]int)
}

// InjectSecretStore 把安全模块的变量库注入到本 agent 的上下文与 stageHost。
//
// ★ 为什么由类型在 Initialize 里调、而不是基座在 New 里做：
// 它要依赖"类型装进来的上下文实现"。基座在 New 时可能还没有上下文。
func (a *Agent) InjectSecretStore() {
	if a == nil {
		return
	}
	gate := a.Gate()
	if gate == nil {
		return
	}
	st := gate.Store()
	if st == nil {
		return
	}
	if a.contextMgr() != nil {
		a.contextMgr().SetSecretStore(st)
	}
	if m := a.MainState(); m != nil && m.stageHost != nil {
		m.stageHost.SetSecretStore(st)
	}
	if a.providerManager != nil {
		a.providerManager.SetSecretStore(st)
	}
}

// Config 返回**归一化后的**构造参数（类型模块初始化时读它）。
//
// ★ 为什么不让类型模块自己再归一化一遍：默认值的推导只该有一处
// （`New` 顶部的那些 `if cfg.X <= 0`）。两处推导迟早漂移 —— 实测就是
// 这里丢的：测试只给了 DistillInterval，类型初始化却拿到未归一化的
// cfg，于是归档/复审/合并任务的 Interval 是 0，注册被拒，归档永远不跑。
func (a *Agent) Config() AgentConfig {
	if a == nil {
		return AgentConfig{}
	}
	return a.cfg
}
