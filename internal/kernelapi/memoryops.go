package kernelapi

// memoryops.go —— **记忆侧的纯函数能力**（判据 §8 第 3 条）。
//
// ## 为什么这些函数要走契约
//
// 基座（`agent/toolcall.go` / `memorypass.go` / `mediaref.go` / `context.go`）
// 要用这些**纯计算**：
//
//	CleanText          清洗文本（去首尾标点/空白）
//	ExtractKeywords    抽关键词（jieba 词性过滤）
//	MediaBlockID       媒体块 ID（内容派生）
//	ParseSortMode      排序模式解析
//	SituationKeys      场景签名的键集
//
// 它们没有状态、不碰存储 —— 但今天住在 `internal/memory`（带 SQLite 的实现包）。
// 基座为几个纯函数 import 它，就是"消费方 import 实现"。
//
// ## 形态
//
//	接口（本文件） ← 实现（memory 包） ← 装配点注入
//
// 基座 `AgentConfig.MemoryOps`，未装配时各方法零值安全（见各方法注释）。

import (
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// MemoryOps 是记忆侧的**纯函数能力面**。
//
// ★ 方法集按**消费方实际用到**的，不是 memory 包的导出面。
type MemoryOps interface {
	// CleanText 清洗文本（去首尾标点与空白；空输入返回空）。
	CleanText(text string) string
	// ExtractKeywords 抽关键词（空输入 ⇒ nil）。
	ExtractKeywords(text string) []string
	// MediaBlockID 是媒体块的**内容派生 ID**（同内容 ⇒ 同 ID）。
	MediaBlockID(mime string, data []byte) string
	// SentenceBlockID 是原句块的派生 ID。
	SentenceBlockID(text string) string
	// ParseSortMode 解析排序模式（无法识别 ⇒ 默认"recent"）。
	ParseSortMode(s string) string
	// SortRecent 是"按最近"模式的**规范值**（比较用）。
	SortRecent() string
	// SituationKeys 返回情景签名的键集（诊断/日志用）。
	SituationKeys(s model.Situation) []string
	// NewSituation 由特征构造一个情景签名（去重保序）。
	NewSituation(features ...model.SituationFeature) model.Situation
}
