package mainagent

// airinterrupt.go —— 主 agent 的**空中断**：自己投给自己的一条无正文自输入。
//
// ## 为什么它必须由类型投，而不是基座替它投
//
// 用户裁定 2026-11-18：
//
//	「空中断由内核发本身就是一种妥协」——
//	妥协在内核（或基座）替 agent 发那条中断；正解是内核触发钩子、钩子自己发。
//
// 所以这里没有 `Agent.RaiseContextOverflow()`（已删），也没有
// `VectorContextOverflow` 中断向量（已删）。主 agent 的超页策略
// 自己往自己的 self input 投一条 `{kind: "air"}`，基座只按标签分派给
// 本类型的 handler —— 基座不认识"air"是什么。
//
// ## 为什么是 self input 而不是中断队列
//
// "抢占 + 在任务边界裁剪"是主 agent 的**排队语义**：
//
//	· 它要的是"当前轮让位，然后由本类型在安全点裁一次"；
//	· 它不需要抢占别的任务、也不需要跨 agent 的中断阶梯。
//
// 走 self input 就拿到了它要的全部：进自己的队列、按自己的顺序执行、
// 由自己的 loop 在安全点处理。rt 子没有队列（滑窗即可）、驻留子整窗不裁
// （上报父），它们的处置完全不同 —— 所以这条通路只属于主 agent。

import "github.com/JianFeeeee/HologramAgent/internal/agent/agent"

// InjectSelfAirInterrupt 投一条**无正文**的空中断到本 agent 的自循环输入。
//
// 语义：抢占当前轮 → 本轮在安全点让位 → 空中断任务被调度时做相关性裁剪。
// 基座侧只需认 `self_kind == "air"` 这个**标签**并回调本类型的 handler；
// "air" 的行为契约（让位、裁剪、裁多少、裁不动怎么办）全在这里。
func InjectSelfAirInterrupt(a *agent.Agent) {
	if a == nil {
		return
	}
	a.InjectSelfChannel(agent.SelfInputMsg{Kind: agent.SelfKindAir})
}
