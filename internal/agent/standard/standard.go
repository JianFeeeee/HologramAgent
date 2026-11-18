// Package standard 是 agentloop 与上下文管理的**标准实现库**（用户裁定 2026-11-17）。
//
// ## 定位：库，不是模块
//
// 它不注册 `kernelmod`、不产生依赖图边、不在任何 `Init` 里被构造。
// 它是一份**可复用的实现**，供 main / resident（将来还有别的同步类型）组合。
//
//	类型模块 → standard → agent（阶段原语）→ agenttypes / kit
//
// ## 与 kit 的分工
//
//	kit       零状态部件（错误值、用量账本、输出壳、StageHost、预算计算…）
//	standard  与 Agent 生命周期/bind 有关的**标准实现**（loop、context、runner、stage）
//
// ## 这里**不放**什么
//
// 事件路由、输出投递、定时器刻度、心跳、工具执行 —— 那些是内核事件循环
// （`internal/kernelloop`）的职责。用户裁定：「eventloop 是内核的核心循环
// （处理输入事件路由，输出事件，维持定时器，维持健康心跳），agentloop 才是
// 各个 agent 的循环」。
package standard

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
)

// Loop 是**标准同步 agentloop**（主 agent / 驻留子的实现）。
//
// 它把"逐 step 推进"交给基座的 `LoopStandard`：那个状态机属于"阶段"，
// 而"用不用它、怎么包一层"属于类型。rt 注册自己的异步实现，与本函数无关。
func Loop(a *agent.Agent, f *agenttypes.Frame) agenttypes.Outcome {
	return agent.LoopStandard(a, f)
}

// RunStageStandard 执行插件阶段。
func RunStageStandard(a *agent.Agent, stage string, ctx any) bool {
	return a.RunStageStandard(stage, ctx)
}

// RunToolBatch is the kernel tool-request boundary. It is not a
// type-specific tool runner.
