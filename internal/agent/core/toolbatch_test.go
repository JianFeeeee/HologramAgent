package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 本文件钉死「同一批多个 tool_call」这条路径的现状行为。
//
// 为何必须先有它（已核实）：仓内此前**没有任何测试直接驱动**
// PendingTools / ToolIdx —— 即批内循环（StepToolBegin → Exec → After →
// StepToolBegin…）**无判据可依**。而阶段 2（并行执行层）要改的正是这段。
// 没有判据就改，等于在无保护的核心路径上动手。
//
// 这些断言全部是**确定性**的：阶段 0 已消除 map 迭代随机性，
// 同一批工具的落序与消息配对可稳定断言。

// batchProvider 依次返回预设响应；ChatStream 不支持流式（驱动回退到 Chat）。
type batchProvider struct {
	mu        sync.Mutex
	responses []*agentAPI.CompletionResponse
	calls     int
}

func (p *batchProvider) Name() string { return "batch" }

func (p *batchProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls-1 < len(p.responses) {
		return p.responses[p.calls-1], nil
	}
	return &agentAPI.CompletionResponse{Content: "done"}, nil
}

func (p *batchProvider) ChatStream(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return nil, context.Canceled
}
func (p *batchProvider) MaxContextTokens() int { return 8192 }

// newBatchAgent 建一个带两枚「记录型」工具的 agent。
// 返回的 *[]string 按调用顺序记录 tool 名，便于断言批内顺序。
func newBatchAgent(t *testing.T, sp agentAPI.Provider) (*Agent, *[]string) {
	t.Helper()
	a := New(AgentConfig{
		ID:              "batchagent",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	var mu sync.Mutex
	var called []string
	// 注意：阶段 2 起组内会**并发**执行，届时 toolFn 可能被多 goroutine 同时调用，
	// 故 mu 必须一直持有（不能为“只在串行时用”而省略）。
	dev := &mockOutputDevice{
		name: "batchdev",
		caps: agentIO.CapText,
		tools: []agentIO.ToolDef{
			{Name: "tool_alpha", Description: "a"},
			{Name: "tool_beta", Description: "b"},
		},
		toolFn: func(tool string, args map[string]interface{}) (interface{}, error) {
			mu.Lock()
			called = append(called, tool)
			mu.Unlock()
			return "ran:" + tool, nil
		},
	}
	if err := a.io.RegisterDevice(dev); err != nil {
		t.Fatalf("注册测试设备失败: %v", err)
	}
	return a, &called
}

// 回归：同一批多个 tool_call 必须**全部**执行，且都进入 ToolResults。
//
// 现状：StepToolBegin 用 f.ToolIdx 遍历 f.PendingTools，逐一执行。
// 批内若有工具被漏掉（如索引推进错误），本判据立即失败。
func TestBatchExecutesEveryToolCall(t *testing.T) {
	tcA := agentAPI.ToolCall{ID: "c1", Name: "tool_alpha", Arguments: map[string]interface{}{}}
	tcB := agentAPI.ToolCall{ID: "c2", Name: "tool_beta", Arguments: map[string]interface{}{}}
	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{Content: "batch text", ToolCalls: []agentAPI.ToolCall{tcA, tcB}},
		{Content: "final"},
	}}
	a, called := newBatchAgent(t, sp)

	resp, toolsUsed, results, err := a.process("go", a.stageCtxFromInput("go", "", ""))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp != "final" {
		t.Errorf("最终响应 = %q，期望 %q", resp, "final")
	}
	if len(toolsUsed) != 2 || toolsUsed[0] != "tool_alpha" || toolsUsed[1] != "tool_beta" {
		t.Errorf("ToolsUsed = %v，期望 [tool_alpha tool_beta]", toolsUsed)
	}
	if len(*called) != 2 {
		t.Fatalf("实际执行 %v，期望两个都执行", *called)
	}
	// 批内顺序必须等于模型给出的顺序（阶段 0 修复的落序问题在批内的体现）。
	if (*called)[0] != "tool_alpha" || (*called)[1] != "tool_beta" {
		t.Errorf("批内执行顺序 = %v，期望 [tool_alpha tool_beta]", *called)
	}
	if len(results) != 2 {
		t.Fatalf("ToolResults 有 %d 条，期望 2", len(results))
	}
	for _, r := range results {
		if r.Output == "" {
			t.Errorf("工具 %s 的结果为空", r.Name)
		}
	}
}

// 回归：批内每个 tool_call_id 都必须有且仅有一条 role=tool 消息配对。
//
// 这是并行化（阶段 2 把落法改成「一个 assistant 带全部 tool_calls + N 条 tool」）
// 的**安全网**：配对一旦断裂，上游会因 tool_call_id 找不到结果而报错。
// 本判据只看配对完整性，不断言消息的物理排列（那正是 2a 要改的部分）。
func TestBatchToolCallIDsAllPaired(t *testing.T) {
	tcA := agentAPI.ToolCall{ID: "c1", Name: "tool_alpha", Arguments: map[string]interface{}{}}
	tcB := agentAPI.ToolCall{ID: "c2", Name: "tool_beta", Arguments: map[string]interface{}{}}
	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{tcA, tcB}},
		{Content: "final"},
	}}
	a, _ := newBatchAgent(t, sp)

	// 直接驱动状态机并保留帧，以便检查 msgs。
	f := a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps = %v, err=%v", out, f.Err)
	}

	// 收集 assistant 声明的 tool_call_id 与 tool 消息回填的 id。
	declared := map[string]int{}
	answered := map[string]int{}
	for _, m := range f.Msgs {
		for _, tc := range m.ToolCalls {
			declared[tc.ID]++
		}
		if m.Role == "tool" {
			answered[m.ToolCallID]++
		}
	}
	for _, id := range []string{"c1", "c2"} {
		if declared[id] != 1 {
			t.Errorf("tool_call_id %q 被声明 %d 次，期望 1 次", id, declared[id])
		}
		if answered[id] != 1 {
			t.Errorf("tool_call_id %q 被回填 %d 次，期望 1 次", id, answered[id])
		}
	}
	// 不得有悬空的 tool 消息。
	for id, n := range answered {
		if declared[id] == 0 {
			t.Errorf("存在无对应 tool_call 声明的 tool 消息: id=%q n=%d", id, n)
		}
	}
}

// 回归：批内 assistant 的文本只应出现**一次**（ContentOnce 语义）。
//
// 现状：f.ContentOnce 保证 f.Resp.Content 只挂在第一个工具的 assistant 消息上，
// 避免同一段文本在批内被重复 N 次、撑爆上下文。
func TestBatchAssistantTextAppearsOnce(t *testing.T) {
	tcA := agentAPI.ToolCall{ID: "c1", Name: "tool_alpha", Arguments: map[string]interface{}{}}
	tcB := agentAPI.ToolCall{ID: "c2", Name: "tool_beta", Arguments: map[string]interface{}{}}
	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{Content: "UNIQUE_BATCH_TEXT", ToolCalls: []agentAPI.ToolCall{tcA, tcB}},
		{Content: "final"},
	}}
	a, _ := newBatchAgent(t, sp)

	f := a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps = %v, err=%v", out, f.Err)
	}
	n := 0
	for _, m := range f.Msgs {
		if m.Role == "assistant" && m.Content == "UNIQUE_BATCH_TEXT" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("批内 assistant 文本出现 %d 次，期望恰好 1 次（ContentOnce 语义）", n)
	}
}

// 回归：批内某个工具**被拒绝**时，批内其余工具仍应继续执行。
//
// 现状：stepToolBegin 的 denied 分支只 f.ToolIdx++ 并 continue，不中断整批。
// 若改成 abort，模型会丢掉本可执行的后续调用。
func TestBatchContinuesAfterDeniedTool(t *testing.T) {
	reason := "策略拒绝"
	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{
			{ID: "c1", Name: "tool_alpha", Arguments: map[string]interface{}{}},
			{ID: "c2", Name: "tool_beta", Arguments: map[string]interface{}{}},
		}},
		{Content: "final"},
	}}
	a, _ := newBatchAgent(t, sp)

	// before_toolcall 只拒绝 tool_alpha；tool_beta 放行。
	a.stageHost.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error {
		if len(ctx.ToolCalls) > 0 && ctx.ToolCalls[0].Name == "tool_alpha" {
			r := reason
			ctx.Response = &r
		}
		return nil
	})

	f := a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps = %v, err=%v", out, f.Err)
	}
	// 批内两个工具都要被「尝试」（拒绝也计入 ToolsUsed）。
	if len(f.ToolsUsed) != 2 {
		t.Errorf("被拒后整批应继续，ToolsUsed = %v 期望两个", f.ToolsUsed)
	}
	// 拒绝理由必须回填给模型，且不阻断 tool_beta 的结果。
	var sawReason, sawBeta bool
	for _, m := range f.Msgs {
		if m.Role == "tool" {
			if m.ToolCallID == "c1" && m.Content == reason {
				sawReason = true
			}
			if m.ToolCallID == "c2" && m.Content != "" {
				sawBeta = true
			}
		}
	}
	if !sawReason {
		t.Errorf("拒绝理由未回填给模型")
	}
	if !sawBeta {
		t.Errorf("tool_beta 的结果未回填（被前一工具的拒绝连带丢弃）")
	}
}

// 阶段 2a：消息落法改为「**一个** assistant 带全部 tool_calls + N 条 tool」。
//
// 现状：每个工具各自 append 一对（assistant[tool_calls=[tc]] + tool），
// 不表达「这是一批」。阶段 2 的批内并发要求消息形态与之对应，且并行下
// 多个 tool message 的相对顺序必须**按 index 确定**，否则模型读到的
// 上下文顺序 ≠ 执行顺序，会诱导出错误的因果推断。
//
// 本判据钉死：新布局下（a）配对仍完整、（b）assistant 只出现一条且带全部
// tool_calls、（c）tool 消息按 index 升序、（d）多模态 user 消息仍紧跟
// 各自的 tool 消息。
func TestBatchLayoutSingleAssistantCarriesAllToolCalls(t *testing.T) {
	tcA := agentAPI.ToolCall{ID: "c1", Name: "tool_alpha", Arguments: map[string]interface{}{}}
	tcB := agentAPI.ToolCall{ID: "c2", Name: "tool_beta", Arguments: map[string]interface{}{}}
	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{Content: "BATCHTEXT", ToolCalls: []agentAPI.ToolCall{tcA, tcB}},
		{Content: "final"},
	}}
	a, _ := newBatchAgent(t, sp)

	f := a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps=%v err=%v", out, f.Err)
	}

	// ① 找带 tool_calls 的 assistant 消息，必须**恰好一条**且带 2 个。
	var assistants []int
	for i, m := range f.Msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			assistants = append(assistants, i)
			if len(m.ToolCalls) != 2 {
				t.Errorf("批内 assistant 应带 2 个 tool_calls，实际 %d", len(m.ToolCalls))
			}
		}
	}
	if len(assistants) != 1 {
		t.Fatalf("带 tool_calls 的 assistant 应恰好 1 条，实际 %d 条（索引 %v）", len(assistants), assistants)
	}

	// ② 该 assistant 之后应紧跟 2 条 tool 消息，且按声明顺序。
	idx := assistants[0]
	var gotIDs []string
	for i := idx + 1; i < len(f.Msgs); i++ {
		if f.Msgs[i].Role == "tool" {
			gotIDs = append(gotIDs, f.Msgs[i].ToolCallID)
		}
	}
	if len(gotIDs) != 2 {
		t.Fatalf("assistant 之后应有 2 条 tool 消息，实际 %d（%v）", len(gotIDs), gotIDs)
	}
	if gotIDs[0] != "c1" || gotIDs[1] != "c2" {
		t.Errorf("tool 消息应按 index 升序，实际 %v", gotIDs)
	}
}

// 阶段 2c：`StageContext` 拆 per-tool。
//
// 现状：f.StageCtx 是**单槽**，批内每个工具都覆写它
// （ToolCalls=[单元素]、ToolResults 覆写、Results[0] 回读）。并行下
// N 个 goroutine 同写一个 ctx = 数据竞争，且 after_toolcall 插件读到的
// 可能是**别的工具**的结果。
//
// 本判据钉死：每个工具的 before/after stage 必须各自看到**自己的**
// ToolCalls[0].Name 与自己的结果，输出通道等 Extra 也要逐份带过去。
func TestBatchEachToolSeesItsOwnStageContext(t *testing.T) {
	tcA := agentAPI.ToolCall{ID: "c1", Name: "tool_alpha", Arguments: map[string]interface{}{}}
	tcB := agentAPI.ToolCall{ID: "c2", Name: "tool_beta", Arguments: map[string]interface{}{}}
	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{tcA, tcB}},
		{Content: "final"},
	}}
	a, _ := newBatchAgent(t, sp)

	var mu sync.Mutex
	beforeSeen := map[string]string{}
	var missingChannel int

	a.stageHost.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error {
		name := ""
		if len(ctx.ToolCalls) > 0 {
			name = ctx.ToolCalls[0].Name
		}
		// 每个工具的 ctx 必须只带它自己（长度恒为 1），否则就是单槽串味。
		if len(ctx.ToolCalls) != 1 {
			t.Errorf("before_toolcall 的 ctx 应只带 1 个 ToolCall，实际 %d", len(ctx.ToolCalls))
		}
		// Extra 里的 output_channel 必须逐份复制过来（stage.go:18 依赖它）。
		if _, ok := ctx.Extra["output_channel"]; !ok {
			mu.Lock()
			missingChannel++
			mu.Unlock()
		}
		mu.Lock()
		beforeSeen[name] = name
		mu.Unlock()
		return nil
	})

	// 复刻生产：prepareInputTask 会往 Extra 写 input_source/output_channel
	//（task.go:380-381）。我的 harness 若不设它，测的就是「Extra 缺失」
	//  这一**自己造的**场景，而不是「per-tool 复制」——先修正 harness。
	ctx := a.stageCtxFromInput("go", "cli", "")
	ctx.Extra["output_channel"] = "cli"
	ctx.Extra["input_source"] = "cli"
	if out := a.runTaskSteps(a.newTaskFrame("go", ctx)); out != outcomeDone {
		t.Fatalf("runTaskSteps 未收敛: %v", out)
	}
	if len(beforeSeen) != 2 {
		t.Errorf("before_toolcall 应被两个工具各触发一次且名字不同，实际 %v", beforeSeen)
	}
	for _, want := range []string{"tool_alpha", "tool_beta"} {
		if beforeSeen[want] != want {
			t.Errorf("工具 %s 的 before_toolcall 未看到自己（看到 %q）", want, beforeSeen[want])
		}
	}
	if missingChannel > 0 {
		t.Errorf("有 %d 个工具的 ctx 缺少 Extra[output_channel]", missingChannel)
	}
}

// 反向断言：after_toolcall 读到的结果必须属于**当前**工具，
// 不能是批内另一个工具的（单槽下极易串味）。
func TestBatchAfterToolcallSeesOwnResult(t *testing.T) {
	tcA := agentAPI.ToolCall{ID: "c1", Name: "tool_alpha", Arguments: map[string]interface{}{}}
	tcB := agentAPI.ToolCall{ID: "c2", Name: "tool_beta", Arguments: map[string]interface{}{}}
	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{tcA, tcB}},
		{Content: "final"},
	}}
	a, _ := newBatchAgent(t, sp)

	var mu sync.Mutex
	bad := map[string]string{}
	a.stageHost.RegisterStage(sdk.StageAfterToolcall, func(ctx *sdk.StageContext) error {
		if len(ctx.ToolResults) == 0 {
			return nil
		}
		name := ctx.ToolResults[0].Name
		res := fmt.Sprint(ctx.ToolResults[0].Result)
		// 结果文案必须含自己的工具名（"ran:tool_alpha"），否则就是串味。
		if !strings.Contains(res, name) {
			mu.Lock()
			bad[name] = res
			mu.Unlock()
		}
		return nil
	})

	if out := a.runTaskSteps(a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))); out != outcomeDone {
		t.Fatalf("runTaskSteps 未收敛: %v", out)
	}
	if len(bad) > 0 {
		t.Errorf("after_toolcall 读到别的工具的结果: %v", bad)
	}
}
