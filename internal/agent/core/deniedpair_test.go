package core

// 判据：「被拒绝 / 被跳过」的工具**也必须落 ToolResults**。
//
// ## 守的是什么（2026-11-04 生产故障，根因）
//
// `stepToolBegin` 的顺序是「先记 call，再判能不能执行」：
//
//	f.appendToolCallRecord(tc)               // ← 先落 ToolCalls
//	if a.runStage(sdk.StageBeforeToolcall, …) {
//	    f.Msgs 补一条 role=tool                // ← 模型能看到
//	    return                                 // ← 但没写 ToolResults
//	}
//
// 于是 `ContextEvent{ToolCalls: 2, ToolResults: 1}`，渲染出
// assistant(tool_calls=[c1,c2]) 却只有 c1 的结果 ⇒ 上游硬拒绝：
//
//	An assistant message with 'tool_calls' must be followed by tool messages
//	responding to each 'tool_call_id'. (insufficient tool messages…)
//
// 而且脏数据**持久化进 context.json**，之后每一轮都带着它。
//
// 实测那一次：qq 插件的来源权限策略拒绝了 `qq_call_state`。
//
//	[42] tool_calls=[qq_call_state, output_send__qq]
//	     tool_results=[output_send__qq]        ← call_state 从未落盘
//
// ## 为什么这个判据必须走真实链路
//
// 只测 `appendToolResultRecord` 本身毫无意义 —— 它当然会 append。
// 要钉的是「stepToolBegin 的拒绝分支有没有调用它」。所以这里注册一个
// **真的会拒绝**的 before_toolcall 钩子，跑完整一轮，再检查落进 context 的
// 那条事件。

import (
	"context"
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// deniedProvider 第一轮要求调用一个工具，之后给最终回复。
type deniedProvider struct {
	calls int
	tc    agentAPI.ToolCall
}

func (p *deniedProvider) Name() string { return "denied" }
func (p *deniedProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.calls++
	if p.calls == 1 {
		return &agentAPI.CompletionResponse{ToolCalls: []agentAPI.ToolCall{p.tc}}, nil
	}
	return &agentAPI.CompletionResponse{Content: "done"}, nil
}
func (p *deniedProvider) ChatStream(_ context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return nil, context.Canceled
}
func (p *deniedProvider) MaxContextTokens() int { return 8192 }

// newDeniedAgent 建一个「工具会被 before_toolcall 拒绝」的 agent。
//
// 与被拒绝工具同时提供一个正常工具，用以断言**不能因噎废食**：
// 拒绝分支修好之后，同批的另一个工具仍需正常落 result。
func newDeniedAgent(t *testing.T, tc agentAPI.ToolCall) *Agent {
	t.Helper()
	a := New(AgentConfig{
		ID:              "deniedagent",
		Provider:        &deniedProvider{tc: tc},
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	dev := &mockOutputDevice{
		name: "denydev",
		caps: agentIO.CapText,
		tools: []agentIO.ToolDef{
			{Name: tc.Name, Description: "会被拒绝"},
		},
		toolFn: func(tool string, args map[string]interface{}) (interface{}, error) {
			t.Fatalf("★ 被拒绝的工具不该真的执行（%s）", tool)
			return nil, nil
		},
	}
	if err := a.io.RegisterDevice(dev); err != nil {
		t.Fatalf("注册测试设备: %v", err)
	}
	return a
}

// TestDeniedToolStillRecordsResult 是主判据。
//
// 变异：删掉 stepToolBegin 拒绝分支里的 f.appendToolResultRecord(...) ⇒ 本例红。
func TestDeniedToolStillRecordsResult(t *testing.T) {
	tc := agentAPI.ToolCall{ID: "call_deny_1", Name: "denied_tool", Arguments: map[string]interface{}{}}
	a := newDeniedAgent(t, tc)

	// ★ 真的会拒绝的 before_toolcall 钩子（拒绝 = ctx.Response != nil）。
	a.stageHost.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error {
		reason := "被来源权限策略拦下（非所有者私聊）"
		ctx.Lock()
		ctx.Response = &reason
		ctx.Unlock()
		return nil
	})

	f := a.newTaskFrame("跑一下被拒绝的工具", a.stageCtxFromInput("跑一下", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps = %v, err=%v", out, f.Err)
	}

	// 前置条件：确认这个工具**确实**被记成了一次调用（否则本判据空转）。
	if len(f.ToolCalls) == 0 {
		t.Fatal("★ 没有记录任何 ToolCall —— 前置条件不成立，判据会空转")
	}

	answered := map[string]bool{}
	for _, r := range f.ToolResults {
		answered[r.CallID] = true
	}
	for _, c := range f.ToolCalls {
		if !answered[c.CallID] {
			t.Fatalf("★ call %s (%s) 没有对应的 ToolResults ——\n"+
				"ContextEvent 会以 ToolCalls=%d / ToolResults=%d 落盘，\n"+
				"渲染出 assistant(tool_calls) 却没有它的结果 ⇒ 上游 400。\n"+
				"实测同型故障：qq_call_state 被权限策略拒绝后每一轮都触发。",
				c.CallID, c.Name, len(f.ToolCalls), len(f.ToolResults))
		}
	}

	// 被拒绝的结果必须带上**拒绝原因**（模型看到什么，落盘就该是什么）。
	for _, r := range f.ToolResults {
		if r.CallID == tc.ID && !strings.Contains(r.Output, "权限策略") {
			t.Fatalf("★ 落盘的拒绝原因丢了（Output=%q）——\n"+
				"模型从 f.Msgs 看到的是原因，context 里却是别的，两者必须一致",
				r.Output)
		}
	}
}

// TestUnhealthyPluginSkipStillRecordsResult 钉住第二条同构分支：
// 插件处于崩溃态、工具被跳过时，同样必须落 result。
//
// 变异：删掉该分支的 f.appendToolResultRecord(...) ⇒ 本例红。
func TestUnhealthyPluginSkipStillRecordsResult(t *testing.T) {
	tc := agentAPI.ToolCall{ID: "call_unhealthy_1", Name: "denied_tool", Arguments: map[string]interface{}{}}
	a := newDeniedAgent(t, tc)

	// 把该工具所属的插件标成崩溃态。
	//
	// 用 recordCrash 打到阈值（maxPluginCrashes=3）而不是直接改私有字段 ——
	// 这样判据依赖的是**公开状态机**，字段改名不会让它变成"编译不过"以外的
	// 沉默失效。插件名必须与 stepToolBegin 里 resolveToolPlugin 的结果一致，
	// 否则 isHealthy 永远返回 true，判据会空转。
	// 显式登记工具归属，否则 resolveToolPlugin 会把它归到 "core"
	// （builtinToolOwner 的兜底），而 health tracker 是按插件名记账的。
	const pluginName = "denyplug"
	if err := a.stageHost.RegisterTool(tc.Name, sdk.ToolDef{
		Name: tc.Name, Description: "x", Plugin: pluginName,
	}, func(map[string]interface{}) (interface{}, error) {
		t.Fatalf("★ 被跳过的工具不该执行")
		return nil, nil
	}); err != nil {
		t.Fatalf("登记工具归属: %v", err)
	}
	if got := a.resolveToolPlugin(tc.Name); got != pluginName {
		t.Fatalf("★ resolveToolPlugin=%q，期望 %q —— 前置条件不成立", got, pluginName)
	}
	// ★ 直接置 Unhealthy，不用 recordCrash 打满阈值。
	//
	// 为什么不走 recordCrash：实测发现它标记 Unhealthy 之后，isHealthy **立刻**
	// 判回健康 —— 因为 recordCrash 从不设置 LastReload，而 isHealthy 的冷却
	// 判据是 `time.Since(r.LastReload) > reloadCooldown`，零值的 Since 是巨大的
	// 正数 ⇒ 冷却期"早已过去" ⇒ 熔断自我解除。
	//
	// 这是**另一个独立缺陷**（插件崩溃熔断实际从不生效），不在本次修复范围
	// （本次只修 tool 配对）。这里直达状态，把判据聚焦在被测分支上；
	// 熔断那个缺陷应单独修并单独钉。
	// LastReload 必须设成"刚刚"，否则 isHealthy 的冷却判据
	//（time.Since(LastReload) > reloadCooldown）立刻判为已冷却 ⇒ 自愈。
	a.pluginHealth.mu.Lock()
	a.pluginHealth.records[pluginName] = &pluginHealthRecord{
		Unhealthy:  true,
		LastReload: time.Now(), // 冷却期未过 ⇒ 熔断生效
	}
	a.pluginHealth.mu.Unlock()
	if a.pluginHealth.isHealthy(pluginName) {
		t.Fatalf("★ 置 Unhealthy 后 %s 仍被判健康，判据前置条件不成立", pluginName)
	}

	f := a.newTaskFrame("跑一下", a.stageCtxFromInput("跑一下", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps = %v, err=%v", out, f.Err)
	}
	if len(f.ToolCalls) == 0 {
		t.Fatal("★ 没有记录 ToolCall —— 前置条件不成立")
	}
	answered := map[string]bool{}
	for _, r := range f.ToolResults {
		answered[r.CallID] = true
	}
	for _, c := range f.ToolCalls {
		if !answered[c.CallID] {
			t.Fatalf("★ 被跳过的工具 %s (%s) 没落 ToolResults —— 同型失配，上游 400",
				c.CallID, c.Name)
		}
	}
}
