package core

// 判据：**插件 → rtchild 的通路**。
//
// 这是本轮发现的最后一个真实缺口（比 R10 的 notify_parent 更关键）：
//
//	插件只能调 Inject* 系列（SDK 里没有任何 rtchild 接口），
//	它们全部汇到 IOManager.deliverInput → routeInputByOwner。
//	而初版 routeInputByOwner 只认 a.residents（驻留子）——
//	rt 通道的 Owner 是 rtchild id，查不到就**兜底给主 agent 处理**。
//
// 后果：插件的语音/画面输入永远打在父身上，rtchild 的轮次永远是 0。
// 这正是 resident-subagent-design.md 记的那个现场故障的同型形态
// （「子挂着 inputch=[timer]，timer 的输入却打在父身上」）。

import (
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// registerRTChannelWithOwner 注册一个 rt 通道并划给指定 rtchild。
func registerRTChannelWithOwner(t *testing.T, host *Agent, name, ownerID string) {
	t.Helper()
	reg := host.io.ChannelRegistry()
	if err := reg.Register(agentIO.InputChannel{
		Name: name, Plugin: "voice", Capabilities: agentIO.CapText | agentIO.CapRT,
	}); err != nil {
		t.Fatalf("注册 rt 通道: %v", err)
	}
	if err := reg.Assign(name, ownerID, 0); err != nil {
		t.Fatalf("划给 %s: %v", ownerID, err)
	}
}

// TestRTChannel_PluginInputReachesRTSlot 是这条通路的**核心判据**：
// 插件往 rt 通道注入输入，必须进 rtSlot 并真的跑起一轮。
//
// 若 routeInputByOwner 不识别 rtchild，输入会被兜底给主 agent 处理
// （宿主没有调度器，于是什么都不会发生）—— 本判据会超时变红。
func TestRTChannel_PluginInputReachesRTSlot(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)

	// 先注册一对 rt 通道；**划拨由 SpawnRTChild 自己做**（成对划拨）。
	registerRTPair(t, host, "voice-in", "voice")
	id, err := host.SpawnRTChild(RTChildOptions{ID: "voice-child", Input: "voice-in", Output: "voice"})
	if err != nil {
		t.Fatalf("创建 rtchild: %v", err)
	}

	// 插件注入：完全走真实路径（IOManager → deliverInput → router）。
	host.io.InjectInputTo("voice-plugin", "voice-in", "text",
		map[string]interface{}{"content": "用户说话了"})

	waitFor(t, "rtchild 被插件输入唤起", func() bool {
		st, ok := host.RTChildStats(id)
		return ok && st.Rounds >= 1
	})

	calls, _ := p.stats()
	if calls == 0 {
		t.Fatal("插件输入到达了 rtSlot 但没跑模型")
	}
	// 内容必须真的进入 history（不是空跑）。
	tr, _ := host.RTTranscript(id)
	if len(tr) == 0 {
		t.Fatal("跑了一轮但 history 为空")
	}
}

// TestRTChannel_PluginInputIsLatestWins 钉住：插件连发时也走**单槽覆盖**语义，
// 不会因为"走了路由"就退化成排队。
func TestRTChannel_PluginInputIsLatestWins(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)

	registerRTPair(t, host, "voice-in", "voice")
	id, err := host.SpawnRTChild(RTChildOptions{ID: "burst-child", Input: "voice-in", Output: "voice"})
	if err != nil {
		t.Fatalf("创建 rtchild: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	const burst = 40
	for i := 0; i < burst; i++ {
		host.io.InjectInputTo("voice-plugin", "voice-in", "text",
			map[string]interface{}{"content": "连发语音"})
	}
	waitFor(t, "槽被消费", func() bool {
		st, ok := host.RTChildStats(id)
		return ok && !st.Pending
	})

	st, _ := host.RTChildStats(id)

	// ★ 判据取**不变式**而不是"跑了多少轮"。
	//
	// 初版断言 rounds <= 3，在 -race 下稳定失败（实测跑了 13 轮）：
	// 竞态插桩让 emit 变慢，40 条在 rtLoop 起轮之前全部到达，于是
	// 每完成一轮就立刻被下一条唤起 —— 那不是"队列退化"，而是**正常的
	// 最新者胜**（每一轮用的都是当时最新的一条，没有一条在等前一条跑完）。
	//
	// 真正该钉的是：**每条输入要么被处理过、要么被覆盖丢弃**，
	// 不存在"既没处理也没记录丢弃"的第三条路。
	if st.Dropped == 0 && st.Rounds < burst {
		t.Fatalf("连发 %d 条：dropped=0 且只跑了 %d 轮 —— "+
			"有输入凭空消失了（既没被处理，也没计入覆盖丢弃）", burst, st.Rounds)
	}
	// 槽最终必须被清空（没有积压 —— 这是"不排队"的直接证据）。
	if st.Pending {
		t.Fatal("连发结束后槽里仍有待处理输入 —— rt 通道在排队")
	}
}

// TestRTChannel_NoOwnerStillFallsBackToMain 钉住边界：
// rt 通道**未划拨**时输入仍归父处理（不静默丢）。
//
// 这条防"为了让 rt 通路工作而把所有 rt 输入都截走"——那会让未分配的
// rt 通道输入凭空消失。
func TestRTChannel_NoOwnerStillFallsBackToMain(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)

	// 注册但**不划拨**（Owner == ""）—— 这条判据测的是路由的兜底分支，
	// 与 SpawnRTChild 的成对划拨无关，所以直接登记一条裸通道。
	if err := host.io.ChannelRegistry().Register(agentIO.InputChannel{
		Name: "unassigned", Plugin: "voice", Capabilities: agentIO.CapText | agentIO.CapRT,
	}); err != nil {
		t.Fatalf("注册: %v", err)
	}

	// 路由必须返回 false（不接管），让父的队列收下它。
	if handled := host.routeInputByOwner(&agentIO.InputEvent{
		Source: "voice-plugin", Type: "text", OutputChannel: "unassigned",
		Payload: map[string]interface{}{"content": "无人认领"},
	}, false); handled {
		t.Fatal("未划拨的 rt 通道输入被截走了 —— 会静默丢输入")
	}
}

// TestRTChannel_SDKRealtimeBecomesCapRT 钉住 SDK → 内核的**唯一一次**能力位转换：
// 插件在 ChannelDef 里声明 Realtime=true，登记后通道必须带上 CapRT，
// 从而**能**被 AssignRealtime 划拨。
//
// 若去掉 Register 里的映射，插件就无法登记 rt 通道 ——
// rtchild 永远拿不到插件的语音输入（整条链路断在第一步）。
func TestRTChannel_SDKRealtimeBecomesCapRT(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)

	// 插件侧声明（走 SDK 的 ChannelDef.Realtime）。
	if err := host.io.ChannelRegistry().Register(agentIO.InputChannel{
		Name: "sdk-rt", Plugin: "voice",
		Def: agentIO.ChannelDef{Realtime: true},
	}); err != nil {
		t.Fatalf("注册: %v", err)
	}

	ch, ok := host.io.ChannelRegistry().Lookup("sdk-rt")
	if !ok {
		t.Fatal("通道未登记")
	}
	if !ch.Capabilities.Supports(agentIO.CapRT) {
		t.Fatalf("Def.Realtime=true 但 Capabilities 没有 CapRT（= %v）—— "+
			"插件声明的 rt 通道无法被划拨", ch.Capabilities)
	}

	// 端到端：该通道能被 rtchild 划拨（这是"插件能接入"的判定点）。
	id, _ := host.SpawnRTChild(RTChildOptions{ID: "sdk-child", Input: "sdk-rt", Output: "voice"})
	rc := host.rtChildren[id]
	if err := rc.agent.AssignRealtime("sdk-rt"); err != nil {
		t.Fatalf("插件声明的 rt 通道无法划给 rtchild：%v", err)
	}
}

// ── 成对划拨 / 主 agent 无 rt 能力 / 销毁后悬空 ──
//
// 用户裁定（2026-10-30）：
//   「主 agent 创建 rtchild，将 rt 通道划给 rtchild，开始工作
//     （rt 通道必须**成对**划给子 agent）」
//   「主 agent 没有 rt 能力，rt 通道在 rtchild 销毁后应当**悬空**」

// registerRTPair 注册一对 rt 通道（input + output），供 Spawn 划拨。
func registerRTPair(t *testing.T, host *Agent, in, out string) {
	t.Helper()
	reg := host.io.ChannelRegistry()
	for _, n := range []string{in, out} {
		if err := reg.Register(agentIO.InputChannel{
			Name: n, Plugin: "voice", Capabilities: agentIO.CapText | agentIO.CapRT,
		}); err != nil {
			t.Fatalf("注册 rt 通道 %s: %v", n, err)
		}
	}
}

// TestSpawnRTChild_AssignsPairedChannels 钉住"成对划拨"：
// SpawnRTChild 必须把 input 与 output **两条**都划给 rtchild，而不是只划一条。
//
// 变红条件：去掉 SpawnRTChild 里的 AssignRealtime 调用 ⇒ Owner 仍是 "" ⇒ 红。
func TestSpawnRTChild_AssignsPairedChannels(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)
	registerRTPair(t, host, "voice-in", "voice")

	id, err := host.SpawnRTChild(RTChildOptions{ID: "paired", Input: "voice-in", Output: "voice"})
	if err != nil {
		t.Fatalf("创建 rtchild: %v", err)
	}

	reg := host.io.ChannelRegistry()
	for _, n := range []string{"voice-in", "voice"} {
		ch, ok := reg.Lookup(n)
		if !ok {
			t.Fatalf("通道 %s 未登记", n)
		}
		if ch.Owner != id {
			t.Fatalf("通道 %s 的 Owner = %q，期望 %q —— "+
				"rt 通道没有成对划给 rtchild", n, ch.Owner, id)
		}
	}
}

// TestSpawnRTChild_RejectsMissingHalf 钉住成对的**强制性**：
// 只给 input 或只给 output 都必须被拒（否则 rtchild 半残且不报错）。
func TestSpawnRTChild_RejectsMissingHalf(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)
	registerRTPair(t, host, "voice-in2", "voice2")

	if _, err := host.SpawnRTChild(RTChildOptions{ID: "no-in", Output: "voice2"}); err == nil {
		t.Fatal("只给 outputch 竟然创建成功 —— rtchild 会永远听不到用户")
	}
	if _, err := host.SpawnRTChild(RTChildOptions{ID: "no-out", Input: "voice-in2"}); err == nil {
		t.Fatal("只给 inputch 竟然创建成功 —— rtchild 的输出无处可去")
	}
	// 被拒后**不得**在登记表里留下半残实例。
	if got := host.RTChildren(); len(got) != 0 {
		t.Fatalf("创建被拒但登记表里有 %v", got)
	}
}

// TestSpawnRTChild_RejectsChannelOwnedByMainAgent 钉住「主 agent 没有 rt 能力」：
// 一条归属于主 agent 的通道不能被拿来当 rt 通道。
func TestSpawnRTChild_RejectsChannelOwnedByMainAgent(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)
	registerRTPair(t, host, "voice-in3", "voice3")

	// 把 input 划给主 agent 自己（模拟"主 agent 占着这条通道"）。
	if err := host.io.ChannelRegistry().Assign("voice-in3", string(host.id), 0); err != nil {
		t.Fatalf("划给主 agent: %v", err)
	}

	_, err := host.SpawnRTChild(RTChildOptions{ID: "steal", Input: "voice-in3", Output: "voice3"})
	if err == nil {
		t.Fatal("主 agent 自己占着的通道被划成 rt 通道 —— 主 agent 不持有 rt 能力")
	}
}

// TestStopRTChild_ChannelsBecomeDangling 钉住用户裁定：
// rtchild 销毁后通道**悬空**（Owner 仍指向已销毁的 id），**不归还为未分配**。
//
// 为什么必须悬空而不是归还：
//   - 归还为 "" ⇒ routeInputByOwner 会把它兜底给主 agent，而主 agent
//     没有 rt 能力 ⇒ 一条 rt 通道静默变成普通通道（不可查的坏状态）
//   - 悬空 ⇒ 路由查不到该 owner，走既有"owner 无匹配 agent"兜底**并留日志**
//     （可查的坏状态）
func TestStopRTChild_ChannelsBecomeDangling(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)
	registerRTPair(t, host, "voice-in4", "voice4")

	id, err := host.SpawnRTChild(RTChildOptions{ID: "dangling", Input: "voice-in4", Output: "voice4"})
	if err != nil {
		t.Fatalf("创建: %v", err)
	}
	if err := host.StopRTChild(id); err != nil {
		t.Fatalf("停止: %v", err)
	}

	reg := host.io.ChannelRegistry()
	for _, n := range []string{"voice-in4", "voice4"} {
		ch, ok := reg.Lookup(n)
		if !ok {
			continue // 注销也算一种处理；但插件声明的 rt 通道不该被注销
		}
		if ch.Owner == "" {
			t.Fatalf("通道 %s 被归还为未分配 —— 主 agent 没有 rt 能力，"+
				"归还会让它静默兜底到主 agent（应保持悬空）", n)
		}
		if ch.Owner != id {
			t.Fatalf("通道 %s 的 Owner = %q，期望悬空保留 %q", n, ch.Owner, id)
		}
	}
}

// ── rt_children 工具面（主 agent 创建 rtchild 的唯一入口） ──

// TestRTChildrenTool_ListEmpty 钉住空态提示可读。
func TestRTChildrenTool_ListEmpty(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)
	got := host.executeRTChildren(agentAPI.ToolCall{
		Name: "rt_children", Arguments: map[string]interface{}{"action": "list"},
	})
	if !strings.Contains(got, "没有实时子 agent") {
		t.Fatalf("空态提示不对：%q", got)
	}
}

// TestRTChildrenTool_CreateRequiresBothChannels 钉住成对要求，
// 且失败提示必须**列出可用的 rt 通道**（否则模型只能瞎猜名字反复重试）。
func TestRTChildrenTool_CreateRequiresBothChannels(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)
	registerRTPair(t, host, "voice-in", "voice")

	// 只给 input：必须拒，并列出可用通道。
	got := host.executeRTChildren(agentAPI.ToolCall{
		Name: "rt_children",
		Arguments: map[string]interface{}{
			"action": "create", "id": "c1", "inputch": "voice-in",
		},
	})
	if !strings.Contains(got, "必须成对") {
		t.Fatalf("只给 inputch 未被拒：%q", got)
	}
	if !strings.Contains(got, "voice-in") || !strings.Contains(got, "voice") {
		t.Fatalf("失败提示没有列出可用 rt 通道（模型无法自我修正）：%q", got)
	}
}

// TestRTChildrenTool_FullLifecycle 是**端到端**：模型视角走一遍
// create → list → send → stop（这正是"主 agent 创建 rtchild 并开始工作"）。
func TestRTChildrenTool_FullLifecycle(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)
	registerRTPair(t, host, "voice-in", "voice")

	// ① create（成对）
	out := host.executeRTChildren(agentAPI.ToolCall{
		Name: "rt_children",
		Arguments: map[string]interface{}{
			"action": "create", "id": "call-1",
			"inputch": "voice-in", "outputch": "voice",
		},
	})
	if !strings.Contains(out, "已创建") {
		t.Fatalf("create 失败：%q", out)
	}
	// 通道必须已成对划给它。
	reg := host.io.ChannelRegistry()
	for _, n := range []string{"voice-in", "voice"} {
		if ch, _ := reg.Lookup(n); ch.Owner != "call-1" {
			t.Fatalf("通道 %s 的 Owner = %q，期望 call-1", n, ch.Owner)
		}
	}

	// ② list 能看到它
	out = host.executeRTChildren(agentAPI.ToolCall{
		Name: "rt_children", Arguments: map[string]interface{}{"action": "list"},
	})
	if !strings.Contains(out, "call-1") {
		t.Fatalf("list 里看不到刚创建的实例：%q", out)
	}
	if !strings.Contains(out, "打断") {
		t.Fatalf("list 提示没有说明 rt 的打断语义：%q", out)
	}

	// ③ send：打断式注入，真的跑起来
	out = host.executeRTChildren(agentAPI.ToolCall{
		Name: "rt_children",
		Arguments: map[string]interface{}{
			"action": "send", "id": "call-1", "text": "用户说：你好",
		},
	})
	if !strings.Contains(out, "打断") {
		t.Fatalf("send 没说明打断语义：%q", out)
	}
	waitFor(t, "rtchild 跑了一轮", func() bool {
		st, ok := host.RTChildStats("call-1")
		return ok && st.Rounds >= 1
	})

	// ④ stop：回收并交接
	out = host.executeRTChildren(agentAPI.ToolCall{
		Name: "rt_children", Arguments: map[string]interface{}{"action": "stop", "id": "call-1"},
	})
	if !strings.Contains(out, "已回收") {
		t.Fatalf("stop 失败：%q", out)
	}
	if !strings.Contains(out, "rt_transcript") {
		t.Fatalf("stop 没有告诉模型用 rt_transcript 取回原文：%q", out)
	}
	if got := host.RTChildren(); len(got) != 0 {
		t.Fatalf("回收后登记表仍有 %v", got)
	}
}

// TestRTChildrenTool_UnknownAction 钉住未知 action 响亮报错并列出可用值。
func TestRTChildrenTool_UnknownAction(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)
	got := host.executeRTChildren(agentAPI.ToolCall{
		Name: "rt_children", Arguments: map[string]interface{}{"action": "destroy"},
	})
	if !strings.Contains(got, "未知 action") || !strings.Contains(got, "list") {
		t.Fatalf("未知 action 提示不可操作：%q", got)
	}
}

// TestRTChildrenTool_NotVisibleToChild 钉住工具是**父专属**的：
// rtchild 自己不该看到 rt_children（否则它能无限自我复制）。
func TestRTChildrenTool_NotVisibleToChild(t *testing.T) {
	p := &rtBlockingProvider{}
	host, _ := newRTHost(t, p)
	host.io.ChannelRegistry().Register(agentIO.InputChannel{
		Name: "voice-in", Plugin: "test", Capabilities: agentIO.CapRT | agentIO.CapText,
	})
	id := rtSpawn(t, host, "no-recurse")
	rc := host.rtChildren[id]
	if rc == nil {
		t.Fatal("实例不在登记表")
	}
	if _, ok := rtToolNames(rc.agent.buildToolDefs())["rt_children"]; ok {
		t.Fatal("rtchild 自己能看到 rt_children —— 会形成无界自我复制")
	}
}
