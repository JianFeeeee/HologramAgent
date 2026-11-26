package mainagent

// marks.go —— **主 agent 自己的**标记解释（用户裁定 2026-11-20）。
//
// ## 为什么解释住在类型里
//
// 标记（`priority` / `stop` / `interrupt`）由注入方原样写进 payload，
// 内核与基座**只搬运不解释**。同一个 `priority="L4"`：
//
//	主 agent   抢占：取消当前 LLM + 现场入栈
//	驻留子     高优先级入队（不挂起）
//	rt         任何输入都覆盖当前轮（它根本没有"级别"概念）
//	插件类型   插件自己的 Loop 定义
//
// 四种解释，所以解释不属于任何共享包 —— 否则加第五种类型时它要回来改。
// 取值原语是 io 包的契约（`io.MarksOf`），不是判定。

import (
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sched "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/sched"
)

// isInterruptInput 判定一条输入是否**声明了自己是中断**（主 agent 的判据）。
func (m *Module) isInterruptInput(evt *agentIO.InputEvent) bool {
	if evt == nil {
		return false
	}
	return agentIO.MarksOf(evt.Payload).IsInterrupt()
}

// stopRequested 判定这条中断是不是**显式停止**。
func stopRequested(evt *agentIO.InputEvent) bool {
	if evt == nil {
		return false
	}
	return agentIO.MarksOf(evt.Payload).Flag("stop")
}

// interruptLevelOf 把**已授权**的优先级标签转成调度级别（**主 agent 的策略**）。
//
// ★ 抢占/挂起/排队的行为全在主 agent 的调度循环里；这里只回答
// "这条中断算哪一级"，那是主 agent 对标记的解释。
func interruptLevelOf(evt *agentIO.InputEvent) sched.Level {
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
