package llm

// 「类型只通过基座暴露的 LLM 接口调用模型」的判据（用户裁定 2026-11-20）。
//
// ## 要钉住什么
//
// ① 基座**同时**提供流式与非流式两个入口，且都是**公开**的
//    （类型在别的包里，私有方法调不到）
// ② 两个入口汇到同一处实现（不能一份走设施、一份走裸 provider）
// ③ 类型的流式路径拿到 chunk channel 后能逐块累积出完整响应
//    （主 agent 的每一轮都走它）

import (
	"context"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// fakeProvider 是最小 provider：Chat 一次给全，ChatStream 分三块吐。
type fakeProvider struct {
	name string
	chat int
	strm int
}

func (p *fakeProvider) Name() string          { return p.name }
func (p *fakeProvider) MaxContextTokens() int { return 4096 }
func (p *fakeProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.chat++
	return &agentAPI.CompletionResponse{Content: "full", FinishReason: "stop"}, nil
}

func (p *fakeProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	p.strm++
	ch := make(chan agentAPI.StreamChunk, 4)
	ch <- agentAPI.StreamChunk{Content: "a"}
	ch <- agentAPI.StreamChunk{Content: "b"}
	ch <- agentAPI.StreamChunk{Content: "c", Done: true, FinishReason: "stop"}
	close(ch)
	return ch, nil
}

// TestCompleteLLMFacadeIsPublic：基座暴露非流式入口。
func TestCompleteLLMFacadeIsPublic(t *testing.T) {
	a := agent.New(agent.AgentConfig{ID: "t-complete"})
	// 没有设施也没有 provider ⇒ 必须是**类别化**错误，不是 panic / 裸字符串。
	_, err := a.CompleteLLM(context.Background(), &agentAPI.CompletionRequest{
		Messages: []agentAPI.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("★ 无源可用时必须报错")
	}
	if !strings.Contains(err.Error(), "llm:") {
		t.Fatalf("★ 错误未经分类：%v", err)
	}
}

// TestStreamLLMFacadeIsPublicAndStreams：基座暴露流式入口，且真的逐块返回。
func TestStreamLLMFacadeIsPublicAndStreams(t *testing.T) {
	a := agent.New(agent.AgentConfig{ID: "t-stream"})
	fp := &fakeProvider{name: "fake"}
	a.MountProviders(fp, nil)

	ch, err := a.StreamLLM(context.Background(), &agentAPI.CompletionRequest{
		Messages: []agentAPI.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("★ 基座必须暴露流式入口：%v", err)
	}
	var sb strings.Builder
	for ck := range ch {
		sb.WriteString(ck.Content)
	}
	if sb.String() != "abc" {
		t.Fatalf("流式内容丢失：得到 %q", sb.String())
	}
	if fp.strm != 1 {
		t.Fatalf("★ 流式入口没有走 ChatStream（调用次数 %d）", fp.strm)
	}
	if fp.chat != 0 {
		t.Fatalf("★ 流式入口不该走 Chat（调用次数 %d）——流式被偷偷降级成非流式", fp.chat)
	}
}

// TestNoRetryFacadeAlsoExists：关闭重试的变体仍在（短生命周期子任务用它）。
func TestNoRetryFacadeAlsoExists(t *testing.T) {
	a := agent.New(agent.AgentConfig{ID: "t-noretry"})
	a.MountProviders(&fakeProvider{name: "fake"}, nil)
	resp, err := a.CompleteLLMNoRetry(context.Background(), &agentAPI.CompletionRequest{
		Messages: []agentAPI.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("NoRetry 入口失效：%v", err)
	}
	if resp.Content != "full" {
		t.Fatalf("NoRetry 内容错：%q", resp.Content)
	}
}
