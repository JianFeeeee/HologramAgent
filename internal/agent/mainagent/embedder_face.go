package mainagent

// embedder_face.go —— 把基座的稀疏语义计算 syscall 包成本类型上下文要的
// `ctx.Embedder`（批量句进 → 向量出）。
//
// ★ 为什么不直接传 `*agent.Agent`：ctx 的接口只有两个方法（EmbedTexts /
// Loaded），传整个基座进去等于把几十个 syscall 都暴露给它 —— 上下文的
// 职责是"算相关性与渲染"，不是"调工具、管子 agent"。窄接口是边界。

import "github.com/JianFeeeee/HologramAgent/internal/agent/agent"

// embedderFace 是本类型私有的稀疏计算适配器。
type embedderFace struct{ a *agent.Agent }

// embedderFaceOf 返回适配器（nil agent ⇒ nil，ctx 侧自己 nil-safe）。
func embedderFaceOf(a *agent.Agent) *embedderFace {
	if a == nil {
		return nil
	}
	return &embedderFace{a: a}
}

// EmbedTexts 批量句 → 稀疏向量（docs §3.4：一 call 多发）。
func (e *embedderFace) EmbedTexts(texts []string) []map[string]float64 {
	if e == nil || e.a == nil {
		return make([]map[string]float64, len(texts))
	}
	return e.a.EmbedTexts(texts)
}

// Loaded 报告词嵌入是否就绪（未就绪 ⇒ 只有 TF-IDF 词频回退）。
func (e *embedderFace) Loaded() bool {
	if e == nil || e.a == nil {
		return false
	}
	return e.a.EmbedderLoaded()
}

// DenseSimilarity 两个稠密向量的余弦（转发基座 syscall）。
func (e *embedderFace) DenseSimilarity(a, b []float64) float64 {
	if e == nil || e.a == nil {
		return 0
	}
	return e.a.DenseSimilarity(a, b)
}
