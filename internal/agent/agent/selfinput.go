package agent

// selfinput.go —— **自循环输入**（类型投给自己的一条输入）。
//
// ★ 为什么归基座：`InjectSelfChannel` 是基座的方法，入参必须与它同住。
//   类型在自己的循环里产出它（"我要再处理一轮这个"），基座只负责投递。
//
// 通道决定处理路径：
//   - `_consolidation_`（io.ChannelConsolidation）：记忆整理，无记忆
//   - 其他值：正常输入路径，写入上下文并发出响应

// SelfInputMsg 是类型**投回自己**的一条输入。
type SelfInputMsg struct {
	Text    string
	Channel string
	// Kind 是**注入方声明的标签**（与 L1..L4 同性质）：基座只透传，不解释。
	//
	// 谁投它、投了之后做什么，全在类型侧（例：主 agent 的空中断投 "air"）。
	Kind string
}

// selfInputMsg 是包内短名（既有调用点逐字不变）。
type selfInputMsg = SelfInputMsg
