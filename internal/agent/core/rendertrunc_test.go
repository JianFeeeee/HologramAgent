package core

// 判据：L1 渲染的**预算截断不得切开 tool_calls 与它的结果**。
//
// ## 守的是一个真实生产故障（2026-11-04）
//
// 生产 A2A 每条消息都回同一句错误：
//
//	An assistant message with 'tool_calls' must be followed by tool messages
//	responding to each 'tool_call_id'. (insufficient tool messages following
//	tool_calls message)
//
// 所有 provider 全部失败，会话彻底不可用。
//
// 排查时先怀疑"历史脏数据"（并发批次漏记 ToolCalls，已另行修复），
// 但**新会话同样报错** —— 说明问题不在历史，在渲染期。
//
// 根因在 renderL1Messages 的预算截断：
//
//	for _, m := range msgs {
//	    if maxTokens > 0 && used+cost > maxTokens && len(out) > 0 {
//	        return out          // ← 逐条累加，可能切在中间
//	    }
//	    out = append(out, m)
//	}
//
// `msgs` 是一组 [user?, assistant(tool_calls), tool, tool, ...]。
// 逐条累加时，`return` 可能发生在 assistant 之后、tool 之前
// ⇒ 序列里有一个"永远等不到结果"的 tool_calls ⇒ 上游 400。
//
// 用生产 context.json 的实测数据：
//
//	budget=480000 → 469 calls / 386 answered / **83 dangling**
//	budget=100000 → 469 calls /  40 answered / 429 dangling
//
// 即默认预算下就已经在切断。
//
// ## 两个约束必须同时满足
//
//	① 组内完整：assistant(tool_calls) 与全部 tool 结果要么都在、要么都不在
//	② 一帧超预算不能吞掉后续：实测见过 468 个 tool_call 的单帧（51 万 token），
//	   若直接 return，它之后的**全部历史**一起丢 —— 49 个事件只剩 8 个

import (
	"strings"
	"testing"
	"time"
)

// 下面用真实的 agentAPI.Message，不做抽象 —— 抽象层会让判据测不到真东西。
func TestRenderBudgetNeverSplitsToolPairing(t *testing.T) {
	a := New(AgentConfig{ID: "trunc-pairing"})

	// 造两帧，每帧 3 个工具调用。
	for f := 0; f < 2; f++ {
		evt := ContextEvent{
			Timestamp: time.Now(), Source: "agent",
			Input: "task",
		}
		for i := 0; i < 3; i++ {
			id := "c" + string(rune('a'+f)) + string(rune('0'+i))
			evt.ToolCalls = append(evt.ToolCalls, ToolCallItem{
				CallID: id, Name: "tool_x",
				Arguments: map[string]interface{}{"pad": strings.Repeat("x", 200)},
			})
			evt.ToolResults = append(evt.ToolResults, ToolResultItem{
				CallID: id, Name: "tool_x", Output: strings.Repeat("r", 200),
			})
		}
		a.context.Append(evt)
	}

	// 扫多个预算档位：任何档位都不得出现悬空调用。
	for _, budget := range []int{1, 50, 100, 200, 400, 800, 1600, 100000} {
		msgs := a.renderL1Messages(budget)
		declared := map[string]bool{}
		answered := map[string]bool{}
		for _, m := range msgs {
			for _, tc := range m.ToolCalls {
				declared[tc.ID] = true
			}
			if m.Role == "tool" {
				answered[m.ToolCallID] = true
			}
		}
		for id := range declared {
			if !answered[id] {
				t.Fatalf("★ budget=%d 时 call %s 无配对结果 ——\n"+
					"这正是生产故障：上游会 400 拒绝整个请求。\n"+
					"declared=%v answered=%v", budget, id, declared, answered)
			}
		}
	}
}

// TestRenderOversizedFrameDoesNotDropNewerHistory 钉住约束 ②。
//
// 变红条件：把"超大帧跳过并继续"改回"直接 return" ⇒ 红。
func TestRenderOversizedFrameDoesNotDropNewerHistory(t *testing.T) {
	a := New(AgentConfig{ID: "trunc-oversize"})

	// ① 一个**异常大**的帧（400 个工具调用，远超正常规模）
	big := ContextEvent{Timestamp: time.Now(), Source: "agent", Input: "big"}
	for i := 0; i < 400; i++ {
		id := "big" + strings.Repeat("0", 4) + string(rune('a'+i%26)) + string(rune('0'+i/26%10))
		big.ToolCalls = append(big.ToolCalls, ToolCallItem{
			CallID: id, Name: "tool_big",
			Arguments: map[string]interface{}{"pad": strings.Repeat("x", 400)},
		})
		big.ToolResults = append(big.ToolResults, ToolResultItem{
			CallID: id, Name: "tool_big", Output: strings.Repeat("r", 400),
		})
	}
	a.context.Append(big)

	// ② 它之后还有正常对话（更新的历史，绝不能被它吞掉）
	after := ContextEvent{
		Timestamp: time.Now(), Source: "agent",
		Input:    "这是超大帧之后的新对话，必须仍然可见",
		Response: "ack",
	}
	a.context.Append(after)

	// 预算远小于那个大帧 ⇒ 大帧必须被跳过，但 after 必须留下。
	msgs := a.renderL1Messages(5000)

	found := false
	for _, m := range msgs {
		if strings.Contains(m.Content, "超大帧之后的新对话") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("★ 超大帧把它之后的新对话一起吞掉了（渲染出 %d 条消息）——\n"+
			"实测影响：49 个事件只剩 8 个，上下文几乎清空。\n"+
			"正确处置：跳过这一组（组内仍完整），继续渲染更新的帧。", len(msgs))
	}

	// 且仍然不得有悬空调用（跳过的整组不进 out，所以自然不会悬空）
	declared := map[string]bool{}
	answered := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			declared[tc.ID] = true
		}
		if m.Role == "tool" {
			answered[m.ToolCallID] = true
		}
	}
	for id := range declared {
		if !answered[id] {
			t.Fatalf("★ 跳过超大帧时产生了悬空调用 %s", id)
		}
	}
}
