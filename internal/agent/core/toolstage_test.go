package core

// 判据：**四种子 agent 类型都必须经过 before/after_toolcall 插件钩子**。
//
// ## 这条缺陷的实测依据（2026-11-04）
//
// `StageBeforeToolcall` / `StageAfterToolcall` 全仓只出现在
// task.go:1022 / 1079 / 1194，三处都在 runOneTool / stepToolAfter 里。
// 而 rtchild 与工作式子直调 `executeToolCallOutcome`，它内部**没有任何 runStage**。
//
// ⇒ rt 与普通子的工具调用对插件完全不可见：不能审计、不能拒绝、不能改参、不能脱敏。
//
// ## 为什么判据要放在"真实路径"上
//
// 直接测 `runToolBatch` 会绿——但那正是 engineering-lessons.md §1.2 说的
// "测接线之后，不测有没有接线"。所以：
//
//	rt      → 经 SpawnRTChild + 插件吐 tool_call + 真实 rtLoop
//	工作式子 → 经 runChildTask 的真实循环
//
// 变红方式：把对应路径改回直调 executeToolCallOutcome ⇒ 钩子不触发 ⇒ 红。

import (
	"context"
	"sync"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// stageRecorder 记录插件钩子被触发的次数与工具名。
type stageRecorder struct {
	mu        sync.Mutex
	before    []string
	after     []string
	denyTool  string // 非空时 before_toolcall 拒绝该工具
	rewriteTo string // 非空时 after_toolcall 把结果改写成它
}

func (r *stageRecorder) snapBefore() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.before...)
}

func (r *stageRecorder) snapAfter() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.after...)
}

// stageHostFor 造一个带 stage 钩子的 StageHost 替身并挂到 agent 上。
//
// 用真实的 a.runStage 路径：把 handler 注册进 stageHost，然后由
// a.setStageHandler（若不存在则直接塞进 stageHost 的实现）。
func (r *stageRecorder) register(a *Agent) {
	if a.stageHost == nil {
		return
	}
	// ★ stageHost 是具体类型 *StageHost，不是接口：直接调方法。
	// RegisterStage 的 handler 签名是 func(*sdk.StageContext) error（见 stages.go:81）。
	a.stageHost.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error {
		name := ""
		if len(ctx.ToolCalls) > 0 {
			name = ctx.ToolCalls[0].Name
		}
		r.mu.Lock()
		r.before = append(r.before, name)
		deny := r.denyTool
		r.mu.Unlock()
		if deny != "" && name == deny {
			// 拦截的判定是 **ctx.Response != nil**（见 stage.go:38 的 return），
			// handler 返回 error 只表示"插件自身出错"，不表示拦截。
			ctx.Response = strPtr("被插件拒绝：" + name)
		}
		return nil
	})
	a.stageHost.RegisterStage(sdk.StageAfterToolcall, func(ctx *sdk.StageContext) error {
		name := ""
		if len(ctx.ToolResults) > 0 {
			name = ctx.ToolResults[0].Name
		}
		r.mu.Lock()
		r.after = append(r.after, name)
		re := r.rewriteTo
		r.mu.Unlock()
		if re != "" && len(ctx.ToolResults) > 0 {
			ctx.ToolResults[0].Result = re
		}
		return nil
	})
}

func strPtr(s string) *string { return &s }

// rtToolCallProvider 第一轮吐一个 tool_call，第二轮吐纯文本（结束）。
//
// 用两段式：rt 的循环要靠"模型不再要工具"退出，单轮会让判据永远等不到收尾。
type rtToolCallProvider struct {
	toolName string
	mu       sync.Mutex
	calls    int
}

func (p *rtToolCallProvider) Name() string          { return "rt-toolcall" }
func (p *rtToolCallProvider) MaxContextTokens() int { return 8192 }

// Chat 是**工作式子**用的路径（它走非流式 chatWithProviderChain）。
//
// ★ 第一版判据只实现了 ChatStream，于是工作式子永远拿不到 tool_call ⇒
// 判据超时。这本身说明"两种子 agent 走的是不同 LLM 入口"这个事实
// 必须被判据覆盖，否则测不到真实路径。
func (p *rtToolCallProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n == 1 {
		return &agentAPI.CompletionResponse{
			ToolCalls: []agentAPI.ToolCall{{
				ID: "tc1", Name: p.toolName,
				Arguments: map[string]interface{}{"x": "1"},
			}},
		}, nil
	}
	return &agentAPI.CompletionResponse{Content: "好的"}, nil
}
func (p *rtToolCallProvider) ChatStream(_ context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()

	ch := make(chan agentAPI.StreamChunk, 3)
	if n == 1 {
		ch <- agentAPI.StreamChunk{
			ToolCall: &agentAPI.ToolCall{ID: "tc1", Name: p.toolName,
				Arguments: map[string]interface{}{"x": "1"}},
		}
		ch <- agentAPI.StreamChunk{Done: true}
	} else {
		ch <- agentAPI.StreamChunk{Content: "好的"}
		ch <- agentAPI.StreamChunk{Done: true}
	}
	close(ch)
	return ch, nil
}

// TestRTChild_ToolsGoThroughPluginStages 是**核心判据**：rt 的工具必须触发两个钩子。
//
// 变红条件：rtRunOnce 改回直调 executeToolCallOutcome ⇒ before/after 均为空 ⇒ 红。
func TestRTChild_ToolsGoThroughPluginStages(t *testing.T) {
	rec := &stageRecorder{}
	p := &rtToolCallProvider{toolName: "rtstage_probe"}

	pio := agentIO.NewIOManager()
	host := newStageTestHost(t, "rtstage-host", pio, p)
	rec.register(host)
	registerStageTool(host, "rtstage_probe")
	registerPairChannels(t, pio, "sout", "sin")

	id, err := host.SpawnRTChild(RTChildOptions{ID: "st-rt", Input: "sin", Output: "sout"})
	if err != nil {
		t.Fatalf("SpawnRTChild: %v", err)
	}
	defer host.StopRTChildren()

	if _, err := host.PushRTInput(id, "调用工具"); err != nil {
		t.Fatalf("PushRTInput: %v", err)
	}
	waitForWithin(t, "rt 工具钩子被触发", 10*time.Second, func() bool {
		return len(rec.snapBefore()) > 0
	})

	if got := rec.snapBefore(); !hasStageHit(got, "rtstage_probe") {
		t.Fatalf("before_toolcall 未收到 rt 的工具调用：%v", got)
	}
	waitFor(t, "after_toolcall 被触发", func() bool { return len(rec.snapAfter()) > 0 })
	if got := rec.snapAfter(); !hasStageHit(got, "rtstage_probe") {
		t.Fatalf("after_toolcall 未收到 rt 的工具调用：%v", got)
	}
}

// TestRTChild_StageCanDenyTool 钉住"插件能拒绝 rt 的工具"（安全语义的实际效果）。
//
// 变红条件：绕开 before_toolcall ⇒ 拒绝不生效 ⇒ 工具真的被执行 ⇒ 红。
func TestRTChild_StageCanDenyTool(t *testing.T) {
	rec := &stageRecorder{denyTool: "rtstage_deny"}
	p := &rtToolCallProvider{toolName: "rtstage_deny"}

	pio := agentIO.NewIOManager()
	host := newStageTestHost(t, "rtdeny-host", pio, p)
	rec.register(host)
	called := registerStageTool(host, "rtstage_deny")
	registerPairChannels(t, pio, "dout", "din")

	id, err := host.SpawnRTChild(RTChildOptions{ID: "dn-rt", Input: "din", Output: "dout"})
	if err != nil {
		t.Fatalf("SpawnRTChild: %v", err)
	}
	defer host.StopRTChildren()

	if _, err := host.PushRTInput(id, "调用工具"); err != nil {
		t.Fatalf("PushRTInput: %v", err)
	}
	waitForWithin(t, "插件拒绝被记录", 10*time.Second, func() bool { return len(rec.snapBefore()) > 0 })
	// 给它时间真的去执行（若拒绝失效才会执行）。
	time.Sleep(200 * time.Millisecond)
	if called() {
		t.Fatal("插件在 before_toolcall 里拒绝了该工具，但它**仍然被执行了** " +
			"—— 说明 rt 路径绕过了钩子")
	}
}

// TestWorkerToolsGoThroughPluginStages 钉住普通子（工作式子）同样经过钩子。
//
// 变红条件：runChildTask 改回直调 executeToolCall ⇒ 钩子不触发 ⇒ 红。
func TestWorkerToolsGoThroughPluginStages(t *testing.T) {
	rec := &stageRecorder{}
	p := &rtToolCallProvider{toolName: "wkstage_probe"}

	pio := agentIO.NewIOManager()
	host := newStageTestHost(t, "wkstage-host", pio, p)
	rec.register(host)
	registerStageTool(host, "wkstage_probe")

	host.childMu.Lock()
	host.childTasks = map[string]*childTaskState{}
	host.childMu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		host.runChildTask("wk_1", "执行任务", "cli", 2)
	}()
	waitForWithin(t, "工作式子的工具钩子被触发", 15*time.Second, func() bool { return len(rec.snapBefore()) > 0 })
	<-done

	if got := rec.snapBefore(); !hasStageHit(got, "wkstage_probe") {
		t.Fatalf("before_toolcall 未收到普通子的工具调用：%v", got)
	}
	if got := rec.snapAfter(); !hasStageHit(got, "wkstage_probe") {
		t.Fatalf("after_toolcall 未收到普通子的工具调用：%v", got)
	}
}

// TestWorkerToolsStaySerial 钉住「工作式子仍是串行」。
//
// 改成并发会改变它已上线行为的时序假设（多步任务依赖前一步结果）。
// 变红条件：去掉 batchOpts.NoBatch ⇒ 并发执行 ⇒ 红。
func TestWorkerToolsStaySerial(t *testing.T) {
	var mu sync.Mutex
	active, maxActive := 0, 0
	probe := func() {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
	}
	_ = probe // 由 registerStageTool 的并发探针占位（见下）

	// 用两个工具在同一条 assistant 消息里（模型一次吐两个 tool_call），
	// 若并发则 maxActive == 2，串行则 == 1。
	multi := &multiToolProvider{toolNames: []string{"wkser_a", "wkser_b"}}
	pio := agentIO.NewIOManager()
	host := newStageTestHost(t, "wkser-host", pio, multi)
	registerConcurrencyProbe(host, "wkser_a", &mu, &active, &maxActive)
	registerConcurrencyProbe(host, "wkser_b", &mu, &active, &maxActive)

	host.childMu.Lock()
	host.childTasks = map[string]*childTaskState{}
	host.childMu.Unlock()

	done := make(chan struct{})
	go func() { defer close(done); host.runChildTask("wk_s", "并行试试", "cli", 2) }()
	waitForWithin(t, "两个工具都跑过", 15*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return active == 0 && maxActive > 0
	})
	<-done

	mu.Lock()
	got := maxActive
	mu.Unlock()
	if got > 1 {
		t.Fatalf("工作式子同时执行了 %d 个工具 —— 它必须保持串行"+
			"（多步任务依赖前一步结果，并发会打乱）", got)
	}
}

// ── 测试脚手架 ──

// hasStageHit 是本文件用的"列表里有没有这个名字"。
//
// ★ 不叫 contains：context_test.go:228 已有 `contains(s, substr string) bool`，
// 同名不同签名在同一个包里是编译错误。
func hasStageHit(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// newStageTestHost 造一个带 stageHost 的 host（stage 钩子必须有地方注册）。
func newStageTestHost(t *testing.T, id string, pio *agentIO.IOManager, p agentAPI.Provider) *Agent {
	t.Helper()
	// ★ stageHost 不会由 New() 自动创建：它来自 AgentConfig.StageHost
	// （agent.go:582）。判据必须自己造一个，否则钩子根本没地方注册 ——
	// 这也是"插件钩子只挂在装配期"这个事实的体现。
	sh := NewStageHost()
	a := New(AgentConfig{ID: types.AgentID(id), IO: pio, Provider: p, StageHost: sh})
	if a.stageHost == nil {
		t.Fatalf("StageHost 未接上 —— 无法测插件钩子")
	}
	return a
}

func registerPairChannels(t *testing.T, pio *agentIO.IOManager, out, in string) {
	t.Helper()
	if err := pio.RegisterDevice(&rtRecordingDevice{name: out}); err != nil {
		t.Fatalf("注册输出设备: %v", err)
	}
	for _, n := range []string{out, in} {
		if err := pio.ChannelRegistry().Register(agentIO.InputChannel{
			Name: n, Plugin: "stagetest",
			Capabilities: agentIO.CapText | agentIO.CapRT,
		}); err != nil {
			t.Fatalf("注册通道 %s: %v", n, err)
		}
	}
}

// registerStageTool 注册一个探针工具，返回"是否真的被执行过"的查询函数。
func registerStageTool(a *Agent, name string) func() bool {
	var called bool
	var mu sync.Mutex
	_ = a.stageHost.RegisterTool(name, sdk.ToolDef{
		Name:        name,
		Description: "stage probe tool",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) {
		mu.Lock()
		called = true
		mu.Unlock()
		return "probe-ok", nil
	})
	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		return called
	}
}

// registerConcurrencyProbe 注册一个记录并发峰值的工具。
func registerConcurrencyProbe(a *Agent, name string, mu *sync.Mutex, active, maxActive *int) {
	_ = a.stageHost.RegisterTool(name, sdk.ToolDef{
		Name:        name,
		Description: "concurrency probe",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) {
		mu.Lock()
		*active++
		if *active > *maxActive {
			*maxActive = *active
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		*active--
		mu.Unlock()
		return "ok", nil
	})
}

// multiToolProvider 第一轮一次吐多个 tool_call，第二轮收尾。
type multiToolProvider struct {
	toolNames []string
	mu        sync.Mutex
	calls     int
}

func (p *multiToolProvider) Name() string          { return "multi-tool" }
func (p *multiToolProvider) MaxContextTokens() int { return 8192 }
func (p *multiToolProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n == 1 {
		calls := make([]agentAPI.ToolCall, 0, len(p.toolNames))
		for i, name := range p.toolNames {
			calls = append(calls, agentAPI.ToolCall{
				ID: "t" + string(rune('a'+i)), Name: name,
				Arguments: map[string]interface{}{},
			})
		}
		return &agentAPI.CompletionResponse{ToolCalls: calls}, nil
	}
	return &agentAPI.CompletionResponse{Content: "done"}, nil
}
func (p *multiToolProvider) ChatStream(_ context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	ch := make(chan agentAPI.StreamChunk, len(p.toolNames)+2)
	if n == 1 {
		for i, name := range p.toolNames {
			ch <- agentAPI.StreamChunk{ToolCall: &agentAPI.ToolCall{
				ID: "t" + string(rune('a'+i)), Name: name,
				Arguments: map[string]interface{}{},
			}}
		}
		ch <- agentAPI.StreamChunk{Done: true}
	} else {
		ch <- agentAPI.StreamChunk{Content: "done"}
		ch <- agentAPI.StreamChunk{Done: true}
	}
	close(ch)
	return ch, nil
}
