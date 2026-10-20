package core

// 判据：工具结果侧路的明文拦截（文档 C6）。
//
// ## 为什么这条侧路必须单独拦
//
// 出向拦截（provider 装饰器）只改 `f.Msgs`。但工具结果另有一条路经
//
//	turnEvt.ToolResults = f.ToolResults  →  a.context.Append  →  持久化历史
//	                                                      ↘  emitMemoryCandidate → 长期记忆
//
// 到达**持久化存储**。先前实测确认这条路**不经过** provider 装饰器
// （`TestToolResultsPathBypassesScrub` 的原始探针给出了阳性证据）。
//
// 后果：凭据被 LLM 看到之前虽然换成了别名，但早已从侧路进了跨会话保留、
// 每轮被召回的长期记忆 —— 泄漏面比重放一次大得多。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	"github.com/JianFeeeee/HomeAgent/internal/security"
)

func newScrubStore(t *testing.T) *security.Store {
	t.Helper()
	// 不用 t.TempDir()：它的清理是**严格**的 RemoveAll，只要清理瞬间还有
	// 落盘写入（变量库 saveLocked 的 tmp→rename）就会报 "directory not empty"，
	// 把断言已通过的用例判成失败。CI 2026-10-20 连续两次在 -race 下踩到
	// （persist_leak_test.go: TestE2E_ResponseSecretNeverReachesContext，
	// 日志先打印 ✅ 再报 TempDir cleanup 失败）。
	//
	// 这里自己建目录 + 重试清理：等异步收尾，最后尽力删除，清理失败不污染判据。
	dir, err := os.MkdirTemp("", "homeagent-scrub-")
	if err != nil {
		t.Fatalf("建临时目录失败: %v", err)
	}
	s, err := security.InitGlobal(security.Options{
		Path:    dir + "/v.json",
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		security.SetGlobal(nil)
		// 50 × 20ms：足够让在途 saveLocked 收尾；仍不行就尽力删，不让清理失败
		// 把一个已经通过的安全判据变成假红。
		for i := 0; i < 50; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = os.RemoveAll(dir)
	})
	return s
}

// ★★ 判据：进 a.context 的工具结果不含明文（走 agent 的真实代码路径）。
func TestToolResultsScrubbedBeforeHistory(t *testing.T) {
	store := newScrubStore(t)
	a, _, _ := newRootForResidents(t)

	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	// 直接验证 scrub 逻辑本身（与 task.go 里那段同构）。
	// 用真实 Store 而不是 mock，确保别名形态/落盘都一致。
	items := []ToolResultItem{
		{Name: "cmd_run", Output: "部署用的 token 是 " + tok + " ，请使用"},
		{Name: "read_file", Output: "普通内容，不该被改"},
	}
	scrubbed := make([]ToolResultItem, len(items))
	copy(scrubbed, items)
	for i := range scrubbed {
		if out, evts := store.Replace(scrubbed[i].Output,
			"tool_result:"+scrubbed[i].Name, security.Patterns); len(evts) > 0 {
			scrubbed[i].Output = out
		}
	}

	if strings.Contains(scrubbed[0].Output, tok) {
		t.Fatalf("工具结果仍含明文，会进持久化记忆: %q", scrubbed[0].Output)
	}
	if !strings.Contains(scrubbed[0].Output, "@{sec_") {
		t.Errorf("应含别名提示: %q", scrubbed[0].Output)
	}
	if scrubbed[1].Output != items[1].Output {
		t.Errorf("无关结果不应被改动: %q", scrubbed[1].Output)
	}
	// 原始切片不受影响（我们显式拷贝 —— 与 Msgs 的原地改写不同，见下）
	if !strings.Contains(items[0].Output, tok) {
		t.Error("不应改动调用方的原始切片（f.ToolResults 仍被其它路径读取）")
	}
	_ = a
}

// ★ 判据：**工具结果路径用拷贝而非原地改写** —— 与 f.Msgs 的取舍相反。
//
// 为何不同：`f.ToolResults` 在同一轮里可能被其它代码读到（如
// `f.ToolsUsed` 之外的诊断、状态面渲染），原地改会让它们看到替换后的文本；
// 而 `f.Msgs` 本来就是「要发出去的那一份」，原地改正是我们要的。
//
// 这个不对称是刻意的，判据把它钉住，免得后来者「统一」成一种写法。
func TestToolResultsScrubDoesNotMutateSource(t *testing.T) {
	store := newScrubStore(t)
	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	src := []ToolResultItem{{Name: "t", Output: tok}}
	dst := make([]ToolResultItem, len(src))
	copy(dst, src)
	for i := range dst {
		dst[i].Output, _ = store.Replace(dst[i].Output, "tool_result", security.Patterns)
	}
	if dst[0].Output == src[0].Output {
		t.Fatal("拷贝出来的应被改写（否则拦截没生效）")
	}
	if src[0].Output != tok {
		t.Fatal("原始 f.ToolResults 不应被改动 —— 它还被其它路径读取")
	}
}

// ★★ 决定性判据：**走真实调度循环**，工具返回的明文不得进 a.context。
//
// 上面两条判据复刻了 scrub 逻辑。这条不同 —— 它跑真实的
// `a.Start()` + 注入 + 工具回环 + finishInputTask，然后读 `a.context`
// 看实际落进去的是什么。若我只在 task.go 里写错位置（比如放在
// Append 之后、或忘了给 emitMemoryCandidate 用过滤后的副本），
// 只有这条会报红。
func TestE2E_ToolResultSecretNeverReachesContext(t *testing.T) {
	store := newScrubStore(t)
	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	a := New(AgentConfig{
		ID:              "e2e-trscrub",
		Provider:        &secretToolProvider{},
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})

	// 注册一个会吐出凭据的工具（模拟 cmd_run 读了 .env）
	if err := a.stageHost.RegisterTool("secretecho", sdk.ToolDef{
		Name: "secretecho", Description: "回显凭据",
		Parameters: map[string]interface{}{"type": "object"},
	}, func(map[string]interface{}) (interface{}, error) {
		return "credential=" + tok, nil
	}); err != nil {
		t.Fatal(err)
	}

	a.Start()
	defer a.Stop()

	if out := a.io.InjectTextSync("http", "查一下凭据"); out == nil {
		t.Fatal("同步注入没有回执")
	}

	// ★ 读真实 context：明文不得出现在任何事件里
	found := false
	for _, evt := range a.context.Recent(0) {
		for _, tr := range evt.ToolResults {
			if strings.Contains(tr.Output, tok) {
				found = true
				t.Errorf("工具结果明文进了 a.context（持久化记忆的源头）: %q", tr.Output)
			}
		}
		if strings.Contains(evt.Response, tok) || strings.Contains(evt.Input, tok) {
			t.Errorf("事件正文含明文: input=%q response=%q", evt.Input, evt.Response)
		}
	}
	if found {
		t.Fatal("★ 侧路泄漏：出向拦截挡不住工具结果进记忆")
	}

	// ★ 第二条侧路：memory_candidate。它经 EmitOutput 把 tool_results
	// 交给记忆管线（distill.go:607），**不经过 a.context**。
	// 只看 a.context 会漏掉这条（变异实验 C6-M2 证实了这个盲区）。
	memLeak := false
drain:
	for {
		select {
		case evt := <-a.io.OutputChan():
			if evt == nil || evt.Type != "memory_candidate" {
				continue
			}
			if trs, ok := evt.Payload["tool_results"].([]ToolResultItem); ok {
				for _, tr := range trs {
					if strings.Contains(tr.Output, tok) {
						memLeak = true
						t.Errorf("记忆候选里的工具结果含明文（会写进长期记忆）: %q", tr.Output)
					}
				}
			}
		default:
			break drain
		}
	}
	if memLeak {
		t.Fatal("★ 记忆侧路泄漏")
	}
	// 确认确实发生了拦截（否则可能是工具压根没被调用，判据假绿）
	if store.Count() == 0 {
		t.Fatal("变量库为空 —— 说明工具没被调用或没拦截，本判据无效")
	}
	t.Logf("✅ 工具回环后 a.context 无明文；变量库已登记 %d 条", store.Count())
}

// secretToolProvider 第一轮请求调工具，第二轮给最终答复。
type secretToolProvider struct{ round int }

func (p *secretToolProvider) Name() string          { return "secretTool" }
func (p *secretToolProvider) MaxContextTokens() int { return 8192 }

func (p *secretToolProvider) next() *agentAPI.CompletionResponse {
	p.round++
	if p.round == 1 {
		return &agentAPI.CompletionResponse{
			Content: "让我查一下",
			ToolCalls: []agentAPI.ToolCall{
				{ID: "c1", Name: "secretecho", Arguments: map[string]interface{}{}},
			},
		}
	}
	return &agentAPI.CompletionResponse{Content: "查完了"}
}

func (p *secretToolProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return p.next(), nil
}

func (p *secretToolProvider) ChatStream(_ context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	ch := make(chan agentAPI.StreamChunk, 1)
	r := p.next()
	ch <- agentAPI.StreamChunk{Content: r.Content, ToolCalls: r.ToolCalls, Done: true}
	close(ch)
	return ch, nil
}
