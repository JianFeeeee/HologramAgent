package device

// toolexec_batch.go —— 一批工具的执行（并发或串行）。
//
// # 参数收敛（用户裁定 2026-11-27）
//
// 改造前调用方要传 `BatchOpts`（7 个字段）。实测执行链只读其中 3 个，而
// 那 3 个里有 2 个是**逃逸**：
//
//	Host        → 已改为装配期注入设施（见 toolexec_host.go）
//	Ctx         → 走函数参数（`opts.Ctx` 一次都没读过）
//	Channel     → 只转递到 `ToolCtx.Channel`，而后者的唯一用途
//	              （spawn_child 授权子）**已随 spawn_child 删除**
//	Scenes      → 只转递到 `ToolCtx.TurnScenes`；场景是**主 agent 私有**
//	              的数据结构（channel→scene 键），内核不该知道
//	NoBatch     → 与工具声明 `ParallelSafe` 重复（worker 的工具本就没标）
//	ToolFilter  → 与 worker 的 render 侧禁令重复（同一份 runnerForbidden）
//	StageCtx    → 零读取
//
// 所以现在只剩：`(ctx, agentID, isChild, calls)`。
//
// # 并发判据
//
// 全部由**工具声明**决定（`ParallelSafe` / `Serial`），不由调用方喊：
//
//	· 全部 ParallelSafe（Serial 优先）
//	· 不含同通道多次输出（同 `output_send__<通道>` 必须保序）
//
// 一条不满足就整批串行 —— 不做"部分并发"：收益不抵其不可预测性。

import (
	"context"
	"sync"

	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// runBatch 执行一批工具（并发或串行），返回同序结果。
func runBatch(ctx context.Context, deps *ExecDeps, h AgentHandle, calls []agentPkg.ToolCall) []agentPkg.BatchResult {
	if len(calls) == 0 {
		return nil
	}

	results := make([]agentPkg.BatchResult, len(calls))
	if !batchParallelSafe(calls, deps) {
		for i := range calls {
			results[i] = runOne(ctx, deps, h, calls[i])
		}
		return results
	}

	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = runOne(ctx, deps, h, calls[idx])
		}(i)
	}
	wg.Wait()
	return results
}

// runOne 执行单条工具调用。
func runOne(ctx context.Context, deps *ExecDeps, h AgentHandle, tc agentPkg.ToolCall) agentPkg.BatchResult {
	res := agentPkg.BatchResult{CallID: tc.ID, Name: tc.Name}

	// ① 工具声明（标记随结果返回，内核不解释业务含义）。
	res.Tool = toolDefOf(deps, tc.Name)

	// ② on_toolcall 阶段：内核执行链在**执行前**跑这个阶段。
	//
	// 与 before_toolcall 是两层各管一段（不是重复）：
	//
	//	on_toolcall     把「正在执行的工具 + 最终参数」交给注册方**处理**
	//	                （可设 Response 拦下、可改参数）
	//	before_toolcall 执行前的最后一道（历史契约，插件用它做审计/改写）
	denied, sctx := runStage(h, sdk.StageOnToolcall, tc, nil)
	if denied {
		res.Text = stageDenialText(sctx, tc.Name)
		res.Denied = true
		return res
	}
	if sctx != nil && len(sctx.ToolCalls) > 0 {
		tc.Arguments = sctx.ToolCalls[0].Arguments
	}

	// ③ before_toolcall 阶段：执行前的最后一道（可拦、可改参）。
	if denied, bctx := runStage(h, sdk.StageBeforeToolcall, tc, nil); denied {
		res.Text = stageDenialText(bctx, tc.Name)
		res.Denied = true
		return res
	} else if bctx != nil && len(bctx.ToolCalls) > 0 {
		tc.Arguments = bctx.ToolCalls[0].Arguments
	}

	// ④ 真实执行（路由 + 授权 + 超时 + 取消 + 脱敏 + 审查，全在 single.go）。
	out := executeOne(ctx, deps, h, tc)
	res.Text = out.Text
	res.Raw = out.Raw
	res.Denied = out.Denied

	// ⑤ after_toolcall 阶段：注册方可脱敏/改写结果。
	success := !kit.IsToolError(out.Raw)
	pres := sdk.ToolResult{CallID: tc.ID, Name: tc.Name, Success: success, Result: out.Raw}
	if denied, actx := runStage(h, sdk.StageAfterToolcall, tc, &pres); denied {
		// after 阶段设 Response = 用它的正文替换结果（不是"拒绝执行"：
		// 工具已经跑过了，副作用已发生）。
		res.Text = stageDenialText(actx, tc.Name)
		return res
	} else if actx != nil && len(actx.ToolResults) > 0 {
		switch r := actx.ToolResults[0].Result.(type) {
		case string:
			res.Text = r
		case nil:
			// 注册方清空：保持原样，不覆盖。
		default:
			res.Raw = r
			res.Text = kit.RenderToolResult(tc.Name, r)
		}
	}
	return res
}

// runStage 跑一个工具阶段（阶段归基座：它要填实例身份、走内核驱动入口）。
func runStage(h AgentHandle, stage sdk.Stage, tc agentPkg.ToolCall, result *sdk.ToolResult) (bool, *sdk.StageContext) {
	if h == nil {
		return false, nil
	}
	return h.RunToolStage(stage, tc, result)
}

// batchParallelSafe 判这批工具能否并发（全部由**工具声明**决定）。
func batchParallelSafe(calls []agentPkg.ToolCall, deps *ExecDeps) bool {
	if len(calls) <= 1 {
		return false
	}
	seenOut := map[string]bool{}
	for _, tc := range calls {
		if !toolParallelSafe(deps, tc.Name) {
			return false
		}
		if ch, isOut := outputChannelOf(tc.Name); isOut {
			if seenOut[ch] {
				return false // 同通道多次输出必须保序
			}
			seenOut[ch] = true
		}
	}
	return true
}

// toolParallelSafe 查工具声明里的并发标记（未登记 ⇒ 保守串行）。
func toolParallelSafe(deps *ExecDeps, name string) bool {
	if deps == nil || deps.Tools == nil {
		return false
	}
	tr := deps.Tools.ToolRegistry()
	if tr == nil {
		return false
	}
	safe, found := tr.ConcurrencySafeOf(name)
	return found && safe
}

// outputChannelOf 从 output_send__<通道> 里取通道名。
func outputChannelOf(name string) (string, bool) {
	const p = "output_send__"
	if len(name) <= len(p) || name[:len(p)] != p {
		return "", false
	}
	return name[len(p):], true
}

// toolDefOf 取工具的完整声明（标记的真实来源）。
func toolDefOf(deps *ExecDeps, name string) sdk.ToolDef {
	if deps == nil || deps.Tools == nil {
		return sdk.ToolDef{}
	}
	tr := deps.Tools.ToolRegistry()
	if tr == nil {
		return sdk.ToolDef{}
	}
	if d := tr.ToolDef(name); d != nil {
		return *d
	}
	return sdk.ToolDef{}
}

// stageDenialText 取阶段拦断时的正文。
func stageDenialText(ctx *sdk.StageContext, tool string) string {
	if ctx == nil {
		return "工具 " + tool + " 被阶段钩子拦下"
	}
	ctx.RLock()
	defer ctx.RUnlock()
	if ctx.Response != nil && *ctx.Response != "" {
		return *ctx.Response
	}
	if len(ctx.Errors) > 0 {
		return "工具 " + tool + " 被阶段钩子拦下：" + ctx.Errors[0]
	}
	return "工具 " + tool + " 被阶段钩子拦下"
}
