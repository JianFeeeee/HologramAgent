package mainagent

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
// 基座自己**不调用**它们（`agent.Agent.Start` 只调注册进来的那个回调）。
// 换句话说：基座持有一个"跑起来"的槽位，主 agent 往里面填自己的实现。
// 等这些原语各自成包后，本文件会随 mainagent 一起搬走 —— 那时基座里
// 一行都不会剩下。

// StartMainRuntime 起主 agent 的 agentloop 与配套（调度器、中断循环、
// 启动期媒体整理）。**只有主 agent 注册它**：rtchild 不吃队列，
// 驻留子有自己的一份（见各自类型模块）。
func (m *Module) StartMainRuntime() {
	// ★ 订阅**类型注册事件**（用户裁定 2026-11-18：内置插件经 EventBus
	//   动态注册/撤销子 agent 类型）。位置在这里而不是 New：
	//   总线在装配点才注入（agent.AgentConfig.EventBus），而订阅要等它到位。
	//
	// （工具表每轮重建），不需要重启内核。

	// ★ **恢复子 agent 不在这里**（2026-11-25 移走）。
	//
	// ## 为什么移走（实测缺陷）
	//
	// 原注释断言"插件已加载（插件在 agent.Agent.Start 之前就装好了）"——
	// **那不是事实**。实际时序（真内核日志）：
	//
	//	20:31:18 [instances] 恢复跳过: q1: 划入 inputch cli: inputch 未注册   ← 恢复在这里
	//	20:31:18 [cli] unix socket listening on .../cli.sock                ← 插件在这里才起
	//
	// 插件是在 `KernelComplete` 之后由 pluginhost 的 `loadRest` 加载的，
	// 而本函数跑在 `agent` 模块的 Init 里（更早）⇒ 恢复时通道表是空的。
	//
	// 后果：**6 个子实例全部恢复失败**，而日志还报 `restored 6`
	// （计数说谎，同一次循环里同时有 6 条"跳过"）。
	//
	// ⇒ 恢复挂在 `SyncBus.OnUpdate`（`PluginsReady`）之后，见
	//   `kernelmod/agent` 的 `mountRestoreOnReady`。
	//

	go m.SchedulerLoop()
	m.a.StartPeriodic()
	m.ReembedStaleMedia()
	m.MigrateLegacyGraphMedia()
}

// StopMainRuntime 停主 agent 的运行期。
//
// ★ 这里**不回收任何子**：子不得比父活得久的约束由 `agent.Agent.Stop` 的内核侧
//
//	统一执行（`children.StopAll`），不是主 agent 的职责 —— 主 agent 不认识
//	任何具体类型的存在（2026-11-19 删掉 `m.a.StopResidents()`）。
//
// 待办任务的终态由基座 Stop 统一补（任何有队列的类型都需要，不该各写一遍）。
func (m *Module) StopMainRuntime() {}

// RestoreChildren 恢复上次运行时的子 agent（实例表驱动）。
//
// 返回成功恢复的数量。单个失败不阻断其余（一个类型的 Construct 有问题
// 不该让全部子 agent 起不来）。
//
// ★ 单独成函数而不是塞进 StartMainRuntime 的正文：判据要能只测恢复，
//
//	不必把调度器/中断循环一起拉起来。
//
// RestoreChildren 恢复上次运行时的子 agent（实例表驱动）。
//
// ★ 生产路径**不再调它**（2026-11-25）：恢复由内核 `InstanceManager.Restore`
// 统一执行，包括根实例自己。本方法保留给"显式要求重建子"的场景（判据/运维），
// 它不是启动流程的一部分。
func (m *Module) RestoreChildren() int {
	im := m.a.EnsureInstanceManager()
	if im == nil {
		return 0
	}
	// 恢复走**实例表**：每个类型的 `Descriptor.Construct` 负责把实例装起来。
	//
	// ★ 这里**没有**任何"驻留子专属"的准备工作（隔离孤儿目录 / 迁旧登记表）。
	//   那些是驻留子类型自己的恢复前置，由它在 `Construct` 里做 —— 基座
	//   不认识任何类型的恢复语义（2026-11-19 删掉这两句具名调用）。
	n, errs := im.Restore()
	for _, e := range errs {
		m.a.Logf("restore child skipped: %s", e)
	}
	return n
}
