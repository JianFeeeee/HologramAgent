package cli

import (
	"fmt"
	"sync"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// push.go —— 输出通道与 stage 处理器把内容**直接**送到当前客户端连接。
//
// ## 为什么没有总线
//
// 用户裁定（2026-11-25）：「这是传递 agent 本身的状态，而非基座状态，
// 不应该借用内部 bus」。
//
// 三条推送源全是**本插件自己的闭包**：
//
//	on_llm_call 阶段  基座在 LLM 流上逐 chunk 推送（流式的唯一载体）
//	stage handler      agent 跑 RunStage → 本插件的回调
//
// 它们与"当前有哪些连接在对话"住在同一个 Plugin 实例里。中间加发布/订阅
// 只是把同步调用改成异步消息 —— 凭空多出订阅者表、事件类型、重放语义。
//
// ★ 与内核事件总线的界限（docs §9.4）：总线是**内核模块之间**的通道。
//   终端帧、进度提示都是插件侧状态，不进总线。

// trackConn 登记一路正在对话的连接，返回注销函数。
func (p *Plugin) trackConn(w *connWriter) func() {
	if p == nil || w == nil {
		return func() {}
	}
	p.activeMu.Lock()
	if p.active == nil {
		p.active = make(map[*connWriter]struct{})
	}
	p.active[w] = struct{}{}
	p.activeMu.Unlock()
	return func() {
		p.activeMu.Lock()
		delete(p.active, w)
		p.activeMu.Unlock()
	}
}

// broadcast 把一帧写给**当前所有**正在对话的连接。
//
// ★ 这是插件的**扇出**，不是发布/订阅：没有事件类型订阅、没有订阅者身份、
// 也没有跨进程。要在将来按连接过滤（例如只推给某一路），在这里加参数即可。
func (p *Plugin) broadcast(frame map[string]interface{}) {
	if p == nil {
		return
	}
	p.activeMu.Lock()
	targets := make([]*connWriter, 0, len(p.active))
	for w := range p.active {
		targets = append(targets, w)
	}
	p.activeMu.Unlock()
	for _, w := range targets {
		w.writeLine(frame)
	}
}

// stagePusher 返回一个"推一条固定阶段"的 stage 处理器。
//
// ★ 为什么 cli 要自己注册 stage：容器型客户端（如 webui）用 SSE 看进度，
// 而 cli 是**唯一**看不到阶段进展的客户端 —— agent 跑长任务时终端长时间
// 静默，用户分不清"在干活"还是"卡住了"。这条通知属于 cli 自己的表现层，
// 不该依赖别的插件去发事件。
func (p *Plugin) stagePusher(phase, tool string) func(*sdk.StageContext) error {
	return func(*sdk.StageContext) error {
		p.broadcast(map[string]interface{}{"type": "stage", "phase": phase, "tool": tool})
		return nil
	}
}

// stageOnLLMCall 消费**流式增量**（基座在 on_llm_call 上逐 chunk 推送）。
//
// 契约（见 sdk.StageContext 注释）：
//
//	Extra["streaming"] = true   增量帧：LLMText 是**当前累计正文**
//	Extra["streaming"] = false  结束帧：带完整正文 / 思维链 / ToolCalls / usage
//
// ★ 为什么用累计值而不是这一片的增量：累计值让消费方不必自己拼接，
//
//	也避免丢片后无法恢复（网络抖动时增量帧可能少一个）。
//	终端只能写"新增的那部分"，所以这里自己 diff —— 那是**显示层的事**，
//	不是协议的一部分。
func (p *Plugin) stageOnLLMCall() func(*sdk.StageContext) error {
	// 每个 agent 各自的已显示长度（并发时不能共用一份，否则串号）。
	var mu sync.Mutex
	shown := map[string][2]int{} // agentID → [正文已显示, 思维链已显示]

	return func(ctx *sdk.StageContext) error {
		if ctx == nil {
			return nil
		}
		streaming, _ := ctx.Extra["streaming"].(bool)
		key := ctx.AgentID
		if key == "" {
			key = "_"
		}

		mu.Lock()
		cur := shown[key]
		var newText, newReasoning string
		if streaming {
			// 增量帧：算出比上次多出来的部分。
			if len(ctx.LLMText) > cur[0] {
				newText = ctx.LLMText[cur[0]:]
			}
			if len(ctx.ReasoningContent) > cur[1] {
				newReasoning = ctx.ReasoningContent[cur[1]:]
			}
			cur[0], cur[1] = len(ctx.LLMText), len(ctx.ReasoningContent)
			shown[key] = cur
		} else {
			// 结束帧：重置计数（下一轮从零开始）。
			delete(shown, key)
		}
		mu.Unlock()

		if newReasoning != "" {
			fmt.Print(newReasoning) // 终端照写（人类可读）；机器客户端走下面的事件帧
			p.broadcastDelta("reasoning_delta", newReasoning, ctx)
		}
		if newText != "" {
			fmt.Print(newText)
			p.broadcastDelta("content_delta", newText, ctx)
		}
		return nil
	}
}

// broadcastDelta 把一段增量推给机器客户端（socket JSON 行协议）。
//
// 带 agent 标识：主 agent 与子 agent 并发跑时，客户端要能分开显示各自的流。
func (p *Plugin) broadcastDelta(frameType, text string, ctx *sdk.StageContext) {
	frame := map[string]interface{}{"type": frameType, "content": text}
	addAgentIdentity(frame, ctx)
	p.broadcast(frame)
}

// stageOnToolcall 推"正在执行哪个工具"（内核执行链触发，带工具名与参数）。
//
// ★ 与 before_toolcall 的分工：那个是**执行前**的钩子（可拦下）；
//
//	on_toolcall 是内核执行链握着工具时的**执行事实**推送 —— 参数已经是
//	最终参数，工具**正在执行中**（不是"将要"：那是 before_toolcall）。
//	cli 用它告诉用户"正在跑什么"。
func (p *Plugin) stageOnToolcall() func(*sdk.StageContext) error {
	return func(ctx *sdk.StageContext) error {
		if ctx == nil || len(ctx.ToolCalls) == 0 {
			return nil
		}
		tc := ctx.ToolCalls[0]
		frame := map[string]interface{}{
			"type":  "tool_call",
			"phase": "on_toolcall",
			"tool":  tc.Name,
			"args":  tc.Arguments,
		}
		addAgentIdentity(frame, ctx)
		p.broadcast(frame)
		return nil
	}
}

// stageToolAfterPusher 推"工具执行完毕"（带结果摘要）。
//
// 与 before 成对：终端用户要看到"这个工具跑完了"这一行，否则长任务里
// 只有开始没有结束。
func (p *Plugin) stageToolAfterPusher() func(*sdk.StageContext) error {
	return func(ctx *sdk.StageContext) error {
		if ctx == nil || len(ctx.ToolCalls) == 0 {
			return nil
		}
		frame := map[string]interface{}{
			"type":  "stage",
			"phase": "after_toolcall",
			"tool":  ctx.ToolCalls[0].Name,
		}
		if len(ctx.ToolResults) > 0 {
			if s, ok := ctx.ToolResults[0].Result.(string); ok {
				frame["result"] = truncateOneLine(s, 160)
			}
		}
		addAgentIdentity(frame, ctx)
		p.broadcast(frame)
		return nil
	}
}

// stageToolPusher 推"即将调用某工具"的阶段（工具名从 ctx 取）。
func (p *Plugin) stageToolPusher() func(*sdk.StageContext) error {
	return func(ctx *sdk.StageContext) error {
		tool := ""
		if ctx != nil && len(ctx.ToolCalls) > 0 {
			tool = ctx.ToolCalls[0].Name
		}
		p.broadcast(map[string]interface{}{"type": "stage", "phase": "before_toolcall", "tool": tool})
		return nil
	}
}

// addAgentIdentity 把**这次阶段属于哪个 agent** 附进帧。
//
// 同一内核里主 agent 与子 agent 并发跑，二者各自的文本与 toolcall 是
// **不同的现场**。不带身份，终端只能把它们混成一条流 —— 看起来像是同一个
// agent 在同时干两件事，排查时也无从知道"这个工具是谁调的"。
//
// 字段由基座填（`StageContext.AgentID/AgentType/IsChild`，见
// `agent.stampStageIdentity`），插件只转发，不自己推断。
func addAgentIdentity(frame map[string]interface{}, ctx *sdk.StageContext) {
	if frame == nil || ctx == nil {
		return
	}
	if ctx.AgentID != "" {
		frame["agent_id"] = ctx.AgentID
	}
	if ctx.AgentType != "" {
		frame["agent_type"] = ctx.AgentType
	}
	frame["is_child"] = ctx.IsChild
}
