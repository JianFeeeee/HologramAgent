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
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

func (a *Agent) cleanText(text string) string {
	if a == nil || a.memOps == nil {
		return text
	}
	return a.memOps.CleanText(text)
}

func (a *Agent) extractKeywords(text string) []string {
	if a == nil || a.memOps == nil {
		return nil
	}
	return a.memOps.ExtractKeywords(text)
}

func (a *Agent) mediaBlockID(mime string, data []byte) string {
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

func (a *Agent) parseSortMode(s string) string {
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

func (a *Agent) situationKeys(s model.Situation) []string {
	if a == nil || a.memOps == nil {
		return nil
	}
	return a.memOps.SituationKeys(s)
}

func (a *Agent) newSituation(features ...model.SituationFeature) model.Situation {
	if a == nil || a.memOps == nil {
		return model.Situation{}
	}
	return a.memOps.NewSituation(features...)
}

// closeGraph 关闭一个图记忆句柄（经**装配面**，未装配时退化为反射式 Close）。
//
// ★ 为什么走 opener 而不是直接调 `.Close()`：`kernelapi.GraphMemory` 契约里
//
//	没有 Close（它是"读面"）。关闭是**生命周期动作**，属于装配面
//	（`GraphMemoryOpener.Close`）—— 谁开的谁关。
func (a *Agent) closeGraph(m any) error {
	if a == nil || m == nil {
		return nil
	}
	if a.graphOpener != nil {
		return a.graphOpener.Close(m)
	}
	if c, ok := m.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// closeGraphWith 用给定的装配面关闭句柄（结构体方法里没有 *Agent 时用）。
// CloseGraphWith 关闭一个图记忆句柄（类型包在自己装配记忆时用）。
func CloseGraphWith(opener kernelapi.GraphMemoryOpener, m any) error {
	if m == nil {
		return nil
	}
	if opener != nil {
		return opener.Close(m)
	}
	if c, ok := m.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}
