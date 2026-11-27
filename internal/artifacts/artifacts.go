// Package artifacts 是**提取器与三元组归一化**的契约层（叶子包，零内部依赖）。
//
// ## 为什么它必须是独立叶子包
//
// `coldDocToTriples`（冷文档 → 三元组）住在基座（`agent/distill.go`），
// 而它要用 NLP 提取器与 `IsValidEntityName` / `FilterNoiseTriples`。
// 那些今天住在 `internal/nlp` 与 `internal/memory` —— 两个**实现包**
// （一个带 ONNX，一个带 SQLite）。基座为它们 import 实现包就是
// 判据 §8 第 3 条要消灭的形态。
//
// 也不能放进 `agenttypes`（它 import `internal/sdk`，sdk 又 import memory ⇒ 环，
// 实测撞过）。
//
// ## 形态：**能力注入**，不是把实现搬来
//
// 本包只声明：
//
//	Extractor  提取器接口（`Extract(text) []Triple`）
//	Triple     提取结果（三字段 + 置信度）
//	Norm       归一化判定（`ValidName(n)` / `IsNoise(n)`）
//
// 装配点（`kernelmod/agent`）把 nlp 的提取器与 memory 的判定函数注入进来。
// ⇒ 基座只依赖本包的**接口**，实现仍在各自包里。
package artifacts

import "strings"

// Triple 是一条提取出来的三元组（**提取器的输出形状**）。
//
// ★ 它与存储侧的 `model.Triple` 不是同一个类型：这里没有 Scene /
//
//	MediaDigests 等存储字段。基座负责把两者接上（那是"提取 ⟶ 落库"
//	这一步的语义，属于类型自己的决定）。
type Triple struct {
	Subject     string
	SubjectType string
	Relation    string
	Object      string
	ObjectType  string
	Confidence  float64
}

// Extractor 是三元组提取器（nlp 模块的实现满足它）。
//
// ★ 接口在这里、实现在 nlp：基座不认识 nlp 的内部分层
//
//	（parser / 依存句法 / 融合权重），只认识"给我一段文本，给我几条三元组"。
type Extractor interface {
	// Extract 从文本里提取三元组（空文本 / 抽不出 ⇒ nil）。
	Extract(text string) []Triple
}

// Norm 是**归一化判定**的注入面（memory 模块的实现满足它）。
type Norm struct {
	// ValidName 报告某个名字能否作为实体落库（长度/字符集约束）。
	//
	// ★ 它必须与存储侧的落库闸门**同口径**：蒸馏侧用更宽的阈值会造出
	//   "必然被拒"的三元组（每轮刷一行 reject 日志，且让「抽不出来」与
	//   「抽出来全被拒」在日志里长得一样）。
	ValidName func(name string) bool
	// IsNoiseEntity 报告某个名字是不是噪声（工具名/路径片段/纯符号）。
	IsNoiseEntity func(name string) bool
}

// ValidName 零值安全地判定名字是否可落库。
//
// ★ 为什么零值**放行**而不是零值全拒：放行最多多写几条噪声三元组（可清理），
//
//	全拒会让归档**一条关系都建不起来**（那是静默失效，比噪声糟得多）。
func (n Norm) NameOK(name string) bool {
	if n.ValidName == nil {
		return strings.TrimSpace(name) != ""
	}
	return n.ValidName(name)
}

// IsNoise 零值安全地判定名字是否噪声（未装配 ⇒ 不判）。
func (n Norm) IsNoise(name string) bool {
	if n.IsNoiseEntity == nil {
		return false
	}
	return n.IsNoiseEntity(name)
}
