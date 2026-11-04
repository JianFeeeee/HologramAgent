package core

// 判据：发送前的 tool_call 配对自检。
//
// ## 守的是什么（2026-11-04 生产故障）
//
// 上游对「assistant(tool_calls) 之后没有对应 tool 结果」是**硬拒绝**：
//
//	An assistant message with 'tool_calls' must be followed by tool messages
//	responding to each 'tool_call_id'. (insufficient tool messages following...)
//
// 后果：该会话所有 provider 全部失败，用户只看到一句英文报错。
// 且协议**不告诉你是哪条 call** —— 没有定位信息。
//
// 修完渲染层（0ed3c89 预算截断、263b651 并发批次）后，生产 context.json
// 已干净（0 个 result 多于 call 的帧），但 A2A 新会话**仍报同一个 400**
// ⇒ 问题在 f.Msgs（多条路径拼装），不在 L1 历史。
//
// ## 判据为什么必须两类都测
//
//	漏掉"call 无结果"  ⇒ 上游 400（本次故障）
//	漏掉"结果无 call"  ⇒ 同样 400（反向失配）
//
// 只测一个方向会放过另一半。

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

func toolMsg(id, content string) agentAPI.Message {
	return agentAPI.Message{Role: "tool", ToolCallID: id, Content: content}
}

func asstWithCalls(content string, ids ...string) agentAPI.Message {
	tcs := make([]agentAPI.ToolCall, 0, len(ids))
	for _, id := range ids {
		tcs = append(tcs, agentAPI.ToolCall{ID: id, Name: "tool_" + id})
	}
	return agentAPI.Message{Role: "assistant", Content: content, ToolCalls: tcs}
}

// TestDropUnpaired_DropsCallWithoutResult 钉住主方向：
// 没有结果的 call 必须被剔掉。
//
// 变异：dropUnpairedToolCalls 直接 return msgs ⇒ 本例红。
func TestDropUnpaired_DropsCallWithoutResult(t *testing.T) {
	msgs := []agentAPI.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "做点事"},
		asstWithCalls("", "c1", "c2"), // c2 没有结果
		toolMsg("c1", "ok"),
		{Role: "user", Content: "继续"},
	}

	got := dropUnpairedToolCalls(msgs)

	// 断言 ①：不许留下未配对的 call
	for _, m := range got {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == "c2" {
				t.Fatalf("★ 未配对的 call c2 仍在消息里 —— 上游必然 400")
			}
		}
	}
	// 断言 ②：配对的 c1 必须保留（不能因噎废食全删）
	found := false
	for _, m := range got {
		for _, tc := range m.ToolCalls {
			if tc.ID == "c1" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("★ 误删了配对的 call c1")
	}
	// 断言 ③：与其相邻的 tool 结果仍在
	if !strings.Contains(joinRoles(got), "tool:c1") {
		t.Fatalf("★ 误删了 c1 的结果消息")
	}
}

// TestDropUnpaired_DropsOrphanToolResult 钉住反方向：
// 找不到 call 的孤立 tool 结果必须被删。
//
// 变异：删掉第二遍（查孤立 tool）的循环 ⇒ 本例红。
func TestDropUnpaired_DropsOrphanToolResult(t *testing.T) {
	msgs := []agentAPI.Message{
		{Role: "user", Content: "hi"},
		toolMsg("ghost", "我没有任何 call 声明"),
		{Role: "user", Content: "继续"},
	}

	got := dropUnpairedToolCalls(msgs)
	for _, m := range got {
		if m.Role == "tool" && m.ToolCallID == "ghost" {
			t.Fatalf("★ 孤立 tool 结果 ghost 仍在 —— 上游同样 400")
		}
	}
}

// TestDropUnpaired_LeavesValidSequenceUntouched 钉住"正常路径零改动"。
//
// 这条很重要：修正逻辑若会重排/重写合法消息，就有引入新问题的风险。
func TestDropUnpaired_LeavesValidSequenceUntouched(t *testing.T) {
	msgs := []agentAPI.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q"},
		asstWithCalls("思考", "a", "b"),
		toolMsg("a", "ra"),
		toolMsg("b", "rb"),
		{Role: "user", Content: "next"},
	}
	before := joinRoles(msgs)
	got := dropUnpairedToolCalls(msgs)
	if joinRoles(got) != before {
		t.Fatalf("★ 合法序列被改动了：\n before=%s\n after =%s", before, joinRoles(got))
	}
	if len(got) != len(msgs) {
		t.Fatalf("★ 合法序列条数变了：%d → %d", len(msgs), len(got))
	}
}

// TestDropUnpaired_DropsEmptyAssistantShell 钉住边界：
// call 全被剔且正文为空时，整条 assistant 消息一起删
// （留一条空 assistant 也是非法形态）。
func TestDropUnpaired_DropsEmptyAssistantShell(t *testing.T) {
	msgs := []agentAPI.Message{
		{Role: "user", Content: "q"},
		asstWithCalls("", "x"), // 无结果、无正文
		{Role: "user", Content: "cont"},
	}
	got := dropUnpairedToolCalls(msgs)
	for _, m := range got {
		if m.Role == "assistant" {
			t.Fatalf("★ 留下了空的 assistant 壳（calls 已剔、正文为空）")
		}
	}
}

// TestDropUnpaired_KeepsContentWhenAllCallsDropped 钉住：
// call 全被剔但**有正文**时，保留正文（那是模型说的话，不能丢）。
func TestDropUnpaired_KeepsContentWhenAllCallsDropped(t *testing.T) {
	msgs := []agentAPI.Message{
		{Role: "user", Content: "q"},
		asstWithCalls("我要先看看", "y"),
		{Role: "user", Content: "cont"},
	}
	got := dropUnpairedToolCalls(msgs)
	for _, m := range got {
		if m.Role == "assistant" {
			if m.Content != "我要先看看" {
				t.Fatalf("★ 正文被改了：%q", m.Content)
			}
			if len(m.ToolCalls) != 0 {
				t.Fatalf("★ 还在声明 call")
			}
			return
		}
	}
	t.Fatalf("★ 有正文的 assistant 被整条删掉了")
}

// joinRoles 把消息序列渲染成 "role:id|role:id" 便于断言。
func joinRoles(msgs []agentAPI.Message) string {
	var parts []string
	for _, m := range msgs {
		s := m.Role
		if m.ToolCallID != "" {
			s += ":" + m.ToolCallID
		}
		for _, tc := range m.ToolCalls {
			s += "+call:" + tc.ID
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "|")
}
