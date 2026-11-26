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
// 它**组合**（而非继承）通用 agent.Agent：`New` 造基座，然后**注册主 agent自己的**
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
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// TypeName 是主 agent 的类型名（与基座注册表里的名字一致）。
const TypeName = agent.TypeNameMain

func init() {
	// ★ 登记进**唯一那张类型表**（`registry.Descriptor`），与 resident / rt /
	//   worker / 插件类型完全同构（docs §7.1「一张表管五件事」）。
	//
	// 主 agent 不是特例：内核按 `core.agent.main_type` 选中谁，就用同一套
	// `Construct` 造它、把默认通道划给它。
	if err := registry.Register(mainDescriptor()); err != nil {
		// 装配期重复注册是计划错误（两个包声称是同一类型）。
		panic("agent/main: " + err.Error())
	}
}

// mainDescriptor 返回主 agent 的类型项。
//
// ★ `Construct` 是**唯一**造主 agent 实例的路径（与 admin_child 创建子
//
//	agent 同一条路）。此前它是另一个函数 `New` + 第二张类型表
//	（`agent.typeRegistry`），两张表各登记一份 —— 那正是被删的形态。
func mainDescriptor() registry.Descriptor {
	return registry.Descriptor{
		Meta: registry.Meta{
			Type:        TypeName,
			AgentCap:    agentIO.CapText,
			Description: "主 agent：内核按 core.agent.main_type 拉起，默认通道全部划给它",
			// ★ 主 agent 是**唯一**可 offload 拉子的类型（文档 §8.2）。
			MayOffload: true,
		},
		// ★ 与子 agent **同一条**创建路径：内核把装配料放进 arg，
		//   类型的 Construct 从 arg 取 cfg 造实例。主 agent 不是特例。
		Construct: func(host registry.Host, arg map[string]interface{}) (string, error) {
			cfg, ok := arg["cfg"].(agent.AgentConfig)
			if !ok {
				return "", fmt.Errorf("main: 缺少装配料（arg[cfg] 必须是 agent.AgentConfig）")
			}
			a, err := New(cfg)
			if err != nil {
				return "", err
			}
			// ★ 主 agent 私有值：随装配料一起进来的（装配点放进 arg）。
			//
			// 2026-11-26 新旧对照实测：这一步缺失导致 `personality=false`
			// （人格一个字节都没进提示词，而所有判据都是绿的）。
			// 挂在这里而不是装配点「造完再挂」——`installState` 跑在
			// Initialize（`Instantiate` 第②步）里，那时读不到后挂的值。
			a.MountMainValues(arg["fusionCfg"], arg["personality"], arg["personaStore"])
			// ★ 唯一的差别：根实例交回**内核**（子实例由各自 construct 登记）。
			//   这不是"主 agent 特殊"，是**宿主不同**：根宿主接收根实例。
			if rh, ok := host.(*agent.RootHost); ok {
				if err := rh.SetRoot(a); err != nil {
					a.Stop()
					return "", err
				}
				return "main 已装配", nil
			}
			a.Stop()
			return "", fmt.Errorf("main: 宿主不接受根实例（%T）", host)
		},
		// Initialize：与其余三个内置类型**同一条路子**（由 registry.Instantiate 调）。
		//
		// ★ 此前它在 `New()` 里自己调 —— 于是主 agent 有两条装配路径
		//   （New 内手调 + 内核 Instantiate），子 agent 只有一条。
		Initialize: func(instance any) error {
			a, _ := instance.(*agent.Agent)
			if a == nil {
				return fmt.Errorf("main: 实例不是 *agent.Agent（装配错误）")
			}
			return InitRuntime(a)
		},
		Help: func(host registry.Host) string {
			return "主 agent = 内核按 core.agent.main_type 选中的那个类型。" +
				"它不是被创建出来的子实例：启动即存在，退出即内核停。"
		},
	}
}

// New 是主 agent的**实例化调用**：组合基座 + 挂上自己的回调。
func New(cfg agent.AgentConfig) (*agent.Agent, error) {
	// 先造载体（上下文统计面要读它），再挂给基座。
	pre := &Module{st: &state{}}
	cfg.TypeName = TypeName
	// ★ 能力面（docs §8.1.1）：主 agent 承接文本。
	cfg.AgentCap = agentIO.CapText
	a := agent.New(cfg)
	pre.a = a
	// ★ 类型自己的面交给基座（docs §0.2 三问：基座要用但不认识实现）。
	a.MountTypeFaces(pre.toolDefsFace, statsFace(pre), nil)
	carriers.Store(a, pre)
	h := a.Hooks()

	// ── 槽位 5：上下文管理（主 agent = **策略裁剪型**，本包实现）──
	//
	// ★ overflow 的三件（写在这里而不是 standard）：阈值 1.25、恢复预算 2、
	//   "frame/水页"判定、剔除顺序、记账 —— 全是**主 agent 自己的上下文哲学**。
	//   驻留子整窗不裁、rt 滑窗，它们一条都用不上。
	if h.ContextBudget == nil {
		h.ContextBudget = ContextBudgetRelevance
	}
	h.OverflowHandler = func(a *agent.Agent, frame any, req agent.OverflowRequest) (bool, int) {
		f, ok := frame.(*TaskFrame)
		if !ok || f == nil {
			return false, 0
		}
		return enforceOverflowMain(a, f, req)
	}

	// ── 已删：Loop 槽位（docs §0.3）──
	//   基座不认识帧，所以没有"驱动一帧"的槽位。本类型的循环在
	//   Runtime.Start 里自己起（见 runtime_main.go 的 StartMainRuntime），
	//   帧推进在本包 task.go。

	// Tool execution is provided by the base/kernel boundary and is not a
	// registered type callback.

	h.Runtime = agent.RuntimeHooks{
		Initialize: func(a *agent.Agent) error { return InitRuntime(a) },
		// ★ 恢复回调：内核把本实例的工作目录交过来（用户裁定 2026-11-19）。
		//   主 agent 要恢复的是**上下文现场**（上一轮聊到哪、哪些页被剔过）。
		Restore: func(a *agent.Agent, dir string) error { return Wrap(a).RestoreContext(dir) },
		Start:   func(x *agent.Agent) { Wrap(x).StartMainRuntime() },
		Stop:    func(x *agent.Agent) { Wrap(x).StopMainRuntime() },
		// ★ 停机给**从未运行**的待办任务补终态（bases 不认识队列，所以是钩子）。
		//   带 ResponseCh 的同步注入方（cli / a2a 无超时）否则会永久挂起。
		DrainTerminal: func(x *agent.Agent, reason string) { Wrap(x).DrainPendingTerminal(reason) },
		// ★ 有待处理中断吗（类型自己的队列状态，基座只经钩子问）。
		InterruptPending: func(x *agent.Agent) bool { return Wrap(x).PendingInterrupts() > 0 },
		// ★ 已执行轮次（状态面用；计数归类型自己的调度器）。
		RoundsExecuted: func(x *agent.Agent) int { return Wrap(x).RoundsExecuted() },
	}

	a.SetHooks(h)

	// 缺槽位 ⇒ 响亮装配错误（基座不静默降级）。
	if err := a.Validate(); err != nil {
		return nil, err
	}
	// ★ **不在这里调 Initialize**（2026-11-25）：
	// 它由 `registry.Instantiate` 统一在 construct 之后调，与其余三个
	// 内置类型、插件类型同一条路径。主 agent 没有第二条装配路径。
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
// 「内核持有基座、类型模块注册回调」⇒ 基座 `agent.Agent.New` 只做**通用**构造
// （字段、槽位、安全闸、IO 路由器）。**主 agent 专属的那些**在这里：
//
//	上下文实现（agenttypes.ContextManager：策略裁剪型）
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
	mm := Wrap(a)
	cfg := a.Config()
	// ★ 内核侧实例管理器（管理本内核下全部子 agent）。
	//
	// 装在这里而不是在 `New` 里：它是**主 agent 作为内核宿主**时才需要的东西
	// （子 agent 自己不是任何实例的宿主）。用户裁定 2026-11-18：
	// 「创建子 agent 执行在主 agent？不应该在内核吗？」—— 归属是内核，
	// 而内核在装配图上就是主 agent 这一层。
	// 上下文实现由**类型**在这里建（路径来自内核分配的实例目录）。
	mm.OpenContext()
	// ★ 顺序要紧：调度循环一起来就读 m.sched（`pumpInbox` 先用 `HasRoom`）。
	//   先装调度器再起循环，否则有一个 nil 窗口（实测 panic）。
	mm.InstallScheduler()
	// ★ 不在这里起运行期：Initialize 只**装配**，Start（Runtime.Start 钩子）
	//   才起循环与恢复。曾经在这里调 StartMainRuntime，后果是
	//   `Instantiate` 就恢复了子 agent —— 于是"重启恢复"这件事在装配期
	//   已经发生，调用方再调一次只会得到"已存在 ⇒ 0 个"（判据当场抓到）。
	installState(mm, cfg)
	// NormalizeNilServices 已删：谁注入谁归一（装配侧的事）
	mm.InjectSecretStore()
	// ★ 内置工具族的执行面由**本类型**注入（基座不认识任何工具族）。
	//   装配点不再替所有类型装一份（那会让"加一种类型"要回来改装配点）。
	mm.InstallBuiltinToolProvider()
	// ★★ 族处理器必须交给基座（2026-11-20 补）。
	//
	// 内核执行设施的 ② 那一步问 `host.BuiltinTools()`；不装 ⇒ 恒 nil ⇒
	// `toolroute.Dispatch` 整条路是死的 ⇒ `output_send__*`（异步通道上
	// **唯一**能对用户说话的路径）、`output_list_channels`、`llm_*`、
	// `secret_*`、`get_plugin_tools`、`plgreload` 全部落到兜底
	// "不存在或未注册"。实测四个工具全废。
	mm.installBuiltinHandlers()
	// ★ 通用扩展槽（docs §6）：**只有被选中为主 agent 的类型才注册**。
	//   这里交出的是图记忆能力（graph_open/recall/commit/close/export）——
	//   子 agent 从此不需要认识本包的 GraphDB / LightMemory。
	if err := a.RegisterExtSlot(mm.graphSlotTools()); err != nil {
		return fmt.Errorf("main: 注册扩展槽失败: %w", err)
	}
	registerStatusSections(a)
	// ★ 子 agent 的恢复**不在这里**：它在 StartMainRuntime 的最前面
	//   （恢复要 Assign inputch ⇒ 必须在调度循环起来之前，且插件已就绪）。
	//   这里只声明维护任务。
	registerMaintenance(a)
	return nil
}
