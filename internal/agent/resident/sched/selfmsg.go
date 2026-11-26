package sched

// selfmsg.go —— 内核自循环消息的**本地**值类型。
//
// ★ 这是**驻留子自己的**那份。
//
// docs §0.3 铁律：agent 间是隔离实例，任何情况不允许共享。
// 所以主 agent 有 `mainagent/sched.SelfInputMsg`，驻留子有本包这份。
// 两份今天字段相同 —— 那是现状，不是允许共用的理由。

// SelfInputMsg 是一条内核自循环消息（记忆整理 / 子任务通知等）。
type SelfInputMsg struct {
	Text    string
	Channel string
	// Kind 是**注入方声明的标签**（与 L1..L4 同性质）：调度器只搬运，不解释。
	Kind string
}
