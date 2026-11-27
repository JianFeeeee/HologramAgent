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

// CompleteLLM 走**内核 LLM 设施**做一次非流式补全。
//
// ★ 类型只调这一个（或 StreamLLM），不许碰 `a.LLM()` / `a.Provider()`。
// 回退链、同源重试、可用性标记、活动源、错误分类全在设施内部。
func (a *Agent) CompleteLLM(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return a.completeLLM(ctx, req, false)
}

// CompleteLLMNoRetry 同上，但关闭同源瞬时错误重试。
//
// 短生命周期的子任务用它：重试只会把一个已经抖动的时间窗拉长。
func (a *Agent) CompleteLLMNoRetry(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return a.completeLLM(ctx, req, true)
}

// completeLLM 是两条补全入口的唯一实现。
func (a *Agent) completeLLM(ctx context.Context, req *agentAPI.CompletionRequest, noRetry bool) (*agentAPI.CompletionResponse, error) {
	// ★ 所有返回都经 `PushLLMFinal` 出去（用户裁定 2026-11-27）：
	//   `on_llm_call` 阶段由**基座**在设施入口触发，携带 agent 标识把响应
	//   推给插件（webui 这类客户端据此流式展示）。类型不需要、也不该
	//   自己再触发一次 —— 那条路 rtchild/worker 从不走。
	if a.llm != nil {
		resp, err := a.llm.Complete(ctx, &kernelapi.LLMRequest{
			AgentID:   string(a.id),
			Preferred: a.PreferredProvider(),
			Request:   req,
			NoRetry:   noRetry,
		})
		if err != nil {
			return nil, err
		}
		final, _ := a.PushLLMFinal(1, resp)
		return final, nil
	}
	if a.provider == nil {
		return nil, fmt.Errorf("%w: no provider configured", kernelapi.ErrLLMAllFailed)
	}
	resp, err := a.provider.Chat(ctx, req)
	if err != nil {
		return nil, kernelapi.ClassifyLLMError(err)
	}
	final, _ := a.PushLLMFinal(1, resp)
	return final, nil
}

// StreamLLM 建立一条流（类型的唯一流式入口）。
//
// 与 CompleteLLM 同构：候选链、回退、标记都在设施内部。
//
// ★ 与 `completeLLM` 同构：候选链、回退、标记都在设施内部。
//
//	抵达这里的错误已经是**类别化**的（`ErrLLMCanceled` / `ErrLLMAllFailed` …），
//	调用方按类别决定"截断还是重跑"。
func (a *Agent) streamLLM(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	if a.llm != nil {
		ch, err := a.llm.Stream(ctx, &kernelapi.LLMRequest{
			AgentID:   string(a.id),
			Preferred: a.PreferredProvider(),
			Request:   req,
			NoRetry:   true,
		})
		if err != nil {
			return nil, err
		}
		// ★ 流式增量在**基座**这一层推送（见 llmstage.go）：
		//   只有这里见过每一个 chunk，类型侧的循环各自实现、必然漏类型。
		return a.wrapStreamWithStage(ctx, ch, 1), nil
	}
	if a.provider == nil {
		return nil, fmt.Errorf("%w: no provider configured", kernelapi.ErrLLMAllFailed)
	}
	ch, err := a.provider.ChatStream(ctx, req)
	if err != nil {
		return nil, kernelapi.ClassifyLLMError(err)
	}
	return a.wrapStreamWithStage(ctx, ch, 1), nil
}

// LLMMaxContextTokens 返回当前源的上下文窗口（0 = 未知）。
//
// ★ 类型算 token 预算要它。窗口是源的属性，源表归内核，基座只转发。
func (a *Agent) LLMMaxContextTokens() int {
	if a.llm != nil {
		return a.llm.MaxContextTokens()
	}
	if a.provider != nil {
		return a.provider.MaxContextTokens()
	}
	return 0
}

// LLMSources 返回候选源快照（名字/声明能力/可用性）。
//
// ★ 类型按能力挑源时读它（主模型看不了图 ⇒ 换一个声明了 vision 的源），
// 然后把自己的选择放进 `CompletionRequest.Source`。类型不碰 ProviderManager。
// 未装配设施 ⇒ 只有一条源（裸 agent / 单测），能力靠类型断言。
func (a *Agent) LLMSources() []kernelapi.LLMSourceInfo {
	if a.llm != nil {
		return a.llm.Sources()
	}
	if a.provider == nil {
		return nil
	}
	name := a.provider.Name()
	return []kernelapi.LLMSourceInfo{{
		Name:      name,
		Vision:    agentAPI.ProviderSupportsVision(a.provider),
		Audio:     agentAPI.ProviderSupportsAudio(a.provider),
		Available: true,
	}}
}

// LLMSourceSupports 回答"当前活动源声明了这个能力吗"（kind: image / audio）。
//
// 快照里找不到活动源（尚未调用过 / 单源裸装配）⇒ false：
// 宁可多走一次回退，也不要让主模型收下一个它看不懂的块。
func (a *Agent) LLMSourceSupports(kind string) bool {
	active := a.ActiveLLMSource()
	for _, si := range a.LLMSources() {
		if si.Name != active {
			continue
		}
		if kind == "audio" {
			return si.Audio
		}
		return si.Vision
	}
	return false
}

// activeLLMSource 返回**当前活动源**（内核的事实；空 = 尚未成功调用过）。
// ActiveLLMSource 返回当前活动 LLM 源名（类型诊断用）。
func (a *Agent) ActiveLLMSource() string {
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
	if a.PreferredProvider() != "" {
		return a.PreferredProvider()
	}
	if a.provider != nil {
		return a.provider.Name()
	}
	return ""
}
