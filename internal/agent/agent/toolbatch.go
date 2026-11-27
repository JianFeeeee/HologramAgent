package agent

// toolbatch.go —— 工具调用的**分层路由**（用户裁定 2026-11-27）。
//
// # 三层，逐层过滤
//
//	① 类型私有   类型自己的 agentloop 执行（`stablemem_*` / `social_*` /
//	             `coldcontext_*` / `persona_set` …）。它们读类型私有状态，
//	             永远不进这里。
//	② 基座       本文件：`registry` 里 Handle 指向**基座方法**的那批
//	             （`admin_child` 族 / 内核族 `llm_*` / `secret_*` / `output_*` /
//	             `plgreload` / `get_plugin_tools` / `input_channels`）。
//	             它们要读写**本实例**字段 ⇒ 只有基座自己能执行。
//	③ device     剩下的全归 device：插件工具 + io/设备工具。
//	             device 是 tool + ch 的抽象，执行链也在它那儿。
//
// # 为什么是"逐层过滤"而不是"一路下派"
//
// 改造前 device 的执行链里做了 `registry.DispatchTool` —— 那是**基座层**的
// 职责，于是 device 必须回调基座的实例方法（`DialTool`）。回调一出现，
// device 就反向依赖 agent，而"谁执行了什么"又变成一处说不清的地方。
//
// 现在每层只碰自己的：基座把能执行的挑走，**剩下的**才交给 device。
// device 不需要认识基座工具，也就不需要回调。

import (
	"context"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// RunToolBatch 执行一批**非类型私有**的工具调用，返回与入参同序的结果。
//
// # 分层与合并
//
// 两层各自只碰自己负责的工具，然后**一起合并**回原位：
//
//	基座层挑走的  → 就地执行，结果放进 out[i]
//	其余的整批    → 交给 device 执行链，结果按 ID 回填 out[i]
//
// ★ 为什么按 ID 回填而不是按下标：device 的并发执行会打乱时间顺序，
//
//	而"同序返回"是调用方的契约（类型的 agentloop 按 idx 对结果）。
func (a *Agent) RunToolBatch(ctx context.Context, calls []ToolCall) []BatchResult {
	if a == nil || len(calls) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = a.ctx
	}

	out := make([]BatchResult, len(calls))

	// ── 分层：基座层负责哪些、device 负责哪些 ──
	restToOut := make([]int, 0, len(calls)) // rest 的下标 → out 的下标
	rest := make([]ToolCall, 0, len(calls))
	for i, c := range calls {
		if a.ownsTool(c.Name) {
			out[i] = a.runOwnTool(ctx, c)
			continue
		}
		restToOut = append(restToOut, i)
		rest = append(rest, c)
	}
	if len(rest) == 0 {
		return out
	}

	// ── device 层：剩下的整批交给执行链 ──
	exec := a.toolExecutor()
	if exec == nil {
		// 装配缺设施 = 响亮失败。**不静默返回空结果**：那会让调用方以为
		// 工具跑了但没输出，是"看起来正常工作"的最坏形态。
		for _, i := range restToOut {
			out[i] = BatchResult{
				CallID: calls[i].ID, Name: calls[i].Name, Denied: true,
				Text: "工具执行设施未装配（kernelapi.FacToolExec 缺失）：本次调用未执行",
			}
		}
		return out
	}

	res := exec.Execute(ctx, a, rest)
	for k, idx := range restToOut {
		if k < len(res) {
			out[idx] = res[k]
		}
	}
	return out
}

// ownsTool 报告某工具是否由**基座层**执行。
//
// 判据是 `registry` 里这条工具有没有 Handle —— 有就是有人登记了执行体。
// 类型私有工具不在这张表里（它们由类型自己执行，不进 RunToolBatch）。
func (a *Agent) ownsTool(name string) bool {
	t, ok := registry.LookupTool(name)
	return ok && t.Handle != nil
}

// runOwnTool 执行一条基座登记的工具（含工具阶段的完整链路）。
//
// 与 device 执行链的阶段顺序一致（on_toolcall → before → 执行 → after），
// 这样插件在不同层看到的工具调用行为是同一个语义。
func (a *Agent) runOwnTool(ctx context.Context, c ToolCall) BatchResult {
	res := BatchResult{CallID: c.ID, Name: c.Name, Tool: a.toolDefOf(c.Name)}

	denied, sctx := a.RunToolStage(sdk.StageOnToolcall, c, nil)
	if denied {
		res.Text = stageDeniedText(sctx, c.Name)
		res.Denied = true
		return res
	}
	if sctx != nil && len(sctx.ToolCalls) > 0 {
		c.Arguments = sctx.ToolCalls[0].Arguments
	}

	denied, bctx := a.RunToolStage(sdk.StageBeforeToolcall, c, nil)
	if denied {
		res.Text = stageDeniedText(bctx, c.Name)
		res.Denied = true
		return res
	}
	if bctx != nil && len(bctx.ToolCalls) > 0 {
		c.Arguments = bctx.ToolCalls[0].Arguments
	}

	text, handled := registry.DispatchTool(registry.ToolCtx{Host: a}, agentAPI.ToolCall{
		ID: c.ID, Name: c.Name, Arguments: c.Arguments,
	})
	if !handled {
		// ownsTool 说归本层、DispatchTool 却说不认识：登记表在两处之间变过
		// （插件热卸载）。响亮报错，不静默当成"工具不存在"。
		res.Text = "工具 " + c.Name + " 的登记项在本次调用中被撤销（插件已卸载？）：本次调用未执行"
		res.Denied = true
		return res
	}
	res.Text = text
	res.Raw = text

	pres := sdk.ToolResult{CallID: c.ID, Name: c.Name, Success: true, Result: text}
	if denied, actx := a.RunToolStage(sdk.StageAfterToolcall, c, &pres); denied {
		res.Text = stageDeniedText(actx, c.Name)
		return res
	} else if actx != nil && len(actx.ToolResults) > 0 {
		if s, ok := actx.ToolResults[0].Result.(string); ok && s != "" {
			res.Text = s
			res.Raw = s
		}
	}
	return res
}

// toolDefOf 取工具声明的完整副本（标记随结果返回给类型的 agentloop）。
func (a *Agent) toolDefOf(name string) sdk.ToolDef {
	if a == nil {
		return sdk.ToolDef{}
	}
	if tr := a.ToolRegistry(); tr != nil {
		if d := tr.ToolDef(name); d != nil {
			return *d
		}
	}
	if t, ok := registry.LookupTool(name); ok {
		return sdk.ToolDef{
			Name: t.Name, Description: t.Description, Plugin: t.Owner,
		}
	}
	return sdk.ToolDef{}
}

// stageDeniedText 取阶段拦断时的正文。
func stageDeniedText(ctx *sdk.StageContext, tool string) string {
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
