package core

import (
	"context"
	"encoding/json"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// 回归：并行多工具调用的流式分片必须按上游 index 字段分桶累积，
// 不能用 Go range 序号（每 chunk 单元素时恒为 0，导致全部污染到同一桶）。
func TestAccumulateStreamParallelToolCallsByIndex(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mk := func(idx int, id, name, raw string) agentAPI.StreamChunk {
		return agentAPI.StreamChunk{
			ToolCalls: []agentAPI.ToolCall{{
				ID: id, Type: "function", Name: name, RawArguments: raw, StreamIndex: idx,
			}},
		}
	}
	ch := make(chan agentAPI.StreamChunk, 32)
	chunks := []agentAPI.StreamChunk{
		{Content: ""},
		// tool_call 0: spawn_child 参数较长，分多片
		mk(0, "call_a", "spawn_child", "{\"task\":"),
		mk(0, "", "", "\"调查大模型排名\"}"),
		// tool_call 1: browser_render
		mk(1, "call_b", "browser_render", "{\"url\":"),
		mk(1, "", "", "\"https://example.com\"}"),
		// tool_call 2: cmd_run
		mk(2, "call_c", "cmd_run", "{\"command\":\"uname -a\"}"),
		// tool_call 3: skill_list
		mk(3, "call_d", "skill_list", "{}"),
		{Done: true, FinishReason: "tool_calls"},
	}
	for _, ck := range chunks {
		ch <- ck
	}
	close(ch)

	resp, err := accumulateStream(ctx, ch, nil, "cli", 4096)
	if err != nil {
		t.Fatalf("accumulateStream: %v", err)
	}
	if len(resp.ToolCalls) != 4 {
		t.Fatalf("expected 4 tool calls, got %d: %+v", len(resp.ToolCalls), resp.ToolCalls)
	}
	want := map[string]string{
		"spawn_child":    `{"task":"调查大模型排名"}`,
		"browser_render": `{"url":"https://example.com"}`,
		"cmd_run":        `{"command":"uname -a"}`,
		"skill_list":     `{}`,
	}
	for _, tc := range resp.ToolCalls {
		raw, _ := json.Marshal(tc.Arguments)
		got := string(raw)
		exp, ok := want[tc.Name]
		if !ok {
			t.Errorf("unexpected tool %q args=%s", tc.Name, got)
			continue
		}
		delete(want, tc.Name)
		if tc.Name == "skill_list" {
			// 无参工具的合法空对象 {}，只需确认没被污染成乱码
			continue
		}
		if len(tc.Arguments) == 0 {
			t.Errorf("tool %q has EMPTY arguments (index pollution regression)", tc.Name)
			continue
		}
		var wantMap map[string]interface{}
		json.Unmarshal([]byte(exp), &wantMap)
		gotB, _ := json.Marshal(wantMap)
		if got != string(gotB) {
			t.Errorf("tool %q args = %s, want %s", tc.Name, got, exp)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing tool calls: %v", want)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls", resp.FinishReason)
	}
}
