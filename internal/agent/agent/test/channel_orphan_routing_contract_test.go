package test

// 悬空通道的**路由**判据：悬空期间输入必须写进 stash，不能落给父。
//
// ## 为什么这条必须单独存在（与 io 层那 3 条互补）
//
// io 层的判据只能验 `ChannelRegistry` 的标记与 stash 机制。
// 真正决定"输入往哪去"的是 **core 的路由器**（`routeInputByOwner`）——
// 它原先对 `Owner == ""` 直接 `return false`，于是：
//
//	父：我释放了这条通道
//	事实：所有消息都在我自己处理，日志里一个字都没有
//
// 同一个状态、两种相反的预期。这条判据钉的是那个 `return false`。

import (
	"github.com/JianFeeeee/HologramAgent/internal/security"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// orphanHarness 造一个带通道登记表与路由器的 agent。
func orphanHarness(t *testing.T) (*agentIO.IOManager, *agentIO.ChannelRegistry) {
	t.Helper()
	iom := agentIO.NewIOManager()
	reg := iom.ChannelRegistry() // Agent.New 会把路由器挂到这份登记表上
	if err := reg.Register(agentIO.InputChannel{Name: "qq", Plugin: "qq"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a, instErr := agent.Instantiate(agent.TypeNameMain, agent.AgentConfig{Security: security.NewDisabledRuntime().AsGate(), ID: "root", IO: iom})
	if instErr != nil {
		t.Fatalf("Instantiate(main): %v", instErr)
	}
	t.Cleanup(a.Stop)
	return iom, reg
}

// TestOrphanedChannelStashesInputInsteadOfFallingBackToParent：
// 悬空通道的输入被暂存，**不进**父的队列。
func TestOrphanedChannelStashesInputInsteadOfFallingBackToParent(t *testing.T) {
	iom, reg := orphanHarness(t)
	if err := reg.Orphan("qq"); err != nil {
		t.Fatalf("Orphan: %v", err)
	}

	iom.InjectInputTo("qq", "qq", "text", map[string]interface{}{"text": "hi"})

	if got := reg.StashLen("qq"); got != 1 {
		t.Fatalf("★ 悬空通道的输入没有进 stash（StashLen=%d）—— 它落给父了", got)
	}
}

// TestUnassignedChannelFallsBackToParent：**未分配**（从未划过、也没悬空）
// 的通道仍由父兜底处理 —— 存量行为必须不变。
//
// ★ 这条与上一条是同一个"Owner 为空"的两种语义，必须分开钉：
// 把两者混为一谈就会在修悬空的时候顺手把所有未分配通道也弄坏。
func TestUnassignedChannelFallsBackToParent(t *testing.T) {
	iom, reg := orphanHarness(t)

	iom.InjectInputTo("qq", "qq", "text", map[string]interface{}{"text": "hi"})

	if got := reg.StashLen("qq"); got != 0 {
		t.Fatalf("★ 未分配的通道却把输入暂存了（StashLen=%d）—— 存量行为被改变", got)
	}
}

// TestHandoverFromOrphanReleasesStash：悬空 → 交接给具体 agent ⇒ stash 交出。
func TestHandoverFromOrphanReleasesStash(t *testing.T) {
	_, reg := orphanHarness(t)
	if err := reg.Orphan("qq"); err != nil {
		t.Fatalf("Orphan: %v", err)
	}
	if err := reg.Stash("qq", []*agentIO.InputEvent{{Source: "qq", OutputChannel: "qq"}}); err != nil {
		t.Fatalf("Stash: %v", err)
	}

	drained, err := reg.AssignAndDrainStash("qq", "res_9", 0)
	if err != nil {
		t.Fatalf("AssignAndDrainStash: %v", err)
	}
	if len(drained) != 1 {
		t.Fatalf("★ 交接时没交出暂存任务（%d 条）", len(drained))
	}
	if reg.IsOrphaned("qq") {
		t.Fatal("★ 交接后仍是悬空状态")
	}
}
