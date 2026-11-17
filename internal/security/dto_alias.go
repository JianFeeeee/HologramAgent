// 共享值类型的**唯一声明点**在 internal/kernelapi/model（见该包注释）。
//
// 本文件把它重新导出为本包的名字，使既有调用点（`security.Decision` 等）
// **逐字不变**：类型恒等（别名而非新类型），因此跨包赋值、方法返回值、
// JSON 契约全部一致。
//
// 目的：消费方（agent 子包等）可以直接写 `model.Decision`，
// 而不必为"拿一个判定值"import 整个安全实现包。
package security

import "github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"

// Decision 是策略判定结论。
type Decision = model.Decision

// Finding / FindingKind 是内容审查命中。
type (
	Finding     = model.Finding
	FindingKind = model.FindingKind
)

// Inspection 是一次过境点的描述。
type Inspection = model.Inspection

// Direction 是数据流方向。
type Direction = model.Direction

// DirectionOutbound / DirectionInbound 是方向取值（model 里是常量，
// 本包以**常量**转发，保持既有写法可用）。
const (
	DirectionOutbound = model.DirectionOutbound
	DirectionInbound  = model.DirectionInbound
)

// FindingKind 的取值（声明点在 model，本包以常量转发）。
const (
	FindingSecret        = model.FindingSecret
	FindingSensitiveWord = model.FindingSensitiveWord
	FindingInjection     = model.FindingInjection
)

// RoundInput 是一轮输入的策略声明。
type RoundInput = model.RoundInput

// Evt 是一次明文→别名的替换记录。
type Evt = model.Evt
