package agent

// llmstage.go —— **`on_llm_call` 阶段**：基座把模型响应流式推给插件。
//
// # 为什么这个阶段归基座（用户裁定 2026-11-27）
//
//	「on_llm_call 是基座携带 agent 标识向插件注册的 handler 流式推送响应，
//	  由插件进行流式消费。目的是为了让 webui 这种流式的展示内容」
//
// LLM 调用归基座：`StreamLLM` / `CompleteLLM` 是唯二入口，回退链、同源重试、
// 可用性标记、活动源、错误分类全在设施内部。所以"模型返回了什么"这件事
// 只有那条链在**第一现场**掌握 —— agentloop 拿到的是类型处理过的副本
// （类型会改写 `resp.Content`、把 ToolCalls 转来转去）。
//
// 而 `on_input` / `pre_action` / `post_action` / `before_output` / `after_output`
// 都是**类型自己调 `RunStage`** 的：rtchild 与 worker 从不调，于是它们的
// 阶段事件根本不产生。挂在本文件的两个入口上，**所有类型自动覆盖** ——
// 不需要每个类型记得触发（那正是"改一处漏一处"的形态）。
//
// # 为什么是阶段而不是事件
//
// 它要能**拦断**（`ctx.Response` 非空 ⇒ 不采用这次响应）与**改写**
// （`ctx.LLMText`）。push-once 的事件语义会把这两件事丢掉。
//
// # 流式形态
//
// 每个增量帧：`Extra["streaming"]=true`，`LLMText` 是**当前累计正文**
// （不是这一片的增量 —— 累计值让消费方不必自己拼接，也避免丢片后无法恢复）。
// 结束帧：`Extra["streaming"]=false`，带完整 `LLMText` / `ReasoningContent` /
// `ToolCalls` / `TokenUsage`。

import (
	"context"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// PushLLMDelta 把一次流式增量推给 `on_llm_call` 的注册方。
//
// round 是本任务内的第几次 LLM 往返（1 起）；消费方据此把增量归到正确的一轮。
func (a *Agent) PushLLMDelta(round int, text, reasoning string) {
	if a == nil {
		return
	}
	ctx := &sdk.StageContext{
		LLMText:          text,
		ReasoningContent: reasoning,
		Extra: map[string]interface{}{
			"streaming": true,
			"round":     round,
		},
	}
	a.RunStage(sdk.StageOnLLMCall, ctx)
}

// PushLLMFinal 把一次 LLM 往返的**最终响应**推给 `on_llm_call` 的注册方。
//
// 返回被改写后的响应：注册方可以改 `ctx.LLMText` / `ctx.ToolCalls` /
// `ctx.Response`（非空 ⇒ 整轮直接采用它，不再使用模型原响应）。
//
// 这是类型拿回"模型说了什么"的**唯一**路径之一（另一种是自己调
// CompleteLLM/StreamLLM 直接拿返回值）；两种情况下推送都会发生 ——
// 插件的流式消费不该因为类型选择了哪条入口而失效。
func (a *Agent) PushLLMFinal(round int, resp *agentAPI.CompletionResponse) (*agentAPI.CompletionResponse, bool) {
	if a == nil || resp == nil {
		return resp, false
	}
	usage := map[string]int{
		"prompt":     resp.TokenUsage.Prompt,
		"completion": resp.TokenUsage.Completion,
		"total":      resp.TokenUsage.Total,
	}
	calls := make([]sdk.ToolCall, 0, len(resp.ToolCalls))
	for _, tc := range resp.ToolCalls {
		calls = append(calls, sdk.ToolCall{
			ID: tc.ID, Name: tc.Name, Plugin: toolPluginOf(tc.Name), Arguments: tc.Arguments,
		})
	}
	ctx := &sdk.StageContext{
		LLMText:          resp.Content,
		ReasoningContent: resp.ReasoningContent,
		TokenUsage:       usage,
		ToolCalls:        calls,
		Extra: map[string]interface{}{
			"streaming": false,
			"round":     round,
		},
	}
	denied := a.RunStage(sdk.StageOnLLMCall, ctx)

	// 拦断：注册方给出了 Response ⇒ 整轮采用它。
	if denied && ctx.Response != nil {
		out := *resp
		out.Content = *ctx.Response
		out.ToolCalls = nil
		return &out, true
	}

	// 改写：正文与工具调用都可被改（与工具阶段的契约一致）。
	out := *resp
	if ctx.LLMText != resp.Content {
		out.Content = ctx.LLMText
	}
	if ctx.ReasoningContent != resp.ReasoningContent {
		out.ReasoningContent = ctx.ReasoningContent
	}
	if len(ctx.ToolCalls) != len(resp.ToolCalls) {
		// 注册方增删了工具调用 ⇒ 按它的来（重建到内核形态）。
		rebuilt := make([]agentAPI.ToolCall, 0, len(ctx.ToolCalls))
		for _, tc := range ctx.ToolCalls {
			rebuilt = append(rebuilt, agentAPI.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
		}
		out.ToolCalls = rebuilt
	}
	return &out, false
}

// wrapStreamWithStage 在基座流上挂一层累加器：每个正文/思维链增量推一次阶段，
// 流结束时再推一帧完整响应。
//
// ★ 为什么在**基座**这一层包：流是设施给的（`streamLLM` 的返回值），
// 只有这里见过每一个 chunk。类型侧的循环各自实现，挂在那里必然漏类型
// （rtchild/worker 从不调 RunStage，连循环都是各自的）。
func (a *Agent) wrapStreamWithStage(ctx context.Context, in <-chan agentAPI.StreamChunk, round int) <-chan agentAPI.StreamChunk {
	if a == nil || in == nil {
		return in
	}
	out := make(chan agentAPI.StreamChunk)
	go func() {
		defer close(out)
		var text, reasoning strings.Builder
		for chunk := range in {
			if chunk.Content != "" {
				text.WriteString(chunk.Content)
				a.PushLLMDelta(round, text.String(), reasoning.String())
			}
			if chunk.ReasoningContent != "" {
				reasoning.WriteString(chunk.ReasoningContent)
				a.PushLLMDelta(round, text.String(), reasoning.String())
			}
			// 结束帧（或工具调用帧）才带完整响应：那些字段一次性给。
			if chunk.Done || len(chunk.ToolCalls) > 0 {
				resp := &agentAPI.CompletionResponse{
					Content:          text.String(),
					ReasoningContent: reasoning.String(),
					FinishReason:     chunk.FinishReason,
					ToolCalls:        chunk.ToolCalls,
				}
				if chunk.Usage != nil {
					resp.TokenUsage = *chunk.Usage
				}
				if final, _ := a.PushLLMFinal(round, resp); final != nil {
					// 把注册方的改写反映到这一帧上，消费方拿到的就是最终值。
					if final.Content != text.String() {
						chunk.Content = ""
						chunk.Done = true
					}
					chunk.ToolCalls = final.ToolCalls
				}
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
