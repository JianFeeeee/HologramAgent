package core

import (
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 这组判据针对一个「发了但没人收到」的缺陷：
//
//	agent 自己在 stepLLM 里把用量记进 usageLedger，也发进了
//	EventAgentLLMChain 事件；但同步注入方（WebUI 的 /v1/chat/completions、
//	cli.sock、clawhubadapter）拿到的回包来自 emitResponse，而 emitResponse
//	**新建**了一个 StageCtx，只从插件侧读 TokenUsage。
//	agent 自己算出来的用量从未传进去 ⇒ 回包 payload 里没有 usage。
//
// 实测证据（部署实例 8080）：
//
//	curl /v1/chat/completions ... → 回包中 "usage" 字段不存在（回复正常）
//
// ★ 还有一个必须一并守住的口径（本项目已踩过一次）：
// handler_openai.go 用 `Payload["usage"].(map[string]interface{})` 取值，
// 而 Go 的 map 类型断言是**精确匹配** —— 给 map[string]int 会断言失败，
// 于是 usage 静默变 nil，两边都不报错。所以回包里的 usage 必须是
// map[string]interface{}，判据就按消费方的要求断言动态类型。

// newEmitAgent 造一个只够跑 emitResponse 的 Agent（照 agent_tools_test.go 的直构法）。
func newEmitAgent() *Agent {
	return &Agent{stageHost: NewStageHost()}
}

// asUsageMap 取出回包里的 usage 并要求它是 map[string]interface{}。
//
// 断言**动态类型**而不是只断言值，是因为消费方（handler_openai.go）用的就是
// 精确类型断言：这里是唯一能拦住「map[string]int 静默变 nil」的地方。
func asUsageMap(t *testing.T, v interface{}) map[string]interface{} {
	t.Helper()
	m, ok := v.(map[string]interface{})
	if !ok {
		t.Fatalf("usage 动态类型=%T，期望 map[string]interface{}（消费方用精确断言，"+
			"给 map[string]int 会让它在回包里静默变 nil）", v)
	}
	return m
}

func tk(prompt, completion, total, cacheRead, cacheMiss, reasoning int, cacheReported bool) agentAPI.TokenUsage {
	return agentAPI.TokenUsage{
		Prompt: prompt, Completion: completion, Total: total,
		CacheRead: cacheRead, CacheMiss: cacheMiss,
		ReasoningTokens: reasoning, CacheReported: cacheReported,
	}
}

// TestEmitResponseCarriesSessionUsage 是核心判据：
// 会话里有过 LLM 记账之后，同步回执必须带上 usage，
// 且数字来自 agent 自己的累计（不是零值占位）。
func TestEmitResponseCarriesSessionUsage(t *testing.T) {
	a := newEmitAgent()
	// 两次 LLM 调用：一次报了缓存，一次没报（分母口径见 usagestats.go）。
	a.usageLedger.record(tk(1000, 200, 1200, 768, 232, 50, true))
	a.usageLedger.record(tk(500, 100, 600, 0, 0, 0, false))
	ch := make(chan *agentIO.OutputEvent, 1)
	evt := &agentIO.InputEvent{
		RequestID: "req-1", Source: "http", OutputChannel: "http", ResponseCh: ch,
	}
	sum := agentAPI.TokenUsage{}
	sum.Add(tk(1000, 200, 1200, 768, 232, 50, true))
	sum.Add(tk(500, 100, 600, 0, 0, 0, false))
	a.emitResponse(evt, "好的", sum)

	select {
	case out := <-ch:
		if out == nil {
			t.Fatal("回执为 nil")
		}
		raw, ok := out.Payload["usage"]
		if !ok || raw == nil {
			t.Fatalf("同步回执缺少 usage（这正是 /v1/chat/completions 取不到的根因）；payload=%v", out.Payload)
		}
		m := asUsageMap(t, raw)
		// 会话累计：prompt 1000+500、completion 200+100、total 1200+600
		for _, c := range []struct {
			key  string
			want int
		}{
			{"prompt_tokens", 1500},
			{"completion_tokens", 300},
			{"total_tokens", 1800},
			{"cache_read_tokens", 768},
			{"cache_miss_tokens", 232},
			{"reasoning_tokens", 50},
		} {
			if got, _ := m[c.key].(int); got != c.want {
				t.Errorf("%s=%v，期望 %v（会话累计）", c.key, m[c.key], c.want)
			}
		}
	default:
		t.Fatal("emitResponse 没有写回执（不变量 I5 被破坏）")
	}
}

// TestEmitResponseOmitsUsageWhenNothingRecorded 是上一条的边界：
// 一次 LLM 都没跑过时不要凭空造 usage —— 否则消费方会把「没有数据」
// 当成「用了 0 token」，与本项目既有的 CacheHitRate ok=false 口径一致。
func TestEmitResponseOmitsUsageWhenNothingRecorded(t *testing.T) {
	a := newEmitAgent()
	ch := make(chan *agentIO.OutputEvent, 1)
	evt := &agentIO.InputEvent{
		RequestID: "req-2", Source: "http", OutputChannel: "http", ResponseCh: ch,
	}
	a.emitResponse(evt, "你好", agentAPI.TokenUsage{})

	select {
	case out := <-ch:
		if u, ok := out.Payload["usage"]; ok && u != nil {
			t.Errorf("没有任何 LLM 调用时报了 usage=%v；应当缺省，让消费方显示「—」", u)
		}
	default:
		t.Fatal("没有写回执")
	}
}

// TestEmitResponsePluginUsageStillHonored 保住既有契约：
// 插件（StageBeforeOutput）显式设置的 TokenUsage 必须仍然生效 ——
// 这是本次改动前唯一能拿到 usage 的路径，不能因为补了 agent 侧就丢掉。
func TestEmitResponsePluginUsageStillHonored(t *testing.T) {
	a := newEmitAgent()
	a.stageHost.RegisterStage(sdk.StageBeforeOutput, func(ctx *sdk.StageContext) error {
		ctx.TokenUsage = map[string]int{"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}
		return nil
	})

	ch := make(chan *agentIO.OutputEvent, 1)
	evt := &agentIO.InputEvent{RequestID: "req-3", Source: "http", OutputChannel: "http", ResponseCh: ch}
	a.emitResponse(evt, "hi", agentAPI.TokenUsage{})

	select {
	case out := <-ch:
		raw, ok := out.Payload["usage"]
		if !ok || raw == nil {
			t.Fatalf("插件显式设置的 usage 丢了；payload=%v", out.Payload)
		}
		m := asUsageMap(t, raw)
		if got, _ := m["prompt_tokens"].(int); got != 7 {
			t.Errorf("prompt_tokens=%v，期望插件设置的 7", m["prompt_tokens"])
		}
	default:
		t.Fatal("没有写回执")
	}
}
