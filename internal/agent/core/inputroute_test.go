package core

import (
	"path/filepath"
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// 输入路由是**独占**的：inputch 划给子之后，该通道的输入只流向子，父不再收到。
//
// 现场缺陷（用户线上联调实录）：子挂着 inputch=[timer]，timer 的输入却打在父身上
// （日志 `[agent] interrupt from timer/timer`），子的轮次永远是 0 —— 因为
// `Assign` 只把 Owner 写进登记表，注入路径根本没有按归属路由。
func TestResident_InputchRoutingIsExclusive(t *testing.T) {
	parent, _, dir := newRootForResidents(t)
	reg := parent.io.ChannelRegistry()
	if err := reg.Register(agentIO.InputChannel{Name: "sub/in", Plugin: "sub"}); err != nil {
		t.Fatal(err)
	}
	spawnTestResident(t, parent, dir, "r-route", "sub/in")
	child := parent.residents["r-route"].agent

	before := parent.DumpScheduler().Stats.Enqueued
	// 插件往"已划给子"的 inputch 投输入
	parent.io.InjectTextTo("plugin-sub", "sub/in", "去查一下这个")

	waitFor(t, "子处理了划给它的输入", func() bool {
		if child.DumpScheduler().Stats.Executed > 0 {
			return true
		}
		return parent.residents["r-route"].info().TableSize > 0
	})
	if got := parent.DumpScheduler().Stats.Enqueued; got != before {
		t.Fatalf("划给子的 inputch，父不应再入队（before=%d after=%d）", before, got)
	}
	// 轮次必须真的涨：此前 info() 根本没填 Rounds ⇒ 父永远读到 0
	// （现场：子处理表已有 2 条，轮次却显示 0，被误判成"子没干活"）。
	if got := parent.residents["r-route"].info().Rounds; got <= 0 {
		t.Fatalf("子处理的轮次应 > 0，实际 %d", got)
	}
}

// 归属到一个不存在（或已销毁）的 agent 时**不吞输入**：父兜底处理。
// 吞掉一条输入比多处理一条更糟 —— 用户会看到"消息发出去了却没人理"，日志里什么都没有。
func TestResident_InputchRoutingFallsBackWhenOwnerMissing(t *testing.T) {
	parent, _, dir := newRootForResidents(t)
	reg := parent.io.ChannelRegistry()
	if err := reg.Register(agentIO.InputChannel{Name: "ghost/in", Plugin: "ghost"}); err != nil {
		t.Fatal(err)
	}
	// 故意划给一个不存在的 agent id
	if err := reg.Assign("ghost/in", "no-such-agent", 0); err != nil {
		t.Fatal(err)
	}
	_ = dir

	before := parent.DumpScheduler().Stats.Enqueued
	parent.io.InjectTextTo("plugin-ghost", "ghost/in", "兜底测试")
	waitFor(t, "父兜底处理了无人认领的输入", func() bool {
		return parent.DumpScheduler().Stats.Enqueued > before
	})
}

// 未划拨的 inputch（Owner 为空）仍然由父处理 —— 路由不能把默认路径也改掉。
func TestResident_UnassignedInputchStaysWithParent(t *testing.T) {
	parent, _, dir := newRootForResidents(t)
	reg := parent.io.ChannelRegistry()
	if err := reg.Register(agentIO.InputChannel{Name: "own/in", Plugin: "own"}); err != nil {
		t.Fatal(err)
	}
	// 造一个子在跑，确保路由逻辑是"有子存在"的情形
	spawnTestResident(t, parent, dir, "r-other", "own/in")
	_ = filepath.Join(dir, "residents")

	// 把通道退还给父（未分配）
	if err := reg.Assign("own/in", "", 0); err != nil {
		t.Fatal(err)
	}
	before := parent.DumpScheduler().Stats.Enqueued
	parent.io.InjectTextTo("plugin-own", "own/in", "还是我的")
	waitFor(t, "未分配的 inputch 仍由父处理", func() bool {
		return parent.DumpScheduler().Stats.Enqueued > before
	})
}
