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
	"fmt"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
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

	// ── 槽位 5：上下文管理（主 agent = **策略裁剪型**，本包实现）──
	//
	// ★ overflow 的三件（写在这里而不是 standard）：阈值 1.25、恢复预算 2、
	//   "frame/水页"判定、剔除顺序、记账 —— 全是**主 agent 自己的上下文哲学**。
	//   驻留子整窗不裁、rt 滑窗，它们一条都用不上。
	if h.ContextBudget == nil {
		h.ContextBudget = ContextBudgetRelevance
	}
	h.OverflowHandler = enforceOverflowMain

	// ── 槽位 6：agent loop（主 agent 用**标准同步**实现）──
	//
	// ★ 同步状态机是**主 agent 自己的**（与驻留子共享同一实现，但各持一份
	//   的语义），它住本包（见 loop.go），不在基座也不在某个中立库。
	h.Loop = Loop

	// Tool execution is provided by the base/kernel boundary and is not a
	// registered type callback.

	h.Runtime = agent.RuntimeHooks{
		Initialize: func(a *agent.Agent) error { return InitRuntime(a) },
		Start:      func(a *agent.Agent) { a.StartMainRuntime() },
		Stop:       func(a *agent.Agent) { a.StopMainRuntime() },
	}

	a.SetHooks(h)

	// 缺槽位 ⇒ 响亮装配错误（基座不静默降级）。
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if err := a.Hooks().Runtime.Initialize(a); err != nil {
		return nil, err
	}
	return a, nil
}

// ── 主 agent 的回调全部来自 standard（组合，不重复实现）──

// 说明：这里不再各写一份实现，直接取 standard 的函数值 —— 那是
// 「类型模块**组合**标准实现」的落点（用户裁定：组合而非继承）。

const ComeFrom = "user"

// InitRuntime 是主 agent 的类型初始化（由 RuntimeHooks.Initialize 触发）。
//
// ## 为什么这里才是主 agent 的装配位置
//
// 「内核持有基座、类型模块注册回调」⇒ 基座 `Agent.New` 只做**通用**构造
// （字段、槽位、安全闸、IO 路由器）。**主 agent 专属的那些**在这里：
//
//	上下文实现（RelevanceContext：策略裁剪型）
//	输入调度器（四级抢占 + 挂起恢复）
//	主 agent 专属状态（记忆整理面 / 媒体 / 插件 / 预算统计 / 设备）
//	typed-nil 归一 + 变量库注入到上下文与 stageHost
//	后台维护任务声明（蒸馏/归档/合并/复审/转投）
//
// rtchild 与驻留子**不走**这条路径：它们各自注册自己的 Initialize，
// 用自己的上下文/调度器（整窗型 / 无队列），于是"主 agent 的复杂性"
// 不再泄漏到它们身上。
func InitRuntime(a *agent.Agent) error {
	if a == nil {
		return fmt.Errorf("main: nil base")
	}
	cfg := a.Config()
	// ★ 内核侧实例管理器（管理本内核下全部子 agent）。
	//
	// 装在这里而不是在 `New` 里：它是**主 agent 作为内核宿主**时才需要的东西
	// （子 agent 自己不是任何实例的宿主）。用户裁定 2026-11-18：
	// 「创建子 agent 执行在主 agent？不应该在内核吗？」—— 归属是内核，
	// 而内核在装配图上就是主 agent 这一层。
	a.InstallInstanceManager(agent.NewInstanceManager(a))
	a.InstallMainRuntime(cfg)
	a.InstallScheduler()
	a.InstallMainState(cfg)
	a.NormalizeNilServices()
	a.InjectSecretStore()
	registerStatusSections(a)
	// ★ 子 agent 的恢复**不在这里**：它在 StartMainRuntime 的最前面
	//   （恢复要 Assign inputch ⇒ 必须在调度循环起来之前，且插件已就绪）。
	//   这里只声明维护任务。
	registerMaintenance(a)
	return nil
}
