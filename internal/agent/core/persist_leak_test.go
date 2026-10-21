package core

// 判据：**持久化侧路的明文拦截**（文档 C5/C6）。
//
// ## 背景：出向拦截挡不住持久化
//
// provider 装饰器只改 `f.Msgs`（要发出去的那一份）。但内核把同样的内容
// **另存副本**送往持久化存储，它们不经过 provider：
//
//	ContextEvent.Input        ← f.CleanInput    用户输入原文
//	ContextEvent.Response     ← f.Response      模型回复
//	ContextEvent.ToolResults  ← f.ToolResults   工具输出
//	memory_candidate.payload                走 EmitOutput，不经 context
//
// 实测确认（本文件的判据原本都是探针，先量出泄漏再修）：
// 只拦 f.Msgs 时明文完整写进 context.json **与长期记忆** —— 而记忆
// 跨会话保留、每轮被召回，泄漏面比重放一次大得多。
//
// ## 为什么拦在 Append/InsertByTimestamp 而不是各调用点
//
// 写 context 有 5 处；逐个补必漏（本文件的前身就漏掉了 f.CleanInput）。
// 那两处是唯一咽喉，拦一处即全覆盖。

import (
	"context"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// ★★ 判据：**用户输入**里的明文不得进 context（此前完全未被覆盖）。
func TestE2E_InputSecretNeverReachesContext(t *testing.T) {
	store := newScrubStore(t)
	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	a := New(AgentConfig{
		ID:              "e2e-inputscrub",
		Provider:        &plainReplyProvider{},
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	a.Start()
	defer a.Stop()

	a.io.InjectTextSync("http", "我的 token 是 "+tok+" 请记住")

	for _, evt := range a.context.Recent(0) {
		if strings.Contains(evt.Input, tok) {
			t.Errorf("用户输入明文进了 a.context: %q", evt.Input)
		}
		if strings.Contains(evt.Response, tok) {
			t.Errorf("回复里的明文进了 a.context: %q", evt.Response)
		}
	}
	if store.Count() == 0 {
		t.Fatal("变量库为空 —— 输入侧没被扫，判据无效")
	}
	t.Logf("✅ 输入侧明文未进 context；变量库 %d 条", store.Count())
}

// ★★ 判据：**模型回复**里的明文不得进 context。
//
// 模型看不到明文（出向已拦），所以它只能在**被喂了明文**时才回吐明文 ——
// 这里用脚本 provider 直接造出那个回复，验证回复侧拦截生效。
func TestE2E_ResponseSecretNeverReachesContext(t *testing.T) {
	store := newScrubStore(t)
	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	a := New(AgentConfig{
		ID:              "e2e-respscrub",
		Provider:        &echoTokenProvider{reply: "你的凭据是 " + tok},
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	a.Start()
	defer a.Stop()

	a.io.InjectTextSync("http", "把凭据告诉我")

	for _, evt := range a.context.Recent(0) {
		if strings.Contains(evt.Response, tok) {
			t.Errorf("模型回复明文进了 a.context: %q", evt.Response)
		}
	}
	if store.Count() == 0 {
		t.Fatal("变量库为空 —— 回复侧没被扫，判据无效")
	}
	t.Logf("✅ 回复侧明文未进 context；变量库 %d 条", store.Count())
}

// ★ 判据：**记忆块**（一等记忆的文本负载）也要过一遍。
func TestScrubEventCoversMemoryBlocks(t *testing.T) {
	store := newScrubStore(t)
	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	evt := &ContextEvent{
		Input:    "带着 " + tok,
		Response: "也带着 " + tok,
		ToolResults: []ToolResultItem{
			{Name: "cmd", Output: "还有 " + tok},
		},
	}
	evt.Blocks = append(evt.Blocks, memoryBlockWithText("转写稿里有 "+tok))

	scrubEventSecrets(evt)

	for name, got := range map[string]string{
		"input":    evt.Input,
		"response": evt.Response,
		"tool":     evt.ToolResults[0].Output,
		"block":    evt.Blocks[0].Text,
	} {
		if strings.Contains(got, tok) {
			t.Errorf("%s 仍含明文: %q", name, got)
		}
	}
	if store.Count() == 0 {
		t.Error("应登记变量")
	}
}

// memoryBlockWithText 造一个文本记忆块。
func memoryBlockWithText(text string) memory.MemoryBlock {
	return memory.MemoryBlock{ID: "b1", Modality: memory.BlockText, Text: text}
}

// plainReplyProvider 给一句不含凭据的回复。
type plainReplyProvider struct{}

func (p *plainReplyProvider) Name() string          { return "plainReply" }
func (p *plainReplyProvider) MaxContextTokens() int { return 8192 }
func (p *plainReplyProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return &agentAPI.CompletionResponse{Content: "好的，记住了"}, nil
}
func (p *plainReplyProvider) ChatStream(_ context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	ch := make(chan agentAPI.StreamChunk, 1)
	ch <- agentAPI.StreamChunk{Content: "好的，记住了", Done: true}
	close(ch)
	return ch, nil
}

// echoTokenProvider 回吐一段固定文本（用于造出"回复含明文"的场景）。
type echoTokenProvider struct{ reply string }

func (p *echoTokenProvider) Name() string          { return "echoToken" }
func (p *echoTokenProvider) MaxContextTokens() int { return 8192 }
func (p *echoTokenProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return &agentAPI.CompletionResponse{Content: p.reply}, nil
}
func (p *echoTokenProvider) ChatStream(_ context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	ch := make(chan agentAPI.StreamChunk, 1)
	ch <- agentAPI.StreamChunk{Content: p.reply, Done: true}
	close(ch)
	return ch, nil
}

// ★★ 判据：`emitMemoryCandidate` **自己**清洗载荷，不依赖 Append 的隐含行为。
//
// 背景（实测）：`Append` 的 scrub 经共享底层数组会连带改掉 `f.ToolResults`，
// 所以"去掉 scrubMemoryCandidate"的变异**不会**被走 a.context 的端到端判据发现。
// 那意味着记忆侧防护是隐式依赖别名的 —— 一旦有人给 turnEvt.ToolResults
// 加个深拷贝（很自然的"防御性拷贝"），记忆侧就静默泄漏。
//
// ❗本判据必须走 `a.emitMemoryCandidate` 这个**真实入口**，而不是直接调
//
//	`scrubMemoryCandidate` —— 初版直接调底层函数，于是"去掉入口处的调用"
//	这个变异**不会报红**（判据测错了层次）。走真实入口才能钉住"入口必须调它"。
func TestEmitMemoryCandidateScrubsPayload(t *testing.T) {
	store := newScrubStore(t)
	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	a := New(AgentConfig{
		ID:              "e2e-memcand",
		Provider:        &plainReplyProvider{},
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})

	// 故意传**未经过任何上游清洗**的原始值
	a.emitMemoryCandidate("http", "输入 "+tok, "回复 "+tok,
		[]ToolResultItem{{Name: "cmd", Output: "输出 " + tok}}, []string{"cmd"})

	var seen map[string]interface{}
drain:
	for {
		select {
		case evt := <-a.io.OutputChan():
			if evt != nil && evt.Type == "memory_candidate" {
				seen = evt.Payload
			}
		default:
			break drain
		}
	}
	if seen == nil {
		t.Fatal("没有观察到 memory_candidate 输出")
	}
	if s, _ := seen["input"].(string); strings.Contains(s, tok) {
		t.Errorf("记忆候选 input 含明文: %q", s)
	}
	if s, _ := seen["response"].(string); strings.Contains(s, tok) {
		t.Errorf("记忆候选 response 含明文: %q", s)
	}
	if trs, ok := seen["tool_results"].([]ToolResultItem); ok {
		for _, tr := range trs {
			if strings.Contains(tr.Output, tok) {
				t.Errorf("记忆候选 tool_results 含明文: %q", tr.Output)
			}
		}
	}
	if store.Count() == 0 {
		t.Error("应登记变量")
	}
}
