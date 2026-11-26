package nlp

import (
	"sort"

	"github.com/JianFeeeee/HologramAgent/internal/vector"
)

// Parser 依存句法分析器接口
type Parser interface {
	Parse(text string) (*ParseResult, error)
}

// defaultParser 包级默认解析器，由 SetDefaultParser 设置
var defaultParser Parser

// SetDefaultParser 设置包级默认解析器。
// 设置后，NewExtractor(nil) 将使用此解析器而非纯降级模式。
func SetDefaultParser(p Parser) {
	defaultParser = p
}

// GetDefaultParser 返回当前包级默认解析器
func GetDefaultParser() Parser {
	return defaultParser
}

// Vectorizer 是**语义计算面**（docs §3.4）：批量句进 → 稀疏向量出。
//
// ★ 批量是形状而不是优化：一次 call 传 N 句、返回 N 个向量（索引对应）。
// 单发场景写长度 1 的调用。这样 `FacEmbedder` 提供的唯一计算面就能
// 直接满足它，不必为"单发/批量"各立一个接口。
type Vectorizer interface {
	EmbedTexts(texts []string) []map[string]float64
	Loaded() bool
}

// ExtractorConfig 三元组提取器配置
type ExtractorConfig struct {
	FusionAlpha     float64 // syntax_conf 权重，默认 0.4
	FusionBeta      float64 // vector_conf 权重，默认 0.6
	FusionThreshold float64 // 最终阈值，默认 0.3
}

// DefaultExtractorConfig 返回默认的提取器配置
func DefaultExtractorConfig() ExtractorConfig {
	return ExtractorConfig{
		FusionAlpha:     0.4,
		FusionBeta:      0.6,
		FusionThreshold: 0.3,
	}
}

// Extractor 三元组提取器
type Extractor struct {
	parser    Parser
	fallack   Parser     // 降级用 POS 模板解析器
	embedder  Vectorizer // 可选：用于 TransE 语义验证
	fusionCfg ExtractorConfig
}

// NewExtractor 创建提取器。
// parser 为 nil 时尝试使用包级默认解析器 (SetDefaultParser)，
// 若仍未设置则纯用 fallback (POS 模板匹配)。
func NewExtractor(parser Parser) *Extractor {
	if parser == nil {
		parser = defaultParser
	}
	return &Extractor{
		parser:    parser,
		fallack:   newFallbackParser(),
		fusionCfg: DefaultExtractorConfig(),
	}
}

// SetEmbedder 设置词嵌入向量化器，用于候选三元组的语义验证
func (e *Extractor) SetEmbedder(ev Vectorizer) {
	e.embedder = ev
}

// SetFusionWeights 设置三元组融合裁决的权重参数。
//   - alpha: syntax_conf 权重 (默认 0.4)
//   - beta:  vector_conf 权重 (默认 0.6)
//   - threshold: 最终阈值 (默认 0.3)
func (e *Extractor) SetFusionWeights(alpha, beta, threshold float64) {
	e.fusionCfg.FusionAlpha = alpha
	e.fusionCfg.FusionBeta = beta
	e.fusionCfg.FusionThreshold = threshold
}

// FusionConfig 返回当前融合裁诀配置
func (e *Extractor) FusionConfig() ExtractorConfig {
	return e.fusionCfg
}

// Extract 从文本中提取三元组（完整四阶段流水线）
//
//	Phase 1: 句法解析（LTP 分词 → POS 标注 → 依存句法树）
//	Phase 2: 结构初筛（依存模板 / POS 模板 → 候选三元组 + syntax_conf）
//	Phase 3: 语义验证（TransE h+r≈t → vector_conf）
//	Phase 4: 融合裁决（线性加权 → 阈值截断 → 降序输出）
func (e *Extractor) Extract(text string) *TripleSet {
	if text == "" {
		return &TripleSet{Src: "", Err: nil}
	}

	var allTriples []Triple
	src := ""

	sentences := splitSentences(text)
	for _, sentence := range sentences {
		if sentence == "" {
			continue
		}
		var triples []Triple

		// ——— Phase 1 & 2: 句法解析 + 结构初筛 ———
		if e.parser != nil {
			result, err := e.parser.Parse(sentence)
			if err == nil && result != nil && len(result.Tokens) > 1 {
				triples = extractFromDep(result, sentence)
				if len(triples) > 0 {
					src = "dep_parser"
				}
			}
		}

		// 降级：POS 模板匹配
		if len(triples) == 0 && e.fallack != nil {
			result, err := e.fallack.Parse(sentence)
			if err == nil && result != nil && len(result.Tokens) > 1 {
				triples = extractFromPOS(result, sentence)
				if len(triples) > 0 {
					src = "fallback"
				}
			}
		}

		// ——— Phase 3: 语义验证 (TransE h+r≈t) ———
		if len(triples) > 0 && e.embedder != nil {
			triples = verifyTriples(triples, e.embedder)
		}

		// ——— Phase 4: 融合裁决 ———
		if len(triples) > 0 {
			triples = fuseTriples(triples, e.fusionCfg)
		}

		allTriples = append(allTriples, triples...)
	}

	if len(allTriples) > 0 {
		return &TripleSet{Triples: allTriples, Src: src}
	}
	return &TripleSet{Src: src}
}

// ——— Phase 3: 语义验证 ———

// verifyTriples 使用 TransE 打分 (h+r≈t) 计算 vector_conf
// 输入：候选三元组（带 syntax_conf）
// 处理：cos(h+r, t) → vector_conf
// 输出：带 vector_conf 的候选三元组
func verifyTriples(triples []Triple, embedder Vectorizer) []Triple {
	if embedder == nil || len(triples) == 0 {
		return triples
	}
	// ★ 一次 call 算全部：每对三元组要 3 个向量，N 条就是 3N 个句子。
	// 逐条调用会把 3N 次往返塞进一个 for 循环里，而它们之间毫无依赖
	// （docs §3.4：批量是形状，不是优化选项）。
	texts := make([]string, 0, len(triples)*3)
	for i := range triples {
		t := &triples[i]
		texts = append(texts, t.Subject, t.Relation, t.Object)
	}
	vecs := embedder.EmbedTexts(texts)

	for i := range triples {
		t := &triples[i]
		h := vecs[i*3]
		r := vecs[i*3+1]
		tv := vecs[i*3+2]

		hr := addVectors(h, r)
		sim := vector.CosineSimilarity(hr, tv)

		// 将 cos 映射到 [0, 1] 区间（原始可能在 [-1, 1]）
		t.VectorConf = (sim + 1.0) / 2.0
	}
	return triples
}

// ——— Phase 4: 融合裁决 ———

// fuseTriples 融合裁决：线性加权计算 final_score，截断阈值，降序输出
// 输入：候选三元组（带 syntax_conf + vector_conf）
// 处理：final_score = α * syntax_conf + β * vector_conf
// 输出：通过阈值且降序排列的最终三元组
func fuseTriples(triples []Triple, cfg ExtractorConfig) []Triple {
	if len(triples) == 0 {
		return triples
	}

	for i := range triples {
		t := &triples[i]
		finalScore := cfg.FusionAlpha*t.Score + cfg.FusionBeta*t.VectorConf
		t.Score = finalScore
	}

	kept := make([]Triple, 0, len(triples))
	for _, t := range triples {
		if t.Score >= cfg.FusionThreshold {
			kept = append(kept, t)
		}
	}

	sort.Slice(kept, func(i, j int) bool {
		return kept[i].Score > kept[j].Score
	})

	return kept
}

// ——— 向量工具 ———

// addVectors 向量加法 (h + r)
func addVectors(a, b vector.Vector) vector.Vector {
	out := make(vector.Vector)
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] += v
	}
	return out
}
