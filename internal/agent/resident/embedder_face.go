package resident

// embedder_face.go —— 驻留子自己的稀疏计算适配器（与主 agent **各持一份**，
// 见 docs §0.3 隔离铁律：两份代码今天相同，改一方不影响另一方）。
//
// 驻留子没有记忆整理面，但它的整窗上下文仍要算向量（溢出判定要用
// 事件向量的规模），所以这一层仍需要 —— 只是能力更窄。

import "github.com/JianFeeeee/HologramAgent/internal/agent/agent"

type embedderFace struct{ a *agent.Agent }

func embedderFaceOf(a *agent.Agent) *embedderFace {
	if a == nil {
		return nil
	}
	return &embedderFace{a: a}
}

func (e *embedderFace) EmbedTexts(texts []string) []map[string]float64 {
	if e == nil || e.a == nil {
		return make([]map[string]float64, len(texts))
	}
	return e.a.EmbedTexts(texts)
}

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
