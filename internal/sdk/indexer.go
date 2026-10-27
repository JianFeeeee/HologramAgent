package sdk

// IndexerAPI exposes the memory context indexer.
type IndexerAPI interface {
	BuildContext(q string) (*IndexContext, error)
	FormatContext(ctx *IndexContext) string
	GetToolDefinitions() []map[string]interface{}
	BuildToolPrompt() string
}

// IndexContext is a neutral snapshot of the injected memory context.
type IndexContext struct {
	Entities      []Entity   `json:"entities"`
	Relations     []Relation `json:"relations"`
	Summary       string     `json:"summary"`
	TokenEstimate int        `json:"token_estimate"`
	// Docs 是 **L2 冷上下文**那一路的结果（文档/媒体片段）。
	//
	// 为什么放在同一个结构里：真实召回是 L2 + L3 **双路**，插件若只看到 L3
	// 就会出现「图里没东西 ⇒ 面板显示无上下文」的假象（L3 有 72h 冷化延迟，
	// 而 L2 一直是活的）。诊断面板必须复现模型真实收到的两路。
	Docs []DocSnippet `json:"docs"`
}

// DocSnippet 是 L2 命中的一条片段。
type DocSnippet struct {
	ID    string  `json:"id"`
	When  string  `json:"when"`
	Text  string  `json:"text"`
	Score float64 `json:"score"`
}
