package test

// 阶段分层调度的判据（2026-11-26）。
//
// # 守的是什么
//
// SDK 定义了 7 个 stage，基座把它们映射成 4 个层（`stageLayers` 是唯一映射表）：
//
//	LayerInput    on_input
//	LayerLLM      pre_action / post_action
//	LayerTool     on_toolcall / before_toolcall / after_toolcall
//	LayerOutput   before_output / after_output
//
// 层决定**谁发起**：
//   - LayerTool 只能由内核工具执行链发起（`RunToolStage`），agentloop 调
//     `RunStage` 必须被拒 —— 否则工具阶段会被驱动两次
//   - 其余三层由 agentloop 发起
//
// # 此前的问题
//
// 这段判定只用 `DriverOfStage`（从层推出的派生结论），而它对**未登记的
// stage 返回 "由 agentloop 驱动" ⇒ 拼错 stage 名会被静默当成普通阶段跑掉，
// 钩子永远不被调用，两边都不报错。
//
// 现在 `RunStage` / `RunKernelDrivenStage` 都先判**层**（`StageLayerOf`
// 的两值形式），未登记 ⇒ 明确拒绝并记日志。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// TestStageLayerMappingIsComplete 判 7 个 stage 都有层归属、且归属正确。
//
// ★ 这是"声明"层面的判据：少映射一个，那个阶段的钩子就永远收不到。
func TestStageLayerMappingIsComplete(t *testing.T) {
	want := map[sdk.Stage]agent.StageLayer{
		sdk.StageOnInput:        agent.LayerInput,
		sdk.StagePreAction:      agent.LayerLLM,
		sdk.StagePostAction:     agent.LayerLLM,
		sdk.StageOnToolcall:     agent.LayerTool,
		sdk.StageBeforeToolcall: agent.LayerTool,
		sdk.StageAfterToolcall:  agent.LayerTool,
		sdk.StageBeforeOutput:   agent.LayerOutput,
		sdk.StageAfterOutput:    agent.LayerOutput,
	}
	for stage, layer := range want {
		got, ok := agent.StageLayerOf(stage)
		if !ok {
			t.Fatalf("stage %q 没有层归属 —— 它的钩子永远收不到", stage)
		}
		if got != layer {
			t.Fatalf("stage %q 的层是 %q，期望 %q", stage, got, layer)
		}
	}
}

// TestUnknownStageIsRejectedNotSilentlyRun 判**未登记的 stage 被拒绝**。
//
// ★ 缺陷形态：`DriverOfStage` 对未登记 stage 返回"agentloop 驱动"，
// 于是拼错的名字会被当作正常阶段执行 —— 钩子不触发，而且没有任何报错。
//
// 判据看两件可观测的事：
//   - `StageLayerOf` 的 ok 为 false（声明层面）
//   - `RunStage` 返回 false 且**没有**给 ctx.Phase 赋值（执行层面：
//     没被当成正常阶段跑过）
func TestUnknownStageIsRejectedNotSilentlyRun(t *testing.T) {
	a := agent.New(agent.AgentConfig{ID: "layer-probe"})
	if a == nil {
		t.Fatal("New 返回 nil")
	}
	t.Cleanup(a.Stop)

	const bogus = sdk.Stage("on_toolcalls") // 多一个 s —— 典型拼写错误
	if _, ok := agent.StageLayerOf(bogus); ok {
		t.Fatalf("未登记的 stage %q 居然有层归属", bogus)
	}

	ctx := &sdk.StageContext{}
	if ran := a.RunStage(bogus, ctx); ran {
		t.Fatal("RunStage 对未登记的 stage 返回 true")
	}
	if ctx.Phase != "" {
		t.Fatalf("未登记的 stage 被当成正常阶段执行了（ctx.Phase=%q）—— "+
			"拼错的名字会静默变成「钩子永远不被调用」，而两边都不报错", ctx.Phase)
	}
}

// TestToolLayerCannotBeStartedByAgentLoop 判工具层**不能由 agentloop 发起**。
//
// ★ 这条挡的是"双重触发"：agentloop 跑一遍 + 内核执行链再跑一遍，
//
//	插件的 before_toolcall 会被调两次（工具也可能被执行两次）。
func TestToolLayerCannotBeStartedByAgentLoop(t *testing.T) {
	a := agent.New(agent.AgentConfig{ID: "tool-layer-probe"})
	if a == nil {
		t.Fatal("New 返回 nil")
	}
	t.Cleanup(a.Stop)

	for _, stage := range []sdk.Stage{
		sdk.StageOnToolcall, sdk.StageBeforeToolcall, sdk.StageAfterToolcall,
	} {
		ctx := &sdk.StageContext{}
		if ran := a.RunStage(stage, ctx); ran {
			t.Fatalf("agentloop 发起了工具层阶段 %q（会与内核执行链双重触发）", stage)
		}
		if ctx.Phase != "" {
			t.Fatalf("工具层阶段 %q 被当成 agentloop 阶段执行了", stage)
		}
	}
}

// TestKernelDrivenEntryOnlyAcceptsToolLayer 判内核入口**只接受工具层**。
//
// 反向：非工具层阶段走内核入口 ⇒ 拒绝（走错入口是接线错误，要响亮）。
func TestKernelDrivenEntryOnlyAcceptsToolLayer(t *testing.T) {
	a := agent.New(agent.AgentConfig{ID: "kernel-entry-probe"})
	if a == nil {
		t.Fatal("New 返回 nil")
	}
	t.Cleanup(a.Stop)

	for _, stage := range []sdk.Stage{
		sdk.StageOnInput, sdk.StagePreAction, sdk.StagePostAction,
		sdk.StageBeforeOutput, sdk.StageAfterOutput,
	} {
		ctx := &sdk.StageContext{}
		if ran := a.RunKernelDrivenStage(stage, ctx); ran {
			t.Fatalf("内核入口接受了非工具层阶段 %q", stage)
		}
	}
}
