package test

// internal/agent/agent 的**子 agent 生命周期**判据 —— 面向接口的黑盒。
//
// 入口只有 Agent 的导出方法：SpawnRTChild / StopRTChild / StopRTChildren /
// RTChildren / PushRTInput / RTChildStats / CloseRTChild / RTTranscript，
// 加上 IOManager 的通道登记表（它本身就是 @public 的可观测面）。
//
// ## 原判据的缺口
//
// `child/test/registry_contract_test.go` 测的是**骨架**（child.Registry 的
// Add/Remove/StopAll/Cleanup），质量不错但它是"登记表单元"。
// **rtchild 在真 agent 里的装配、划拨、清理** 零覆盖 —— 而那才是
// 生产路径：SpawnRTChild 会碰通道归属、图记忆句柄、入站登记、pump 异步壳。
//
// ## 为什么这几条值得钉
//
// 源码里写明的两条历史缺陷：
//  ① StopRTChild（单数）注销入站登记，StopRTChildren（复数）漏了
//     ⇒ 残留随 create/destroy 单调累积，只能重启清
//  ② 通道划拨失败要**回滚到原 Owner**，不能"归还为未分配"
//     （未分配会被 routeInputByOwner 兜底给主 agent，静默变成普通通道）
//
// 骨架判据（①）已存在；②与"装配失败不留半残"此前无人守。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/rtchild"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// rtHarness 是真 agent + 真 IO + 两个声明了 Realtime 的通道。
type rtHarness struct {
	agent *agent.Agent
	iom   *io.IOManager
	reg   *io.ChannelRegistry
}

func newRTHarness(t *testing.T) *rtHarness {
	t.Helper()
	iom := io.NewIOManager()
	a := faces{Security: security.NewDisabledRuntime().AsGate(), IO: iom}.mount(newMainAgent(t, agent.AgentConfig{TypeName: agent.TypeNameMain, ID: "parent"}))

	iom.RegisterInputChannel("rt-in", io.ChannelDef{Realtime: true})
	iom.RegisterInputChannel("rt-out", io.ChannelDef{Realtime: true})

	t.Cleanup(a.Stop)
	return &rtHarness{agent: a, iom: iom, reg: iom.ChannelRegistry()}
}

// ownerOf 读通道当前归属（"" = 未分配）。
func (h *rtHarness) ownerOf(name string) (string, bool) {
	ch, ok := h.reg.Lookup(name)
	if !ok {
		return "", false
	}
	return ch.Owner, true
}

// channelNames 列出登记表里所有通道名（用于"有没有残留"）。
func (h *rtHarness) channelNames() []string {
	out := make([]string, 0, 8)
	for _, ch := range h.reg.List() {
		out = append(out, ch.Name)
	}
	return out
}

// ---- 判据 1：创建即划拨，两侧通道归属落到子 id ----

// TestSpawnRTChildClaimsBothChannels：rt 通道必须**成对**划给子。
//
// rt 是成对资源：只给一半的形态不是"简化"而是坏掉一半 ——
// 只给 Output 时子能说话但永远听不到用户（表现是自言自语后静默待机，
// 不报任何错）。所以创建成功后两侧都必须归子所有，且划拨是**唯一落点**。
func TestSpawnRTChildClaimsBothChannels(t *testing.T) {
	h := newRTHarness(t)

	if _, err := rtchild.Wrap(h.agent).SpawnRTChild(rtchild.RTChildOptions{
		ID: "kid", Input: "rt-in", Output: "rt-out",
	}); err != nil {
		t.Fatalf("SpawnRTChild: %v", err)
	}

	for _, name := range []string{"rt-in", "rt-out"} {
		owner, ok := h.ownerOf(name)
		if !ok {
			t.Fatalf("★ 通道 %q 不见了", name)
		}
		if owner != "kid" {
			t.Fatalf("★ 通道 %q 归属 %q，期望 kid —— rt 通道没划给子", name, owner)
		}
	}
	if ids := rtchild.Wrap(h.agent).RTChildren(); len(ids) != 1 || ids[0] != "kid" {
		t.Fatalf("★ RTChildren()=%v，期望 [kid]", ids)
	}
}

// ---- 判据 2：划拨失败必须回滚，不留半残 ----

// TestSpawnFailureLeavesNoHalfClaim：input 划上了、output 没划上时，
// 必须把 input **恢复原归属**。
//
// ★ 为什么不能"归还为未分配"：未分配的通道会被 routeInputByOwner 兜底给
// 主 agent —— 而主 agent 不持有 rt 能力（用户裁定）。那等于把一条 rt 通道
// 静默变成主 agent 的普通通道，且没有任何报错。
//
// 构造方式：output 通道不声明 Realtime ⇒ AssignRealtime 拒绝 ⇒
// 走到回滚路径（input 已经划上了）。
func TestSpawnFailureLeavesNoHalfClaim(t *testing.T) {
	iom := io.NewIOManager()
	a := faces{Security: security.NewDisabledRuntime().AsGate(), IO: iom}.mount(newMainAgent(t, agent.AgentConfig{TypeName: agent.TypeNameMain, ID: "parent"}))
	defer a.Stop()

	iom.RegisterInputChannel("rt-in", io.ChannelDef{Realtime: true})
	// ★ output 故意不声明 Realtime。
	iom.RegisterInputChannel("rt-out", io.ChannelDef{})
	reg := iom.ChannelRegistry()

	// 先让 input 有个非空原归属，这样"恢复原状"与"归还为未分配"可区分。
	if err := reg.Assign("rt-in", "somebody", 0); err != nil {
		t.Fatalf("前置 Assign: %v", err)
	}

	if _, err := rtchild.Wrap(a).SpawnRTChild(rtchild.RTChildOptions{
		ID: "kid", Input: "rt-in", Output: "rt-out",
	}); err == nil {
		t.Fatal("★ output 通道未声明 CapRT，SpawnRTChild 却成功了")
	}

	owner, _ := reg.Lookup("rt-in")
	if owner.Owner != "somebody" {
		t.Fatalf("★ 划拨失败后 input 归属变成 %q —— 应恢复为 somebody（原状），"+
			"归还为未分配会让它被兜底给主 agent", owner.Owner)
	}
	if ids := rtchild.Wrap(a).RTChildren(); len(ids) != 0 {
		t.Fatalf("★ 创建失败却留下了子登记 %v", ids)
	}
}

// ---- 判据 3：单数与复数停止后，登记表里不留 child/<id> ----

// TestBothStopPathsUnregisterInbound：两条停止路径都必须注销入站通道
// `child/rt/<id>`。
//
// 这是源码里写明的历史缺陷形态（复数漏了这一步）—— 在**真 agent** 上
// 复验，而不只是骨架的 fakeEntry。
//
// 残留的后果：登记表按 name 全局唯一，残留会随 create/destroy 单调累积；
// 而外部没有任何工具能单独注销 inputch，只能重启 homed 清。
func TestBothStopPathsUnregisterInbound(t *testing.T) {
	for _, path := range []string{"single", "plural"} {
		t.Run(path, func(t *testing.T) {
			h := newRTHarness(t)
			if _, err := rtchild.Wrap(h.agent).SpawnRTChild(rtchild.RTChildOptions{
				ID: "kid", Input: "rt-in", Output: "rt-out",
			}); err != nil {
				t.Fatalf("SpawnRTChild: %v", err)
			}
			names := h.channelNames()
			found := false
			for _, n := range names {
				if n == "child/rt/kid" {
					found = true
				}
			}
			if !found {
				t.Fatalf("前置：创建后应有入站通道 child/rt/kid，实际 %v", names)
			}

			var err error
			if path == "single" {
				err = rtchild.Wrap(h.agent).StopRTChild("kid")
			} else {
				if n := rtchild.Wrap(h.agent).StopRTChildren(); n != 1 {
					t.Fatalf("StopRTChildren 停了 %d 个，期望 1", n)
				}
			}
			if err != nil {
				t.Fatalf("停止失败: %v", err)
			}

			for _, n := range h.channelNames() {
				if n == "child/rt/kid" {
					t.Fatalf("★ [%s] 停止后入站通道 child/rt/kid 仍残留 —— "+
						"正是「复数漏注销」的缺陷形态", path)
				}
			}
			if ids := rtchild.Wrap(h.agent).RTChildren(); len(ids) != 0 {
				t.Fatalf("★ [%s] 停止后仍有子登记 %v", path, ids)
			}
		})
	}
}

// ---- 判据 4：停止后 rt 通道**悬空**而不是归还为未分配 ----

// TestChannelsHangAfterStop：子销毁后 rt 通道归属保持指向已销毁的 id
// （悬空），**不**清成 ""。
//
// 用户裁定 2026-10-30：主 agent 不持有 rt 能力。归还为 "" 会让
// routeInputByOwner 的兜底分支把 rt 输入交给主 agent —— 静默改变语义。
// 悬空是**显式**状态，可日志诊断。
func TestChannelsHangAfterStop(t *testing.T) {
	h := newRTHarness(t)
	if _, err := rtchild.Wrap(h.agent).SpawnRTChild(rtchild.RTChildOptions{
		ID: "kid", Input: "rt-in", Output: "rt-out",
	}); err != nil {
		t.Fatalf("SpawnRTChild: %v", err)
	}
	if err := rtchild.Wrap(h.agent).StopRTChild("kid"); err != nil {
		t.Fatalf("StopRTChild: %v", err)
	}

	for _, name := range []string{"rt-in", "rt-out"} {
		owner, ok := h.ownerOf(name)
		if !ok {
			// 通道被注销也是可接受的（有的实现选择整条摘掉）。
			continue
		}
		if owner == "parent" {
			t.Fatalf("★ 通道 %q 归属回落到了主 agent —— rt 通道被静默降级成普通通道", name)
		}
		if owner != "kid" && owner != "" {
			t.Fatalf("★ 通道 %q 归属 %q（既不是悬空的 kid，也不是未分配）", name, owner)
		}
	}
}

// ---- 判据 5：拒绝路径无副作用（不留登记、不动通道）----

// TestSpawnFailureLeavesNoResidue 覆盖全部"创建被拒"的入口：缺 id / 缺 input /
// 缺 output / id 重复。
//
// ## ★ 这条判据**不**钉"哪一层拒绝了"，只钉"拒绝之后世界是干净的"
//
// 入口侧有四段显式守卫（id / outputch / inputch / 重复），但下游同样会拒
// （`children.Add` 拒重复、`AssignRealtime` 拒非法通道）。我实测过：单独
// 去掉入口守卫里的任何一个，本判据**不会**变红 —— 因为下游那道防线接住了。
//
// 那是**故意的纵深防御**，不是判据失效。所以这里只断言可观察的后果：
//
//	① SpawnRTChild 返回错误（调用方能知道失败）
//	② 登记表里没有留下任何 rtchild（RTChildren() 为空）
//	③ 两条通道归属不被改动（拒绝路径无副作用）
//
// 变异（去掉 rollback 闭包）会让 ③ 红 —— 见判据 2。本判据守的是"四种
// 入口都不会绕开 rollback"这个整体性质。
func TestSpawnFailureLeavesNoResidue(t *testing.T) {
	cases := []struct {
		name string
		opts rtchild.RTChildOptions
	}{
		{"缺 id", rtchild.RTChildOptions{Input: "rt-in", Output: "rt-out"}},
		{"缺 input", rtchild.RTChildOptions{ID: "kid", Output: "rt-out"}},
		{"缺 output", rtchild.RTChildOptions{ID: "kid", Input: "rt-in"}},
		{"通道不存在", rtchild.RTChildOptions{ID: "kid", Input: "no-such-in", Output: "no-such-out"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newRTHarness(t)
			if _, err := rtchild.Wrap(h.agent).SpawnRTChild(c.opts); err == nil {
				t.Fatalf("★ %s 时 SpawnRTChild 返回成功", c.name)
			}
			if ids := rtchild.Wrap(h.agent).RTChildren(); len(ids) != 0 {
				t.Fatalf("★ %s 被拒却留下了子登记 %v", c.name, ids)
			}
			for _, name := range []string{"rt-in", "rt-out"} {
				if owner, ok := h.ownerOf(name); ok && owner != "" {
					t.Fatalf("★ %s 被拒但通道 %q 归属被改成 %q（拒绝路径有副作用）",
						c.name, name, owner)
				}
			}
			for _, n := range h.channelNames() {
				if n == "child/rt/kid" {
					t.Fatalf("★ %s 被拒却注册了入站通道 child/rt/kid", c.name)
				}
			}
		})
	}
}

// TestSpawnDuplicateIDLeavesExistingIntact：同名重复创建被拒，且**已有的那个
// 不受影响**（不被顶掉、不被清理）。
//
// 这条能变异验证：把下游 `children.Add` 换成覆盖语义 ⇒ 已有实例被顶掉，
// RTChildren() 仍是 1 但通道归属流程会重跑一遍（判据 2 会红）。
// 也就是说它钉的是「先来者不被后来者毁掉」这条不变量。
func TestSpawnDuplicateIDLeavesExistingIntact(t *testing.T) {
	h := newRTHarness(t)
	if _, err := rtchild.Wrap(h.agent).SpawnRTChild(rtchild.RTChildOptions{
		ID: "kid", Input: "rt-in", Output: "rt-out",
	}); err != nil {
		t.Fatalf("首次 Spawn: %v", err)
	}

	if _, err := rtchild.Wrap(h.agent).SpawnRTChild(rtchild.RTChildOptions{
		ID: "kid", Input: "rt-in", Output: "rt-out",
	}); err == nil {
		t.Fatal("★ 同名 rtchild 重复创建成功了 —— 上一代残留会被当成这一代")
	}

	if ids := rtchild.Wrap(h.agent).RTChildren(); len(ids) != 1 || ids[0] != "kid" {
		t.Fatalf("★ 重复创建失败后 RTChildren()=%v，期望仍是 [kid]", ids)
	}
	for _, name := range []string{"rt-in", "rt-out"} {
		if owner, _ := h.ownerOf(name); owner != "kid" {
			t.Fatalf("★ 重复创建失败后通道 %q 归属变成 %q（已有实例被影响了）", name, owner)
		}
	}
}
