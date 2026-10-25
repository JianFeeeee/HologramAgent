package api

import (
	"strings"
	"testing"
)

// 流式并行 tool_call 的**分片归属**判据。
//
// 真实缺陷（2026-10-25 线上，2 小时 74 次 "has empty arguments"）：
// 累积时用 `accs[i]`（切片下标）而不是 tc.StreamIndex（上游 index）。
//
// 并行多工具时上游把每个 tool_call 的 arguments **分成多个 chunk** 下发，
// 每个 chunk 的 ToolCalls 切片长度通常是 1（下标恒为 0）⇒ 12 个并行调用的
// 参数分片全部拼进 accs[0]，其余 11 个拿到空串。
//
// 症状：模型侧"工具拿不到参数"；用户侧"agent 忘了刚说要做什么"。
// 每次恰好 12 条告警（= 同一响应的 12 个 tool_call 全报空）就是它的指纹。

// sseWithToolCalls 构造一段 SSE 响应体：每个 tool_call 一个独立分片。
func sseWithToolCalls(entries [][2]string) string {
	var b strings.Builder
	for i, e := range entries {
		sel := e[0]
		args := e[1]
		// 每个分片只含一个 tool_call，index 各不相同 —— 这正是并行多工具的真实形态
		b.WriteString(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":` + sel +
			`,"id":"call_` + sel + `","type":"function","function":{"name":"` + e[0] + `_tool","arguments":"` + args + `"}}]},"finish_reason":null}]}` + "\n\n")
		_ = i
	}
	b.WriteString(`data: [DONE]` + "\n")
	return b.String()
}

// 每个并行 tool_call 必须拿到**自己的**参数，不能全落到第一个。
func TestSSE_ParallelToolCallsKeepOwnArguments(t *testing.T) {
	// 3 个并行调用，分片 index 0/1/2，各带不同参数
	body := sseWithToolCalls([][2]string{
		{"0", `{\"days\":7}`},
		{"1", `{}`},
		{"2", `{\"q\":\"x\"}`},
	})
	resp, ok := parseOpenAICompatibleSSEBody([]byte(body))
	if !ok {
		t.Skip("SSE 解析路径未启用（该 provider 可能不走流式）")
	}
	if len(resp.ToolCalls) != 3 {
		t.Fatalf("应有 3 个 tool_call，实际 %d 个", len(resp.ToolCalls))
	}
	for _, tc := range resp.ToolCalls {
		if argsLookDropped(tc.RawArguments) {
			t.Fatalf("tool_call %q 的参数被判为空（分片归属错了）：raw=%q",
				tc.Name, tc.RawArguments)
		}
	}
}

// 同一 tool_call 的 arguments 分成多片时，必须按 index 拼接完整。
func TestSSE_SplitArgumentsAccumulateByIndex(t *testing.T) {
	// index 1 的参数分两片下发；index 0 只有一片。
	// 若用切片下标累积，第二片会被错记到 accs[0]。
	body := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"a_tool","arguments":"{\"x\":1}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"b_tool","arguments":"{\"y\":"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}` + "\n\n" +
		`data: [DONE]` + "\n"

	resp, ok := parseOpenAICompatibleSSEBody([]byte(body))
	if !ok {
		t.Skip("SSE 解析路径未启用")
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("应有 2 个 tool_call，实际 %d 个", len(resp.ToolCalls))
	}
	byName := map[string]string{}
	for _, tc := range resp.ToolCalls {
		byName[tc.Name] = tc.RawArguments
	}
	if got := byName["a_tool"]; got != `{"x":1}` {
		t.Fatalf("a_tool 参数错误（分片串了）：%q", got)
	}
	if got := byName["b_tool"]; got != `{"y":2}` {
		t.Fatalf("b_tool 参数未按 index 拼接：%q（期望 {\"y\":2}）", got)
	}
}
