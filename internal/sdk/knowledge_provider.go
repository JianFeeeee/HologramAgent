package sdk

// knowledge_provider.go —— 知识库能力的**发布/订阅点**。
//
// ## 为什么在 SDK 里
//
// 知识库是内部插件：它实现能力，内核不持有句柄。但**别的插件**
// （cli / webui / healthcheck / lua）仍要在进程内用它。
//
// 于是需要一处"能力目录"：插件 Start 时发布、Stop 时撤回，消费者按名取。
// 它住在 SDK 而不是内核，因为这是**插件之间的能力协商**，与内核无关 ——
// 内核删干净之后（grep 零 knowledge 符号）这里仍然成立。
//
// ## 契约
//
//   - 只有一个实现者（知识库插件）。重复发布 = 装配错误，响亮报错。
//   - 未发布时 `Knowledge()` 返回 nil，调用方必须自己判空（既有调用点都判）。
var knowledgeProvider KnowledgeAPI

// SetKnowledgeProvider 发布知识库能力（由知识库插件在 Start 时调用）。
func SetKnowledgeProvider(k KnowledgeAPI) { knowledgeProvider = k }

// ClearKnowledgeProvider 撤回（Stop 时调用；幂等）。
func ClearKnowledgeProvider() { knowledgeProvider = nil }

// KnowledgeProvider 返回当前发布的知识库能力（可能 nil）。
func KnowledgeProvider() KnowledgeAPI { return knowledgeProvider }
