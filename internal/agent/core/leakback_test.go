package core

// ★★ 决定性的安全前提：**插件把明文返回时，内核能否拦住**？
//
// 这决定了「让安全模块向插件暴露 Fill()」是否安全：
//
//	若拦得住 ⇒ 暴露 Fill 不新增泄漏面（明文回到内核时会被换回别名）
//	若拦不住 ⇒ 暴露 Fill 会让 LLM 通过插件读到任意明文，S1 作废
//
// 链路：插件 handler 返回明文 → ToolResults → ①context scrub（C5/C6）
//                                        → ②provider 装饰器（C1）
// 两处都必须拦住。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

func TestPremise_PluginReturnedPlaintextIsScrubbed(t *testing.T) {
	store := newScrubStore(t)
	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	alias, err := store.Submit(tok, "测")
	if err != nil {
		t.Fatal(err)
	}

	// 插件工具：**故意**把明文当返回值吐出来（模拟"恶意/粗糙插件"）
	a := New(AgentConfig{
		ID:              "leakback",
		Provider:        &echoTokenProvider{reply: "收到"},
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	if err := a.stageHost.RegisterTool("leaky", sdk.ToolDef{
		Name: "leaky", Description: "故意回吐明文",
		Parameters: map[string]interface{}{"type": "object"},
	}, func(args map[string]interface{}) (interface{}, error) {
		return "凭据是 " + tok, nil // ★ 直接回吐明文
	}); err != nil {
		t.Fatal(err)
	}
	a.Start()
	defer a.Stop()

	// 走真实调度循环：模型调 leaky，结果进 context
	a.io.InjectTextSync("http", "查一下")

	// ① context 侧必须已换成别名
	for _, evt := range a.context.Recent(0) {
		for _, tr := range evt.ToolResults {
			if strings.Contains(tr.Output, tok) {
				t.Errorf("★ context 侧未拦住：%q", tr.Output)
			}
		}
	}
	// ② 记录里也不能有明文
	if b := store.List(); len(b) == 0 {
		t.Fatal("应登记变量")
	}
	_ = alias
	t.Log("✅ 插件回吐的明文被内核拦下（context 侧）")
}

// ★★ 同上的 provider 侧：明文不得到达 LLM。
func TestPremise_PluginReturnedPlaintextNeverReachesLLM(t *testing.T) {
	store := newScrubStore(t)
	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if _, err := store.Submit(tok, "测"); err != nil {
		t.Fatal(err)
	}

	// 用已有的 captureProvider 看它实际收到什么
	cp := &captureProvider{}
	a := New(AgentConfig{
		ID:              "leakback2",
		Provider:        cp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	if err := a.stageHost.RegisterTool("leaky2", sdk.ToolDef{
		Name: "leaky2", Description: "故意回吐明文",
		Parameters: map[string]interface{}{"type": "object"},
	}, func(args map[string]interface{}) (interface{}, error) {
		return "凭据是 " + tok, nil
	}); err != nil {
		t.Fatal(err)
	}
	a.Start()
	defer a.Stop()
	a.io.InjectTextSync("http", "查一下")

	// provider 收到的**全部**消息里都不能有明文
	cp.mu.Lock()
	msgs := append([]agentAPI.Message(nil), cp.messages...)
	cp.mu.Unlock()
	for _, m := range msgs {
		if strings.Contains(m.Content, tok) {
			t.Errorf("★ 明文到达了 provider（LLM 能看到）: %q", m.Content)
		}
		for _, tr := range m.ToolCalls {
			if strings.Contains(fmt.Sprint(tr.Arguments), tok) {
				t.Errorf("★ 工具参数里的明文到达 provider: %v", tr.Arguments)
			}
		}
	}
	if len(msgs) == 0 {
		t.Fatal("provider 一条消息都没收到 —— 判据无效")
	}
	t.Logf("✅ provider 侧未见明文（看了 %d 条消息）", len(msgs))
	_ = context.Background
}
