package sched

// selfmsg.go —— 内核自循环消息的**本地**值类型。
//
// ★ 原先是 `agenttypes.SelfInputMsg`（公共值层）。按 docs §0.3 铁律
//   （agent 间是隔离实例，任何情况不允许共享），它归**用它的调度器**：
//   主 agent 的这份在这里，驻留子的那份在 resident/sched 里各自定义。

// SelfInputMsg 是一条内核自循环消息（记忆整理 / 子任务通知等）。
type SelfInputMsg struct {
	Text    string
	Channel string
	// Kind 是**注入方声明的标签**（与 L1..L4 同性质）：调度器只搬运，不解释。
	Kind string
}
