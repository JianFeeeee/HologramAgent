package core

// 发送前的消息配对自检（2026-11-04）。
//
// ## 要解决的是什么
//
// 上游（OpenAI 兼容协议）对这个形态是**硬拒绝**：
//
//	An assistant message with 'tool_calls' must be followed by tool messages
//	responding to each 'tool_call_id'.
//	(insufficient tool messages following tool_calls message)
//
// 一旦发生，该会话的所有 provider 全部失败 —— 用户看到的是一句英文报错，
// 而不是任何业务回复。且**没有定位信息**：协议不告诉你是哪条 call。
//
// ## 为什么光是修渲染层不够
//
// 2026-11-04 修了两处（并发批次漏记 ToolCalls、L1 预算截断切开配对），
// 用生产 context.json 验证历史已干净（0 个 result 多于 call 的帧）。
// 但 A2A 新会话**仍然报同一个 400**。
//
// 原因：`f.Msgs` 是**多条路径**拼出来的 ——
//
//	prepare 重建      buildMessages
//	中断恢复          rebaseFramePrefix（重建 prefix + 接回 tail）
//	批末附加          flushBatchAttachments
//	占位补全          dropContinuationPlaceholders / continuation
//
// 任何一条漏掉配对，最终都表现为同一个 400。所以把自检放在**唯一一次
// 组装 provider 请求的位置**（stepLLM），而不是散在各条路径上。
//
// ## 为什么剔 call 而不是补占位
//
// 补一条 "tool result: (missing)" 会让模型看到它从未调用过的结果，
// 可能据此继续推理 —— 那是**编造事实**。剔掉 call 只是少了一次尝试，
// 模型可以重来，语义上是安全的。
//
// 被剔的 call 会记录到日志（工具名 + ID），用于定位是哪条路径产生的。

import (
	"log"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// dropUnpairedToolCalls 剔掉没有配对结果的 tool_calls。
//
// 规则（与协议严格对齐）：
//   - 一条 assistant 消息声明的每个 call，必须在**紧随其后的连续 tool 消息**
//     里各有一条同 ID 的结果；
//   - 缺失的 call 从该 assistant 消息里删除；若删空且正文也为空，整条消息删除；
//   - 对应的孤立 tool 消息（找不到 call 的）一并删除。
//
// 只在**真的有问题**时才改动切片，正常路径零开销（一次线性扫描）。
func dropUnpairedToolCalls(msgs []agentAPI.Message) []agentAPI.Message {
	if len(msgs) == 0 {
		return msgs
	}

	// 先扫一遍判断有没有问题：绝大多数请求是干净的，不做任何分配。
	if !hasUnpairedToolCalls(msgs) {
		return msgs
	}

	out := make([]agentAPI.Message, 0, len(msgs))
	dropped := 0
	var droppedNames []string

	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			// 非 call 声明者：原样保留（孤立的 tool 消息在下面单独处理）。
			out = append(out, m)
			continue
		}

		// 收集紧随其后的连续 tool 消息里出现的 call ID。
		answered := make(map[string]bool, len(m.ToolCalls))
		for j := i + 1; j < len(msgs); j++ {
			if msgs[j].Role != "tool" {
				break // 协议要求 tool 消息紧接在 assistant 之后
			}
			if id := msgs[j].ToolCallID; id != "" {
				answered[id] = true
			}
		}

		kept := make([]agentAPI.ToolCall, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			if tc.ID == "" || answered[tc.ID] {
				kept = append(kept, tc)
				continue
			}
			dropped++
			if len(droppedNames) < 8 { // 日志别刷屏
				droppedNames = append(droppedNames, tc.Name)
			}
		}
		if len(kept) == len(m.ToolCalls) {
			out = append(out, m)
			continue
		}
		// 有 call 被剔：若正文也没了，整条 assistant 消息一起删。
		if len(kept) == 0 && m.Content == "" {
			continue
		}
		m.ToolCalls = kept
		out = append(out, m)
	}

	// 第二遍：删掉找不到 call 的孤立 tool 消息。
	declared := make(map[string]bool)
	for _, m := range out {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				declared[tc.ID] = true
			}
		}
	}
	final := out[:0]
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID != "" && !declared[m.ToolCallID] {
			dropped++
			continue
		}
		final = append(final, m)
	}

	log.Printf("[agent] ★ dropped %d unpaired tool message(s) before send "+
		"(names: %v) — 上游会以 400 拒绝这类序列，剔掉后请求可继续",
		dropped, droppedNames)
	return final
}

// hasUnpairedToolCalls 是 dropUnpairedToolCalls 的**只读探测**版本。
//
// 单独写一份的理由：正常路径（绝大多数请求）应当零分配返回，
// 而 dropUnpairedToolCalls 需要建 map/slice。两份逻辑必须同源，
// 所以这里只判"是否合法"，不为修正做准备。
func hasUnpairedToolCalls(msgs []agentAPI.Message) bool {
	// 先收集所有声明过的 call ID（含后面出现的），用于判断孤立 tool。
	allDeclared := make(map[string]bool)
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				allDeclared[tc.ID] = true
			}
		}
	}

	for i, m := range msgs {
		if m.Role == "tool" && m.ToolCallID != "" && !allDeclared[m.ToolCallID] {
			return true // 孤立 tool
		}
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		answered := make(map[string]bool, len(m.ToolCalls))
		for j := i + 1; j < len(msgs) && msgs[j].Role == "tool"; j++ {
			if id := msgs[j].ToolCallID; id != "" {
				answered[id] = true
			}
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && !answered[tc.ID] {
				return true
			}
		}
	}
	return false
}
