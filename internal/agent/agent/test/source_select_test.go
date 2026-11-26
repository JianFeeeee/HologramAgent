package test

// 「按能力选源」不关闭回退链的判据（用户裁定 2026-11-20）。
//
// ## 要钉住什么
//
// 模态回退（主模型看不了图 ⇒ 换一个声明了 vision 的源）必须仍然走**候选链**：
// 点名的源只是排到最前，不可用/失败时照样往下试。
//
// ★ 为什么这条必须钉：如果把"点名"实现成"独占"，首选源一抖动
//   整条回退链就废了 —— 而界面上只会显示"转写失败"，
//   看不出真正原因是"点名把链锁死了"。

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentProvider "github.com/JianFeeeee/HologramAgent/internal/agent/provider"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod/llm"
)

type recProvider struct {
	name string
	fail bool
	hits *[]string
}

func (p *recProvider) Name() string          { return p.name }
func (p *recProvider) MaxContextTokens() int { return 8192 }
func (p *recProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	if p.hits != nil {
		*p.hits = append(*p.hits, p.name)
	}
	if p.fail {
		return nil, &agentAPI.ProviderError{StatusCode: 500, Message: "boom"}
	}
	return &agentAPI.CompletionResponse{Content: p.name + ":ok"}, nil
}
func (p *recProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return nil, errors.New("not used")
}

// TestSourcePinDoesNotCloseFallbackChain：点名源失败 ⇒ 仍试下一条。
func TestSourcePinDoesNotCloseFallbackChain(t *testing.T) {
	mgr := agentProvider.NewProviderManager()
	var hits []string
	mgr.Register("fast", &recProvider{name: "fast", fail: true, hits: &hits})
	mgr.Register("slow", &recProvider{name: "slow", hits: &hits})
	_ = mgr.SetDefault("slow")

	c := llm.NewClient(mgr)
	// NoRetry：本判据只看**跨源**顺序，同源瞬时重试会把 fast 打两次。
	resp, err := c.Complete(context.Background(), &kernelapi.LLMRequest{
		AgentID: "t",
		NoRetry: true,
		Request: &agentAPI.CompletionRequest{
			Messages: []agentAPI.Message{{Role: "user", Content: "hi"}},
			Source:   "fast", // 点名快源（模拟按能力选源）
		},
	})
	if err != nil {
		t.Fatalf("点名源失败后应回退到下一条：%v", err)
	}
	if resp.Content != "slow:ok" {
		t.Fatalf("回退到了错误的源：%q", resp.Content)
	}
	if len(hits) != 2 || hits[0] != "fast" || hits[1] != "slow" {
		t.Fatalf("★ 候选链顺序不是「点名源 → 其余」：%v", hits)
	}
}

// TestSourceSnapshotExposesDeclaredCapabilities：能力快照来自内核，类型只读。
func TestSourceSnapshotExposesDeclaredCapabilities(t *testing.T) {
	mgr := agentProvider.NewProviderManager()
	mgr.Register("textonly", &recProvider{name: "textonly"})
	mgr.Register("vision", &recProvider{name: "vision"})
	mgr.SetCapabilities("textonly", agentProvider.ProviderCaps{Vision: false})
	mgr.SetCapabilities("vision", agentProvider.ProviderCaps{Vision: true})

	c := llm.NewClient(mgr)
	var found bool
	for _, s := range c.Sources() {
		if s.Name == "vision" {
			found = true
			if !s.Vision {
				t.Fatal("★ 快照丢了声明能力 —— 类型就挑不出能看图的源")
			}
		}
		if s.Name == "textonly" && s.Vision {
			t.Fatal("★ 快照把没声明能力的源报成能看图")
		}
	}
	if !found {
		t.Fatal("快照里没有 vision 源")
	}
}

// TestSetDefaultGoesThroughFacility：切源是内核策略，走设施。
func TestSetDefaultGoesThroughFacility(t *testing.T) {
	mgr := agentProvider.NewProviderManager()
	mgr.Register("a", &recProvider{name: "a"})
	mgr.Register("b", &recProvider{name: "b"})
	_ = mgr.SetDefault("a")

	c := llm.NewClient(mgr)
	if err := c.SetDefault("b"); err != nil {
		t.Fatalf("切源失败：%v", err)
	}
	if got := mgr.Default(); got == nil || got.Name() != "b" {
		t.Fatalf("切源没生效：%v", got)
	}
	if err := c.SetDefault("nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("★ 切到不存在的源必须报错并点名：%v", err)
	}
}
