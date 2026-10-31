package core

// rtchild 的**同名复用**与**批量停止**的回收纪律（2026-10-30 修）。
//
// ## 这一组判据锁的是什么
//
// rtchild 的留档表与通道登记表都以 **id 字符串**为键，而 id 由模型给。
// 「拨完一通、再拨同名的一通」是完全正常的使用方式，但它会让上一代
// 留下的东西被当成这一代的 —— 而且**完全不报错**，只是内容错了。
//
// 三条缺陷、三条判据、每条都能变红（变异方式写在各自注释里）：
//
//	① 留档跨代串线     —— rt_transcript 读到上一通的对话
//	② 批量停止漏注销   —— child/rt-<id> 登记单调累积（只能重启 homed 清）
//	③ 留档遮蔽在跑实例 —— 通话进行中 rt_transcript 读不到自己的历史
//
// ③ 与 ① 是同一个读路径的两端：修复前 RTTranscript 先查留档，只要留档
// 命中就返回。于是不仅"重建后读到老的"，连"老的在跑、留档还在"时也会
// 读到旧的那份。

import (
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// TestRTChild_ReuseAfterCloseKeepsTranscriptsApart 钉住①：
// 同名 rtchild 回收后重建，新实例的 rt_transcript **不得**返回上一代原文。
//
// 变异方式：把 RTTranscript 改回"先查留档、命中即返回"（archive-first）
// ⇒ 本判据必红（新实例读出的第一条是"上一通的原话"）。已实测。
func TestRTChild_ReuseAfterCloseKeepsTranscriptsApart(t *testing.T) {
	p := &rtLongHistoryProvider{}
	host, _ := newRTHost(t, p)

	const id = "reuse"
	first := rtSpawn(t, host, id)
	host.PushRTInput(first, "上一通的原话")
	waitFor(t, "上一通第一轮完成", func() bool { return p.rounds() >= 1 })

	if n, err := host.CloseRTChild(id); err != nil {
		t.Fatalf("回收上一通失败：%v", err)
	} else if n == 0 {
		t.Fatal("上一通没有交接任何消息 —— 判据前提不成立")
	}
	// 回收后留档必须仍在（这是「回收后原文仍可取」的契约）。
	if got, ok := host.RTTranscript(id); !ok || len(got) == 0 {
		t.Fatalf("回收后取不到原文，留档机制本身坏了（ok=%v len=%d）", ok, len(got))
	}

	// 同一个 id 再开一通 —— 这是真实用法（模型常常复用 id）。
	second := rtSpawn(t, host, id)

	got, ok := host.RTTranscript(id)
	if !ok {
		t.Fatal("重建后的实例取不到 transcript")
	}
	for _, m := range got {
		if m.Content == "上一通的原话" {
			t.Fatalf("★ 同名重建后 rt_transcript 读到了**上一通**的原话 —— 两通电话串线")
		}
	}
	if len(got) != 0 {
		t.Fatalf("新一代刚创建、还没说话，transcript 应为空；实际 %d 条：%+v",
			len(got), truncateStr(FormatRTTranscript(got), 200))
	}

	// 新一代说自己的话，必须只看到自己的。
	p.mu.Lock()
	base := len(p.reqs)
	p.mu.Unlock()
	host.PushRTInput(second, "这一通的原话")
	waitFor(t, "这一通第一轮完成", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.reqs) > base
	})

	got, _ = host.RTTranscript(id)
	if len(got) == 0 || got[0].Content != "这一通的原话" {
		t.Fatalf("新一代的历史第一条应是「这一通的原话」，实际：%s",
			truncateStr(FormatRTTranscript(got), 200))
	}
}

// TestRTTranscript_PrefersLiveInstanceOverArchive 钉住③：
// 通话**进行中**（实例在跑）时，rt_transcript 必须返回在跑实例的历史，
// 而不是该 id 上一代的留档。
//
// ## 为什么要先 Spawn 再注入留档（两版假绿的教训）
//
// 这个判据前两版都是**假绿**的，教训值得钉在注释里：
//
//	版本 A：先手塞留档再 Spawn → 活实例建立时留档已存在，但当时判据
//	        断言过弱（只查"没有旧内容"），变异下仍绿。
//	版本 B：真回收再重建，但新实例不 push（history 空）
//	        ⇒ "读到旧留档"与"读到空"表面都是 len==0 ⇒ 仍然假绿。
//
// 现在：先 Spawn 建活实例 → **再**注入同 id 的陈旧留档 → 再 push 一句。
// 这样两种实现才分得清楚（活实例优先 / 留档优先）。
func TestRTTranscript_PrefersLiveInstanceOverArchive(t *testing.T) {
	p := &rtLongHistoryProvider{}
	host, _ := newRTHost(t, p)

	// ① 先建活实例。
	const id = "live"
	rtSpawn(t, host, id)

	// ② 再注入同 id 的陈旧留档 —— "活实例与留档共存"的那一瞬。
	host.rtMu.Lock()
	if host.rtClosedTranscripts == nil { // 懒初始化：没人回收过就该是 nil
		host.rtClosedTranscripts = map[string][]agentAPI.Message{}
	}
	host.rtClosedTranscripts[id] = []agentAPI.Message{
		{Role: "user", Content: "上一通的陈旧原话"},
	}
	host.rtMu.Unlock()

	// ③ 让活实例真的说一句（否则 history 为空，判据会假绿）。
	host.PushRTInput(id, "正在说的这句")
	waitFor(t, "活实例第一轮完成", func() bool { return p.rounds() >= 1 })

	got, ok := host.RTTranscript(id)
	if !ok {
		t.Fatal("在跑实例取不到 transcript")
	}
	for _, m := range got {
		if m.Content == "上一通的陈旧原话" {
			t.Fatalf("★ 在跑实例的 transcript 返回了留档里的陈旧原文 —— 留档遮蔽了活实例")
		}
	}
	found := false
	for _, m := range got {
		if m.Content == "正在说的这句" {
			found = true
		}
	}
	if !found {
		t.Fatalf("在跑实例的 transcript 里没有它自己的输入：%s",
			truncateStr(FormatRTTranscript(got), 200))
	}
}

// TestStopRTChildren_UnregistersInbound 钉住②：
// 批量停止（父退出路径）必须与单实例停止**行为对称**，注销 child/rt-<id>。
//
// 为什么必须一致：登记表是共享的、按 name 全局唯一，残留会随
// create/destroy 次数单调累积，而外部没有任何工具能单独注销一条 inputch
// —— 只能重启 homed 清（驻留子那边已有同形态的历史缺陷记录）。
//
// 变异方式：去掉 StopRTChildren 循环里的 Unregister ⇒ 本判据红。
func TestStopRTChildren_UnregistersInbound(t *testing.T) {
	p := &rtLongHistoryProvider{}
	host, _ := newRTHost(t, p)
	reg := host.io.ChannelRegistry()

	const id = "bulk"
	rtSpawn(t, host, id)

	inbound := inboundChannelName("rt/" + id)
	if _, ok := reg.Lookup(inbound); !ok {
		t.Fatalf("创建后没有登记入站通道 %q —— 判据前提不成立", inbound)
	}

	if n := host.StopRTChildren(); n != 1 {
		t.Fatalf("StopRTChildren 返回 %d，期望 1", n)
	}
	if _, ok := reg.Lookup(inbound); ok {
		t.Fatalf("★ 批量停止后入站登记 %q 仍留在共享登记表里 —— "+
			"会随 create/destroy 次数单调累积，且外部无法单独注销", inbound)
	}
}

// TestStopRTChild_SingleAlsoUnregisters 是②的对照组：
// 单实例停止**一直**是对的，这条留着防止"修复数时把单数改坏"。
func TestStopRTChild_SingleAlsoUnregisters(t *testing.T) {
	p := &rtLongHistoryProvider{}
	host, _ := newRTHost(t, p)
	reg := host.io.ChannelRegistry()

	const id = "single"
	rtSpawn(t, host, id)
	inbound := inboundChannelName("rt/" + id)
	if _, ok := reg.Lookup(inbound); !ok {
		t.Fatalf("创建后没有登记入站通道 %q", inbound)
	}
	if err := host.StopRTChild(id); err != nil {
		t.Fatalf("停止失败：%v", err)
	}
	if _, ok := reg.Lookup(inbound); ok {
		t.Fatalf("单实例停止后入站登记 %q 仍残留", inbound)
	}
}

// TestStopRTChildren_RtChannelsStayDangling 守住用户裁定：
// 批量停止**不得**动插件声明的 rt 通道（那两条销毁后悬空）。
//
// 这条是②的护栏：注销入站登记很容易顺手写成"把 rt 通道也清掉"，
// 而那样会让主 agent（不持有 rt 能力）通过 Owner=="" 的兜底分支
// 静默接管一条 rt 通道 —— 正是裁定明确禁止的。
func TestStopRTChildren_RtChannelsStayDangling(t *testing.T) {
	p := &rtLongHistoryProvider{}
	host, _ := newRTHost(t, p)
	reg := host.io.ChannelRegistry()

	const id = "dangling"
	rtSpawn(t, host, id)
	host.StopRTChildren()

	for _, name := range []string{"voice-in", "voice"} {
		ch, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("批量停止把插件声明的 rt 通道 %q 也删了 —— 通道应保持悬空而非消失", name)
		}
		if ch.Owner != id {
			t.Fatalf("rt 通道 %q 的归属被改成了 %q（期望仍悬空指向已销毁的 %q）",
				name, ch.Owner, id)
		}
	}
	// 悬空是可诊断的：路由查不到该 owner 时应留日志并兜底给父。
	// 这里只断言"不会凭空变成可复用通道"。
	time.Sleep(10 * time.Millisecond)
}
