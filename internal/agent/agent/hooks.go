package agent

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// hooks.go —— **类型行为注册表**（用户裁定的核心机制）。
//
// ## 用户原话（2026-11-17，两次纠正）
//
// > 「有一个 agent 基类，主 agent 与不同子 agent 是这个基类的派生。
// >    注意是通过组合实现，而非继承。」
//
// > 「主 agent 设计上的记忆与上下文管理系统最为复杂，各个子 agent 的上下文
// >    管理方式各不相同，因此应该各个类型的 agent 通过回调注册各自的管理方式。
// >    同时 stage 与 agentloop 也是同理不同类型的 agent 对应的处理应该有所不同，
// >    比如 rt agent 的一切接口都需要是异步的」
//
// > 「不是说了采用回调注册的形式吗，agent『基类提供基础字段与阶段，各个子 agent
// >    （含主 agent）各自挂载各自的回调函数』」
//
// ## 这个结构解决什么
//
// 基座**必须不知道自己是哪一类 agent**。此前基座靠
// `isLightKernel()`（= `parentID != ""`）/ `isRealtime` 字段 /
// `e.(*residentChild)` 断言按名字认识子类型 —— 实测 12 处分支，
// 每加一种类型就要回来给这些 if 再加一条。那种写法是本设计要消灭的嵌合。
//
// 现在：基座只声明**阶段点与槽位**，类型的差异全部由类型模块通过
// `SetHooks` 挂上来。基座代码里**没有任何**"我是 rt / 我是驻留子"的知识。
//
// ## 约定（与七公共面一一对应）
//
//   - 所有槽位 **nil 安全**：未注册即视为**装配错误**（`Validate()` 响亮报错，
//     不静默降级）。唯一的例外是"本类型明确不需要"的几个（`Prune` 对整窗
//     上下文类型是合法的 no-op，见下）。
//   - `NewBase()` **不自带任何默认实现**：默认值会让"忘了注册"变成静默行为漂移。
//   - 钩子收 `*Agent` 而不是把状态拷出来：钩子就是"这个类型对基座的操作"，
//     拷出来反而会让两边状态分叉。
//
// RuntimeHooks groups the lifecycle callbacks for one registered agent type.
// The kernel owns Agent; the type module only supplies this behavior object.
type RuntimeHooks struct {
	// Initialize wires type-owned runtime state into the kernel-owned Agent.
	// It runs during type instantiation, before Start.
	Initialize func(a *Agent) error
	Start      func(a *Agent)
	Stop       func(a *Agent)
}

type Hooks struct {
	// ── 类型身份：**不占槽位**（用户裁定 2026-11-18）──
	//
	// IsChild / ComeFrom 是**数据事实**（层级 → `AgentConfig.ParentID`），
	// 不是行为回调；IsRealtime 是 rt 自己知道的事（自我 set/get 的循环已删）；
	// MayOffload 是**类型能力**，归 `registry.Descriptor.Meta`。
	// 数据和能力都走"声明"，不走"槽位"。

	// Runtime is the single lifecycle registration for this type.
	Runtime RuntimeHooks

	// ── 槽位 5：上下文管理 ──

	// ContextBudget 返回拼装时间线时可用的 token 预算。
	//
	//	策略裁剪型（主）：动态上下文算出的 ContextTokens
	//	整窗型（驻留/rt）：整个模型窗口（只受硬上限约束）
	ContextBudget func(a *Agent, b kit.TokenBudget) int
	// EnforceOverflow 执行超页处置，返回是否已处置与裁掉的事件数。
	//
	// ★ 这是**唯一的超页决策点**（用户 2026-11-18：「overflow 里不应该有具体
	// 逻辑，它应该是一个预定义的钩子，各个子 agent 注册这个钩子来定义各自的
	// overflow 行为」）。基座只提供事实与原语：
	//
	//	OverflowRatioNow / AccumulatedTokens / ContextLen          看得见什么
	//	TokenOf / ContextWindowTokens                              看得见什么
	//	EvictOversizedPages / PruneContext                         能做什么
	//
	// 「水位多少该裁、先剔页还是先归档、恢复时保护多少条」全是策略 ⇒ 在类型侧。
	//
	// nil ⇒ 本类型不做超页处置。那是**明确的策略**（整窗型靠 contextfull
	// 上报父 agent 决策，父来决定压缩/回收/销毁），不是漏接线。
	OverflowHandler func(a *Agent, f *TaskFrame, req agenttypes.OverflowRequest) (handled bool, pruned int)

	// ── 槽位 6：agent loop ──

	// Loop 驱动一帧任务直到终态（`runTaskSteps` 的标准实现挂这里）。
	// rt 模块注册异步版；主/驻留注册 standard 同步版。
	Loop func(a *Agent, f *TaskFrame) agenttypes.Outcome

	// ── 槽位 7：stage ──

	// RunStage 执行某个阶段的插件处理器。nil ⇒ 基座退化为"无插件阶段"。
	RunStage func(a *Agent, stage string, ctx any) bool

	// ── 这里**没有**什么（用户裁定 2026-11-18）──
	//
	//	GraphMemory   图记忆是**类型内部实现**（三形态 + 一无：主 `*GraphDB` /
	//	              驻留·rt `*LightMemory` / worker 无）。一个槽位表达不了，
	//	              已改为类型自己持有，句柄经 `AgentConfig` 落到 `Agent.graph`。
	//	SecurityGate  安全判定归**安全模块**；配置事实进 `AgentConfig.Security`。
	//	              基座只被查询（`Gate()`），未装配时运行态是 `Unguarded`。
	//	Prune / OverBudget  已并入 `OverflowHandler` 的策略内部。
}

// Tool execution is a base/kernel service and is not a hook. The tool-request
// boundary is Agent.RunToolBatch.// Validate 校验**必填槽位**是否齐全（装配期调用，缺了就响亮报错）。
//
// 这里刻意区分"必填"与"合法为空"：
//
// 必填：ContextBudget / Loop / SecurityGate / Runtime
// Tool execution is a base/kernel service and is not a hook.
//
//	合法为空：OverflowHandler（整窗策略由「上报父」处置）
//	        RunStage（无插件阶段时基座退化为直通）
//
// 返回值是**给装配点看的错误文案**，说明缺哪个槽位以及该由谁注册。
func (h Hooks) Validate(who string) error {
	type slot struct {
		name string
		set  bool
		hint string
	}
	slots := []slot{
		{"ContextBudget", h.ContextBudget != nil, "由类型模块注册（主：RelevanceContext；驻留/rt：整窗）"},
		{"Loop", h.Loop != nil, "由类型模块注册（kit.Loop 或 rt 的 Asyncloop）"},
		{"Runtime", h.Runtime.Initialize != nil && h.Runtime.Start != nil && h.Runtime.Stop != nil, "由类型模块注册完整 runtime 生命周期对象"},
	}
	for _, s := range slots {
		if !s.set {
			return &MissingHookError{Agent: who, Slot: s.name, Hint: s.hint}
		}
	}
	return nil
}

// MissingHookError 是"装配期少注册了一个必填槽位"。
//
// ★ 为什么是**错误**而不是默认值：默认值会让"忘了注册"变成静默的行为漂移
// （本仓反复栽过的形态：配了没生效、看起来正常）。装配点必须失败。
type MissingHookError struct {
	Agent string
	Slot  string
	Hint  string
}

func (e *MissingHookError) Error() string {
	return "agent " + e.Agent + ": hooks." + e.Slot + " 未注册 —— " + e.Hint +
		"（未注册的槽位是装配错误，不许静默降级）"
}

// SecurityGate 是安全防火墙的**数据面闸门**（槽位 3）。
//
// ★ 它直接复用 `kernelapi.SecurityGate`：安全模块是**强制的透明层**
// （用户裁定），接口只有一份定义（在叶子包里），这里不再自造一份
// （两份会让"哪份才是契约"变成不可判定）。
type SecurityGate = kernelapi.SecurityGate

// Embedder 是共享词嵌入的窄面（蒸馏与上下文语义相似度用）。
//
// 实现者：`*memory.StaticEmbedder`。方法名带 Map：接口包/本包都不能
// import vector 实现包，而 `vector.Vector` 底层就是 `map[string]float64`。
type Embedder interface {
	VectorizeMap(text string) map[string]float64
	VectorizeCleanMap(text string) map[string]float64
	Loaded() bool
}
