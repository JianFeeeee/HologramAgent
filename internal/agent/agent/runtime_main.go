package agent

// runtime_main.go —— **主 agent 类型**的运行时装配（由 mainagent 注册）。
//
// ## 为什么这些会出现在基座包里
//
// 它们调用的原语（schedulerLoop / InterceptLoop / 记忆整理 / 驻留子回收）
// 目前仍住在基座（拆包未完成），所以"组合它们"的函数也只能在这里。
// 但它的**归属**是主 agent：
//
//	mainagent 的 RuntimeHooks.Start = StartMainRuntime
//	mainagent 的 RuntimeHooks.Stop  = StopMainRuntime
//
// 基座自己**不调用**它们（`Agent.Start` 只调注册进来的那个回调）。
// 换句话说：基座持有一个"跑起来"的槽位，主 agent 往里面填自己的实现。
// 等这些原语各自成包后，本文件会随 mainagent 一起搬走 —— 那时基座里
// 一行都不会剩下。

// StartMainRuntime 起主 agent 的 agentloop 与配套（调度器、中断循环、
// 启动期媒体整理）。**只有主 agent 注册它**：rtchild 不吃队列，
// 驻留子有自己的一份（见各自类型模块）。
func (a *Agent) StartMainRuntime() {
	go a.schedulerLoop()
	go a.InterceptLoop()
	a.startPeriodic()
	a.ReembedStaleMedia()
	a.MigrateLegacyGraphMedia()
}

// StopMainRuntime 停主 agent 的运行期：先回收驻留子（子不得比父活得久），
// 再给待办任务补终态。
func (a *Agent) StopMainRuntime() {
	a.StopResidents()
	a.drainPendingInterrupts("agent_stopped")
}
