package agent

// memoryops_face.go —— 基座对记忆**纯函数能力**的转发（零值安全）。
//
// ## 为什么每个方法都要零值分支
//
// `AgentConfig.MemoryOps` 为 nil 的场合是**真实存在**的（单测、裁剪版内核）。
// 未装配时：
//
//	cleanText       → 原样返回（清洗是好化，不是正确性前提）
//	extractKeywords → nil（调用方判断空即跳过）
//	mediaBlockID    → 空串（调用方据此"不落块"）
//	parseSortMode   → "recent"（默认排序，与实现侧一致）
//	newSituation    → 空签名
//
// ★ 这与 artifacts.Norm 的零值语义同一纪律：缺能力时**退化**，而不是崩。

import (
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

func (a *Agent) CleanText(text string) string {
	if a == nil || a.memOps == nil {
		return text
	}
	return a.memOps.CleanText(text)
}

func (a *Agent) ExtractKeywords(text string) []string {
	if a == nil || a.memOps == nil {
		return nil
	}
	return a.memOps.ExtractKeywords(text)
}

func (a *Agent) MediaBlockID(mime string, data []byte) string {
	if a == nil || a.memOps == nil {
		return ""
	}
	return a.memOps.MediaBlockID(mime, data)
}

func (a *Agent) sentenceBlockID(text string) string {
	if a == nil || a.memOps == nil {
		return ""
	}
	return a.memOps.SentenceBlockID(text)
}

// ParseSortMode 解析排序模式（记忆族工具用）。
func (a *Agent) ParseSortMode(s string) string {
	if a == nil || a.memOps == nil {
		return "recent"
	}
	return a.memOps.ParseSortMode(s)
}

func (a *Agent) sortRecent() string {
	if a == nil || a.memOps == nil {
		return "recent"
	}
	return a.memOps.SortRecent()
}

func (a *Agent) SituationKeys(s model.Situation) []string {
	if a == nil || a.memOps == nil {
		return nil
	}
	return a.memOps.SituationKeys(s)
}

func (a *Agent) NewSituation(features ...model.SituationFeature) model.Situation {
	if a == nil || a.memOps == nil {
		return model.Situation{}
	}
	return a.memOps.NewSituation(features...)
}

// ★ 已删 closeGraph / CloseGraphWith（2026-11-20）：子 agent 不再自己开库，
//   图句柄的关闭发生在**主 agent 的 graph_close 槽内工具**里。
