package kernelapi

// llmclient.go —— **LLM 设施**的对外契约（§3.2.1）。
//
// ## 归属（用户裁定）
//
// 重试、fallback、可用性标记、活动源选择、用量记账**永远在内核**。
// 基座只描述「这一轮我要一个补全」，并收到**可分类的错误**。
//
// 改造前这些全在 `internal/agent/agent`（`callLLMWithFallback` 120 行 +
// `chatStreamWithFallback` 110 行 + `streamWithFallback` 45 行 + 各类型的
// 裸 `a.provider.Chat` 调用）：
//
//	· 同一套回退语义在四个地方各写一遍（主 / rt / spawn / resident）
//	· 基座知道"有两个源"这件事 —— 而它本该只知道"我要一次补全"
//	· `a.provider = fbProvider` 把活动源变成**基座字段**，
//	  于是"现在到底在用哪个源"有两个来源（manager 的排序 + 那个字段）
//
// 现在：基座只持一个 `LLMClient` 句柄并转发。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// LLMRequest 是一次补全请求 + **调用方上下文**（只用于源选择与审计）。
type LLMRequest struct {
	// AgentID 是发起方身份（审计与用量归集用；不参与策略）。
	AgentID string
	// Preferred 是优先源名（空 = 用总默认源）。**优先，不是独占**：
	// 它把该源排到候选链最前，其余源仍作回退。
	Preferred string
	// Request 是标准的补全请求（消息 / 工具 / 上限）。
	Request *agentAPI.CompletionRequest
	// NoRetry 关闭"同源瞬时错误重试"（短生命周期的子任务用它：
	// 重试只会把一个已经抖动的时间窗拉长）。
	NoRetry bool
}

// LLMClient 是内核给基座的稳定抽象。
type LLMClient interface {
	// Complete 一轮非流式请求。
	//
	// 内部完成：源解析（含 Preferred 提前）→ 每源用自己的 model →
	// 瞬时错误重试（NoRetry 时跳过）→ 跨源回退 → 可用性标记/复位 →
	// 活动源更新。返回的错误**已分类**（见下面的哨兵值）。
	Complete(ctx context.Context, req *LLMRequest) (*agentAPI.CompletionResponse, error)

	// Stream 建立一条流。
	//
	// 语义与 Complete 一致，额外一条：流**建立失败**时回落到非流式的
	// 逐块合成（某些 provider 不支持流式，它们用 Canceled 表示"请走非流式"）。
	// 注意：响应**内容**的累积、增量事件的发布归**基座**（那是它与通道的约定）。
	Stream(ctx context.Context, req *LLMRequest) (<-chan agentAPI.StreamChunk, error)

	// ActiveSource 返回**当前活动源**的名字（内核的事实，不是基座字段）。
	ActiveSource(agentID string) string
}

// ───────────────────────── 错误分类（契约的一部分） ─────────────────────────
//
// 类型要按**类别**做不同处置，不能靠字符串匹配：
//
//	ErrLLMContextFull  调 OverflowHandler（A-1 的感知链）
//	ErrLLMAuth         不重试；整源或模型级分别标记
//	ErrLLMTransient    已按策略重试过仍失败 ⇒ 帧失败或自己的降级
//	ErrLLMCanceled     终止还是重跑 = 类型决定
//	ErrLLMAllFailed    全部源试过（带每源原因清单）
var (
	ErrLLMContextFull = errors.New("llm: upstream says context does not fit")
	ErrLLMAuth        = errors.New("llm: credential/permission rejected")
	ErrLLMTransient   = errors.New("llm: transient failure (already retried)")
	ErrLLMCanceled    = errors.New("llm: canceled")
	ErrLLMAllFailed   = errors.New("llm: every source failed")
)

// AllFailedError 携带**每源的原因清单**（没有它就只能说"全挂了"，
// 而"全挂了"恰恰是最需要细节的那句话）。
type AllFailedError struct {
	Reasons []string
	Last    error
}

func (e *AllFailedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if len(e.Reasons) == 0 {
		return "llm: every source failed"
	}
	return "llm: every source failed — " + strings.Join(e.Reasons, "; ")
}

// Unwrap 保留最后一层的错误链（`errors.Is(err, ErrLLMContextFull)` 仍可判）。
func (e *AllFailedError) Unwrap() error { return e.Last }

// ClassifyLLMError 把任意 provider 错误映射成上面的类别（唯一实现）。
//
// 为什么只有一处：类别决定处置，判定写两遍必然漂移（一处把 429 当瞬态、
// 另一处当配额）。它是**纯协议事实**，与哪个 agent 类型无关。
func ClassifyLLMError(err error) error {
	if err == nil {
		return nil
	}
	// 已是本包的类别：原样返回（避免二次包装）。
	for _, known := range []error{
		ErrLLMContextFull, ErrLLMAuth, ErrLLMTransient, ErrLLMCanceled, ErrLLMAllFailed,
	} {
		if errors.Is(err, known) {
			return err
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrLLMCanceled, err)
	}
	if agentAPI.IsContextFull(err) {
		return fmt.Errorf("%w: %v", ErrLLMContextFull, err)
	}
	var pe *agentAPI.ProviderError
	if errors.As(err, &pe) {
		switch {
		case pe.StatusCode == 401 || pe.StatusCode == 403:
			return fmt.Errorf("%w: %v", ErrLLMAuth, err)
		case pe.Kind == agentAPI.ErrContextFull:
			return fmt.Errorf("%w: %v", ErrLLMContextFull, err)
		case pe.Kind == agentAPI.ErrTransient || pe.StatusCode == 429 || pe.StatusCode >= 500:
			return fmt.Errorf("%w: %v", ErrLLMTransient, err)
		case pe.Kind == agentAPI.ErrCredential:
			return fmt.Errorf("%w: %v", ErrLLMAuth, err)
		}
	}
	// 未知错误按瞬态处理：网络层错误往往没有状态码，而"直接放弃"比
	// "按瞬态处置"更糟（后者至少会尝试下一源）。
	return fmt.Errorf("%w: %v", ErrLLMTransient, err)
}
