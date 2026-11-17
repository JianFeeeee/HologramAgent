package test

// 通道**悬空**语义的判据（2026-11-17 新增）。
//
// ## 被修的缺陷
//
// 原先 `ChannelRelease` 与 `ChannelSuspend` 在机制上**完全等价**
// （都是 `Assign(name, "", 0)`，唯一差别是一行日志）。用户指出
// 「释放和挂起语义重复了」—— 核实属实，而且注释声称的差别也不成立：
//
//   · 「挂起不参与自动分配」→ 根本没有挂起标记字段
//   · 「挂起期间输入被暂存」→ stash 只在残余任务处置时写，不是投递路径
//
// 根因是登记表**没有"悬空"这个真实状态**。Owner 清空后输入会
// `return false` ⇒ 落给根 agent 处理，而根以为通道已经放手了 ——
// 同一个状态、两种相反的预期，日志里还什么都看不到。
//
// ## 现在的契约
//
//	Owner != ""                ⇒ 路由给那个 agent
//	Owner == "" && !Orphaned   ⇒ 未分配：根 agent 兜底
//	Owner == "" &&  Orphaned   ⇒ 悬空：输入写进 stash，等显式接回/划拨
//
// 入口只用 `ChannelRegistry` 的导出方法。

import (
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

func newChanRegistry(t *testing.T) *agentIO.ChannelRegistry {
	t.Helper()
	reg := agentIO.NewChannelRegistry()
	if err := reg.Register(agentIO.InputChannel{Name: "qq", Plugin: "qq"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// TestAssignToOwnerClearsOrphan：划给某个 agent 之后不再是悬空状态。
//
// ★ 这条守的是"悬空不是终态"：如果不在这里清标记，通道会永远悬着，
// 输入永远只进 stash，谁都收不到 —— 那比原先的"静默回落"更难查
// （至少回落还能让父看见消息）。
func TestAssignToOwnerClearsOrphan(t *testing.T) {
	reg := newChanRegistry(t)
	if err := reg.Orphan("qq"); err != nil {
		t.Fatalf("Orphan: %v", err)
	}
	if !reg.IsOrphaned("qq") {
		t.Fatal("★ Orphan 之后没有悬空标记")
	}

	if err := reg.Assign("qq", "res_1", 0); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if reg.IsOrphaned("qq") {
		t.Fatal("★ 划给 res_1 之后仍标记悬空 —— 输入会永远进 stash")
	}
	ch, ok := reg.Lookup("qq")
	if !ok || ch.Owner != "res_1" {
		t.Fatalf("Owner = %q，期望 res_1", ch.Owner)
	}
}

// TestOrphanKeepsStashAndHandoverDrainsIt：悬空期间暂存的任务，
// 显式交接时一并交出。
func TestOrphanKeepsStashAndHandoverDrainsIt(t *testing.T) {
	reg := newChanRegistry(t)
	if err := reg.Orphan("qq"); err != nil {
		t.Fatalf("Orphan: %v", err)
	}

	evt := &agentIO.InputEvent{Source: "qq", OutputChannel: "qq", RequestID: "r1"}
	if err := reg.Stash("qq", []*agentIO.InputEvent{evt}); err != nil {
		t.Fatalf("Stash: %v", err)
	}
	if got := reg.StashLen("qq"); got != 1 {
		t.Fatalf("StashLen = %d，期望 1", got)
	}

	// 交接给 res_2 ⇒ 暂存应被交出并清空
	drained, err := reg.AssignAndDrainStash("qq", "res_2", 0)
	if err != nil {
		t.Fatalf("AssignAndDrainStash: %v", err)
	}
	if len(drained) != 1 || drained[0] != evt {
		t.Fatalf("交出的暂存任务 = %v，期望那条 evt", drained)
	}
	if got := reg.StashLen("qq"); got != 0 {
		t.Fatalf("★ 交出后 stash 仍有 %d 条（会重复释放）", got)
	}
}

// TestAssignDoesNotDrainForEmptyOwner：把通道"挂起/悬空"（owner 为空）
// **不清空** stash —— 悬空的语义正是"先放着，等有人接"。
func TestAssignDoesNotDrainForEmptyOwner(t *testing.T) {
	reg := newChanRegistry(t)
	evt := &agentIO.InputEvent{Source: "qq", OutputChannel: "qq"}
	if err := reg.Stash("qq", []*agentIO.InputEvent{evt}); err != nil {
		t.Fatalf("Stash: %v", err)
	}
	if _, err := reg.AssignAndDrainStash("qq", "", 0); err != nil {
		t.Fatalf("AssignAndDrainStash: %v", err)
	}
	if got := reg.StashLen("qq"); got != 1 {
		t.Fatalf("★ owner 置空时清掉了 stash（%d 条）—— 悬空期间的任务被丢了", got)
	}
}
