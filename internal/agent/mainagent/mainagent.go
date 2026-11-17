// Package main 是**主 agent 类型模块**（用户裁定 2026-11-17）。
//
// ## 用户原话
//
// > 「同时每个 agent 都有对应的实例化调用，没有被调用就没有这个 agent 的实例，
// >    内核启动后自动实例化一个主 agent」
//
// > 「有一个 agent 基类，主 agent 与不同子 agent 是这个基类的派生。
// >    注意是通过组合实现，而非继承。」
//
// > 「不是说了采用回调注册的形式吗，agent『基类提供基础字段与阶段，各个子 agent
// >    （含主 agent）各自挂载各自的回调函数』」
//
// ## 本包做什么
//
// 它**组合**（而非继承）通用 Agent：`New` 造基座，然后**注册主 agent自己的**
// 那套回调（上下文策略 / loop / toolcall / stage），最后登记到类型注册表。
//
//	base := agent.New(cfg)              ← 通用基础设施（槽位全空）
//	base.SetHooks(mainHooks(...))       ← 主 agent 挂自己的实现
//	base.Validate()                     ← 缺槽位就是装配错误
//
// ## 为什么它是独立包
//
// 装配点（`kernelmod/agent`）只需要 `agent.Instantiate("main", cfg)`，
// 于是"主 agent 怎么装"完全在这一个包里 —— 加/换主 agent 的实现不用改装配点，
// 也不用改基座。与驻留子 / rtchild / worker 走**同一张表、同一个调用**。
package mainagent

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/standard"
)

// TypeName 是主 agent 的类型名（与基座注册表里的名字一致）。
const TypeName = agent.TypeNameMain

func init() {
	if err := agent.RegisterType(TypeName, New); err != nil {
		// 装配期重复注册是计划错误（两个包声称是同一类型）。
		panic("agent/main: " + err.Error())
	}
}

// New 是主 agent的**实例化调用**：组合基座 + 挂上自己的回调。
func New(cfg agent.AgentConfig) (*agent.Agent, error) {
	a := agent.New(cfg)
	h := a.Hooks()

	// ── 槽位 5：上下文管理（主 agent = **策略裁剪型**，来自 standard）──
	if h.ContextBudget == nil {
		h.ContextBudget = standard.ContextBudgetRelevance
	}
	if h.OverBudget == nil {
		h.OverBudget = standard.OverBudgetRatio
	}
	if h.Prune == nil {
		h.Prune = standard.PruneByQuery
		h.EnforceOverflow = standard.EnforceOverflowStandard
	}

	// ── 槽位 6：agent loop（主 agent 用**标准同步**实现）──
	h.Loop = standard.Loop

	// ── 槽位 1：toolcall（同步提交；执行由内核事件循环推进）──
	if h.ToolRunner == nil {
		h.ToolRunner = standard.SynchronousRunner{}
	}

	// ── 槽位 7：stage（标准阶段执行）──
	if h.RunStage == nil {
		h.RunStage = standard.RunStageStandard
	}

	// ── 类型身份（数据事实，不是行为）──
	h.ComeFrom = ComeFrom

	a.SetHooks(h)

	// 缺槽位 ⇒ 响亮装配错误（基座不静默降级）。
	if err := a.Validate(); err != nil {
		return nil, err
	}

	// 主 agent 的后台维护任务：只**声明**，刻度由内核事件循环承担。
	a.RegisterMaintenanceTasks()
	return a, nil
}

// ── 主 agent 的回调全部来自 standard（组合，不重复实现）──

// 说明：这里不再各写一份实现，直接取 standard 的函数值 —— 那是
// 「类型模块**组合**标准实现」的落点（用户裁定：组合而非继承）。

const ComeFrom = "user"
