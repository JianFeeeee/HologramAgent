package agent

// stages_exported.go —— 基座对外暴露的**阶段**（用户裁定 2026-11-17）。
//
// ## 用户原话
//
// > 「agent『基类提供基础字段与阶段，各个子 agent（含主 agent）
// >    各自挂载各自的回调函数』」
//
// 所以基座的公共面分两半：
//
//	字段 —— 构造面 + 槽位（见 AgentConfig / Hooks）
//	阶段 —— **本文件的导出方法**：把一次任务推进到下一个安全点、执行一批工具、
//	        裁剪一次上下文、跑一个阶段……
//
// 类型的差异（用哪种 loop、同步还是异步、裁不裁）由类型模块在 `standard`
// 之上实现，再挂进槽位。基座不认识它们 —— 这就是"组合而非继承"的落点。
//
// ## 为什么必须导出
//
// 在此之前 `runTaskSteps` / `runToolBatch` / `pruneByQuery` 都是私有方法，
// 于是 `internal/agent/standard` 想实现 Loop 槽位就只能 import agent 的
// **私有实现**（不可能），或者搬进 agent（那就不是 standard 了）。
// 导出这几个阶段，standard 才能：
//
//	standard.Loop(a, f) → a.RunTaskSteps(f) → 逐 step 推进
//
// 依赖方向仍是单向：standard → agent → agenttypes/kit。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
)

// RunToolBatch 是基座提供给所有 agentloop 的工具请求入口。
//
// agentloop 只决定何时发起请求以及本批约束；实际执行属于 kernel eventloop。
func (a *Agent) RunToolBatch(calls []agenttypes.ToolCall, opts agenttypes.BatchOpts) []agenttypes.BatchResult {
	ctx := opts.Ctx
	if ctx == nil {
		ctx = a.ctx
	}
	return a.runToolBatch(ctx, calls, opts)
}

// PruneContext 是**裁剪原语**：按相关度裁一次，返回裁掉的事件数。
//
// ★ 与 `PruneByQuery`（入口）的分工：入口把裁剪交给**注册的钩子**（策略），
// 本方法才是真正执行裁剪（基座原语）。类型模块的钩子实现调本方法，
// 于是依赖是单向的、也不会递归。
//
// protected 是**本帧现场**的保留条数；负值表示用全局默认。
func (a *Agent) PruneContext(query string, protected int) int {
	if a == nil || a.context == nil {
		return 0
	}
	// ★ 这里是**真正执行裁剪**（不是把活再交给钩子）：钩子实现调本方法，
	//   本方法若回头调钩子就成了无限递归（实测栈溢出）。
	if protected >= 0 {
		return a.context.PruneWithProtected(query, a.ContextTopKForProtected(protected), a.MainState().coldcontextStore, protected)
	}
	return a.context.PruneWithProtected(query, a.ContextTopK(), a.MainState().coldcontextStore, a.EffectiveProtectedCount())
}

// Hooks 返回当前槽位表（**类型工厂**用它填自己的回调后 SetHooks 挂回）。
//
// ★ 返回副本而不是引用：槽位是装配期的一次性写入，让工厂"先拿到、改好、
// 再整体挂回"是清晰的；返回引用会让两处同时改同一张表。
func (a *Agent) Hooks() Hooks {
	if a == nil {
		return Hooks{}
	}
	return a.hooks
}
