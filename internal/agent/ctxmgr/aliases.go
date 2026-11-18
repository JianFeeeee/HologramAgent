package ctxmgr

// aliases.go —— 本包用的**值类型别名**（声明点在 agenttypes / model）。
//
// 拆包后这些类型由基座与类型模块共用；本包做别名，既有写法逐字不变。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

type (
	// 上下文（L1）与工具结果的纯数据。
	ContextEvent    = agenttypes.ContextEvent
	CompactPlan     = agenttypes.CompactPlan
	ColdRecallPlan  = agenttypes.ColdRecallPlan
	ToolResultStore = agenttypes.ToolResultStore
	ToolResultItem  = agenttypes.ToolResultItem
	ToolCallItem    = agenttypes.ToolCallItem
	MemoryBlock     = model.MemoryBlock
)

// Embedder 是词嵌入面（与基座的同名接口**结构相同**，可互换赋值）。
//
// ★ 为什么不 verifier 成同一个具名类型：agent 包 import 本包，本包不能
//
//	反向 import agent（成环）。让两边的接口**结构一致**即可互通 ——
//	Go 的接口是结构化类型，赋值时自动满足。
type Embedder interface {
	VectorizeMap(text string) map[string]float64
	VectorizeCleanMap(text string) map[string]float64
	Loaded() bool
}
