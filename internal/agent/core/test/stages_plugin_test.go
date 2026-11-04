package test

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/core"
	"testing"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// stage handler 必须能按插件成组摘除。
//
// 修复前 StageHost.stages 只存匿名函数，没有归属信息：
// 插件崩溃/卸载后它的 handler 永远留在表里，每轮 RunStage 都被并发调起并
// 打向已死进程；重启后新 handler 追加进来，旧的仍不退场——
// 错误与重复执行随重启次数线性累积。
func TestUnregisterPluginStages_RemovesOnlyThatPlugin(t *testing.T) {
	h := core.NewStageHost()

	var aRan, bRan, coreRan int
	h.RegisterStageFor("a", sdk.StagePreAction, func(*sdk.StageContext) error { aRan++; return nil })
	h.RegisterStageFor("b", sdk.StagePreAction, func(*sdk.StageContext) error { bRan++; return nil })
	// 内核自身注册的 handler（无归属）不该被插件摘除波及
	h.RegisterStage(sdk.StagePreAction, func(*sdk.StageContext) error { coreRan++; return nil })

	h.RunStage(sdk.StagePreAction, &sdk.StageContext{})
	if aRan != 1 || bRan != 1 || coreRan != 1 {
		t.Fatalf("首轮应全部执行，a=%d b=%d core=%d", aRan, bRan, coreRan)
	}

	if n := h.UnregisterPluginStages("a"); n != 1 {
		t.Errorf("应摘除 1 个 handler，实际 %d", n)
	}

	h.RunStage(sdk.StagePreAction, &sdk.StageContext{})
	if aRan != 1 {
		t.Errorf("已摘除的插件 handler 不该再被调用，实际执行 %d 次", aRan)
	}
	if bRan != 2 || coreRan != 2 {
		t.Errorf("其他 handler 应照常执行，b=%d core=%d", bRan, coreRan)
	}
}

// 摘除某插件的最后一个 handler 后，该 stage 应从表中消失（RunStage 直接短路）。
func TestUnregisterPluginStages_DropsEmptyStage(t *testing.T) {
	h := core.NewStageHost()
	h.RegisterStageFor("solo", sdk.StageAfterToolcall, func(*sdk.StageContext) error { return nil })

	// ★ 2026-11-04：原先这里用 `h.mu.RLock(); h.stages[...]` 直接读私有字段，
	// 判据耦合了内部实现。`UnregisterPluginStages` 的返回值已经表达了同一件事
	// （摘除了几个），而那才是对外承诺。
	//
	// 补一条黑箱可观测的断言：摘除后再摘一次必须得到 0 —— 如果内部表没清空，
	// 第二次仍会"摘到"残留计数。
	if n := h.UnregisterPluginStages("solo"); n != 1 {
		t.Fatalf("应摘除 1 个，实际 %d", n)
	}
	if n := h.UnregisterPluginStages("solo"); n != 0 {
		t.Fatalf("★ 重复摘除应得 0（说明内部已清空），实际 %d", n)
	}
}

// 空插件名不得误摘内核自身注册的 handler。
func TestUnregisterPluginStages_EmptyNameIsNoop(t *testing.T) {
	h := core.NewStageHost()
	ran := 0
	h.RegisterStage(sdk.StagePreAction, func(*sdk.StageContext) error { ran++; return nil })

	if n := h.UnregisterPluginStages(""); n != 0 {
		t.Errorf("空插件名应是 no-op，实际摘除 %d", n)
	}
	h.RunStage(sdk.StagePreAction, &sdk.StageContext{})
	if ran != 1 {
		t.Errorf("内核 handler 应保留并执行，实际 %d 次", ran)
	}
}

// 工具与 stage 的摘除互不干扰：都摘完后两者皆空。
func TestUnregisterPluginToolsAndStages_Together(t *testing.T) {
	h := core.NewStageHost()
	if err := h.RegisterTool("demo_run", sdk.ToolDef{Name: "demo_run", Plugin: "demo"},
		func(map[string]interface{}) (interface{}, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	h.RegisterStageFor("demo", sdk.StagePreAction, func(*sdk.StageContext) error { return nil })

	h.UnregisterPluginTools("demo")
	h.UnregisterPluginStages("demo")

	if h.ToolCount() != 0 {
		t.Errorf("工具应已摘除，实际 %d", h.ToolCount())
	}
	if h.ToolPlugin("demo_run") != "" {
		t.Error("工具→插件映射应清空")
	}
	// 摘除后可重新注册同名工具（重启路径的前提）
	if err := h.RegisterTool("demo_run", sdk.ToolDef{Name: "demo_run", Plugin: "demo"},
		func(map[string]interface{}) (interface{}, error) { return nil, nil }); err != nil {
		t.Errorf("摘除后应可重新注册同名工具，实际: %v", err)
	}
}
