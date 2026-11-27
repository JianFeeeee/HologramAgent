package agent

import (
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
	// Restore 从 `dir`（**内核传入的本实例工作目录**）读回自己的运行数据。
	//
	// ## 用户裁定（2026-11-19）
	//
	// > 「恢复也是基座提供挂点每个 agent 自己挂自己的恢复回调函数，
	// >    agentloop 持久化数据是由内核传入的路径下存储的，恢复对应的 agent
	// >    时，该回调会被传入对应的路径，也就是说 agent 是把恢复函数挂到这个
	// >    回调由内核驱动恢复的」
	//
	// > 「内核持有的 agent 表里就有 agent 的工作数据目录」
	//
	// > 「agent 初始化包括运行时的数据目录是内核传入的参数，相当于目录是
	// >    内核统一管理的」
	//
	// ⇒ 类型**不拼路径**、**不自己决定何时恢复**：内核在构造/重启实例之后
	// 调本回调，并把该实例的目录交过来。目录里存什么、怎么解析，是类型的事。
	//
	// nil ⇒ 本类型没有需要恢复的数据（明确事实，不是漏接线）。
	Restore func(a *Agent, dir string) error
	Start   func(a *Agent)
	Stop    func(a *Agent)
	// DrainTerminal 给**从未运行**的待办任务补终态（停机时）。
	//
	// ★ 为什么是槽位而不是基座代码：队列归类型（docs §0.3），基座不认识
	//   队列，也就不能枚举里面的任务。类型交出的这个钩子做这件事 ——
	//   带 ResponseCh 的同步注入方（cli / a2a 无超时）否则会永久挂起。
	//
	// nil ⇒ 本类型没有队列（rt / worker），跳过。
	DrainTerminal func(a *Agent, reason string)
	// InterruptPending 报告是否有待处理的中断（类型自己的队列状态）。
	InterruptPending func(a *Agent) bool
	// RoundsExecuted 返回本类型已执行的轮次数（状态面用）。
	RoundsExecuted func(a *Agent) int
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
	// ★ 签名里的帧是 `any`（docs §0.3：基座不认识帧）。
	//   类型把自己的帧传进来并自己断言回去 —— 基座只负责"在正确的时机叫它"。
	OverflowHandler func(a *Agent, frame any, req OverflowRequest) (handled bool, pruned int)

	// ── 槽位 6：agent loop ──

	// ── 已删：Loop 槽位（2026-11-20，docs §0.3）──
	//
	// 它带 `TaskFrame` 签名 ⇒ 基座必须认识"帧"这个概念，而帧是**类型内部
	// 的数据结构**（主 agent 一份、驻留子一份、rt 根本没有）。
	// 删掉之后，类型在 `Runtime.Start` 里起自己的循环，基座只调 Start。

	// ── 槽位 7（已删）：stage ──
	//
	// `RunStage` 曾是槽位，但它的实现**只有一份**（`runStageDefault`：
	// 发事件 + 转给 StageHost），而所有调用点都在本包内。
	// 一个"只能填一种函数"的槽位不是扩展点，是转发层 —— 已删（I 批）。
	// 类型要拦/改阶段行为，走 StageHost 注册的 handler，不走这里。

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
// 必填：ContextBudget / SecurityGate / Runtime
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

// Embedder 是共享词嵌入的**计算面**（docs §3.4：句进 → 向量出，批量）。
//
// 实现者：`kernelapi/semantic` 的 `*StaticEmbedder`（内核设施 FacEmbedder）。
// 基座只按这个窄接口持用 —— 它不认识 fastText / jieba / 模型文件。
type Embedder interface {
	// EmbedTexts 一次输入 N 个句子，返回 N 个稀疏向量（索引一一对应）。
	EmbedTexts(texts []string) []map[string]float64
	// Loaded 报告模型是否真加载（否则只有 TF-IDF 词频回退，仍可算余弦）。
	Loaded() bool
}
