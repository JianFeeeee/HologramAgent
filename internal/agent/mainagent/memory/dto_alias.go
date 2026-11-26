// 共享值类型的**唯一声明点**在 internal/kernelapi/model（见该包注释）。
//
// 本文件把它重新导出为本包的名字，使既有调用点（`memory.Triple` 等）**逐字不变**：
// 类型恒等（别名而非新类型），因此跨包赋值、方法集、JSON 契约全部一致。
package memory

import "github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"

// BlockModality 是 model.BlockModality 的别名。
type BlockModality = model.BlockModality

// PageKind 是 model.PageKind 的别名。
type PageKind = model.PageKind

// BlockRole 是 model.BlockRole 的别名。
type BlockRole = model.BlockRole

// MemoryBlock 是 model.MemoryBlock 的别名。
type MemoryBlock = model.MemoryBlock

// MemoryBlockEdge 是 model.MemoryBlockEdge 的别名。
type MemoryBlockEdge = model.MemoryBlockEdge

// BlockRecallQuery 是 model.BlockRecallQuery 的别名。
type BlockRecallQuery = model.BlockRecallQuery

// BlockHit 是 model.BlockHit 的别名。
type BlockHit = model.BlockHit

// Entity 是 model.Entity 的别名。
type Entity = model.Entity

// Relation 是 model.Relation 的别名。
type Relation = model.Relation

// Triple 是 model.Triple 的别名。
type Triple = model.Triple

// RecallResult 是 model.RecallResult 的别名。
type RecallResult = model.RecallResult

// SortMode 是 model.SortMode 的别名。
type SortMode = model.SortMode

// DeleteResult 是 model.DeleteResult 的别名。
type DeleteResult = model.DeleteResult

// SituationFeature 是 model.SituationFeature 的别名。
type SituationFeature = model.SituationFeature

// Situation 是 model.Situation 的别名。
type Situation = model.Situation

// TurnScene 是 model.TurnScene 的别名。
type TurnScene = model.TurnScene

// ── 召回/仲裁/注入口径的值类型（同样声明在 kernelapi/model）──

// InjectedContext 是记忆注入上下文（实体/关系/场景块）。
type InjectedContext = model.InjectedContext

// FusedHit 是跨模态融合后的一条候选（块 + 分数 + 归因）。
type FusedHit = model.FusedHit

// AbstainReason 是拒答判定结果（符号路零命中时给出可读归因）。
type AbstainReason = model.AbstainReason

// ArbitrationResult 是时间仲裁的结论（保留 vs 被取代）。
type ArbitrationResult = model.ArbitrationResult
