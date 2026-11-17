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
	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
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

	// ── 槽位 5：上下文管理（主 agent = 相关性裁剪 + 动态预算）──
	if h.ContextBudget == nil {
		h.ContextBudget = ContextBudgetRelevance
	}
	if h.OverBudget == nil {
		h.OverBudget = OverBudgetRatio
	}
	if h.Prune == nil {
		h.Prune = PruneByQuery
		h.EnforceOverflow = EnforceOverflowStandard
	}

	// ── 槽位 6：agent loop（主 agent 用标准同步实现）──
	h.Loop = Loop

	// ── 槽位 1：toolcall（同步提交，执行由内核推进）──
	if h.ToolRunner == nil {
		h.ToolRunner = SynchronousRunner{}
	}

	// ── 槽位 7：stage（标准阶段执行）──
	if h.RunStage == nil {
		h.RunStage = RunStageStandard
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

// ContextBudgetRelevance 是**策略裁剪型**的预算（主 agent）。
func ContextBudgetRelevance(a *agent.Agent, b kit.TokenBudget) int {
	return b.ContextTokens
}

// OverBudgetRatio 判定"是否已超出窗口预算"。
func OverBudgetRatio(a *agent.Agent) bool { return a.OverBudgetRatio() }

// PruneByQuery 按相关度裁剪上下文。
//
// ★ 走**基座的裁剪原语**（`a.PruneContext`），不是 `a.PruneByQuery`：
// 后者是"把裁剪交给注册的钩子"这个入口，再调它就成了
// PruneByQuery → pruneWithHook → PruneByQuery 的无限递归（实测栈溢出）。
// 入口与实现必须是两个不同的东西。
func PruneByQuery(a *agent.Agent, query string, protected int) int {
	return a.PruneContext(query, protected)
}

// EnforceOverflowStandard 是标准超页处置。
func EnforceOverflowStandard(a *agent.Agent, f *agenttypes.Frame, query string, force bool) (bool, int) {
	return a.EnforceOverflowStandard(f, query, force)
}

// RunStageStandard 执行插件阶段。
func RunStageStandard(a *agent.Agent, stage string, ctx any) bool {
	return a.RunStageStandard(stage, ctx)
}

// Loop 是主 agent 的标准同步 loop。
//
// ★ 它注册的是基座的 `LoopStandard`（**逐 step 推进的状态机**），
// 而不是 `a.RunTaskSteps` —— 后者是"把帧交给注册的 loop"这个**入口**，
// 再调它就成了 runTaskSteps → Loop → runTaskSteps 的无限递归（实测）。
//
// 入口（`runTaskSteps`）与实现（`LoopStandard`）必须是两个不同的东西：
// 这正是"基座只转发、类型提供实现"的分工。
func Loop(a *agent.Agent, f *agenttypes.Frame) agenttypes.Outcome {
	return agent.LoopStandard(a, f)
}

// SynchronousRunner 是标准同步 toolcall（提交一批、按索引拿回结果）。
type SynchronousRunner struct{}

// RunBatch 执行一批工具并按索引顺序返回（协议要求配对完整）。
func (SynchronousRunner) RunBatch(a *agent.Agent, calls []agenttypes.ToolCall, opts agenttypes.BatchOpts) []agenttypes.BatchResult {
	return a.RunToolBatch(calls, opts)
}

// ComeFrom 给出主 agent 发起工具调用时的来源标签（根 = user）。
const ComeFrom = "user"
