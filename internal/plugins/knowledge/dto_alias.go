// 共享值类型的**唯一声明点**在 internal/kernelapi/model（见该包注释）。
//
// 本文件把它重新导出为本包的名字，使既有调用点（`memory.Triple` 等）**逐字不变**：
// 类型恒等（别名而非新类型），因此跨包赋值、方法集、JSON 契约全部一致。
package knowledgeplugin

import "github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"

// KnowledgeMediaRef 是 model.KnowledgeMediaRef 的别名。
type KnowledgeMediaRef = model.KnowledgeMediaRef

// MediaRef 是 KnowledgeMediaRef 的别名（照顾内部可读性，与 model 包外的
// 既有写法逐字一致：`MediaRef = KnowledgeMediaRef`）。
type MediaRef = KnowledgeMediaRef

// IndexItem 是 model.IndexItem 的别名。
type IndexItem = model.IndexItem

// TreeIndex 是 model.TreeIndex 的别名。
type TreeIndex = model.TreeIndex

// ImportOptions 是 model.ImportOptions 的别名。
type ImportOptions = model.ImportOptions

// ImportStats 是 model.ImportStats 的别名。
type ImportStats = model.ImportStats

// Knowledge 是知识库里的一条条目。
type Knowledge = model.Knowledge
