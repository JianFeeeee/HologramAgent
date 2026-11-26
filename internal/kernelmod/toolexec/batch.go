package toolexec

// batch.go —— 批执行的**调度**（并发判据 + 同序落结果）。
//
// ## 同序是硬约束
//
// assistant(tool_calls) 与其后的 tool 结果消息必须相邻且一一对应。
// 按完成顺序落消息会被上游以 400 拒绝（insufficient tool messages）。
// 所以并发只作用于**执行**，落结果一律按索引顺序。
//
// ## 并发判据（与工具声明一致）
//
//	批内 >1 个工具
//	全部声明 ParallelSafe（Serial 胜出：Serial=true ⇒ 不可并发）
//	不含同通道多次输出发送（同 output_send__<通道> 必须保序）
//	opts.NoBatch 未置位
//
// 一条不满足就整批串行 —— 不做"部分并发"：部分并发的收益不抵其不可预测性。

import (
	"context"
	"sync"

	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// runBatch 执行一批工具（并发或串行），返回同序结果。
func runBatch(ctx context.Context, calls []agentPkg.ToolCall, opts agentPkg.BatchOpts) []agentPkg.BatchResult {
	if len(calls) == 0 {
		return nil
	}
	host, ok := opts.Host.(hostFace)
	if !ok || host == nil {
		// 宿主缺失 = 装配错误。**不静默返回空结果**：那会让模型以为工具跑了
		// 但没输出，是 "看起来正常工作" 的最坏形态。
		results := make([]agentPkg.BatchResult, len(calls))
		for i, tc := range calls {
			results[i] = agentPkg.BatchResult{
				CallID: tc.ID, Name: tc.Name,
				Text: "工具执行设施未就绪（宿主未装配）：本次调用未执行",
			}
		}
		return results
	}

	results := make([]agentPkg.BatchResult, len(calls))
	if opts.NoBatch || !batchParallelSafe(calls, host) {
		for i := range calls {
			results[i] = runOne(ctx, host, calls[i], opts)
		}
		return results
	}

	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = runOne(ctx, host, calls[idx], opts)
		}(i)
	}
	wg.Wait()
	return results
}

// runOne 执行单条工具调用（含类型级过滤）。
func runOne(ctx context.Context, host hostFace, tc agentPkg.ToolCall, opts agentPkg.BatchOpts) agentPkg.BatchResult {
	res := agentPkg.BatchResult{CallID: tc.ID, Name: tc.Name}

	// ① 类型级过滤（工作式子用它禁输出工具；主 agent 不用）。
	// ★ 这是**调用方声明的约束**，不是类型的私有工具 —— 私有工具根本不会
	//   走到这里（agentloop 自己执行）。
	if opts.ToolFilter != nil {
		if deny := opts.ToolFilter(tc); deny != "" {
			res.Text = deny
			res.Denied = true
			return res
		}
	}

	// ② 工具声明（标记随结果返回，内核不解释业务含义）。
	res.Tool = toolDefOf(host, tc.Name)

	// ③ on_toolcall 阶段：内核执行链在**执行前**跑这个阶段。
	//
	// 与 before_toolcall 是两层各管一段（不是重复）：
	//
	//	on_toolcall     把「正在执行的工具 + 最终参数」交给注册方**处理**
	//	                （可设 Response 拦下、可改参数）
	//	before_toolcall 执行前的最后一道（历史契约，插件用它做审计/改写）
	//
	// ★ 由内核链发起而不是 agentloop：只有这条链知道自己正握着哪个工具。
	denied, sctx := host.RunToolStage(sdk.StageOnToolcall,
		agentAPI.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}, nil, opts.Channel)
	if denied {
		res.Text = stageDenialText(sctx, tc.Name)
		res.Denied = true
		return res
	}
	// 注册方可以改写参数（与 before_toolcall 同一契约）。
	if sctx != nil && len(sctx.ToolCalls) > 0 {
		tc.Arguments = sctx.ToolCalls[0].Arguments
	}

	// ④ before_toolcall 阶段：执行前的最后一道（可拦、可改参）。
	//
	// 与 on_toolcall 是同一层的**两个阶段**（都归 LayerTool，都由内核链驱动），
	// 差别是契约：on_toolcall 宣告"正在执行什么"，before_toolcall 是执行前
	// 的最后一次介入机会（插件用它做审计/改写/拒绝）。两个都保留是因为
	// 插件依赖 before_toolcall 的既有契约（SDK 文档与 skill 都按它写）。
	if denied, bctx := host.RunToolStage(sdk.StageBeforeToolcall,
		agentAPI.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}, nil, opts.Channel); denied {
		res.Text = stageDenialText(bctx, tc.Name)
		res.Denied = true
		return res
	} else if bctx != nil && len(bctx.ToolCalls) > 0 {
		tc.Arguments = bctx.ToolCalls[0].Arguments
	}

	// ⑤ 真实执行（路由 + 授权 + 超时 + 取消 + 脱敏 + 审查，全在 single.go）。
	out := executeOne(ctx, host, tc, opts.Channel, opts.Scenes)
	res.Text = out.Text
	res.Raw = out.Raw
	res.Denied = out.Denied

	// ⑥ after_toolcall 阶段：注册方可脱敏/改写结果。
	//
	// 结果先按"成功/失败"成形再交给阶段，注册方拿到的是**最终会发给模型
	// 的那份**；它改写 `ToolResults[0].Result` 即改写结果。
	success := !kit.IsToolError(out.Raw)
	pres := sdk.ToolResult{CallID: tc.ID, Name: tc.Name, Success: success, Result: out.Raw}
	if denied, actx := host.RunToolStage(sdk.StageAfterToolcall,
		agentAPI.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}, &pres, opts.Channel); denied {
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

// batchParallelSafe 判这批工具能否并发。
func batchParallelSafe(calls []agentPkg.ToolCall, host hostFace) bool {
	if len(calls) <= 1 {
		return false
	}
	seenOut := map[string]bool{}
	for _, tc := range calls {
		if !host.ToolParallelSafe(tc.Name) {
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

// outputChannelOf 从 output_send__<通道> 里取通道名。
func outputChannelOf(name string) (string, bool) {
	const p = "output_send__"
	if len(name) <= len(p) || name[:len(p)] != p {
		return "", false
	}
	ch := name[len(p):]
	// `<通道>_help` 不是发送（它是查说明），不参与保序判据。
	if len(ch) > 5 && ch[len(ch)-5:] == "_help" {
		return "", false
	}
	return ch, true
}

// toolDefOf 取工具的完整声明（标记的真实来源）。
//
// 三处按序问，都是**登记**而不是名字猜测：
//
//	① registry    内核侧登记的工具（含类型私有族的归属声明）
//	② toolReg     插件工具
//	③ io          设备/通道工具（声明在 Device.Tools() 里）
func toolDefOf(host hostFace, name string) (def sdkToolDef) {
	if host.ToolRegistry() != nil {
		if d := host.ToolRegistry().ToolDef(name); d != nil {
			return *d
		}
	}
	// ★ io 设备工具的声明只有 {Name, Description, Parameters, ParallelSafe, Serial}：
	//   设备的 PathArgs / NetworkArgs / Timeout / NoMemory 在 io.ToolDef 里**没有**
	//   字段（设备是 C 侧实现，那套接口没有这些声明）。所以这里只搬有的字段；
	//   缺的就是"没声明"，内核按保守默认处理（不判路径、不判网络、用默认超时）。
	if ioM := host.IO(); ioM != nil {
		if d, ok := ioM.ToolDefOf(name); ok {
			return sdkToolDef{
				Name: d.Name, Description: d.Description, Parameters: d.Parameters,
				ParallelSafe: d.ParallelSafe, Serial: d.Serial,
			}
		}
	}
	return sdkToolDef{}
}

// stageDenialText 是"阶段注册方拦下了这次工具调用"的结果文案。
//
// 注册方通过 `ctx.Response` 表达拒绝（与 on_input 阶段拦断同一契约）。
// 文案必须把**谁拦的**与**原话**都带上：模型据此决定换工具还是回话，
// 而不是反复重试同一个调用。
func stageDenialText(ctx *sdk.StageContext, tool string) string {
	if ctx == nil || ctx.Response == nil {
		return "Tool " + tool + " was not executed (blocked in a tool stage)."
	}
	return *ctx.Response
}
