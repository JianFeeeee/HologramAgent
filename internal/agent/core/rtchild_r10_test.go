package core

// R10 判据：rtchild 的工具面（用户裁定 R3 / R4）。
//
//	R3：rt 流**不禁工具**；`askparent` 工具**保留**
//	R4：提示词要求「调用工具前先用 outputch 告知用户稍等」
//
// `askparent` 在本仓的实际形态是 `notify_parent`（子侧向父发问的唯一通道，
// 见 tooldefs.go:874 的注册条件 parentID != ""）。所以这里钉的是：
//
//	1. rtchild 的工具表里**有** notify_parent（且不是被剥掉的）
//	2. 它的回调**真的接线**（初版没接 ⇒ 模型一调就得到"没有上级"）
//	3. 工具面**不因 rt 而收缩**（R3：不禁工具）

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// rtToolNames 从 buildToolDefs 的结果里抽工具名。
func rtToolNames(defs []interface{}) map[string]bool {
	out := map[string]bool{}
	for _, t := range defs {
		m, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]interface{})
		if !ok {
			continue
		}
		if name, ok := fn["name"].(string); ok {
			out[name] = true
		}
	}
	return out
}

// TestRTChild_AskparentToolPresent 钉住 R3 后半：`askparent`（= notify_parent）保留。
//
// 若有人"顺手"在 rtStripOutputTools 里把它也剥掉（它确实和输出有关），本判据必红。
func TestRTChild_AskparentToolPresent(t *testing.T) {
	p := &rtBlockingProvider{}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "ask")
	rc := host.rtChildren[id]
	if rc == nil {
		t.Fatal("实例不在登记表")
	}

	tools := rtStripOutputTools(rc.agent.buildToolDefs())
	names := rtToolNames(tools)

	// carrier 的 parentID != "" ⇒ notify_parent 应在子侧工具表里。
	// 若这里没有，说明 rtchild 的 carrier 没被正确标记为"子"。
	if !names["notify_parent"] {
		t.Fatalf("rtchild 工具表里没有 notify_parent（askparent）—— 用户裁定 R3 要求保留\n已有工具：%v",
			func() []string {
				var ks []string
				for k := range names {
					ks = append(ks, k)
				}
				return ks
			}())
	}
	// output_send__* 必须被剥掉（rt 输出走 outputch(rt) 流式管线）。
	for name := range names {
		if strings.HasPrefix(name, "output_send__") {
			t.Fatalf("rtchild 工具表里仍有 %s —— 会与流式管线产生两套输出语义", name)
		}
	}
}

// TestRTChild_NotifyParentIsWired 钉住**接线**（这是初版的真实缺陷）：
// notify_parent 的回调必须已接上，调用后不能返回"没有上级"。
//
// 初版 carrier 没设 notifyParent ⇒ notifyParentFrom 直接返回
// 「本 agent 没有上级，无法发送消息」——工具在、调用必失败。
func TestRTChild_NotifyParentIsWired(t *testing.T) {
	p := &rtBlockingProvider{}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "wire")
	rc := host.rtChildren[id]
	if rc == nil || rc.agent.notifyParent == nil {
		t.Fatal("rtchild 的 notifyParent 未接线 —— notify_parent 工具会永远失败")
	}

	// 走真实工具入口（不是直接调回调）：这才是模型看到的那条路。
	got := rc.agent.executeNotifyParent(agentAPI.ToolCall{
		Name:      "notify_parent",
		Arguments: map[string]interface{}{"text": "我想问一下"},
	})
	if strings.Contains(got, "没有上级") {
		t.Fatalf("notify_parent 走真实入口仍报「没有上级」：%s", got)
	}

	// 入站通道只登记一次：反复发问不应让登记表累积。
	reg := host.io.ChannelRegistry()
	before := reg.Count()
	for i := 0; i < 5; i++ {
		rc.agent.executeNotifyParent(agentAPI.ToolCall{
			Name:      "notify_parent",
			Arguments: map[string]interface{}{"text": "再问一次"},
		})
	}
	if after := reg.Count(); after != before {
		t.Fatalf("连发 5 次 notify_parent 后登记表从 %d 涨到 %d —— 每次发问都在重复登记（写放大+脏数据）",
			before, after)
	}
}

// TestRTChild_ToolFaceNotNarrowedByRT 钉住 R3 前半：**rt 流不禁工具**。
//
// 判据取法：把 rtchild 的工具数量与"同一个 agent 不用 rt 时的工具数量"比 ——
// rt 只该剥掉 output_send__*，不该额外剥任何东西。
func TestRTChild_ToolFaceNotNarrowedByRT(t *testing.T) {
	p := &rtBlockingProvider{}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "face")
	rc := host.rtChildren[id]

	all := rtToolNames(rc.agent.buildToolDefs())
	kept := rtToolNames(rtStripOutputTools(rc.agent.buildToolDefs()))

	// 只允许 output_send__* 与 output_list_channels 被剥。
	for name := range all {
		if kept[name] {
			continue
		}
		if !strings.HasPrefix(name, "output_send__") && name != "output_list_channels" {
			t.Fatalf("rt 额外剥掉了工具 %s —— 用户裁定 R3「rt 流不禁工具」", name)
		}
	}
	// 至少要保留一些工具（否则"不禁工具"名存实亡）。
	if len(kept) == 0 {
		t.Fatal("rtchild 工具表为空 —— 等于禁了所有工具")
	}
}

// TestRTChild_StopUnregistersInbound 钉住 Stop 时注销入站通道（不留脏登记）。
func TestRTChild_StopUnregistersInbound(t *testing.T) {
	p := &rtBlockingProvider{}
	host, _ := newRTHost(t, p)

	reg := host.io.ChannelRegistry()
	before := reg.Count()

	id := rtSpawn(t, host, "unreg")
	if reg.Count() <= before {
		t.Fatal("创建后没有登记入站通道 —— 父侧收不到 rtchild 的发问")
	}
	if err := host.StopRTChild(id); err != nil {
		t.Fatalf("停止: %v", err)
	}
	if got := reg.Count(); got != before {
		t.Fatalf("停止后登记表 %d，期望回到 %d —— 入站通道未注销", got, before)
	}
}

// 确保 agentIO 被使用（ChannelRegistry 类型在断言中通过接口访问）。
var _ = agentIO.CapText
