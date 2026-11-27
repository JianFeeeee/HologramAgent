package llm

// client.go —— `kernelapi.LLMClient` 的实现：**重试/回退/记账只在内核**（§3.2.1）。
//
// ## 从基座搬来的四份实现（合并成一份）
//
//	a/callLLMWithFallback    120 行  主 agent 的非流式回退 + 同源重试
//	a/chatStreamWithFallback 110 行  流建立 + 非流式回退 + 增量累积
//	a/streamWithFallback      45 行  rt 的流建立回退
//	a/chatWithProviderChain   30 行  spawn 的非流式回退（不重试）
//
// 四份的**共同部分**（源解析、每源自己的 model、跨源回退、可用性标记、
// 活动源更新）在这里；差异部分作为**参数**（`NoRetry`）。
//
// ## 保留在基座的
//
//   · 流内容的累积（`accumulateStream`）与增量事件发布 —— 那是基座与通道的约定
//   · 请求的组装（消息/工具/预算）—— 那是类型的决定
//   · `ErrContextFull` 的**处置**（调 OverflowHandler）—— 那是类型的策略
//
// 即：**请求边界在基座，执行与错误分类在内核**。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentProvider "github.com/JianFeeeee/HologramAgent/internal/agent/provider"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// client 实现 kernelapi.LLMClient。
type client struct {
	mgr *agentProvider.ProviderManager

	// activeByAgent 记录**当前活动源**（内核的事实）。
	//
	// ★ 为什么不在基座留 `a.provider`：那会让"现在用哪个源"有两个来源
	//   （manager 的排序 + 基座字段），而两者迟早不一致。字段删除后，
	//   唯一事实在这里。
	mu            sync.RWMutex
	activeByAgent map[string]string
}

// NewClient 构造 LLM 设施（装配期一次）。
func NewClient(mgr *agentProvider.ProviderManager) kernelapi.LLMClient {
	return &client{mgr: mgr, activeByAgent: map[string]string{}}
}

// 编译期确认。
var _ kernelapi.LLMClient = (*client)(nil)

// ActiveSource 返回当前活动源名（空 = 尚未成功调用过）。
func (c *client) ActiveSource(agentID string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.activeByAgent[agentID]
}

// MaxContextTokens 返回默认源的上下文窗口（0 = 未知/未装配）。
func (c *client) MaxContextTokens() int {
	if c == nil || c.mgr == nil {
		return 0
	}
	p := c.mgr.Default()
	if p == nil {
		return 0
	}
	return p.MaxContextTokens()
}

// SetDefault 切换总默认源（空/未注册 ⇒ 报错，调用方原样呈现给模型）。
func (c *client) SetDefault(name string) error {
	if c == nil || c.mgr == nil {
		return fmt.Errorf("llm: provider manager not available")
	}
	return c.mgr.SetDefault(name)
}

// Sources 返回候选源快照（内核排序 + 声明能力 + 可用性）。
//
// 这是**唯一的**源清单出口：类型不再持有 ProviderManager，也不做能力探测。
func (c *client) Sources() []kernelapi.LLMSourceInfo {
	if c == nil || c.mgr == nil {
		return nil
	}
	all := c.mgr.OrderedProviders()
	if len(all) == 0 {
		if d := c.mgr.Default(); d != nil {
			all = []agentAPI.Provider{d}
		}
	}
	out := make([]kernelapi.LLMSourceInfo, 0, len(all))
	for _, p := range all {
		name := p.Name()
		info := kernelapi.LLMSourceInfo{Name: name, Available: c.mgr.IsAvailable(name)}
		if caps, ok := c.mgr.Capabilities(name); ok {
			info.Vision = caps.Vision
			info.Audio = caps.Audio
		} else {
			// 配置没声明能力 ⇒ 退回类型断言（adapter/装饰器漏实现方法时
			// 让能力凭空消失比"多认一次"更糟）。
			info.Vision = agentAPI.ProviderSupportsVision(p)
			info.Audio = agentAPI.ProviderSupportsAudio(p)
		}
		out = append(out, info)
	}
	return out
}

func (c *client) setActive(agentID, name string) {
	if c == nil || agentID == "" {
		return
	}
	c.mu.Lock()
	c.activeByAgent[agentID] = name
	c.mu.Unlock()
}

// resolve 解析候选源：请求模型优先，其次总排序；`Preferred` 提前。
func (c *client) resolve(req *kernelapi.LLMRequest) []agentAPI.Provider {
	if c == nil || c.mgr == nil {
		return nil
	}
	var all []agentAPI.Provider
	if m := req.Request.Model; m != "" && !equalFold(m, "AUTO") {
		all = c.mgr.ResolveForModel(m)
	} else {
		all = c.mgr.OrderedProviders()
	}
	out := make([]agentAPI.Provider, 0, len(all))
	for _, p := range all {
		if c.mgr.IsAvailable(p.Name()) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		// manager 无可用源：退回默认源（回退链要能给出"至少试一次"）。
		if d := c.mgr.Default(); d != nil {
			out = []agentAPI.Provider{d}
		}
	}
	// 请求点名了源（按能力选源）⇒ 它排在候选链最前，但不独占：
	// 点名源不可用/失败时仍走其余源（回退链不因点名而关闭）。
	want := req.Preferred
	if req.Request != nil && req.Request.Source != "" {
		want = req.Request.Source
	}
	return preferByName(out, want, req.AgentID)
}

// preferByName 把指定源提到候选链最前（**优先，不是独占**）。
//
// 名字对不上时**响亮回退**并记日志（含可用源清单）：拼错的配置如果悄悄
// 无效，用户会以为"我配了但没生效"，而日志里一个字都没有。
func preferByName(cands []agentAPI.Provider, want, agentID string) []agentAPI.Provider {
	want = trimSpace(want)
	if want == "" || len(cands) <= 1 {
		return cands
	}
	for i, p := range cands {
		if !equalFold(p.Name(), want) {
			continue
		}
		if i == 0 {
			return cands
		}
		out := make([]agentAPI.Provider, 0, len(cands))
		out = append(out, p)
		out = append(out, cands[:i]...)
		out = append(out, cands[i+1:]...)
		return out
	}
	names := make([]string, 0, len(cands))
	for _, p := range cands {
		names = append(names, p.Name())
	}
	log.Printf("[llm] agent %s: preferred source %q is not among available (%v), falling back to default %q",
		agentID, want, names, cands[0].Name())
	return cands
}

// Complete 一轮非流式请求（重试 + 跨源回退 + 可用性标记）。
func (c *client) Complete(ctx context.Context, req *kernelapi.LLMRequest) (*agentAPI.CompletionResponse, error) {
	if req == nil || req.Request == nil {
		return nil, fmt.Errorf("%w: nil request", kernelapi.ErrLLMAllFailed)
	}
	providers := c.resolve(req)
	if len(providers) == 0 {
		return nil, fmt.Errorf("%w: no provider available", kernelapi.ErrLLMAllFailed)
	}

	var reasons []string
	var lastErr error
	for pi, p := range providers {
		if pi > 0 {
			log.Printf("[llm] fallback: trying provider %q (#%d/%d, previous: %v)",
				p.Name(), pi, len(providers)-1, lastErr)
		}
		// ★ 每个源必须用**自己的** model（真机教训：`model` 是 provider 的
		//   属性，不是请求的共享字段 —— 复用会把上一个源的 model 泄给下一个源，
		//   触发 403 model_not_allowed 并把整源标记不可用）。
		attempt := *req.Request
		if rp, ok := p.(agentAPI.RoutableProvider); ok {
			if m := rp.Model(); m != "" && m != attempt.Model {
				if pi > 0 {
					log.Printf("[llm] fallback provider %q uses its own model %q (was %q)",
						p.Name(), m, attempt.Model)
				}
				attempt.Model = m
			}
		}

		maxAttempts := 2
		if req.NoRetry {
			maxAttempts = 1
		}
		var resp *agentAPI.CompletionResponse
		var err error
		for attemptNo := 1; attemptNo <= maxAttempts; attemptNo++ {
			if attemptNo > 1 {
				log.Printf("[llm] provider %q transient failure, retry %d/%d in 2s: %v",
					p.Name(), attemptNo, maxAttempts, err)
				select {
				case <-time.After(2 * time.Second):
				case <-ctx.Done():
					return nil, fmt.Errorf("%w: %v", kernelapi.ErrLLMCanceled, ctx.Err())
				}
			}
			resp, err = p.Chat(ctx, &attempt)
			if err == nil {
				break
			}
			// 取消与凭证错误不重试（凭证重试无意义；取消是调用方决定）。
			cls := kernelapi.ClassifyLLMError(err)
			if errors.Is(cls, kernelapi.ErrLLMCanceled) {
				return nil, cls
			}
			if errors.Is(cls, kernelapi.ErrLLMAuth) {
				break
			}
			if errors.Is(cls, kernelapi.ErrLLMContextFull) {
				// 上下文超页：换源重发**同一个**超限请求没有意义。
				// 交由调用方（类型的 OverflowHandler）处置。
				c.noteFailure(p, err)
				return nil, cls
			}
		}
		if err == nil {
			c.mgr.ResetAvailability(p.Name())
			c.setActive(req.AgentID, p.Name())
			return resp, nil
		}
		lastErr = err
		c.noteFailure(p, err)
		reasons = append(reasons, fmt.Sprintf("%s: %v", p.Name(), err))
	}

	if len(reasons) == 0 {
		return nil, fmt.Errorf("%w: %v", kernelapi.ErrLLMAllFailed, lastErr)
	}
	return nil, &kernelapi.AllFailedError{
		Reasons: reasons,
		Last:    kernelapi.ClassifyLLMError(lastErr),
	}
}

// Stream 建立一条流（失败时调用方自行回落非流式）。
func (c *client) Stream(ctx context.Context, req *kernelapi.LLMRequest) (<-chan agentAPI.StreamChunk, error) {
	if req == nil || req.Request == nil {
		return nil, fmt.Errorf("%w: nil request", kernelapi.ErrLLMAllFailed)
	}
	providers := c.resolve(req)
	if len(providers) == 0 {
		return nil, fmt.Errorf("%w: no provider available", kernelapi.ErrLLMAllFailed)
	}

	var reasons []string
	var lastErr error
	for i, p := range providers {
		if i > 0 {
			log.Printf("[llm] stream fallback: trying provider %q (#%d/%d, previous: %v)",
				p.Name(), i, len(providers)-1, lastErr)
		}
		// 每次尝试一份浅拷贝，只改 Model：不污染调用方的 req，
		// 也不让上一个源的 model 泄给下一个源。
		attempt := *req.Request
		if rp, ok := p.(agentAPI.RoutableProvider); ok {
			if m := rp.Model(); m != "" {
				attempt.Model = m
			}
		}
		ch, err := p.ChatStream(ctx, &attempt)
		if err == nil {
			c.mgr.ResetAvailability(p.Name())
			c.setActive(req.AgentID, p.Name())
			return ch, nil
		}
		cls := kernelapi.ClassifyLLMError(err)
		// ★ 取消/超时：用户中断（或整轮被停），**不换源**。
		//
		// 判定取 ctx.Err() 而不是"错误是不是 Canceled"：很多 provider 在
		// 不支持流式时也回 Canceled 表示"请走非流式"，那种必须继续回退，
		// 否则会把"不支持流式"误当成"已被取消"。
		if ctx.Err() != nil || errors.Is(cls, kernelapi.ErrLLMCanceled) {
			if ctx.Err() == nil {
				// provider 自己报的取消（不支持流式）：继续试下一个源，
				// 但不打成"用户中断"。
				lastErr = err
				reasons = append(reasons, fmt.Sprintf("%s: %v", p.Name(), err))
				continue
			}
			return nil, fmt.Errorf("%w: %v", kernelapi.ErrLLMCanceled, ctx.Err())
		}
		lastErr = err
		c.noteFailure(p, err)
		reasons = append(reasons, fmt.Sprintf("%s: %v", p.Name(), err))
	}
	if lastErr == nil {
		return nil, fmt.Errorf("%w: no stream", kernelapi.ErrLLMAllFailed)
	}
	return nil, &kernelapi.AllFailedError{Reasons: reasons, Last: kernelapi.ClassifyLLMError(lastErr)}
}

// noteFailure 按类别标记源不可用（**模型级问题不判死整源**）。
func (c *client) noteFailure(p agentAPI.Provider, err error) {
	if c == nil || c.mgr == nil || err == nil {
		return
	}
	cls := kernelapi.ClassifyLLMError(err)
	var pe *agentAPI.ProviderError
	if errors.Is(cls, kernelapi.ErrLLMAuth) && errors.As(err, &pe) {
		if isModelNotAllowed(err) {
			log.Printf("[llm] provider %q rejected model (HTTP %d) — model-level issue, provider stays available: %v",
				p.Name(), pe.StatusCode, err)
			return
		}
		c.mgr.ReportStatus(p.Name(), pe.StatusCode)
		log.Printf("[llm] provider %q marked unavailable (HTTP %d)", p.Name(), pe.StatusCode)
		return
	}
	c.mgr.MarkUnavailable(p.Name())
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// isModelNotAllowed 判断错误是不是"这个模型名不被该 key 允许"。
//
// 与"凭证失效"处置相反：前者只否掉一个 model（provider 仍可服务其他 model，
// 也仍可用于视觉回退），后者才该把整源标记不可用。
func isModelNotAllowed(err error) bool {
	if err == nil {
		return false
	}
	msg := lowerASCII(err.Error())
	for _, m := range []string{
		"model_not_allowed",
		"is not allowed for this key",
		"model not allowed",
		"model_not_found",
		"does not exist or you do not have access",
	} {
		if containsASCII(msg, m) {
			return true
		}
	}
	return false
}

func lowerASCII(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

func containsASCII(hay, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
