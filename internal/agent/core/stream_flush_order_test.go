package core

import (
	"context"
	"fmt"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// 回归：并行多工具调用的 flush 顺序必须按上游 index 升序，与到达顺序无关。
//
// 为什么需要这条：flushToolCall 由 `for idx := range accs` 驱动，而 Go 的 map
// 迭代顺序是**随机化**的 —— 同一批并行 tool_call 因此可能以任意顺序进入
// resp.ToolCalls。对 output_send__ 这类**用户可见消息**通道，结果就是分段
// 消息的到达顺序每次运行都可能不同（不可复现的外部行为）。
//
// 判据的可执行性说明：map 随机化不是「每进程一次」，而是每次遍历都可能不同；
// 这里用「工具数 × 重复轮次」放大命中概率，让判据在修复前几乎必然失败、
// 修复后必然通过，而不是偶尔抖一下。
//
// ❗判据只断言**代码真实产出的字段**（ID/Name，由投递顺序决定），
// 不断言 StreamIndex —— flushToolCall 并不把 idx 写进 ToolCall
// （见 process.go 的 tc := ToolCall{ID, Name, Arguments}），
// 拿它当判据会得到一个恒真/恒假的假信号。
func TestAccumulateStreamFlushesToolCallsInIndexOrder(t *testing.T) {
	const (
		tools  = 8   // 单批并行工具数
		rounds = 200 // 重复轮次，放大 map 随机化命中概率
	)

	for round := 0; round < rounds; round++ {
		ctx, cancel := context.WithCancel(context.Background())

		ch := make(chan agentAPI.StreamChunk, 2*tools+4)
		// 按 index 升序投递：第 i 个工具声明 StreamIndex=i
		for i := 0; i < tools; i++ {
			ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{{
				ID:           fmt.Sprintf("call_%02d", i),
				Type:         "function",
				Name:         fmt.Sprintf("tool_%02d", i),
				RawArguments: "{}",
				StreamIndex:  i,
			}}}
		}
		ch <- agentAPI.StreamChunk{Done: true, FinishReason: "tool_calls"}
		close(ch)

		resp, err := accumulateStream(ctx, ch, nil, "cli", 4096)
		cancel()
		if err != nil {
			t.Fatalf("round %d: accumulateStream: %v", round, err)
		}
		if len(resp.ToolCalls) != tools {
			t.Fatalf("round %d: got %d tool calls, want %d", round, len(resp.ToolCalls), tools)
		}
		// 判据：必须严格按投递（index）升序出现。
		for i, tc := range resp.ToolCalls {
			want := fmt.Sprintf("tool_%02d", i)
			if tc.Name != want {
				t.Fatalf("round %d: 第 %d 个 tool call 是 %q，期望 %q；实际顺序 %v —— "+
					"map 迭代随机化导致 flush 乱序", round, i, tc.Name, want, toolNameOrder(resp.ToolCalls))
			}
		}
	}
}

// toolNameOrder 提取实际到达顺序，供失败信息定位。
func toolNameOrder(tcs []agentAPI.ToolCall) []string {
	out := make([]string, len(tcs))
	for i, tc := range tcs {
		out[i] = tc.Name
	}
	return out
}
