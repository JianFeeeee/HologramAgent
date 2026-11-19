package agent

// ctxaliases.go —— 上下文实现的**别名转发**（实现已搬到 internal/agent/ctxmgr）。
//
// ## 为什么用别名而不是让调用点改包名
//
// `a.contextMgr()` 的类型在基座里出现 20+ 处（`*RelevanceContext`），
// 而方法（`Append`/`Prune`/`Len`/`Recent`…）在类型模块里也用。
// 用类型别名 + 方法转发，既有写法**逐字不变**，同时实现只有一份。
//
// ## 为什么要转发方法（而不是别名直接暴露）
//
// 别名能让 `a.contextMgr().Len()` 直接可用（方法集随类型走），但**小写方法**
// （`tokenAt` / `persistToolResult` / `evictPagesLocked` …）别名带不过来。
// 那些必须显式转发 —— 它们正是"基座与实现在同一包"时代留下的内部接口。

import (
	mainctx "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/ctx"
)

type (
	// RelevanceContext 是上下文实现（声明点在 ctxmgr）。
	RelevanceContext = mainctx.RelevanceContext
)

// NewRelevanceContext 构造上下文实现。
func NewRelevanceContext(savePath string, embedder Embedder) *RelevanceContext {
	return mainctx.NewRelevanceContext(savePath, embedder)
}
