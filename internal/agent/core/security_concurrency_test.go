package core

// 安全模块的**端到端并发压测**。
//
// ## 为什么必须做
//
// 安全模块挂在每次 LLM 调用与每次工具调用的必经路径上，而内核是
// **并发**的：多 agent、多任务、调度器并行、插件回调各自的 goroutine。
//
// security 包自己的并发判据只验了 Store 内部；
// **Store 的使用方**（provider 装饰器、ExecuteTool、Append、emitMemoryCandidate）
// 是否并发安全，只有让真实 Agent 并跑才看得出来。

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// ★★ 判据：**多 agent 并发出向** —— 明文一律不得泄漏到 provider。
//
// 每个 agent 有自己的 provider 实例，但它们共享**同一个**
// ProviderManager 装饰器与变量库单例（这正是设计要验的点）。
func TestConcurrency_MultiAgentOutboundScrub(t *testing.T) {
	store := newScrubStore(t)
	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	const agents = 16
	const rounds = 40

	var leaked atomic.Int64
	var wg sync.WaitGroup

	for a := 0; a < agents; a++ {
		wg.Add(1)
		go func(a int) {
			defer wg.Done()
			var captured string
			mgr := agentAPI.NewProviderManager()
			mgr.Register("p", &fwCaptureProvider{name: "p", capture: &captured})
			p := mgr.Default()

			for r := 0; r < rounds; r++ {
				// 并发登记（同明文 ⇒ 应合并到同一别名）
				if _, err := store.Submit(tok, "conc"); err != nil {
					t.Errorf("Submit: %v", err)
					return
				}
				req := &agentAPI.CompletionRequest{Messages: []agentAPI.Message{
					{Role: "user", Content: fmt.Sprintf("agent%d round%d token=%s", a, r, tok)},
				}}
				if _, err := p.Chat(nil, req); err != nil { //nolint:staticcheck
					t.Errorf("Chat: %v", err)
					return
				}
				if strings.Contains(captured, tok) {
					leaked.Add(1)
				}
			}
		}(a)
	}
	wg.Wait()

	if leaked.Load() > 0 {
		t.Fatalf("★ 并发下 %d 次出向泄漏明文（装饰器不是并发安全的）", leaked.Load())
	}
	// 同明文必须合并成一条
	if got := store.Count(); got != 1 {
		t.Errorf("★ 同明文并发登记应合并为 1 条，实际 %d", got)
	}
	t.Logf("✅ %d agent × %d 轮并发出向，零泄漏；变量库 1 条", agents, rounds)
}

// ★★ 判据：**并发工具调用** —— 参数回填不串号。
//
// 真实风险：A 的工具参数里的别名被填成 B 的凭据（串号），
// 这会造成"用了错的凭据"且**不报错**。
func TestConcurrency_ToolArgsDoNotCrossTalk(t *testing.T) {
	store := newScrubStore(t)
	a, _, _ := newRootForResidents(t)

	// 每个 goroutine 用**自己**的凭据与别名
	const workers = 12
	aliases := make([]string, workers)
	plains := make([]string, workers)
	for i := 0; i < workers; i++ {
		plains[i] = fmt.Sprintf("distinct-secret-%02d-aaaaaaaaaaaaaaaaaaaa", i)
		al, err := store.Submit(plains[i], fmt.Sprintf("w%d", i))
		if err != nil {
			t.Fatal(err)
		}
		aliases[i] = al
	}

	var mixed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var seen string
			tool := fmt.Sprintf("conc_tool_%d", i)
			// 每个 goroutine 注册自己的工具（StageHost 的注册是加锁的）
			if err := a.stageHost.RegisterTool(tool, sdk.ToolDef{
				Name: tool, Description: "并发压测",
				Parameters: map[string]interface{}{"type": "object"},
			}, func(args map[string]interface{}) (interface{}, error) {
				seen, _ = args["secret"].(string)
				return "ok", nil
			}); err != nil {
				t.Errorf("RegisterTool: %v", err)
				return
			}
			for r := 0; r < 60; r++ {
				seen = ""
				if _, err := a.stageHost.ExecuteTool(tool, map[string]interface{}{
					"secret": "@{" + aliases[i] + "}",
				}); err != nil {
					t.Errorf("ExecuteTool: %v", err)
					return
				}
				// ★ 必须填成**自己的**那个，不能是别人的
				if seen != plains[i] {
					mixed.Add(1)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	if mixed.Load() > 0 {
		t.Fatal("★ 并发工具调用发生**串号**（填成了别的凭据）—— 用错凭据且不报错")
	}
	t.Logf("✅ %d goroutine × 60 轮并发工具调用，零串号", workers)
}

// ★★ 判据：**并发上下文写入**（Append 咽喉的 scrub）不丢不串。
func TestConcurrency_ConcurrentContextScrub(t *testing.T) {
	store := newScrubStore(t)
	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if _, err := store.Submit(tok, "ctx"); err != nil {
		t.Fatal(err)
	}

	a, _, _ := newRootForResidents(t)

	const writers = 10
	const perWriter = 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				a.context.Append(ContextEvent{
					Source:   fmt.Sprintf("w%d", w),
					Input:    fmt.Sprintf("in %d token=%s", i, tok),
					Response: "out contains " + tok,
					ToolResults: []ToolResultItem{
						{Name: "t", Output: "tool " + tok},
					},
				})
			}
		}(w)
	}
	wg.Wait()

	// 全部事件都不该含明文
	bad := 0
	for _, evt := range a.context.Recent(0) {
		if strings.Contains(evt.Input, tok) || strings.Contains(evt.Response, tok) {
			bad++
		}
		for _, tr := range evt.ToolResults {
			if strings.Contains(tr.Output, tok) {
				bad++
			}
		}
	}
	if bad > 0 {
		t.Fatalf("★ 并发写入下有 %d 处明文未被拦下", bad)
	}
	if n := a.context.Len(); n < writers*perWriter {
		t.Errorf("并发写入丢事件：期望至少 %d，实际 %d", writers*perWriter, n)
	}
	t.Logf("✅ %d goroutine × %d 次并发写入全部拦下（事件数 %d）",
		writers, perWriter, a.context.Len())
}

// ★★ 判据：**并发记忆候选**（emitMemoryCandidate 的 scrub）。
func TestConcurrency_ConcurrentMemoryCandidate(t *testing.T) {
	store := newScrubStore(t)
	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if _, err := store.Submit(tok, "mem"); err != nil {
		t.Fatal(err)
	}

	a := New(AgentConfig{
		ID:              "conc-mem",
		Provider:        &plainReplyProvider{},
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.emitMemoryCandidate("src",
				fmt.Sprintf("in %d %s", i, tok),
				"resp "+tok,
				[]ToolResultItem{{Name: "t", Output: "tool " + tok}},
				[]string{"t"})
		}(i)
	}
	wg.Wait()

	// 排空输出，逐条检查载荷
	leaked := 0
	got := 0
	for {
		select {
		case evt := <-a.io.OutputChan():
			if evt == nil || evt.Type != "memory_candidate" {
				continue
			}
			got++
			if s, _ := evt.Payload["input"].(string); strings.Contains(s, tok) {
				leaked++
			}
			if s, _ := evt.Payload["response"].(string); strings.Contains(s, tok) {
				leaked++
			}
			if trs, ok := evt.Payload["tool_results"].([]ToolResultItem); ok {
				for _, tr := range trs {
					if strings.Contains(tr.Output, tok) {
						leaked++
					}
				}
			}
		default:
			goto done
		}
	}
done:
	if leaked > 0 {
		t.Fatalf("★ 并发记忆候选中 %d 处含明文（会写进长期记忆）", leaked)
	}
	if got == 0 {
		t.Fatal("没有观察到记忆候选 —— 判据无效")
	}
	t.Logf("✅ %d 条并发记忆候选全部已清洗", got)
}

// ★ 判据：并发守卫（未启用安全模块）时零成本且不 panic。
func TestConcurrency_DisabledIsSafe(t *testing.T) {
	security.SetGlobal(nil)
	t.Cleanup(func() { newScrubStore(t) })

	a, _, _ := newRootForResidents(t)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.context.Append(ContextEvent{
				Source: "s", Input: "in", Response: "out",
				ToolResults: []ToolResultItem{{Name: "t", Output: "o"}},
			})
			a.emitMemoryCandidate("s", "in", "out", nil, nil)
		}(i)
	}
	wg.Wait()
	t.Log("✅ 未启用时并发调用不 panic")
}
