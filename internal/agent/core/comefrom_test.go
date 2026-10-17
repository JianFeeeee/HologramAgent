package core

// 工具调用帧的「来源标识」判据（2026-10-16）。
//
// 要证明的核心是：**驻留子的工具调用能被识别为"内部委派"，
// 而不是被当成"那位用户发起的请求"** —— 后者会让通道插件的
// 用户权限模型去拦一份已经准入的工作（现场实证：驻留子零产出）。

import (
	"testing"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	"github.com/JianFeeeee/HomeAgent/pkg/types"
)

// ★★★ 判据：来源标识**必须在帧的起点就写入**（不能只在工具帧上）。
//
// 为什么：插件可能在 on_input 阶段就据此决定是否介入，
// 而那个阶段**早于**工具帧构造。只在工具帧写会漏掉这条路径。
func TestComeFromIsStampedAtFrameOrigin(t *testing.T) {
	root := newBareAgent("main", "")
	ctx := root.stageCtxFromInput("hi", "", "")
	got, ok := ctx.Extra[extraKeyComeFrom]
	if !ok {
		t.Fatal("★ 帧起点未写 comefrom（插件在 on_input 阶段拿不到来源标识）")
	}
	if got != comeFromUser {
		t.Errorf("根 agent 的帧应为 %q，实际 %q", comeFromUser, got)
	}

	// 驻留子：有 parentID ⇒ internal
	child := newBareAgent("worker-1", "main")
	cctx := child.stageCtxFromInput("hi", "", "")
	if got := cctx.Extra[extraKeyComeFrom]; got != comeFromInternal {
		t.Errorf("★ 驻留子的帧应为 %q（它是父的委派体，不是用户请求的边界），实际 %q",
			comeFromInternal, got)
	}
}

// ★★★ **最关键的一条**：工具调用帧必须带标识，且驻留子的是 internal。
//
// 这是本次缺陷的直接回归判据：缺了它，通道插件的 before_toolcall
// 会把驻留子的调用按"非所有者用户"判定 ⇒ 开发工具全被拒。
func TestToolFrameCarriesComeFrom(t *testing.T) {
	// 驻留子（有父）⇒ 工具帧必须是 internal
	child := newBareAgent("worker-1", "main")
	base := child.stageCtxFromInput("do work", "", "")
	f := &TaskFrame{StageCtx: base, PendingTools: []agentAPI.ToolCall{
		{Name: "cmd_run"}, {Name: "files_ls"},
	}}
	f.buildToolContexts()
	if len(f.toolCtxs) != 2 {
		t.Fatalf("工具帧数量不对: %d", len(f.toolCtxs))
	}
	for i, name := range []string{"cmd_run", "files_ls"} {
		got, ok := f.toolCtxs[i].Extra[extraKeyComeFrom]
		if !ok {
			t.Fatalf("★ 工具 %s 的帧没有 comefrom（缺陷回归）", name)
		}
		if got != comeFromInternal {
			t.Errorf("★ 驻留子的工具 %s 应为 %q，实际 %q —— "+
				"会被通道插件按用户权限模型拦住", name, comeFromInternal, got)
		}
	}

	// 根 agent ⇒ 工具帧必须是 user（不能因为加了这个就放宽用户权限）
	root := newBareAgent("main", "")
	rbase := root.stageCtxFromInput("hi", "", "")
	rf := &TaskFrame{StageCtx: rbase, PendingTools: []agentAPI.ToolCall{{Name: "cmd_run"}}}
	rf.buildToolContexts()
	if got := rf.toolCtxs[0].Extra[extraKeyComeFrom]; got != comeFromUser {
		t.Errorf("★ 根 agent 的工具帧应为 %q（否则会放宽用户权限门），实际 %q",
			comeFromUser, got)
	}
}

// ★★★ 判据：即使 base 帧**没有**标识，工具帧也必须带上（不变量兜底）。
//
// 为什么这条必须独立存在（本会话第 9 次同类教训）：
// 初版判据只验证"走 stageCtxFromInput 的正常路径"，而 copyExtraMap
// 已经把 comefrom 复制进工具帧 —— 于是**删掉工具帧里那行 stampComeFrom
// 判据依然全绿**（变异不咬）。那说明判据测的是"继承"，没测"兜底"。
//
// 而兜底是有意义的：TaskFrame 可以被手搓（新内部路径、测试替身），
// 那种帧的 StageCtx 不经 stageCtxFromInput。没有兜底就会出现
// "某些路径的工具帧没有来源标识" —— 而缺标识时插件按旧语义（用户）
// 判定 ⇒ 驻留子又被拦。不变量必须由**工具帧自己**保证。
func TestToolFrameStampsComeFromEvenWhenBaseLacksIt(t *testing.T) {
	// 手搓一个**没有** comefrom 的 base（模拟不经 stageCtxFromInput 的路径）
	base := &sdk.StageContext{Extra: map[string]interface{}{}, Phase: sdk.StageOnInput}
	if _, ok := base.Extra[extraKeyComeFrom]; ok {
		t.Fatal("前置条件错：这个 base 不该有标识")
	}
	f := &TaskFrame{StageCtx: base, PendingTools: []agentAPI.ToolCall{{Name: "cmd_run"}}}
	f.buildToolContexts()
	got, ok := f.toolCtxs[0].Extra[extraKeyComeFrom]
	if !ok {
		t.Fatal("★ 工具帧未兜底写入 comefrom —— base 没标时就完全没标识了")
	}
	// 兜底值必须是**保守**的 user（宁可多查一次，不可静默放宽权限）
	if got != comeFromUser {
		t.Errorf("兜底值应为保守的 %q，实际 %q", comeFromUser, got)
	}
}

// ★★★ 判据：工具帧的 Extra 仍然**逐份独立**（不能因为加键而退化成共享 map）。
//
// 既有纪律（task.go 注释）：Extra 被 stage handler 写入，若多份共享同一
// map，一个工具的身份绑定会串到同批其他工具上 —— 那是越权。
func TestToolFramesHaveIndependentExtra(t *testing.T) {
	a := newBareAgent("worker", "main")
	base := a.stageCtxFromInput("x", "", "")
	f := &TaskFrame{StageCtx: base, PendingTools: []agentAPI.ToolCall{{Name: "t1"}, {Name: "t2"}}}
	f.buildToolContexts()
	if len(f.toolCtxs) != 2 {
		t.Fatal("工具帧数量不对")
	}
	f.toolCtxs[0].Extra["probe"] = "only-first"
	if _, leaked := f.toolCtxs[1].Extra["probe"]; leaked {
		t.Fatal("★ 工具帧共享了 Extra（一个工具的改写会串到别的工具上 = 越权风险）")
	}
}

// ★★★ 判据：**不覆盖**已有标识（更内层知道更准）。
func TestStampComeFromDoesNotOverwrite(t *testing.T) {
	m := map[string]interface{}{extraKeyComeFrom: comeFromInternal}
	stampComeFrom(m, comeFromUser)
	if m[extraKeyComeFrom] != comeFromInternal {
		t.Errorf("★ 不该覆盖已有值（内层更准），实际被改成 %v", m[extraKeyComeFrom])
	}
	// nil map 不得 panic
	stampComeFrom(nil, comeFromUser)
}

// ★★★ 判据：缺标识时的语义 = 旧行为（user）。
//
// 已发布插件不认识 comefrom。内核必须保证：**缺失时插件看到的行为
// 与改动前完全一致**，否则这个改动对存量插件是破坏性的。
func TestMissingComeFromKeepsLegacySemantics(t *testing.T) {
	// 一个没有任何标识的帧（模拟旧路径）
	ctx := &sdk.StageContext{Extra: map[string]interface{}{}}
	if _, ok := ctx.Extra[extraKeyComeFrom]; ok {
		t.Fatal("前置条件错：这个帧不该有标识")
	}
	// 内核侧的保守默认就是 comeFromUser —— 插件缺失时按旧语义（用户）处理
	if comeFromUser != "user" {
		t.Errorf("默认值必须是与旧行为等价的 %q，实际 %q", "user", comeFromUser)
	}
}

// ---- 测试脚手架 ----

// newBareAgent 造一个只有身份（id + parentID）的最小 Agent。
//
// 不装配 io/provider：判据只验证"来源标识怎么算、怎么落到帧上"，
// 装重型依赖会让判据变慢且掩盖真正的断言点。
func newBareAgent(id, parent string) *Agent {
	return New(AgentConfig{
		ID:       types.AgentID(id),
		ParentID: parent,
		IO:       agentIO.NewIOManager(),
	})
}
