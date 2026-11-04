package core

// toolbatch.go 是**唯一**的工具批执行实现（骨架级基础设施）。
//
// # 它解决什么（实测缺陷，2026-11-04）
//
// 全仓有**三**条工具执行路径，其中两条绕过了插件的 before/after_toolcall：
//
//	路径             before_toolcall  after_toolcall  并发
//	主 agent          ✅               ✅               ✅  stepToolBatch
//	驻留子            ✅               ✅               ✅  同主 agent（走 runInputTask）
//	rtchild           ❌               ❌               ❌  直调 executeToolCallOutcome
//	工作式子          ❌               ❌               ❌  直调 executeToolCall
//
// 实测依据：`StageBeforeToolcall` / `StageAfterToolcall` 全仓只出现在
// task.go:1022 / 1079 / 1194，三处都在 runOneTool / stepToolAfter 里；
// 而 `executeToolCallOutcome` 内部**没有任何 runStage 调用**。
//
// 后果：rt 与普通子的工具调用对插件**完全不可见** —— 不能审计、不能拒绝、
// 不能改参、不能脱敏。README 承诺的 before_toolcall「审计、拒绝、改参」
// 在这两条路径上不成立。
//
// # 为什么不做成"让它们复用 TaskFrame"
//
// TaskFrame 承载 7 个 Step + 挂起/恢复/中断栈 + 场景解析 + 上下文预算，
// 那是为「可抢占、现场可恢复」设计的。rt 明确不要这些（见 rtchild.go 的
// 743 行注释），工作式子也不要（它是一次性局部循环）。
// 复用 TaskFrame 会让两边都被拖进对方的复杂度里。
//
// ⇒ 抽出**最小公共核**：批并发 + 两个钩子 + 顺序落结果。
// 三种类型都用它，差异通过参数传入。

import (
	"context"
	"fmt"
	"sort"
	"sync"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// batchOpts 是批执行的可变部分。其余全部共享。
type batchOpts struct {
	// Channel 是本次批量要回的通道（进 StageContext.Extra，插件据此判权限）。
	Channel string
	// Scenes 是本轮的场景键（rt / 工作式子传 nil）。
	Scenes []string
	// StageCtx 是基底 StageContext；内部按 idx 拆分（Extra 逐份复制）。
	// nil 时用一份空基底（仍然带 comefrom 戳）。
	StageCtx *sdk.StageContext
	// NoBatch 为 true 时强制串行（工作式子历史上是逐条串行）。
	NoBatch bool
	// ToolFilter 可在执行前改写/拒绝单个工具（工作式子用它禁 output_send__*）。
	//
	// 返回非空字符串表示"拒绝并以此作为结果"，不回落到真实执行。
	ToolFilter func(tc agentAPI.ToolCall) string
}

// batchResult 是单条工具的结果。
type batchResult struct {
	CallID string
	Name   string
	Text   string
	Raw    interface{}
	Denied bool
}

// runToolBatch 执行一批工具，返回与 calls **同序**的结果。
//
// ## 为什么必须同序
//
// assistant(tool_calls) 与其后的 tool 结果消息必须相邻且一一对应。
// 按完成顺序落消息会被上游以 400 拒绝（insufficient tool messages）。
// 所以并发只作用于"执行"，落结果一律按索引顺序。
//
// ## 并发条件
//
// 走与主 agent 相同的判据（batchRunnable）：批内 >1、全部声明 ParallelSafe、
// 不含同通道多次输出。这里额外允许 opts.NoBatch 强制串行 ——
// 工作式子历史上是串行的，改成并发会改变它已上线行为的时序假设，
// 不在本次重构范围内（判据 TestWorkerToolsStaySerial 钉住）。
func (a *Agent) runToolBatch(ctx context.Context, calls []agentAPI.ToolCall, opts batchOpts) []batchResult {
	if len(calls) == 0 {
		return nil
	}
	// 预建每份 StageContext：Extra 必须逐份复制（插件会写它，共享即竞争）。
	ctxs := make([]sdk.StageContext, len(calls))
	for i := range calls {
		// ★ 不复制 StageContext 整体：它内含 mutex（go vet 会报
		// "assignment copies lock value"）。只逐字段取真正需要的那两项，
		// 与 task.go 的 buildToolContexts 同一写法。
		var extra map[string]interface{}
		var noMemory bool
		if opts.StageCtx != nil {
			extra = opts.StageCtx.Extra
			noMemory = opts.StageCtx.NoMemory
		}
		ctxs[i] = sdk.StageContext{
			Extra:    copyExtraMap(extra),
			NoMemory: noMemory,
		}
		if ctxs[i].Extra == nil {
			ctxs[i].Extra = map[string]interface{}{}
		}
		if opts.Channel != "" {
			ctxs[i].Extra["output_channel"] = opts.Channel
		}
		// ★ 工具调用帧必须带来源标识（comefrom）：通道插件的权限门靠它区分
		// "用户请求" 与 "agent 内部委派"。缺了它，子 agent 的工具调用会被按
		// **用户**权限模型判定 ⇒ 合法工作全被拦（comefrom.go 有 2026-10-16 实证）。
		//
		// 子 agent 是内部委派 ⇒ comeFromInternal。主 agent 传 comeFromUser
		// 由调用方在 StageCtx 里已标好（这里只在缺失时补保守值）。
		if _, ok := ctxs[i].Extra["comefrom"]; !ok {
			stampComeFrom(ctxs[i].Extra, comeFromInternal)
		}
	}

	results := make([]batchResult, len(calls))

	runOne := func(i int) {
		tc := calls[i]
		res := batchResult{CallID: tc.ID, Name: tc.Name}

		// ① 类型级过滤（工作式子禁 output_send__* / spawn_child 等）。
		if opts.ToolFilter != nil {
			if deny := opts.ToolFilter(tc); deny != "" {
				res.Text = deny
				res.Denied = true
				results[i] = res
				return
			}
		}

		tctx := &ctxs[i]
		tctx.ToolCalls = []sdk.ToolCall{{
			ID: tc.ID, Name: tc.Name,
			Plugin: a.resolveToolPlugin(tc.Name), Arguments: tc.Arguments,
		}}
		tctx.ToolResults = nil

		// ② before_toolcall —— **这一步就是本文件存在的理由**。
		if a.runStage(sdk.StageBeforeToolcall, tctx) {
			res.Text = denialResultText(tctx, tc.Name)
			res.Denied = true
			results[i] = res
			return
		}
		// 插件可以改参（before_toolcall 的契约之一）。
		tc.Arguments = tctx.ToolCalls[0].Arguments

		// ③ 插件崩溃态：不执行（与主 agent 一致）。
		if pluginName := a.resolveToolPlugin(tc.Name); pluginName != "" {
			if !a.pluginHealth.isHealthy(pluginName) {
				res.Text = pluginNameCrashedText(pluginName)
				res.Denied = true
				results[i] = res
				return
			}
		}

		// ④ 真实执行。
		out := a.executeToolCallOutcome(tc, opts.Channel, opts.Scenes...)
		res.Text = out.Text
		res.Raw = out.Raw
		tctx.ToolResults = []sdk.ToolResult{{
			CallID: tc.ID, Name: tc.Name,
			Success: !isToolError(out.Raw), Result: out.Raw,
		}}

		// ⑤ after_toolcall —— 插件可脱敏/改写结果。
		a.runStage(sdk.StageAfterToolcall, tctx)
		if len(tctx.ToolResults) > 0 {
			switch r := tctx.ToolResults[0].Result.(type) {
			case string:
				res.Text = r
			case nil:
				// 插件清空：保持原样，不覆盖。
			default:
				res.Raw = r
				res.Text = toolErrorText(tc.Name, r)
				if !isToolError(r) {
					res.Text = fmt.Sprintf("%v", r)
				}
			}
		}
		results[i] = res
	}

	// 串行 or 并发。
	if opts.NoBatch || !a.batchParallelSafe(calls) {
		for i := range calls {
			runOne(i)
		}
		return results
	}
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func(idx int) { defer wg.Done(); runOne(idx) }(i)
	}
	wg.Wait()
	return results
}

// batchParallelSafe 判断这批工具能否并发（与主 agent 的 batchRunnable 同判据）。
func (a *Agent) batchParallelSafe(calls []agentAPI.ToolCall) bool {
	if len(calls) <= 1 {
		return false
	}
	chans := map[string]bool{}
	for _, tc := range calls {
		if !a.toolParallelSafe(tc.Name) {
			return false
		}
		if isOutputDeliveryTool(tc.Name) {
			ch := trimOutputPrefix(tc.Name)
			if chans[ch] {
				return false // 同通道多次输出必须保序
			}
			chans[ch] = true
		}
	}
	return true
}

// sortBatchByIndex 是给调用方落消息用的辅助：按原始索引排（防御性）。
//
// 现有实现本来就按索引写 results，这里提供它是因为"按完成顺序落消息"
// 是本仓踩过的坑，留一个显式函数让纪律可见。
func sortBatchByIndex(rs []batchResult, order []string) []batchResult {
	if len(order) == 0 {
		return rs
	}
	idx := map[string]int{}
	for i, id := range order {
		idx[id] = i
	}
	out := append([]batchResult(nil), rs...)
	sort.SliceStable(out, func(i, j int) bool { return idx[out[i].CallID] < idx[out[j].CallID] })
	return out
}

// 以下两个小工具避免在热路径里引 fmt 的多次分配。
func pluginNameCrashedText(p string) string {
	return "插件 " + p + " 处于崩溃状态，已跳过执行，等待自动恢复重载"
}

func trimOutputPrefix(name string) string {
	const p = "output_send__"
	if len(name) > len(p) {
		return name[len(p):]
	}
	return name
}
