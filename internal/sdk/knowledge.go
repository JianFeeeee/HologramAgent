package sdk

import (
	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// KnowledgeAPI 是内置插件使用的全量知识库接口。
//
// 为什么不把多模态加进 pubsdk.KnowledgeAPI：那会改动
// third_party/homeagent-sdk/sdk/ 的公开契约，而发布纪律要求
// `git diff main -- third_party/homeagent-sdk/sdk/` 恒为 0
// （改动它等于一次 major bump + 完整的外部发版流程）。
// internal/sdk 明确不受此约束（"内部实现自由"），且这里本来就是
// "内核侧接口 = 公共接口 + 内置插件额外能力" 的既有范式
// （见 PluginSDK 遮蔽访问器：Settings/Memory/TextMemory/DocMemory…）。
// 结果是：内置插件（含子进程插件的 core handler）拿到多模态能力，
// 而外部 SDK 契约保持逐字不变。
type KnowledgeAPI interface {
	pubsdk.KnowledgeAPI
	// Stats 返回知识库的运行统计。
	Stats() map[string]interface{}
	// Remove 按名称删除一条知识。
	Remove(name string) error

	// SearchIn 在某个分类子树内检索（category 为空 = 全库）。
	SearchIn(query, category string, topK int) ([]*Knowledge, error)

	// Tree 返回分类树视图（面向服务：不带向量，只带计数与摘要）。
	Tree(opt KnowledgeTreeOptions) (*KnowledgeTreeView, error)
	// Subtree 返回某棵子树；category 为空等价于 Tree。
	Subtree(category string, opt KnowledgeTreeOptions) (*KnowledgeTreeView, error)
	// Categories 列出全部分类路径（去重排序）。
	Categories() ([]string, error)
	// CategoryCounts 给出每个分类的条目数，按数量倒序。
	CategoryCounts() ([]KnowledgeCategoryCount, error)
	// AddWithMedia 写入带媒体的知识。媒体是一等节点：其向量会与正文向量
	// 在多模态统一空间内融合，使该条目能按图本身被召回。
	//
	// 未接入多模态空间时与 Add 等价（媒体仍被记录，只是不参与召回）。
	AddWithMedia(name, content string, media []KnowledgeMediaRef) error
	// AttachMedia 给已有知识追加媒体，并当场重算其稠密向量。
	AttachMedia(name string, media ...KnowledgeMediaRef) error
	// ReindexDense 重建稠密向量（模型/维度变化后调用），返回新建与跳过条数。
	ReindexDense() (built, skipped int)
	// ImportDir 从目录批量导入知识（复制，不是引用），见 knowledge.ImportDir。
	//
	// 放接口里而不是只用内核：子进程插件与外部 agent 拿到知识库后，
	// "把这份资料灌进来"是常见诉求，不该逼它们回去调内核工具。
	ImportDir(opt knowledge.ImportOptions) (knowledge.ImportStats, error)
	// DenseStats 报告稠密路的接线与覆盖情况。
	DenseStats() map[string]interface{}
}

// KnowledgeImportOptions 是 ImportDir 的参数（别名，便于外部引用）。
type KnowledgeImportOptions = knowledge.ImportOptions

// KnowledgeImportStats 是 ImportDir 的结果（别名）。
type KnowledgeImportStats = knowledge.ImportStats

// KnowledgeMediaRef 是媒体在知识条目中的一等引用。
//
// 与内核 knowledge.KnowledgeMediaRef 是**类型别名**而非新类型：别名
// 才能穿过 C ABI / JSON 边界；若是两种结构，core handler 还得再做一次
// 手工转换，漏一处就是「媒体被静默丢弃」。
type KnowledgeMediaRef = knowledge.KnowledgeMediaRef

// Knowledge 沿用公共 SDK 的类型，保证内外两侧对同一批知识条目的
// 字段理解一致（跨 ABI 传递时按此结构序列化）。
type Knowledge = pubsdk.Knowledge

// KnowledgeTreeOptions 控制树视图的取舍。
type KnowledgeTreeOptions struct {
	// MaxDepth 限制层数，0 = 不限。分类多时用它做懒加载。
	MaxDepth int
	// IncludeItems 是否填充条目详情（只看结构时可关掉）。
	IncludeItems bool
	// PreviewLimit 预览字数上限，0 用内核默认。
	PreviewLimit int
}

// KnowledgeTreeView 是分类树节点。
type KnowledgeTreeView = knowledge.TreeView

// KnowledgeTreeItemView 是树上的知识条目。
type KnowledgeTreeItemView = knowledge.TreeItemView

// KnowledgeTreeMediaView 是条目挂载的媒体摘要。
type KnowledgeTreeMediaView = knowledge.TreeMediaView

// KnowledgeCategoryCount 是一个分类的条目数。
type KnowledgeCategoryCount = knowledge.CategoryCount
