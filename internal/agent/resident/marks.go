package resident

// marks.go —— **驻留子自己的**标记解释。
//
// ★ 解释归类型（docs §7）：驻留子**不挂起、不抢占**，它只有"入队"。
//   所以这里只关心两件事：是不是中断、是不是显式停止。
//   同一个 `priority="L4"` 在主 agent 那里意味着"取消+入栈"，
//   在这里只意味着"排到队首"。

import (
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sched "github.com/JianFeeeee/HologramAgent/internal/agent/resident/sched"
)

// isInterrupt 报告这条输入是否声明了自己是中断。
func isInterrupt(evt *agentIO.InputEvent) bool {
	if evt == nil {
		return false
	}
	return agentIO.MarksOf(evt.Payload).IsInterrupt()
}

// stopRequested 报告这条输入是不是显式停止。
func stopRequested(evt *agentIO.InputEvent) bool {
	if evt == nil {
		return false
	}
	return agentIO.MarksOf(evt.Payload).Flag("stop")
}

// levelOf 把优先级标签转成**驻留子自己的**调度级别。
//
// ★ 与主 agent 的那份不共享：驻留子的映射是它自己的策略，
//
//	主 agent 改自己的阶梯不该影响它（docs §0.3）。
func levelOf(evt *agentIO.InputEvent) sched.Level {
	if evt == nil {
		return sched.DefaultLevel
	}
	switch agentIO.MarksOf(evt.Payload).Priority() {
	case "L4":
		return sched.LevelCritical
	case "L3":
		return sched.LevelInteractive
	case "L2":
		return sched.LevelMessage
	case "L1":
		return sched.LevelBackground
	}
	return sched.DefaultLevel
}
