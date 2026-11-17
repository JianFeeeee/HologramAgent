// 共享值类型的**唯一声明点**在 internal/kernelapi/model（见该包注释）。
//
// 本文件把它重新导出为本包的名字，使既有调用点（`memory.Triple` 等）**逐字不变**：
// 类型恒等（别名而非新类型），因此跨包赋值、方法集、JSON 契约全部一致。
package promptsec

import "github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"

// Section 是 model.Section 的别名。
type Section = model.PromptSection
