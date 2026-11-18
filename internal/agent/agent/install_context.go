package agent

import "log"

// InstallMainRuntime 装入**主 agent 的上下文实现**并与服务接线。
//
// ★ 为什么这段不在 Agent.New 里：它构造的是 `RelevanceContext`（策略裁剪型），
// 而"用哪种上下文"是类型的决定。整窗型的类型根本不进这条路径。
func (a *Agent) InstallMainRuntime(cfg AgentConfig) {
	if a == nil {
		return
	}
	embedder := cfg.Embedder
	if embedder == nil {
		// 未注入共享实例时自建：`memory.NewStaticEmbedder` 是**实现构造**，
		// 但 agent 不允许 import 实现包 ⇒ 构造点挪到装配处（kernelmod/agent
		// 从 kernelapi.FacEmbedder 取）。这里只接受已注入的实例。
		log.Printf("[agent] WARNING: no shared embedder injected — embedding-based similarity disabled " +
			"(assembly should Provide(kernelapi.FacEmbedder, *memory.StaticEmbedder))")
	}
	// DocStore 无需处理：TF-IDF 内置为 fallback，不需要外部注入向量器。

	rc := NewRelevanceContext(cfg.ContextSavePath, cfg.Embedder)
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
		// 场景召回的相关性判定用同一空间：块里存的就是这个空间的向量，
		// 所以"相关"只是一次余弦比较（缺了它就只能按时间倒序灌旧事实）。
		if cfg.Indexer != nil {
			cfg.Indexer.SetDenseSpace(cfg.MultimodalSpace)
		}
		if cfg.DocStore != nil {
			cfg.DocStore.SetDenseSpace(cfg.MultimodalSpace)
			cfg.DocStore.BuildDenseIndex(cfg.MultimodalSpace)
		}
	}
	a.context = rc
}
