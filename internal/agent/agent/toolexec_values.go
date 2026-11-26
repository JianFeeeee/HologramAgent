package agent

// toolexec_values.go —— **基座 ↔ 内核工具执行设施**之间的契约值（§3.2.3）。
//
// ## 为什么与 `ToolExecutor` 同住基座包
//
// 这三个类型只服务一件事：类型把批请求交给基座，基座 call 内核设施，
// 内核执行完把结果（**带工具声明**）返回来。
//
// 所以它们与 `ToolExecutor` 是同一份契约的两面，声明点必须在一起：
//
//	类型 ──▶ 基座（ToolExecutor + 这三个值） ◀── kernelmod/toolexec
//
// ★ 为什么不能放 `kernelapi`：那是叶子层，而本文件经 `sdk` → `agent/provider`
//   是长链（`sdk` 又 import `kernelapi`）—— 放进去实测直接成环。
//   基座本来就 import 这两样，放这里零代价。

import (
	"context"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// OverflowKind 是超页处置的**触发形态**。
//
// ★ 为什么它归基座：它是 `Hooks.OverflowHandler` 的入参之一，而那个钩子
//
//	是基座与类型之间的契约（「overflow 是预定义钩子，各类型注册自己的行为」）。
type OverflowKind int

const (
	// OverflowLocal 是常规入口（发请求前的水位判定 / 上游报错）。
	OverflowLocal OverflowKind = iota
	// OverflowResume 是任务恢复后的重新装配入口。
	OverflowResume
	// OverflowAir 是**类型自己投给自己的空中断**被调度时的入口。
	//
	// 与上面两者同一格钩子：主 agent 超页时投一条无正文自输入，
	// 该任务被调度时回到这里 —— 于是"抢占"与"裁剪"仍是同一条策略的两步，
	// 而不是基座替它发中断（用户裁定：「空中断由内核发是妥协」）。
	OverflowAir
)

// OverflowRequest 是一次超页处置请求（钩子的入参）。
type OverflowRequest struct {
	// Kind 说明这是哪个时机触发的。
	Kind OverflowKind
	// Query 是相关性基准。**由类型推导、类型决定是否采信**。
	Query string
	// Force 表示上游已**明确**报过 context_full（判定已被外部短路）。
	Force bool
}

// ToolCall 是一次工具调用（与 agent/api 的模型同源）。
type ToolCall = agentAPI.ToolCall

// BatchOpts 是一批工具执行的可变参数。
type BatchOpts struct {
	// Host 是**执行宿主**（类型无关的 any：内核设施自己断言它需要的能力）。
	//
	// ★ 与 `registry.ToolCtx.Host` 同一形态、同一理由：本包不 import
	//   任何 agent 实现细节，所以宿主以 any 传递，由**消费方**按自己的
	//   契约断言。内核工具执行设施断言成它自己声明的最小接口。
	//
	// nil = 该调用方没有提供宿主 ⇒ 设施**响亮失败**（不静默放行）。
	Host any
	// Ctx 是本次批量的执行作用域（rt 传整轮 roundCtx，其余传自身 ctx）。
	//
	// 各类型注册的实现必须能接受"这一批属于哪个作用域"——rt 的取消域是
	// 整轮，主 agent 是自身 ctx。让 runner 自己猜（读 a.ctx）会让 rt 的
	// 取消域失效（实测会退化成"打断不了"）。
	Ctx context.Context
	// Channel 是本次批量要回的通道（进 StageContext.Extra，插件据此判权限）。
	Channel string
	// Scenes 是本轮的场景键（rt / 工作式子传 nil）。
	Scenes []string
	// StageCtx 是基底 StageContext；内部按 idx 拆分（Extra 逐份复制）。
	StageCtx *sdk.StageContext
	// NoBatch 为 true 时强制串行（工作式子历史上是逐条串行）。
	NoBatch bool
	// ToolFilter 可在执行前改写/拒绝单个工具（工作式子用它禁输出工具）。
	//
	// 返回非空字符串表示"拒绝并以此作为结果"，不回落到真实执行。
	ToolFilter func(tc ToolCall) string
}

// BatchResult 是单条工具的执行结果（与入参**同序**返回）。
type BatchResult struct {
	CallID string
	Name   string
	Text   string
	Raw    interface{}
	Denied bool

	// Tool 是这条结果对应工具的**完整声明**（SDK 的 ToolDef）。
	//
	// ## 为什么结果必须带声明（用户裁定 2026-11-19）
	//
	// 流向：类型 agentloop 过滤出**自己定义的工具**自己执行，其余丢给基座
	// → 基座 call 内核 → 内核执行 → **把结果 + 声明返给 agentloop**，
	// agentloop 按结果与声明里的标记处理（NoMemory 等）。
	//
	// ToolDef 的字段按「谁懂它的业务含义」分成两半：
	//
	//	内核侧（执行用）  PathArgs / NetworkArgs / ParallelSafe / Serial / Timeout
	//	类型侧（后处理）  NoMemory / Cleaner / ContextPolicy / RecallPolicy
	//
	// 声明只在**注册处**存在，而处理只在**类型**里发生。不带回来的后果（实测）：
	// 类型只能自己去 `stageHost.ToolDef(name)` 再查一遍 —— 那正是
	// "它必须自己执行、不能交给内核"的病根。
	//
	// 零值表示该工具没有任何声明（内核按保守默认处理）。
	Tool sdk.ToolDef
}
