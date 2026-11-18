package agent

// runtime_hooks.go —— 槽位表的**局部改写**工具。
//
// ★ 为什么需要它：`SetHooks` 是**整体覆盖**（装配期语义，用户裁定过）。
// 类型模块在装配点挂完自己的槽位后，运行期要补一个 Runtime 时，
// 直接 SetHooks 会把已挂的 security/上下文策略全清空。
//
// 所以运行期只允许"取出现有表 → 改这一项 → 挂回"，由本函数统一表达。
func withRuntimeHooks(h Hooks, rt RuntimeHooks) Hooks {
	h.Runtime = rt
	return h
}
