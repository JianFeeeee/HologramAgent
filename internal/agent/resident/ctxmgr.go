package resident

// ctxmgr.go —— 驻留子的**上下文管理面**（本类型自己的实现句柄）。
//
// ★ 基座不持有 `*RelevanceContext`：上下文是 agentloop 的资产。
//   驻留子用整窗策略（不裁），但它**确实**持有自己那份上下文实现。

import (
	rctx "github.com/JianFeeeee/HologramAgent/internal/agent/resident/ctx"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// ContextMgr 是驻留子的上下文最小面。
//
// ★ 这里**没有** Messages：渲染归类型（用户裁定 2026-11-19），
//
//	而"事件序列 → []Message"这一步由本类型的 agentloop 用 `mainagent`
//	的渲染入口完成，不需要上下文实现出自己的拼装方法。
type ContextMgr interface {
	Len() int
	PlanCompaction(keepRecent int) CompactPlan
	TrimKeepRecent(keepRecent int) int
	ApplyCompaction(plan CompactPlan, summary string) int
	Append(evt ContextEvent)
	EventsCopy() []*ContextEvent
	Recent(n int) []ContextEvent
}

// contextFileName 是本类型的上下文持久化文件名（内核只给目录，名字归类型）。
const contextFileName = "context.json"

// 值类型别名（声明点在**本包自己的** ctx 子包 —— 同一 agent 内部，不是跨类型共享）。
type (
	CompactPlan     = rctx.CompactPlan
	ContextEvent    = rctx.ContextEvent
	ColdRecallPlan  = rctx.ColdRecallPlan
	ToolResultStore = rctx.ToolResultStore
)

var _ = model.ColdDoc{}
