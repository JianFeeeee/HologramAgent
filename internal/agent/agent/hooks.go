package agent

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
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
type Hooks struct {
	// ── 类型身份（**数据事实**，不是类型结论）──
	//
	// IsChild 回答"我是不是某个父下的子 agent"。它是**层级事实**
	// （`parentID != ""`），不是类别判断 —— 驻留子与 rtchild 都是子，
	// 但它们的上下文/loop/toolcall 完全不同，那些差异走下面的槽位。
	IsChild bool
	// IsRealtime 回答"我是不是实时通话子 agent"（rt 的语义：单槽覆盖、
	// 最新者胜、一切接口异步）。由 rt 模块注册为 true。
	IsRealtime bool
	// ComeFrom 给出本 agent 发起工具调用时的来源标签
	// （`comefrom.go` 用它让插件判定"这次调用是谁发起的"）。
	// 子 agent 是 internal（转投保留原事件来源，但执行者变了）；根是 user。
	ComeFrom string
	// MayOffload 回答"本 agent 允不允许把积压任务转投给驻留子"。
	//
	// 只有主 agent 注册为 true：让子再去拉孙子会形成无界增殖
	// （每层都能拉 MAX 个），而积压的源头是根那一条调度链。
	MayOffload bool

	// ── 槽位 5：上下文管理 ──

	// ContextBudget 返回拼装时间线时可用的 token 预算。
	//
	//	策略裁剪型（主）：动态上下文算出的 ContextTokens
	//	整窗型（驻留/rt）：整个模型窗口（只受硬上限约束）
	ContextBudget func(a *Agent, b kit.TokenBudget) int
	// Prune 按相关性裁剪上下文，返回裁掉的事件数；0 = 不做。
	//
	// 整窗型注册为 nil ⇒ 基座视为 no-op（这是**唯一**合法的空槽位：
	// "我不做裁剪"是一个明确的策略，不是漏接线）。
	Prune func(a *Agent, query string, protected int) int
	// OverBudget 是"是否已超出窗口预算"的纯判据。nil ⇒ 恒 false。
	OverBudget func(a *Agent) bool
	// EnforceOverflow 执行超页处置（裁剪 + 上报 L4），返回是否需要重跑。
	// nil ⇒ 不做（整窗型依赖 contextfull 上报父 agent，见 overflow.go 的注释）。
	EnforceOverflow func(a *Agent, f *TaskFrame, query string, force bool) (handled bool, pruned int)

	// ── 槽位 6：agent loop ──

	// Loop 驱动一帧任务直到终态（`runTaskSteps` 的标准实现挂这里）。
	// rt 模块注册异步版；主/驻留注册 standard 同步版。
	Loop func(a *Agent, f *TaskFrame) agenttypes.Outcome

	// ── 槽位 7：stage ──

	// RunStage 执行某个阶段的插件处理器。nil ⇒ 基座退化为"无插件阶段"。
	RunStage func(a *Agent, stage string, ctx any) bool

	// ── 槽位 1：toolcall ──

	// ToolRunner 执行一批工具，返回与入参**同序**的结果。
	// rt 模块注册全异步实现（调用方不被阻塞）。
	ToolRunner ToolRunner

	// ── 槽位 2：可选记忆（可运行期覆盖）──

	// GraphMemory 是本 agent 的图记忆面（可 nil = 没有图记忆）。
	//
	// ★ 它是**可选槽位**（用户 2026-11-17 纠正：图记忆是可选配置）：
	//   主 agent 装 `*memory.GraphDB`；驻留/rt 装 `*memory.LightMemory`；
	//   工作式子与独立工具装 nil。
	GraphMemory GraphMemory

	// ── 槽位 3：内核服务（安全等）──

	// SecurityGate 是安全防火墙的数据面闸门（可 nil = 未启用）。
	//
	// 其余内核能力（插件注册表、文档记忆、知识库…）**不进 Agent 字段**：
	// 类型模块在自己的装配点经 `kstate.Require` 取，基座一个都不持有。
	SecurityGate SecurityGate
}

// ToolRunner 是 toolcall 槽位的契约。
//
// 只声明"执行一批调用并按索引顺序返回结果"这一件事：具体的授权闸、
// schema 校验、超时、脱敏都在实现里（standard 提供同步版，rt 提供异步版）。
type ToolRunner interface {
	RunBatch(a *Agent, calls []agentAPI.ToolCall, opts BatchOpts) []BatchResult
}

// Validate 校验**必填槽位**是否齐全（装配期调用，缺了就响亮报错）。
//
// 这里刻意区分"必填"与"合法为空"：
//
//	必填：ContextBudget / OverBudget / Loop / ToolRunner / SecurityGate
//	合法为空：Prune / EnforceOverflow（整窗策略明确不做裁剪与超页处置）
//	        GraphMemory（图记忆是可选配置）
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
		{"OverBudget", h.OverBudget != nil, "由类型模块注册（主：按窗口预算判定；驻留/rt：恒 false）"},
		{"Loop", h.Loop != nil, "由类型模块注册（kit.Loop 或 rt 的 Asyncloop）"},
		{"ToolRunner", h.ToolRunner != nil, "由类型模块注册（kit.ToolRunner 或 rt 的 AsyncToolRunner）"},
		{"SecurityGate", h.SecurityGate != nil, "由类型模块在装配点从 kernelapi.FacSecurity 取"},
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
