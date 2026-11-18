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
	if cfg.Knowledge != nil {
		cfg.Knowledge.SetVectorizer(embedder)
		cfg.Knowledge.ReindexWithVectorizer(embedder)
	}

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
				log.Printf("[knowledge] multimodal dense index: built %d, skipped %d (rest hit cache)", built, skipped)
			}
		}
	}
	a.context = rc
}
