// 共享值类型的**唯一声明点**在 internal/kernelapi/model（见该包注释）。
//
// 本文件把它重新导出为本包的名字，使既有调用点（`memory.Triple` 等）**逐字不变**：
// 类型恒等（别名而非新类型），因此跨包赋值、方法集、JSON 契约全部一致。
package coldcontext

import "github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"

// ColdDoc 是 model.ColdDoc 的别名。
type ColdDoc = model.ColdDoc

// ColdEvent 是 model.ColdEvent 的别名。
type ColdEvent = model.ColdEvent

// ColdHit 是 model.ColdHit 的别名。
type ColdHit = model.ColdHit

// ToolResultItem 是 model.ToolResultItem 的别名。
type ToolResultItem = model.ToolResultItem

// ToolCallItem 是 model.ToolCallItem 的别名。
type ToolCallItem = model.ToolCallItem

// ContextEntry 是 model.ContextEntry 的别名。
type ContextEntry = model.ContextEntry
