package core

// 回归判据：M3 实现与设计稿 §5.1/§4.3 的两处偏离。
//
// 这两条是**先写判据、确认失败、再修**的（修完保留为回归测试）。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// cancelAwareProvider 第一次调用进入后阻塞，直到 ctx 取消；记录是否被取消。
type cancelAwareProvider struct {
	once     sync.Once
	entered  chan struct{}
	canceled atomic.Bool
}

func newCancelAwareProvider() *cancelAwareProvider {
	return &cancelAwareProvider{entered: make(chan struct{})}
}

func (p *cancelAwareProvider) Name() string { return "cancel-aware" }
func (p *cancelAwareProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.once.Do(func() { close(p.entered) })
	<-ctx.Done()
	p.canceled.Store(true)
	return nil, ctx.Err()
}
func (p *cancelAwareProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return nil, context.Canceled
}
func (p *cancelAwareProvider) MaxContextTokens() int { return 8192 }

// 设计 §5.1 ③：interruptLoop 定级/决策后必须**唤醒调度器**。
// 现状：中断只被放进 pendingInterrupts，而调度器空闲时阻塞在 select（只看
// InputChan/selfInputCh/ctx.Done）——没有任何东西会把它叫醒。
func TestGap_IdleInterruptIsProcessed(t *testing.T) {
	sp := &countingProvider{}
	a := New(AgentConfig{
		ID:              "idle-intr",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	a.Start()
	defer a.Stop()

	// 完全空闲时投递一条中断（模拟定时器通知/插件提醒）。
	a.io.InjectInterruptText("qq", "cli", "空闲时的通知")

	deadline := time.Now().Add(3 * time.Second)
	for sp.n.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if sp.n.Load() == 0 {
		snap := a.DumpScheduler()
		t.Fatalf("空闲时到达的中断未被处理：pendingInterrupts=%d（调度器未被唤醒）",
			len(snap.PendingInterrupts))
	}
}

// 设计 §4.3/§5.2：`_consolidation_` 是整任务临界区——抢占请求必须排队等它结束，
// 而不是取消它。现状：requestPreempt 不判临界区，interceptLoop 照常 cancelLLM，
// 于是正在流式的记忆整理被中断 → stepLLM 直接以 error 结束（整理丢一半）。
func TestGap_ConsolidationMustNotBeCancelled(t *testing.T) {
	sp := newCancelAwareProvider()
	a := New(AgentConfig{
		ID:              "consol",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	a.Start()
	defer a.Stop()

	// 走自循环通道发起一次记忆整理。
	a.selfInputCh <- selfInputMsg{text: "合并实体", channel: channelConsolidation}

	select {
	case <-sp.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("记忆整理未进入 LLM 调用")
	}

	// L4 中断到达。
	a.io.InjectInterruptText("cli", "cli", "L4 打断")

	time.Sleep(500 * time.Millisecond)
	if sp.canceled.Load() {
		t.Fatal("记忆整理是临界区，其 LLM 不该被取消（设计 §4.3/§5.2）")
	}
}
