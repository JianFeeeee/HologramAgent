package core

// R1 判据：「只有 rtchild 可被划分 rt 通道」（用户裁定 R2）+ 通道侧能力校验。
//
// 设计见 plan.md 十一.三 F / 十一.七 R1。
//
// ## 这两条判据为什么必须成对存在
//
// 校验是**双向**的，任何一侧单独成立都不足以拦住"慢 agent 混进通话链路"：
//   · 只校验 agent 侧 ⇒ 一个没声明 CapRT 的普通通道被划进来，插件会把它
//     当成普通消息收（不知道"新值作废旧值"），rt 语义静默丢失；
//   · 只校验通道侧 ⇒ 主 agent（实测往返 2–5s）能划 rt 通道，用户听到长静默。
//
// 所以下面两条各钉一侧，删掉任一侧的实现，对应判据必须变红。

import (
	"errors"
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// newRTTestAgent 造一个最小可用的 agent（带 io + 共享通道登记表）。
func newRTTestAgent(t *testing.T, realtime bool) *Agent {
	t.Helper()
	io := agentIO.NewIOManager()
	a := New(AgentConfig{ID: "rt-test", IO: io})
	if realtime {
		a.AsRealtime()
	}
	return a
}

// registerRTChannel 往登记表里放一个 inputch（caps 决定它声明了什么能力）。
func registerRTChannel(t *testing.T, a *Agent, name string, caps agentIO.OutputCapability) {
	t.Helper()
	err := a.io.ChannelRegistry().Register(agentIO.InputChannel{
		Name:         name,
		Plugin:       "test",
		Capabilities: caps,
	})
	if err != nil {
		t.Fatalf("注册 inputch %s: %v", name, err)
	}
}

// TestAssignRealtimeRejectsNonRTAgent 钉住 agent 侧：
// 非 rtchild **不得**被划分 rt 通道。
//
// 删掉 AssignRealtime 里的 `if !a.isRealtime` 判断，本判据必红。
func TestAssignRealtimeRejectsNonRTAgent(t *testing.T) {
	a := newRTTestAgent(t, false) // 普通 agent，不是 rtchild
	registerRTChannel(t, a, "voice", agentIO.CapText|agentIO.CapRT)

	err := a.AssignRealtime("voice")
	if err == nil {
		t.Fatalf("非 rtchild 竟然被划到 rt 通道 —— 「只有 rtchild 可被划分 rt 通道」未生效")
	}
	if !errors.Is(err, ErrAgentNotRealtime) {
		t.Fatalf("错误类型不对：%v（应可用 errors.Is 匹配 ErrAgentNotRealtime，"+
			"否则调用方只能靠字符串判断）", err)
	}

	// 配套断言：拒绝后 Owner 必须**未被写入**。
	// 只返回错误但已经把 Owner 改了，同样是坏的（调用方看到报错，通道却已被占）。
	ch, ok := a.io.ChannelRegistry().Lookup("voice")
	if !ok {
		t.Fatal("通道不见了")
	}
	if ch.Owner != "" {
		t.Fatalf("划拨被拒但 Owner 已被写成 %q —— 拒绝路径必须无副作用", ch.Owner)
	}
}

// TestAssignRealtimeRejectsChannelWithoutCapRT 钉住通道侧：
// 没有声明 CapRT 的通道**不得**被划作 rt 通道。
//
// 删掉 AssignRealtime 里的 ch.Capabilities.Supports(CapRT) 判断，本判据必红。
func TestAssignRealtimeRejectsChannelWithoutCapRT(t *testing.T) {
	a := newRTTestAgent(t, true) // 确实是 rtchild
	registerRTChannel(t, a, "plain", agentIO.CapText)

	err := a.AssignRealtime("plain")
	if err == nil {
		t.Fatalf("未声明 CapRT 的通道竟被划成 rt 通道 —— 插件会把它当普通消息收，rt 语义静默丢失")
	}
	if !errors.Is(err, agentIO.ErrChannelNotRealtime) {
		t.Fatalf("错误类型不对：%v（应匹配 ErrChannelNotRealtime）", err)
	}
}

// TestAssignRealtimeAllowsPlainChannel 钉住计划原文的一条边界：
//
//	「标签不是准入门槛：**不带 rt 标签的普通通道同样可以划给 rt agent**」
//
// 即：rtchild 用普通通道是**允许**的 —— 只是那条通道不能承载 rt 流。
// 若有人"顺手"把普通 Assign 也加上 rt 校验，本判据会红。
func TestAssignRealtimeAllowsPlainChannel(t *testing.T) {
	a := newRTTestAgent(t, true)
	registerRTChannel(t, a, "cli", agentIO.CapText)

	// 走**普通** Assign（不是 AssignRealtime）：rtchild 划普通通道必须放行。
	if err := a.io.AssignInputChannel("cli", "rt-test", 0); err != nil {
		t.Fatalf("rtchild 划普通通道被拒 —— 违反「标签不是准入门槛」：%v", err)
	}
	ch, _ := a.io.ChannelRegistry().Lookup("cli")
	if ch.Owner != "rt-test" {
		t.Fatalf("Owner 未写入：%q", ch.Owner)
	}
}

// TestAssignRealtimeSucceedsOnRTChannel 是正例：两个条件都满足时必须成功。
//
// ★ 只有负例判据时，"永远返回错误的实现"也能全绿 —— 正例是必需品。
func TestAssignRealtimeSucceedsOnRTChannel(t *testing.T) {
	a := newRTTestAgent(t, true)
	registerRTChannel(t, a, "voice", agentIO.CapText|agentIO.CapRT)

	if err := a.AssignRealtime("voice"); err != nil {
		t.Fatalf("rtchild 划 rt 通道失败：%v", err)
	}
	ch, _ := a.io.ChannelRegistry().Lookup("voice")
	if ch.Owner != "rt-test" {
		t.Fatalf("Owner = %q，期望 rt-test", ch.Owner)
	}
	// Capacity 必须是 0：rt 通道是**单槽覆盖**，不是"容量=1 的队列"
	// （后者会让新输入等旧输入跑完，正是契约 1 禁止的，见 plan.md 十一.五）。
	if ch.Capacity != 0 {
		t.Fatalf("rt 通道的 Capacity = %d，必须为 0（单槽覆盖，不是队列）", ch.Capacity)
	}
}

// TestAssignRealtimeRejectsUnknownChannel 引用未注册通道必须报错（不静默成功）。
func TestAssignRealtimeRejectsUnknownChannel(t *testing.T) {
	a := newRTTestAgent(t, true)
	if err := a.AssignRealtime("nope"); !errors.Is(err, agentIO.ErrInputChannelUnknown) {
		t.Fatalf("未注册通道应报 ErrInputChannelUnknown，得到：%v", err)
	}
}
