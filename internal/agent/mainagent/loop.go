package mainagent

// loop.go —— 主 agent 的**同步 agentloop**（逐 step 推进，安全点之间可挂起）。
//
// ## 它为什么住本包
//
// 「用哪种方式推进一帧」是**类型的决定**（用户裁定：基座不假设任何
// agentloop 样式）。rt 注册自己的异步驱动，工作式子根本没有状态机。
//
// 所以它在这里，而不是某个"中立的 standard 库"：
//
//	主 agent   本文件（同步）
//	驻留子     同一实现（见 resident 包的注册）
//	rt         自己的异步驱动
//
// 唯一的共享部分是**阶段原语**（`stepPrepare` / `stepLLM` / …）——
// 那些仍住基座，因为它们要读基座的阶段面（RunStage / CompleteLLM …）。
// 等四个类型各自把自己的那一份搬完之后，它们会跟着去各自的包。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
)

// Loop 是标准同步 loop 的注册入口（基座把它填进 `Hooks.Loop`）。
func Loop(a *agent.Agent, f *agenttypes.Frame) agenttypes.Outcome {
	return agent.RunTaskSteps(a, f)
}
