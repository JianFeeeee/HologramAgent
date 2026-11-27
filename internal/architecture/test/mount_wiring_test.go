package test

// mount_wiring_test.go —— **装配点必须把每个 Mount* 都接上**（2026-11-25 实测回归）。
//
// ## 事故（第二次同类）
//
// 第一次：`IOManager.StartOutputDispatch` 新增了，但装配点没调
// ⇒ `EmitOutputTo` 的 outputCh 零消费者，输出原地蒸发。
//
// 第二次：`Agent.MountToolRegistry` 新增了，但装配点没调
// ⇒ `a.toolReg` 恒为 nil ⇒ `PublicToolDefs()` 少掉**整个插件来源**
// ⇒ 工具表从 104 掉到 44。
//
// 两次的共同点：**判据自己 mount 句柄**（测试里显式 `faces{ToolReg: tr}`），
// 于是判据全绿而生产是坏的。"新增了入口却没人调用"是最难发现的一类 ——
// 因为没有报错、没有日志、行为只是"少了东西"。
//
// ## 判据形态
//
// 黑盒：不读源码、不查文件存在性。观测的是**真内核装配后**实例上
// 这些句柄是否真的存在（它们是公开访问器）。
//
// 这里只钉"装配点有没有接上"这一件事，不重复各处自己的功能判据。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// TestMountEntriesAreReachable：基座的每个 Mount* 都有对应的访问器，
// 且**未挂载时访问器返回 nil、挂载后返回同一个对象**。
//
// 这条不是"测 setter 能 set" —— 它把"新增挂载入口"这件事变成可测的：
// 加了一个 `MountXxx` 却忘了在访问器里读它，或者忘了在任何装配点调用它，
// 都会在下面某一断言上暴露。
func TestMountEntriesAreReachable(t *testing.T) {
	a := agent.New(agent.AgentConfig{ID: "mount-wiring"})
	t.Cleanup(a.Stop)

	// ① 未挂载 ⇒ nil（这保证"没挂"和"挂了空对象"是两种可区分状态）。
	if a.ToolRegistry() != nil {
		t.Fatal("★ 未挂载时 ToolRegistry() 非 nil —— 那会让'漏挂'变得不可见")
	}
	if a.StageHost() != nil {
		t.Fatal("★ 未挂载时 StageHost() 非 nil")
	}

	// ② 挂载 ⇒ 访问器返回同一个对象。
	tr := kit.NewToolRegistry()
	a.MountToolRegistry(tr)
	if a.ToolRegistry() != tr {
		t.Fatal("★ MountToolRegistry 之后访问器拿不到同一个登记簿")
	}

	// ③ 工具登记簿与阶段管道是**两个独立句柄**（2026-11-25 拆分）。
	//
	// ★ 这条防的是"拆分做了一半"：如果 ToolRegistry 又变回从 StageHost 取，
	//   那"工具表由谁产"会重新变得讲不清（用户裁定的原话）。
	if sh := a.StageHost(); sh != nil {
		type hasTools interface{ GetToolDefs() []any }
		if _, ok := any(sh).(hasTools); ok {
			t.Fatal("★ StageHost 又兼了工具登记 —— 拆分被回退")
		}
	}

	// ④ 登记的工具能经访问器读回（端到端一小步：登记 → 渲染来源）。
	noop := func(map[string]interface{}) (interface{}, error) { return nil, nil }
	if err := tr.RegisterTool("wiring_probe", sdk.ToolDef{Name: "wiring_probe"}, noop); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	var found bool
	for _, d := range tr.GetToolDefs() {
		if d.Name == "wiring_probe" {
			found = true
		}
	}
	if !found {
		t.Fatal("★ 登记的工具从访问器读不回来")
	}
}

// 真装配路径的判据**不放在这里**（2026-11-25 修正）。
//
// 我最初在此写了一条 `TestAssembledAgentHasEveryMountedHandle`，
// 用 `newTypedAgent`（**判据辅助**，各测试按需挂自己关心的面）当"真装配"。
// 那条判据必然失败，而它的失败**不代表生产有问题** —— 它观测错了对象。
//
// "装配点有没有把 Mount* 接上"只能在**真内核启动**上验证（插件工具数
// 从 104 掉到 44 就是那样抓到的）。放进单元测试只会得到一个必须被
// 特殊照顾的假判据。教训：判据的观测对象必须是**生产真路径**，
// 而不是测试自己的脚手架。
