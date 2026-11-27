package agenttype

// runtime.go —— 给插件的**中立运行时视图**（AgentRuntime 的实现）。
//
// 插件只经这里碰内核：LLM / 工具 / 事件 / 日志。它拿不到 IO 通道、
// 实例管理器、安全闸的原始策略。

import (
	"fmt"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// pluginRuntime 是 AgentRuntime 的实现（一个实例一个）。
type pluginRuntime struct{ a *agent.Agent }

func newRuntime(a *agent.Agent) pubsdk.AgentRuntime {
	if a == nil {
		return &pluginRuntime{}
	}
	return &pluginRuntime{a: a}
}

func (r *pluginRuntime) ID() string {
	if r.a == nil {
		return ""
	}
	return string(r.a.ID())
}

func (r *pluginRuntime) IsChild() bool {
	if r.a == nil {
		return false
	}
	return r.a.IsChildAgent()
}

// Complete 跑一次阻塞的 LLM 调用。
//
// ★ 不做并发/重试/fallback：那些在内核的 LLM 设施里（`AgentConfig.LLM`），
//
//	agentloop 是**阻塞流水线**（target-architecture §5.2）。
func (r *pluginRuntime) Complete(req *pubsdk.CompletionRequest) (*pubsdk.CompletionResponse, error) {
	if r.a == nil || req == nil {
		return nil, fmt.Errorf("agenttype: runtime 未装配")
	}
	// 中立 Message → api Message
	msgs := make([]agentAPI.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		am := agentAPI.Message{Role: m.Role, Content: m.Content}
		for _, tc := range m.ToolCalls {
			am.ToolCalls = append(am.ToolCalls, agentAPI.ToolCall{
				ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
			})
		}
		am.ToolCallID = m.ToolCallID
		msgs = append(msgs, am)
	}
	resp, err := r.a.CompleteLLMNoRetry(r.a.Ctx(), &agentAPI.CompletionRequest{
		Messages:        msgs,
		MaxTokens:       req.MaxTokens,
		ToolChoice:      req.ToolChoice,
		DisableThinking: req.DisableThinking,
	})
	if err != nil {
		return nil, err
	}
	out := &pubsdk.CompletionResponse{
		Content: resp.Content, Reasoning: resp.ReasoningContent,
		FinishReason: resp.FinishReason,
	}
	for _, tc := range resp.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, pubsdk.ToolCall{
			ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
		})
	}
	return out, nil
}

// RunTools 提交一批工具，拿回同序结果。
//
// ★ 结果里带**工具声明**（`Tool`），标记（NoMemory / ContextPolicy / RecallPolicy）
//
//	由插件的 Loop 自己消费 —— 内核只执行、不解释（用户裁定 2026-11-19）。
func (r *pluginRuntime) RunTools(calls []pubsdk.ToolCall) []pubsdk.ToolResult {
	if r.a == nil || len(calls) == 0 {
		return nil
	}
	kc := make([]agent.ToolCall, 0, len(calls))
	for _, c := range calls {
		kc = append(kc, agent.ToolCall{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
	}
	// 通道：插件类型不持有默认通道，用内置标记（工具执行本身不产出用户可见输出）。
	results := r.a.RunToolBatch(r.a.Ctx(), kc)
	out := make([]pubsdk.ToolResult, 0, len(results))
	for _, br := range results {
		out = append(out, pubsdk.ToolResult{
			CallID: br.CallID, Name: br.Name,
			Success: br.Raw == nil || !kit.IsToolError(br.Raw),
			Result:  br.Text,
		})
	}
	return out
}

// Publish 是**内核模块之间**的事件通道，不是 agent 的。
//
// ★ agent 侧没有事件总线（docs §9）：agent 与外界只有三条通道 ——
//
//	阶段钩子、输出通道、输入通道。插件类型要通知外界，走输出通道。
//
// 保留本方法是为了不改 SDK 契约（插件类型仍拿同一个 AgentRuntime），
// 但它**不发总线事件**：发出去也没人该订阅。
func (r *pluginRuntime) Publish(eventType string, payload map[string]interface{}) {
	if r.a == nil {
		return
	}
	r.a.Logf("agenttype: Publish(%q) ignored (agent types have no event bus, see docs §9)", eventType)
}

func (r *pluginRuntime) Logf(format string, args ...interface{}) {
	if r.a == nil {
		return
	}
	r.a.Logf(format, args...)
}

var _ = types.AgentID("")
