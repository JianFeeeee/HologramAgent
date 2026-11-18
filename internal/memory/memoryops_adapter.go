package memory

// memoryops_adapter.go —— 把本包的**纯函数**适配成 `kernelapi.MemoryOps`。
//
// ## 为什么要有这一层
//
// 基座要用 `CleanText` / `ExtractKeywords` / `MediaBlockID` / `ParseSortMode` /
// `SituationKeys` 这些**纯计算**，而它不能 import 本包（带 SQLite，判据 §8 第 3 条）。
//
// 契约在 `kernelapi.MemoryOps`，实现在本包，适配也在本包：
// 于是"谁是实现"一目了然，装配点只做一次注入。

import (
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// MemoryOps 实现 kernelapi.MemoryOps（零状态，可全局共用一实例）。
type MemoryOps struct{}

// NewMemoryOps 构造能力面（装配点 Provide 一次，或直接传给 AgentConfig）。
func NewMemoryOps() MemoryOps { return MemoryOps{} }

// 编译期确认。
var _ kernelapi.MemoryOps = MemoryOps{}

func (MemoryOps) CleanText(text string) string { return CleanText(text) }

func (MemoryOps) ExtractKeywords(text string) []string { return ExtractKeywords(text) }

func (MemoryOps) MediaBlockID(mime string, data []byte) string { return MediaBlockID(mime, data) }

func (MemoryOps) SentenceBlockID(text string) string { return SentenceBlockID(text) }

func (MemoryOps) ParseSortMode(s string) string {
	return string(ParseSortMode(s))
}

func (MemoryOps) SortRecent() string { return string(SortRecent) }

func (MemoryOps) SituationKeys(s model.Situation) []string { return SituationKeys(s) }

func (MemoryOps) NewSituation(features ...model.SituationFeature) model.Situation {
	return NewSituation(features...)
}
