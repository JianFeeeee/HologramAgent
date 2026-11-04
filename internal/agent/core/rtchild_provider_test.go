package core

// 阶段 1 判据：rtchild 的模型源必须走**候选链**（优先源 + 回退），不是裸 a.provider。
//
// ## 为什么这组判据必须走 SpawnRTChild
//
// 现有 TestPreferredProvider_* 有 6 条，全部用 `New(AgentConfig{...})` 直接构造
// agent，断言的是 `resolveProviders` 的排序结果。它们证明「配置被记住了」，
// 但证明不了「执行时用它了」。
//
// 而缺陷正在 SpawnRTChild 铺出来的那条路径上：carrier 的 PreferredProvider
// 是对的（候选链首位就是它），可 rtStream 当时调的是 `a.provider.ChatStream`，
// 绕过候选链 ⇒ 优先源一次都不被调用，且没有任何回退。
//
// ⇒ 本文件的每条判据都必须经 SpawnRTChild + 真实 PushRTInput 触发一轮。
// 这是 engineering-lessons.md §1.2「判据测接线之后 ≠ 测有没有接线」的直接应用。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// rtCountProvider 记录调用次数，并可选地返回错误。
type rtCountProvider struct {
	name string
	// failWith 非 nil 时 ChatStream 返回它（模拟源故障）。
	failWith error
	// content 是成功时吐的正文。
	content string

	mu       sync.Mutex
	streams  int
	muCalled sync.Mutex
}

func (p *rtCountProvider) Name() string          { return p.name }
func (p *rtCountProvider) MaxContextTokens() int { return 8192 }

func (p *rtCountProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return &agentAPI.CompletionResponse{Content: p.content}, nil
}

func (p *rtCountProvider) ChatStream(ctx context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	p.mu.Lock()
	p.streams++
	p.mu.Unlock()

	if p.failWith != nil {
		return nil, p.failWith
	}
	ch := make(chan agentAPI.StreamChunk, 2)
	ch <- agentAPI.StreamChunk{Content: p.content}
	ch <- agentAPI.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func (p *rtCountProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streams
}

// rtProviderHost 造一个带 providerManager + 一对 rt 通道 + 输出设备的父 agent。
func rtProviderHost(t *testing.T, pm *agentAPI.ProviderManager, def string) *Agent {
	t.Helper()
	pio := agentIO.NewIOManager()
	host := New(AgentConfig{
		ID: "prov-host", IO: pio,
		Provider: pm.Get(def), ProviderManager: pm,
	})
	if err := pio.RegisterDevice(&rtRecordingDevice{name: "pvout"}); err != nil {
		t.Fatalf("注册输出设备: %v", err)
	}
	for _, n := range []string{"pvout", "pvin"} {
		if err := pio.ChannelRegistry().Register(agentIO.InputChannel{
			Name: n, Plugin: "provtest",
			Capabilities: agentIO.CapText | agentIO.CapRT,
		}); err != nil {
			t.Fatalf("注册通道 %s: %v", n, err)
		}
	}
	return host
}

// rtWaitRound 等到 rtchild 至少跑完 rounds 轮（等状态，不等时间）。
func rtWaitRound(t *testing.T, host *Agent, id string, rounds uint64) {
	t.Helper()
	waitFor(t, "rtchild 跑够轮次", func() bool {
		st, ok := host.RTChildStats(id)
		return ok && st.Rounds >= rounds
	})
}

// TestRTChild_PreferredProviderIsUsed 钉住 D3 的正面：优先源必须真的被调用。
//
// 变红条件（已实测可红）：rtStream 改回 `a.provider.ChatStream(...)`
// ⇒ fast 源 calls()==0、slow 源被调 ⇒ 本判据红。
func TestRTChild_PreferredProviderIsUsed(t *testing.T) {
	pm := agentAPI.NewProviderManager()
	slow := &rtCountProvider{name: "slow-default", content: "slow"}
	fast := &rtCountProvider{name: "fast-rt", content: "fast"}
	pm.Register("slow-default", slow)
	pm.Register("fast-rt", fast)
	if err := pm.SetDefault("slow-default"); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}

	host := rtProviderHost(t, pm, "slow-default")
	defer host.StopRTChildren()

	id, err := host.SpawnRTChild(RTChildOptions{
		ID: "prov-rt", Input: "pvin", Output: "pvout",
		PreferredProvider: "fast-rt",
	})
	if err != nil {
		t.Fatalf("SpawnRTChild: %v", err)
	}

	// 先证明「配置被记住了」（候选链首位是 fast-rt）——
	// 这样如果后面失败，失败点唯一地指向"执行时没用它"。
	carrier := host.rtChildOf(id).agent
	if got := chainNames(carrier); len(got) == 0 || got[0] != "fast-rt" {
		t.Fatalf("前置条件失败：carrier 候选链首位 = %v，期望 fast-rt", got)
	}

	if _, err := host.PushRTInput(id, "喂"); err != nil {
		t.Fatalf("PushRTInput: %v", err)
	}
	rtWaitRound(t, host, id, 1)

	if fast.calls() == 0 {
		t.Fatalf("rtchild 声明 PreferredProvider=fast-rt 且候选链首位就是它，"+
			"但 fast-rt 一次都没被调用（slow 被调 %d 次）—— "+
			"rtStream 绕过了 resolveProviders", slow.calls())
	}
	if slow.calls() != 0 {
		t.Fatalf("首选源健康时不该调用默认源：slow=%d fast=%d", slow.calls(), fast.calls())
	}
}

// TestRTChild_ProviderFallsBackOnFailure 钉住「优先，不是独占」。
//
// 首选源故障时必须切到下一个源，而不是整个通话失败。
// 变红条件：streamWithFallback 只试第一个源就返回 ⇒ 红。
func TestRTChild_ProviderFallsBackOnFailure(t *testing.T) {
	pm := agentAPI.NewProviderManager()
	backup := &rtCountProvider{name: "backup-src", content: "backup"}
	fast := &rtCountProvider{
		name:     "broken-fast",
		failWith: errors.New("upstream 503"),
	}
	pm.Register("backup-src", backup)
	pm.Register("broken-fast", fast)
	if err := pm.SetDefault("backup-src"); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}

	host := rtProviderHost(t, pm, "backup-src")
	defer host.StopRTChildren()

	id, err := host.SpawnRTChild(RTChildOptions{
		ID: "fb-rt", Input: "pvin", Output: "pvout",
		PreferredProvider: "broken-fast", // 首选源是坏的
	})
	if err != nil {
		t.Fatalf("SpawnRTChild: %v", err)
	}

	if _, err := host.PushRTInput(id, "喂"); err != nil {
		t.Fatalf("PushRTInput: %v", err)
	}
	rtWaitRound(t, host, id, 1)

	if fast.calls() == 0 {
		t.Fatalf("首选源 broken-fast 没被尝试过（回退逻辑没走候选链）")
	}
	if backup.calls() == 0 {
		t.Fatalf("首选源故障后没有回退到 backup-src —— "+
			"rt 变成零回退的单源，抖一次就断一通（broken=%d backup=%d）",
			fast.calls(), backup.calls())
	}
}

// TestRTChild_InterruptDoesNotFallback 钉住「取消不是可回退错误」。
//
// 契约 1：context.Canceled 的语义是「用户又说话了，上一轮作废」，
// 不是「这个源坏了」。若把它当可回退错误，用户一插话就会把新请求
// 打到第二个源上，而第一个源其实完全健康。
//
// 变红条件：streamWithFallback 去掉 context.Canceled 的特判 ⇒ 红。
func TestRTChild_InterruptDoesNotFallback(t *testing.T) {
	// 直接单测 streamWithFallback：造一个「永远返回 Canceled」的源。
	cancelSrc := &rtCountProvider{name: "cancel-src", failWith: context.Canceled}
	backup := &rtCountProvider{name: "backup", content: "should-not-be-used"}

	pm := agentAPI.NewProviderManager()
	pm.Register("cancel-src", cancelSrc)
	pm.Register("backup", backup)
	if err := pm.SetDefault("cancel-src"); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}

	a := New(AgentConfig{
		ID: "cancel-agent", Provider: pm.Get("cancel-src"), ProviderManager: pm,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := &agentAPI.CompletionRequest{Messages: nil}
	providers := []agentAPI.Provider{cancelSrc, backup}
	_, err := a.streamWithFallback(ctx, req, providers)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应原样返回 context.Canceled，实际 = %v", err)
	}
	if backup.calls() != 0 {
		t.Fatalf("取消被当成了可回退错误：backup 被调用 %d 次 —— "+
			"用户插话会白打一个请求到备用源", backup.calls())
	}
}

// TestRTChild_StreamFallbackDoesNotLeakModel 钉住「model 是 provider 的属性」。
//
// 见 callLLMWithFallback 的 2026-10-27 真机记录：回退时复用同一个 req，
// 会让备用源收到上一个源的 model，被网关 403 model_not_allowed 拒掉。
// 变红条件：streamWithFallback 直接改调用方的 req.Model ⇒ 红。
func TestRTChild_StreamFallbackDoesNotLeakModel(t *testing.T) {
	seen := make(chan string, 4)
	first := &modelProbeProvider{name: "first", failWith: errors.New("boom"), seen: seen}
	second := &modelProbeProvider{name: "second", model: "second-model", seen: seen}

	pm := agentAPI.NewProviderManager()
	pm.Register("first", first)
	pm.Register("second", second)
	if err := pm.SetDefault("first"); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	a := New(AgentConfig{ID: "m", Provider: pm.Get("first"), ProviderManager: pm})

	req := &agentAPI.CompletionRequest{Model: "original-model"}
	if _, err := a.streamWithFallback(context.Background(), req,
		[]agentAPI.Provider{first, second}); err != nil {
		t.Fatalf("streamWithFallback: %v", err)
	}

	if req.Model != "original-model" {
		t.Fatalf("调用方的 req.Model 被改写成 %q —— 回退不该污染请求", req.Model)
	}
	// 第二个源拿到的必须是**它自己的** model。
	var gotSecond string
	timeout := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case m := <-seen:
			gotSecond = m
		case <-timeout:
			t.Fatalf("只观测到 %d 次 ChatStream 调用", i)
		}
	}
	if gotSecond != "second-model" {
		t.Fatalf("回退源收到的 model = %q，期望 second-model（模型名串台）", gotSecond)
	}
}

// modelProbeProvider 记录每次被调用时 req.Model 的值。
type modelProbeProvider struct {
	name     string
	model    string
	failWith error
	seen     chan string
}

func (p *modelProbeProvider) Name() string          { return p.name }
func (p *modelProbeProvider) Model() string         { return p.model }
func (p *modelProbeProvider) Priority() int         { return 0 }
func (p *modelProbeProvider) MaxContextTokens() int { return 8192 }
func (p *modelProbeProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return &agentAPI.CompletionResponse{Content: "x"}, nil
}
func (p *modelProbeProvider) ChatStream(_ context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	select {
	case p.seen <- req.Model:
	default:
	}
	if p.failWith != nil {
		return nil, p.failWith
	}
	ch := make(chan agentAPI.StreamChunk, 2)
	ch <- agentAPI.StreamChunk{Content: "ok"}
	ch <- agentAPI.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}
