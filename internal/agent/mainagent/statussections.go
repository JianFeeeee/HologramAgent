package mainagent

// statussections.go —— **主 agent 注册自己的状态段**（谁持有数据谁注册）。
//
// ## 为什么这些段不由基座填
//
// 基座曾经有一个 20 字段的 `KernelStatus`，其中 9 个在生产装配下恒为零值
// （句柄为 nil）、3 个装的是**类型专属结构**（scheduler / residents / onnx）。
// 那等于"基座假设所有 agent 都有这些面"，与 `agent.Hooks.GraphMemory` 同一个病。
//
// 现在：主 agent 在自己 `Initialize` 里注册它持有的那些段；
// 驻留子注册 `"resident"`；rt 注册 `"rt"`；knowledge 插件注册 `"knowledge"`。
// 基座只留 `runtime` / `llm` 两个与 agent 无关的段（见 agent/status.go）。
//
// ## 本文件目前只注册两段
//
//	"context"    5 个纯读数字 + 超页记账（**主 agent 的策略形态**）
//	"scheduler"  主 agent 自己那份（驻留子另注册它自己的）
//
// 记忆/文档/人物/文本/追踪/ONNX 六个段的数据句柄现在由 `mainState` 持有，
// 它们的注册点在 `agent/status.go`；I 批把 `mainState` 搬进本包时，
// 注册点一起搬过来（那时才不会"从别的包读私有字段"）。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

// ContextSection 是主 agent 的 `"context"` 段。
//
// ★ 这 5 个数字都在基座的**纯读接口**上，但"什么时候算、算哪些、怎么解释"
// 是主 agent 的策略：驻留子是整窗、rt 是滑窗，它们的段完全不同。
type ContextSection struct {
	Events       int     `json:"events"`
	Accumulated  int     `json:"accumulated_tokens"`
	WindowTokens int     `json:"window_tokens"`
	Ratio        float64 `json:"ratio"`
	// Overflow 是主 agent 的超页处置记账（策略自己的账）。
	Overflow OverflowStat `json:"overflow"`
}

// registerStatusSections 注册主 agent 持有的状态段（`Initialize` 里调一次）。
//
// 段内容用**函数**延迟求值：状态面每 3 秒被轮询，而有些值要现场读句柄
// （当前条数、当前水位）。基座只管序列化，不在错误时刻算它。
func registerStatusSections(a *agent.Agent) {
	if a == nil {
		return
	}
	a.RegisterStatusSection(agent.StatusSection{Key: "context", Payload: func() any {
		return ContextSection{
			Events:       Wrap(a).ContextLen(),
			Accumulated:  Wrap(a).AccumulatedTokens(),
			WindowTokens: Wrap(a).ContextWindowTokens(),
			Ratio:        Wrap(a).OverflowRatioNow(),
			Overflow:     ledgerFor(a).Snapshot(),
		}
	}})

	// ★ 记忆 / 文档 / 文本记忆三段：**数据在主 agent 手里**（2026-11-20）。
	//   基座不注册它们 —— 它没有那些句柄（`GraphDB()` 是基座的通用读接口，
	//   不同类型可以有也可以没有；"有就拿去算段"属于持有者的决定）。
	if m := Wrap(a).st; m != nil {
		if m.indexer != nil {
			a.RegisterStatusSection(agent.StatusSection{Key: "memory", Payload: func() any {
				return memoryStatusOf(m.indexer)
			}})
		}
		if m.coldcontextStore != nil {
			a.RegisterStatusSection(agent.StatusSection{Key: "documents", Payload: func() any {
				return documentsStatusOf(m.coldcontextStore)
			}})
		}
		if m.textMem != nil {
			a.RegisterStatusSection(agent.StatusSection{Key: "text_memory", Payload: func() any {
				tm := m.textMem
				return struct {
					Available bool `json:"available"`
					FileCount int  `json:"file_count"`
				}{true, tm.FileCount()}
			}})
		}
	}

	// "scheduler"：**主 agent 与驻留子各自注册自己那份**（各自持有实例）。
	// rt 没有 scheduler（rtSlot 单槽覆盖）⇒ 不注册。
	a.RegisterStatusSection(agent.StatusSection{Key: "scheduler", Payload: func() any {
		return Wrap(a).SchedulerStatus()
	}})
}
