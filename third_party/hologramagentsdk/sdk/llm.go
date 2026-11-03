package sdk

// LLMAPI provides access to the LLM provider manager.
type LLMAPI interface {
	ListSources() []string
	SetSource(name string) error
	CurrentSource() string
}
