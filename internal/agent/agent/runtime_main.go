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
	// ★ 订阅**类型注册事件**（用户裁定 2026-11-18：内置插件经 EventBus
	//   动态注册/撤销子 agent 类型）。位置在这里而不是 New：
	//   总线在装配点才注入（AgentConfig.EventBus），而订阅要等它到位。
	//
	// 订阅之后，插件新注册的类型会立刻出现在 admin_child 的 type 枚举里
	// （工具表每轮重建），不需要重启内核。
	a.SubscribeTypeEvents()

	// ★ 先恢复子 agent 再起自己的循环（用户裁定 2026-11-18 的持久化落点）。
	//
	// ## 顺序为什么必须这样（两个实测约束）
	//
	//	① 恢复调用各类型的 Construct ⇒ 它要 `io.ChannelRegistry().Assign(...)`。
	//	   插件已加载（插件在 Agent.Start 之前就装好了），但**父的 input 接收槽**
	//	   是 Start 里才注册的 —— 所以必须在 `go a.schedulerLoop()` 之前。
	//	② 恢复出来的子如果立刻投任务，父的调度器还没起 ⇒ 任务只能排队
	//	   （不会丢），调度器一起来就处理。反之若先起循环再恢复，
	//	   父可能已经在处理"子不存在"期间进来的输入了。
	//
	// 原先 `RestoreResidents` **零调用方** ⇒ 重启后驻留子全丢（这就是那条
	// 缺陷的落点）。现在恢复走**实例表**，不只驻留子：任何注册了
	// Construct 的类型都能被恢复。
	a.RestoreChildren()

	go a.schedulerLoop()
	// ★ 不再起 InterceptLoop：中断分拣已并入 schedulerLoop 的 acceptInput
	//   （单输入通道 + 类型侧标签解释）。两条循环同时读一个 channel 会抢事件。
	a.startPeriodic()
	a.ReembedStaleMedia()
	a.MigrateLegacyGraphMedia()
}

// StopMainRuntime 停主 agent 的运行期：回收驻留子（子不得比父活得久）。
//
// 待办任务的终态由基座 Stop 统一补（任何有队列的类型都需要，不该各写一遍）。
func (a *Agent) StopMainRuntime() {
	a.StopResidents()
}

// RestoreChildren 恢复上次运行时的子 agent（实例表驱动）。
//
// 返回成功恢复的数量。单个失败不阻断其余（一个类型的 Construct 有问题
// 不该让全部子 agent 起不来）。
//
// ★ 单独成函数而不是塞进 StartMainRuntime 的正文：判据要能只测恢复，
//
//	不必把调度器/中断循环一起拉起来。
func (a *Agent) RestoreChildren() int {
	m := a.EnsureInstanceManager()
	if m == nil {
		return 0
	}
	// ★ 顺序：先隔离孤儿目录 → 再迁旧登记表 → 最后按实例表恢复。
	//
	// 为什么隔离必须在恢复之前：孤儿目录里的 graph.db/context.json 会让
	// 一个"看起来该恢复"的实例指向一段来历不明（可能是崩溃残留）的记忆。
	// 先挪到 _legacy/ 再恢复，恢复到的每个实例都有明确来源。
	a.QuarantineLegacyResidentDirs()
	migrateLegacyResidentRegistry(a.dataDir, m)
	n, errs := m.Restore()
	for _, e := range errs {
		a.Logf("restore child skipped: %s", e)
	}
	return n
}
