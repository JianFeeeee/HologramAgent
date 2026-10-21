package core

import (
	"context"
	"fmt"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// 这组判据走**真实调度循环**（a.Start + 经 channel 注入 + 等 ResponseCh），
// 而不是直接调 emitResponse。
//
// 为什么非要全链路：同步回包的 usage 要穿过
//
//	stepLLM 记账 → TaskFrame.turnUsage 累加 → finishInputTask → emitResponse
//
// 四个环节，其中任何一处漏掉，单元判据（emitresponse_usage_test.go）都看不见。
// 这正是本缺陷当初能漏出去的原因 —— 每一段单独看都"是对的"。

// usageScriptProvider 是一个会吐 usage 的脚本 provider。
//
// 它必须**同时**支持 ChatStream 与 Chat：真实内核按 provider 能力选路，
// 只实现其中一条会走到另一条上去（脚本 provider 那条 ChatStream 直接报错，
// 就会把测试变成在测错误路径）。
type usageScriptProvider struct {
	script []*agentAPI.CompletionResponse
	idx    int
}

func (s *usageScriptProvider) Name() string { return "usagescript" }

func (s *usageScriptProvider) next() *agentAPI.CompletionResponse {
	if s.idx >= len(s.script) {
		return &agentAPI.CompletionResponse{Content: "done"}
	}
	r := s.script[s.idx]
	s.idx++
	return r
}

func (s *usageScriptProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return s.next(), nil
}

func (s *usageScriptProvider) ChatStream(_ context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	ch := make(chan agentAPI.StreamChunk, 1)
	resp := s.next()
	// ❗必须透传 ToolCalls：只发 Content 会让工具回环不发生，
	// 于是「多轮求和」这条判据实际只跑到第一轮（实测踩到）。
	ch <- agentAPI.StreamChunk{
		Content: resp.Content, ToolCalls: resp.ToolCalls, Done: true, Usage: &resp.TokenUsage,
	}
	close(ch)
	return ch, nil
}

func (s *usageScriptProvider) MaxContextTokens() int { return 8192 }

// TestE2E_SyncReceiptCarriesUsage 是本次修复的**决定性判据**。
//
// 部署实例上实测到的现象：/v1/chat/completions 的回包里没有 usage ——
// 而那条路径就是 InjectTextSyncNoMemory → 调度器 → emitResponse → ResponseCh。
// 本判据用同一根链条（只是把 HTTP 换成直接注入）断言 usage 真的到了回执里。
func TestE2E_SyncReceiptCarriesUsage(t *testing.T) {
	sp := &usageScriptProvider{script: []*agentAPI.CompletionResponse{{
		Content: "收到",
		TokenUsage: agentAPI.TokenUsage{
			Prompt: 1000, Completion: 200, Total: 1200,
			CacheRead: 768, CacheMiss: 232, CacheReported: true, ReasoningTokens: 50,
		},
	}}}
	a := New(AgentConfig{
		ID:              "e2e-usage",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	a.Start()
	defer a.Stop()

	// 同步注入：内核会把终态写进这张 cap=1 的通道（不变量 I5）。
	out := a.io.InjectTextSync("http", "你好")
	if out == nil {
		t.Fatal("同步注入没有收到回执")
	}

	raw, ok := out.Payload["usage"]
	if !ok || raw == nil {
		t.Fatalf("★ 同步回执缺少 usage —— 这就是 /v1/chat/completions 取不到 usage 的根因；payload=%v", out.Payload)
	}
	m := asUsageMap(t, raw)
	for _, c := range []struct {
		key  string
		want int
	}{
		{"prompt_tokens", 1000},
		{"completion_tokens", 200},
		{"total_tokens", 1200},
		{"cache_read_tokens", 768},
		{"cache_miss_tokens", 232},
		{"reasoning_tokens", 50},
	} {
		if got, _ := m[c.key].(int); got != c.want {
			t.Errorf("%s=%v，期望 %v", c.key, m[c.key], c.want)
		}
	}
}

// TestE2E_TurnUsageSumsAcrossToolLoop 断言「本次请求」的口径：
// 一轮任务里若发生多次 LLM 调用（工具回环），对外 usage 应是**求和**，
// 而不是只报最后一次 —— 只报最后一次会系统性低估成本。
//
// 同时守住与会话累计的区别：ledger 是跨请求的，turnUsage 是本请求的，
// 两者不能互相顶替。
func TestE2E_TurnUsageSumsAcrossToolLoop(t *testing.T) {
	sp := &usageScriptProvider{script: []*agentAPI.CompletionResponse{
		{
			Content:    "",
			ToolCalls:  []agentAPI.ToolCall{tc("c1", "knowledge_list")},
			TokenUsage: agentAPI.TokenUsage{Prompt: 300, Completion: 30, Total: 330},
		},
		{
			Content:    "完成",
			TokenUsage: agentAPI.TokenUsage{Prompt: 700, Completion: 70, Total: 770},
		},
	}}
	a := New(AgentConfig{
		ID:              "e2e-sum",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	a.Start()
	defer a.Stop()

	out := a.io.InjectTextSync("http", "跑个工具")
	if out == nil {
		t.Fatal("同步注入没有收到回执")
	}
	raw, ok := out.Payload["usage"]
	if !ok || raw == nil {
		t.Fatalf("同步回执缺少 usage；payload=%v", out.Payload)
	}
	m := asUsageMap(t, raw)
	if got, _ := m["prompt_tokens"].(int); got != 1000 {
		t.Errorf("prompt_tokens=%v，期望 1000（300+700，本轮全部调用求和）", m["prompt_tokens"])
	}
	if got, _ := m["total_tokens"].(int); got != 1100 {
		t.Errorf("total_tokens=%v，期望 1100（330+770）", m["total_tokens"])
	}

	// 会话累计与本次不同：这里恰好相等（只有一个请求），但**字段必须都在**，
	// 因为两者在多请求时会分叉。
	if sess := a.usageLedger.snapshot(); sess.Calls != 2 {
		t.Errorf("ledger 调用数=%d，期望 2", sess.Calls)
	}
}

// TestE2E_SecondRequestDoesNotDoubleCount 守住 turnUsage 的**每任务清零**语义。
//
// 若 turnUsage 被错误地做成跨任务累积（或 emitResponse 误用 ledger 的会话累计），
// 第二次请求就会报出翻倍的数字。这类错误在单请求测试里完全看不见。
func TestE2E_SecondRequestDoesNotDoubleCount(t *testing.T) {
	sp := &usageScriptProvider{script: []*agentAPI.CompletionResponse{
		{Content: "一", TokenUsage: agentAPI.TokenUsage{Prompt: 100, Completion: 10, Total: 110}},
		{Content: "二", TokenUsage: agentAPI.TokenUsage{Prompt: 100, Completion: 10, Total: 110}},
	}}
	a := New(AgentConfig{
		ID:              "e2e-twice",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	a.Start()
	defer a.Stop()

	for i := 1; i <= 2; i++ {
		// ⚠️ 两次必须用**不同**文本：内核会把完全相同的输入判为 duplicate
		// 并直接 skipped 掉（实测 reason:duplicate），那样第二次根本没跑 LLM。
		out := a.io.InjectTextSync("http", fmt.Sprintf("第%d次", i))
		if out == nil {
			t.Fatalf("第 %d 次注入没有回执", i)
		}
		raw, ok := out.Payload["usage"]
		if !ok || raw == nil {
			t.Fatalf("第 %d 次回执缺少 usage", i)
		}
		m := asUsageMap(t, raw)
		if got, _ := m["prompt_tokens"].(int); got != 100 {
			t.Errorf("第 %d 次 prompt_tokens=%v，期望 100 —— 说明报的是会话累计而非本次请求（会翻倍）", i, got)
		}
	}

	// 而会话累计应当确实是 2 次调用的和。
	if sess := a.usageLedger.snapshot(); sess.Prompt != 200 {
		t.Errorf("会话累计 prompt=%d，期望 200（两次各 100）", sess.Prompt)
	}
}
