package core

// R7 判据：rtchild 的上下文 = **普通上下文 + 上下文窗口**。
//
// 用户 2026-10-30 裁定：没有页、没有锚点、没有降级阶梯 —— 就是普通对话历史，
// 进模型前按窗口裁。本文件钉住三件事：
//
//  1. 历史**跨轮累积**（通话是一条连续对话，不能每轮失忆）
//  2. 超窗口时**从最旧丢**，system 与本轮输入永不丢
//  3. 裁剪**绝不拆开** assistant(tool_calls) 与其配对 tool 结果
//     （切在中间上游直接 400，见 context.go:102 的生产事故）

import (
	"context"
	"sync"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// ── 纯函数判据：rtTrimToWindow ──

// TestRTTrim_KeepsSystemAndLatestInput 钉住：system 与本轮 user 输入永不裁。
//
// 这两条一丢，rt 就彻底坏了：
//   - system 丢了 ⇒ 人格与【实时通话】约束消失（模型不再先说"稍等"）
//   - 本轮输入丢了 ⇒ 模型在回答上一轮的问题
func TestRTTrim_KeepsSystemAndLatestInput(t *testing.T) {
	msgs := []agentAPI.Message{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "旧问题"},
		{Role: "assistant", Content: "旧回答"},
		{Role: "user", Content: "本轮问题"},
	}
	// 窗口小到只装得下首尾两条。
	out := rtTrimToWindow(msgs, 10)
	if len(out) < 2 {
		t.Fatalf("裁完只剩 %d 条，system 或本轮输入被丢了", len(out))
	}
	if out[0].Role != "system" || out[0].Content != "你是助手" {
		t.Fatalf("system 被裁掉了：%+v", out[0])
	}
	if last := out[len(out)-1]; last.Role != "user" || last.Content != "本轮问题" {
		t.Fatalf("本轮输入被裁掉了：%+v", last)
	}
}

// TestRTTrim_DropsOldestFirst 钉住「从最旧丢」。
func TestRTTrim_DropsOldestFirst(t *testing.T) {
	msgs := []agentAPI.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "最早的问题"},
		{Role: "assistant", Content: "最早的回答"},
		{Role: "user", Content: "最近的问题"},
		{Role: "assistant", Content: "最近的回答"},
		{Role: "user", Content: "本轮"},
	}
	out := rtTrimToWindow(msgs, 12)
	for _, m := range out {
		if m.Content == "最早的问题" {
			t.Fatalf("最早的消息仍在（应从最旧丢）：%+v", contentsOf(out))
		}
	}
}

// TestRTTrim_NeverSplitsToolPairs 是最重要的一条：
// assistant(tool_calls) 与其配对 tool 结果必须**整组进出**。
//
// 若实现按单条消息裁（而不是按组），这里会切出「有 tool_calls 但没有结果」
// 或「有 tool 结果但找不到调用」的残缺序列 —— 上游直接 400。
func TestRTTrim_NeverSplitsToolPairs(t *testing.T) {
	msgs := []agentAPI.Message{
		{Role: "system", Content: "sys"},
		// ── 组 A：一次工具调用 + 两个配对结果 ──
		{Role: "assistant", ToolCalls: []agentAPI.ToolCall{
			{ID: "c1", Name: "t1"}, {ID: "c2", Name: "t2"},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "结果1"},
		{Role: "tool", ToolCallID: "c2", Content: "结果2"},
		{Role: "user", Content: "中间问题"},
		{Role: "assistant", Content: "中间回答"},
		{Role: "user", Content: "本轮"},
	}

	// 用一个"刚好要丢掉一部分"的窗口，验证组不会被切开。
	for _, window := range []int{20, 40, 60, 200} {
		out := rtTrimToWindow(msgs, window)
		assertToolPairsIntact(t, window, out)
	}
}

// assertToolPairsIntact 断言 out 里每个 assistant(tool_calls) 的 ID 集合
// 与紧随其后的 tool 消息 ID 集合完全一致（不残缺、不多余）。
func assertToolPairsIntact(t *testing.T, window int, out []agentAPI.Message) {
	t.Helper()
	for i := 0; i < len(out); i++ {
		m := out[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		want := map[string]bool{}
		for _, tc := range m.ToolCalls {
			want[tc.ID] = true
		}
		got := map[string]bool{}
		for j := i + 1; j < len(out) && out[j].Role == "tool"; j++ {
			got[out[j].ToolCallID] = true
		}
		if len(got) != len(want) {
			t.Fatalf("window=%d: tool_call 与结果不配对（call=%v result=%v）—— 上游会 400\n序列：%+v",
				window, want, got, contentsOf(out))
		}
		for id := range want {
			if !got[id] {
				t.Fatalf("window=%d: 调用 %s 没有配对结果 —— 上游会 400", window, id)
			}
		}
	}
	// 反向：不得出现没有前置 assistant 的孤儿 tool 消息。
	for i, m := range out {
		if m.Role != "tool" {
			continue
		}
		if i == 0 {
			t.Fatalf("window=%d: 序列以 tool 消息开头（孤儿结果）", window)
		}
	}

}

func contentsOf(msgs []agentAPI.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		s := m.Role + ":"
		if m.Content != "" {
			s += m.Content
		}
		if len(m.ToolCalls) > 0 {
			s += "[calls]"
		}
		out = append(out, s)
	}
	return out
}

// TestRTTrim_ShortHistoryUntouched 钉住：本来就没超窗口时**一条都不删**。
//
// 这一条防"为了让窗口判据变绿而无脑裁剪"——那会把还装得下的历史白白丢掉。
func TestRTTrim_ShortHistoryUntouched(t *testing.T) {
	msgs := []agentAPI.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: "c"},
	}
	out := rtTrimToWindow(msgs, 100000)
	if len(out) != len(msgs) {
		t.Fatalf("窗口充裕却裁掉了消息：%v → %v", contentsOf(msgs), contentsOf(out))
	}
}

// ── 集成判据：rtchild 跨轮累积上下文 ──

// rtMsgCountProvider 记录每次 ChatStream 收到的消息条数，
// 用来证明"历史真的被带进了下一轮"（而不是每轮失忆）。
type rtMsgCountProvider struct {
	mu    sync.Mutex
	sizes []int
}

func (p *rtMsgCountProvider) Name() string          { return "rtMsgCount" }
func (p *rtMsgCountProvider) MaxContextTokens() int { return 8192 }
func (p *rtMsgCountProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return &agentAPI.CompletionResponse{}, nil
}
func (p *rtMsgCountProvider) ChatStream(_ context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	p.mu.Lock()
	p.sizes = append(p.sizes, len(req.Messages))
	p.mu.Unlock()
	ch := make(chan agentAPI.StreamChunk, 2)
	ch <- agentAPI.StreamChunk{Content: "回"}
	ch <- agentAPI.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}
func (p *rtMsgCountProvider) snapshot() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.sizes...)
}

// TestRTChild_HistoryAccumulatesAcrossRounds 是「普通上下文」的核心判据：
// 第二轮发给模型的消息数必须**多于**第一轮（历史被带上了）。
//
// 若 buildRTContext 每轮只发 system+input（我最初的"无历史"最小版），
// 两轮都是 2 条 —— 本判据必红。
func TestRTChild_HistoryAccumulatesAcrossRounds(t *testing.T) {
	p := &rtMsgCountProvider{}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "hist")

	host.PushRTInput(id, "第一句")
	waitFor(t, "第一轮完成", func() bool { return len(p.snapshot()) >= 1 })

	host.PushRTInput(id, "第二句")
	waitFor(t, "第二轮完成", func() bool { return len(p.snapshot()) >= 2 })

	sizes := p.snapshot()
	if sizes[1] <= sizes[0] {
		t.Fatalf("第二轮消息数 %d 不多于第一轮 %d —— 上下文没有跨轮累积"+
			"（rtchild 应是普通上下文，通话不能每轮失忆）", sizes[1], sizes[0])
	}
	// 第二轮 = system + (user+assistant) + user = 4 条（window 充裕时）
	if sizes[1] < 4 {
		t.Fatalf("第二轮只有 %d 条消息，期望 ≥4（system + 上一轮问答 + 本轮输入）", sizes[1])
	}
}
