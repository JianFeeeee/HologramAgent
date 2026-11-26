package rtchild

// runtime.go —— **rt 类型**的运行时装配（由 rt 类型注册）。
//
// ## rt 的语义与另外两类根本不同（用户裁定：一切接口异步）
//
//	· 不吃队列：单槽覆盖，最新输入赢（没有 schedulerLoop）
//	· 无轮询：只有一个 rtLoop 消费自己的槽
//	· 异步体现在输入注入与输出消费，而不是第二个工具执行器
//
// 所以它注册的 Start/Stop 跟另两类不同 —— 这正是"类型注册自己的运行行为"
// 的意义：基座只持一个槽位，填什么由类型决定。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

// StartRuntime 起 rt 的运行期（只起它的中断分拣，不起队列）。
//
// ★ rt 的主驱动是 `rtLoop`（由 SpawnRTChild 在实例建好之后起）；
//
//	这里只装"停止/打断"这一路 —— 它不吃队列，所以没有 schedulerLoop。
func StartRuntime(a *agent.Agent) {
	if a == nil {
		return
	}
	// ★ 分拣是**本类型自己的**（rtchild/intercept.go）：rt 没有级别概念，
	//   任何输入都覆盖当前轮。借主 agent 的 InterceptLoop 会让 rt 走进
	//   "四级抢占阶梯"那条完全不属于它的路。
	go Wrap(a).InterceptLoop()
}

// StopRuntime 停 rt 的运行期。
//
// ★ 不回收子：子不得比父活得久由内核侧 `Agent.Stop` 统一执行。
func StopRuntime(a *agent.Agent) {}
