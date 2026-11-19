package agent

// ctxface.go —— 基座对**上下文实现**的转发表面。
//
// ## 为什么是转发而不是持有
//
// §6.2 #1：`context` 实现归**类型**，基座零持有。主 agent 用策略裁剪型
// （`mainagent/ctx.RelevanceContext`），驻留子用整窗型，rt 用滑窗——
// 基座不知道有几种。
//
// 但基座**有代码要读它**（状态面要条数、崩溃恢复要落盘、工具结果要存）。
// 所以只有两条路：① 把实现句柄留在基座上（违背零持有）；
// ② 类型注册一个能力面，基座**只按契约调**。这里是 ②。
//
// 能力面收成「基座真的会读/写的那些」，不是把 RelevanceContext 的
// 全部方法照抄一遍 —— 抄一遍等于换个名字持有它。

import (
	mainctx "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/ctx"
)

// ContextMgr 是上下文实现的能力面（由类型注册）。
type ContextMgr = mainctx.RelevanceContext

// SetContextMgr 由类型在 Initialize 里注册自己的上下文实现。
func (a *Agent) SetContextMgr(c *ContextMgr) {
	if a == nil {
		return
	}
	a.ctxHolder = c
}

// contextMgr 返回上下文实现的句柄（未装配 ⇒ nil）。
func (a *Agent) contextMgr() *ContextMgr {
	if a == nil {
		return nil
	}
	return a.ctxHolder
}

// ContextMgr 返回本 agent 的上下文管理器（类型的 agentloop 要读写它）。
func (a *Agent) ContextManager() *ContextMgr {
	if a == nil {
		return nil
	}
	return a.contextMgr()
}
