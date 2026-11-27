package test

// 软重启（`admin_child(type, action="restart")`）的契约判据（2026-11-26）。
//
// # 为什么单独一条
//
// 软重启此前**没有任何判据**，而它一直是坏的 —— 实测：
//
//	建子 → 成功（r1 能正常干活）
//	软重启 → 「实例 "r1" 没有创建参数快照（无法重建）」
//
// 根因是两个**各自都正确**的步骤串起来的语义错误：
//
//	DestroyChild(id) → 末尾 Forget(id)   ← 创建参数快照被删（这步对）
//	RecreateChild()  → 读记录表          ← 已经不在了（这步也对）
//	⇒ 组合起来永远失败
//
// 所以判据必须钉**组合行为**：建 → 重启 → 重建出来的还在。
// 分别钉"能建"和"能读表"会双双通过，而软重启依然全坏。
//
// # 为什么用真实 `resident` 类型
//
// `Descriptor.Construct` 是私有字段，自造类型要绕过类型注册面 ——
// 那测的就不是生产路径了。类型经各自的 `init()` 注册（匿名导入即生效）。
//
// # 前置条件必须真的成立，不许 skip 掉当作通过
//
// 本判据需要：① 类型已注册 ② root 有 data_dir（内核据此分配工作目录）。
// 缺任一条就直接 t.Fatal —— 悄悄跳过等于"什么都没测"，
// 而我正是在这里先被自己的 Skip 骗过一次。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/resident"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// TestSoftRestartPreservesInstance 判"创建 → 重启"这条链整体成立。
//
// ★ 只看**外部可观测结果**：两个动作都返回成功、且实例仍在。
// 不读记录表、不查源码里有没有 Forget。
func TestSoftRestartPreservesInstance(t *testing.T) {
	const typeName = "resident"
	if _, ok := lookupType(typeName); !ok {
		t.Fatalf("类型 %q 未注册 —— 判据环境坏了，不是被测代码的问题", typeName)
	}

	dir := t.TempDir()
	iom := io.NewIOManager()
	iom.RegisterInputChannel("probe_in", io.ChannelDef{})
	root := agent.New(agent.AgentConfig{
		ID: types.AgentID("sr-root"), TypeName: agent.TypeNameMain, DataDir: dir,
	})
	root.MountSurface(iom, nil, security.NewDisabledRuntime().AsGate(), nil, "", nil)
	t.Cleanup(root.Stop)
	im := root.EnsureInstanceManager()

	// 前置：能创建。这步就失败说明判据环境不对（不是重启的问题）。
	arg := map[string]interface{}{"id": "sr1", "input_chs": "probe_in"}
	if _, err := registry.Dispatch(im, typeName, registry.ActionCreate, arg); err != nil {
		t.Fatalf("前置失败（创建实例）: %v", err)
	}

	// ★ 缺陷的核心：重启必须成功，而不是报"没有快照"。
	msg, err := registry.Dispatch(im, typeName, registry.ActionRestart,
		map[string]interface{}{"id": "sr1"})
	if err != nil {
		t.Fatalf("软重启失败: %v\n"+
			"历史根因：销毁阶段的 Forget 删掉了创建参数快照，"+
			"而重建阶段正要读它 —— 快照必须在销毁之前取", err)
	}
	if msg == "" {
		t.Fatal("软重启返回空消息（调用方无从知道发生了什么）")
	}

	// 重启 ≠ 删掉：实例必须还在。
	found := false
	for _, b := range im.ListChildren("") {
		if b.ID == "sr1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("软重启后实例不见了 —— 那是销毁，不是重启")
	}
}
