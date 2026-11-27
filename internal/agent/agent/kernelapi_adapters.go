package agent

// kernelapi_adapters.go —— 把 agent 依赖的实现面适配成 `kernelapi` 的接口。
//
// 唯一剩下的适配是 `vector.Vector`（`map[string]float64` 的**命名类型**）：
// 接口包与本包都不 import vector 实现包 ⇒ 用 Map 形态的方法名。

import (
	"github.com/JianFeeeee/HologramAgent/internal/vector"
)

// ★ 已删 `graphFace`（2026-11-20）：子 agent 不再有"轻量图记忆实例"，
// 于是不需要"完整面 → 窄面"的适配。图库只有主 agent 的 GraphDB 一份，
// 直接满足 `GraphMemory`。

var _ = vector.Vector{}
