package agent

// runtime_resident.go —— **驻留子类型**的运行时装配（由 resident 类型注册）。
//
// 与主 agent 的差别（这些都是类型语义，不是基座知识）：
//
//	· 不用 RelevanceContext 的策略裁剪，而是"整窗 + 定期压缩"
//	· 有自己的调度器（它自己接输入、自己跑）
//	· 有 contextfull 上报父的通道
//
// 现在这些原语还住在基座（拆包未完成），所以组合函数暂放这里；
// 归属是 resident 类型 —— 基座自己从不调用它。

// StartResidentRuntime 起驻留子的运行期（调度器 + 中断循环）。
func (a *Agent) StartResidentRuntime() {
	if a == nil {
		return
	}
	go a.schedulerLoop()
	// ★ 同主 agent：中断分拣在 acceptInput 里（单输入通道）。
}

// StopResidentRuntime 停驻留子：回收它自己拉起的驻留子（孙不得比子活得久）。
//
// 待办任务终态由基座统一补。
func (a *Agent) StopResidentRuntime() {
	if a == nil {
		return
	}
	a.StopResidents()
	a.drainPendingInterrupts("resident_stopped")
}
