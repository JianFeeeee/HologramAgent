package agent

// runtime_rt.go —— **rtchild 类型**的运行时装配（由 rt 类型注册）。
//
// rt 的语义与另外两类**根本不同**（用户裁定：一切接口异步）：
//
//	· 不吃队列：单槽覆盖，最新输入赢（没有 schedulerLoop）
//	· 无轮询：只有一个 rtLoop 消费自己的槽
//	· 异步体现在输入注入与输出消费，而不是第二个工具执行器
//
// 所以它注册的 Start/Stop 跟另两类不同 —— 这正是"类型注册自己的运行行为"
// 的意义：基座只持一个槽位，填什么由类型决定。

// StartRTRuntime 起 rt 的运行期（只起 rtLoop 的驱动，不起队列）。
func (a *Agent) StartRTRuntime() {
	if a == nil {
		return
	}
	go a.InterceptLoop()
}

// StopRTRuntime 停 rt：把在跑的 rt 实例全部回收。
func (a *Agent) StopRTRuntime() {
	if a == nil {
		return
	}
	a.StopRTChildren()
}
