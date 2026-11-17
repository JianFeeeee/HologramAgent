package agenttypes

// types.go —— agent 模块的**公共值类型**（供各类型模块共享）。
//
// ## 为什么需要这一层（2026-11-17 拆包）
//
// 拆包后 base / kit / main / resident / rtchild / worker 互相要传值：
// `Step`、`Outcome`、`TaskFrame`。它们若留在 base 包里，各类型模块为了
// 命名这些类型就得 import base 的**实现**（而 base 又要 import kit…），
// 于是"类型模块 → 基座"这条合法方向会被拉成环。
//
// 值类型单独一层 ⇒ 依赖方向变成严格单向：
//
//	值类型 ← kit ← base ← {main, resident, rtchild, worker}
//
// 与 `internal/kernelapi/model` 的定位相同：**唯一声明点 + 实现包反向别名**。

// Step 是任务状态机的游标（声明点）。
//
// 各类型模块的 agentloop 实现按它推进；具体有哪些 step 由**该类型的
// loop 决定**，本层只提供值类型不规定转移。
type Step int

const (
	// StepPrepare 构建消息与工具表、应用中断标记、跑 pre_action 阶段。
	StepPrepare Step = iota
	// StepLLM 轮次顶部（中断/占位）+ LLM 调用 + post_action。
	StepLLM
	// StepToolBatch 并发执行整批工具（仅当全批可并发时）。
	StepToolBatch
	// StepToolBegin 取本批下一个工具，跑 before_toolcall。
	StepToolBegin
	// StepToolExec 执行工具（**临界区**：副作用不可回滚）。
	StepToolExec
	// StepToolAfter after_toolcall、裁剪、消息与事件组装。
	StepToolAfter
	// StepTurnEnd 收尾本批并进入下一轮。
	StepTurnEnd
)

// Outcome 是一次 step / 一帧任务的终态（声明点）。
//
// ★ 它必须是**公共值类型**而不是私有类型：`Hooks.Loop` 的签名出现在
// base 的公共面上，而函数类型可以返回任何**值类型**；私有类型会让
// 外部类型模块无法实现 Loop 槽位（那是拆包的第一道硬墙）。
type Outcome int

const (
	// OutcomeContinue 继续执行下一个 step（游标可能停在原地以表达"重跑"）。
	OutcomeContinue Outcome = iota
	// OutcomeDone 任务成功结束，响应在帧里。
	OutcomeDone
	// OutcomeFailed 任务失败结束，错误在帧里。
	OutcomeFailed
	// OutcomeSuspended 任务在安全点被抢占挂起，帧已保存。
	OutcomeSuspended
)
