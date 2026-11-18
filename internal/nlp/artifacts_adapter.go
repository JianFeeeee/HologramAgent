package nlp

// artifacts_adapter.go —— 把提取器适配成 `internal/artifacts.Extractor`。
//
// ## 为什么需要这一层
//
// 基座（`agent/distill.go` 的 `coldDocToTriples`）要用"给我文本、给我三元组"
// 这一件事，而 `internal/nlp` 是一个带 ONNX 依存句法的**实现包**。
// 契约在 `internal/artifacts`（叶子）、实现在本包、适配也在本包 ——
// 于是基座不必 import nlp（判据 §8 第 3 条）。
//
// ★ 适配放**实现侧**而不是装配侧：它只依赖本包与契约包，放在这里
//   "谁是实现"一目了然；放到装配侧会让 kernelmod 多一个无谓的转接文件。

import "github.com/JianFeeeee/HologramAgent/internal/artifacts"

// sessionExtractor 是一次提取的会话（复用一个 Extractor 实例，避免每篇文档新建）。
//
// ★ 为什么不把 SetEmbedder 做成每次调用参数：embedder 在进程生命周期内
//
//	不变（装配期注入），每次传一遍纯属噪声。
type sessionExtractor struct{ e *Extractor }

// Extract 实现 artifacts.Extractor。
func (s sessionExtractor) Extract(text string) []artifacts.Triple {
	if s.e == nil || text == "" {
		return nil
	}
	set := s.e.Extract(text)
	if set == nil || len(set.Triples) == 0 {
		return nil
	}
	out := make([]artifacts.Triple, 0, len(set.Triples))
	for _, t := range set.Triples {
		out = append(out, artifacts.Triple{
			Subject:    t.Subject,
			Relation:   t.Relation,
			Object:     t.Object,
			Confidence: t.Score,
		})
	}
	return out
}

// NewArtifactsExtractor 构造契约层的提取器（embedder 可为 nil）。
//
// 装配点（`kernelmod/agent`）把它注入 `AgentConfig.Extractor`。
func NewArtifactsExtractor(embedder Vectorizer) artifacts.Extractor {
	e := NewExtractor(nil)
	if embedder != nil {
		e.SetEmbedder(embedder)
	}
	return sessionExtractor{e: e}
}
