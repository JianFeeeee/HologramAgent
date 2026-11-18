package agent

// complete.go —— 基座对 **LLM 设施**的转发（§3.2.1）。
//
// ## 基座在这里做什么 / 不做什么
//
//	做：把"我要一次补全"翻译成设施的一个调用（带上本 agent 的身份与优先源）
//	不做：解析候选源、重试、跨源回退、标记可用性、切活动源、分类错误
//
// 后者全在 `kernelapi.LLMClient` 内部 —— 那是**内核的策略**，与类型无关。
//
// ★ 为什么 `Preferred` 走请求参数而不是设施状态：它是 `AgentConfig` 的
//   公有字段（每个 agent 可以有自己的首选源），由调用方随请求传入。
//   设施不排序、不判可用性，只按参数调整候选链顺序。

import (
	"context"
	"fmt"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// completeLLMNoRetry 与 completeLLM 相同，但关闭同源瞬时错误重试。
func (a *Agent) completeLLMNoRetry(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	if a.llm != nil {
		return a.llm.Complete(ctx, &kernelapi.LLMRequest{
			AgentID:   string(a.id),
			Preferred: a.preferredProvider,
			Request:   req,
			NoRetry:   true,
		})
	}
	if a.provider == nil {
		return nil, fmt.Errorf("%w: no provider configured", kernelapi.ErrLLMAllFailed)
	}
	resp, err := a.provider.Chat(ctx, req)
	if err != nil {
		return nil, kernelapi.ClassifyLLMError(err)
	}
	return resp, nil
}

// streamLLM 建立一条流（RT 的路径）。
//
// ★ 与 `completeLLM` 同构：候选链、回退、标记都在设施内部。
//
//	抵达这里的错误已经是**类别化**的（`ErrLLMCanceled` / `ErrLLMAllFailed` …），
//	调用方按类别决定"截断还是重跑"。
func (a *Agent) streamLLM(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	if a.llm != nil {
		return a.llm.Stream(ctx, &kernelapi.LLMRequest{
			AgentID:   string(a.id),
			Preferred: a.preferredProvider,
			Request:   req,
			NoRetry:   true,
		})
	}
	if a.provider == nil {
		return nil, fmt.Errorf("%w: no provider configured", kernelapi.ErrLLMAllFailed)
	}
	ch, err := a.provider.ChatStream(ctx, req)
	if err != nil {
		return nil, kernelapi.ClassifyLLMError(err)
	}
	return ch, nil
}

// activeLLMSource 返回**当前活动源**（内核的事实；空 = 尚未成功调用过）。
func (a *Agent) activeLLMSource() string {
	if a.llm == nil {
		if a.provider != nil {
			return a.provider.Name()
		}
		return ""
	}
	if name := a.llm.ActiveSource(string(a.id)); name != "" {
		return name
	}
	// 设施还没记过 ⇒ 用配置里的默认/优先源（状态面要能回答"打算用哪个"）。
	if a.preferredProvider != "" {
		return a.preferredProvider
	}
	if a.provider != nil {
		return a.provider.Name()
	}
	return ""
}
