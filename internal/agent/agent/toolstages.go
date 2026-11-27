package agent

// toolstages.go —— 工具阶段由**内核执行链**驱动时，阶段怎么跑、结果怎么回。
//
// ## 为什么这个入口在基座（用户裁定 2026-11-26）
//
// 工具执行链在内核（toolroute → security → device → 清洗）。只有那条链
// 知道自己手里握着哪个工具、参数是什么、结果是啥。它调
// `hostFace.RunToolStage` 跑阶段，基座把阶段交给**注册方**：
//
//	类型钩子（OnStage 注册）→ 插件面（StageHost，SDK 的 RegisterStage）
//
// 于是 stage 是**跨多层**的：同一次 on_toolcall，类型的处理与插件的处理
// 都收到，各管自己那一段。
//
// ## 为什么是"跑阶段"而不是"推事件"
//
// stage 是**处理**接口。注册方拿到 ctx 后可以：
//
//	· 设 ctx.Response                 ⇒ 拦下这次工具调用（不执行）
//	· 改 ctx.ToolCalls[0].Arguments   ⇒ 改写参数
//	· 改 ctx.ToolResults[0].Result    ⇒ 改写结果
//
// 一次发射不管结果的"push"（事件总线语义）把这些全丢了。
//
// ## 为什么不由 agentloop 发起
//
// agentloop 只有"一批请求"，没有"正在执行哪一个"的事实。

import (
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// RunToolStage 实现 ToolHost：内核执行链在工具前后跑阶段，拿回处理结果。
//
// 与 `RunStage` 同一套处理语义（类型钩子 → 插件面），差别只在**谁发起**。
//
// tc 是**正在执行的这一个**工具（不是整批）；result 只在 after_toolcall 传，
// before / on 阶段为 nil。
//
// 返回 (是否被拦下, ctx)。调用方据 ctx 的新值继续：`Response` 非空 ⇒ 不执行；
// `ToolCalls[0].Arguments` / `ToolResults[0].Result` 可能已被注册方改写。
func (a *Agent) RunToolStage(stage sdk.Stage, tc agentAPI.ToolCall, result *sdk.ToolResult) (bool, *sdk.StageContext) {
	if a == nil {
		return false, nil
	}
	ctx := &sdk.StageContext{
		Phase: stage,
		// ★ 不再带 `output_channel`（2026-11-27）：通道在工具阶段曾被用来判
		//   "这条消息该不该发到我这"，但那需要调用方传 Channel —— 而内核
		//   **不自动转发回复到输出通道**（核心原则）。工具阶段要看通道就查
		//   自己那批工具的参数，不该由执行链代传。
		Extra: map[string]interface{}{},
		ToolCalls: []sdk.ToolCall{{
			ID:        tc.ID,
			Name:      tc.Name,
			Plugin:    toolPluginOf(tc.Name),
			Arguments: tc.Arguments,
		}},
	}
	if result != nil {
		ctx.ToolResults = []sdk.ToolResult{*result}
	}
	return a.RunKernelDrivenStage(stage, ctx), ctx
}

// toolPluginOf 取工具的归属插件名（阶段里给注册方判"是不是我的工具"）。
func toolPluginOf(name string) string {
	if tr := ToolRegistryOf(); tr != nil {
		return tr.ToolPlugin(name)
	}
	return ""
}
