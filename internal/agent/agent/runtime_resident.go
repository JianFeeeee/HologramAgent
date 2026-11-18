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
	go a.InterceptLoop()
}

// StopResidentRuntime 停驻留子：把自己的驻留子也回收（孙不得比子活得久）。
func (a *Agent) StopResidentRuntime() {
	if a == nil {
		return
	}
	a.StopResidents()
	a.drainPendingInterrupts("resident_stopped")
}
