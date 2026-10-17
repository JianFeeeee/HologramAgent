package nlp

// ParseResult 依存句法分析结果
type ParseResult struct {
	Tokens  []string
	POS     []string
	Heads   []int    // 父节点索引，0=ROOT
	DepRels []string // 依存关系标签
}

// Triple 三元组 (subject, relation, object)
type Triple struct {
	Subject     string
	Relation    string
	Object      string
	Score       float64 // syntax_conf：句法置信度（Phase 2 输出）
	VectorConf  float64 // vector_conf：语义向量置信度（Phase 3 输出）
	Src         string  // "dep" / "dep_coo" / "pos" / "fallback"
	SentenceRef string  // 原始句子，用于LLM复审时修正
}

// TripleSet 提取结果
type TripleSet struct {
	Triples []Triple
	Src     string // "dep_parser" / "fallback" / ""
	Err     error
}
