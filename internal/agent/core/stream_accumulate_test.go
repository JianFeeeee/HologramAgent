package core

import (
	"context"
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// 验证流式 tool call 分片累积：模拟 llmsproxy/big-pickle 的分片序列
func TestAccumulateStreamToolCalls(t *testing.T) {
	ch := make(chan agentAPI.StreamChunk, 10)
	go func() {
		// 分片1: name + id + arguments 开头
		ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{
			{ID: "call_1", Name: "cmd_run", RawArguments: "{\""},
		}}
		// 分片2-3: 只有 arguments 分片
		ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{
			{RawArguments: "command\""},
		}}
		ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{
			{RawArguments: ":\"date\"}"},
		}}
		ch <- agentAPI.StreamChunk{Done: true, FinishReason: "tool_calls"}
		close(ch)
	}()

	resp, err := accumulateStream(context.Background(), ch, nil, "cli", 4096)
	if err != nil {
		t.Fatalf("accumulateStream: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.Name != "cmd_run" || tc.ID != "call_1" {
		t.Fatalf("bad name/id: %s/%s", tc.ID, tc.Name)
	}
	cmd, _ := tc.Arguments["command"].(string)
	if cmd != "date" {
		t.Fatalf("arguments not merged, got: %v", tc.Arguments)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("finish reason: %q", resp.FinishReason)
	}
}

// 验证 content/reasoning 增量累积
func TestAccumulateStreamContent(t *testing.T) {
	ch := make(chan agentAPI.StreamChunk, 5)
	go func() {
		ch <- agentAPI.StreamChunk{ReasoningContent: "think "}
		ch <- agentAPI.StreamChunk{Content: "你"}
		ch <- agentAPI.StreamChunk{Content: "好"}
		ch <- agentAPI.StreamChunk{Done: true, FinishReason: "stop"}
		close(ch)
	}()
	resp, err := accumulateStream(context.Background(), ch, nil, "cli", 4096)
	if err != nil {
		t.Fatalf("accumulateStream: %v", err)
	}
	if resp.Content != "你好" {
		t.Fatalf("content: %q", resp.Content)
	}
	if resp.ReasoningContent != "think " {
		t.Fatalf("reasoning: %q", resp.ReasoningContent)
	}
}

// 回归（2026-09-19 实测事故）：长参数工具调用被 max_tokens 从中间截断时，
// 上游发 finish_reason="length"、参数 JSON 残缺。旧实现把残缺 JSON 静默降级成
// 空 map，工具只报 "path is required"，模型看不出真因、原样重试四次。
//
// 本测试钉死：截断必须变成带指引的 __arg_error，而不是空参数。
func TestAccumulateStreamTruncatedArgsSurfaced(t *testing.T) {
	ch := make(chan agentAPI.StreamChunk, 10)
	go func() {
		ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{
			{ID: "call_1", Name: "files_write", RawArguments: `{"path":"/tmp/a.py","content":"# -*- coding`},
		}}
		// 参数写到一半被切断，随后到达 length 终止块
		ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{
			{RawArguments: `: utf-8 -*-\nimport openpyxl\nfor i in range(80):\n    w`},
		}}
		ch <- agentAPI.StreamChunk{Done: true, FinishReason: "length"}
		close(ch)
	}()

	resp, err := accumulateStream(context.Background(), ch, nil, "cli", 4096)
	if err != nil {
		t.Fatalf("accumulateStream: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(resp.ToolCalls))
	}
	msg, ok := resp.ToolCalls[0].Arguments["__arg_error"].(string)
	if !ok || msg == "" {
		t.Fatalf("截断的参数必须带 __arg_error，实际 Arguments=%v", resp.ToolCalls[0].Arguments)
	}
	// 指引必须可执行：说出真因（截断/max_tokens）并给出拆小方案
	for _, want := range []string{"截断", "max_tokens=4096", "拆成多次调用"} {
		if !strings.Contains(msg, want) {
			t.Errorf("指引缺少 %q：%s", want, msg)
		}
	}
	// 截断时绝不能把残缺 JSON 解析出的空 map 当参数交出去
	if _, hasPath := resp.ToolCalls[0].Arguments["path"]; hasPath {
		t.Error("截断参数不应残留任何可用字段（否则会以残缺参数执行）")
	}
}

// 非截断的残缺 JSON 也要拦住工具调用（不然工具只会报 “path is required”），
// 但**必须与真截断用不同的文案** —— 否则模型会去“拆小参数”，而它其实是写坏了。
// 这里同时钉死两件事：①不丢给工具 ②两种成因可区分。
func TestAccumulateStreamMalformedArgsDistinctFromTruncated(t *testing.T) {
	ch := make(chan agentAPI.StreamChunk, 10)
	go func() {
		ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{
			{ID: "call_1", Name: "files_write", RawArguments: `{"path":`},
		}}
		ch <- agentAPI.StreamChunk{Done: true, FinishReason: "tool_calls"}
		close(ch)
	}()

	resp, err := accumulateStream(context.Background(), ch, nil, "cli", 4096)
	if err != nil {
		t.Fatalf("accumulateStream: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(resp.ToolCalls))
	}
	msg, ok := resp.ToolCalls[0].Arguments["__arg_error"].(string)
	if !ok || msg == "" {
		t.Fatalf("残缺参数必须被拦住，实际 Arguments=%v", resp.ToolCalls[0].Arguments)
	}
	// 不能被说成“截断”：真因是 JSON 写坏，两者对模型要求的动作完全不同。
	if strings.Contains(msg, "max_tokens") || strings.Contains(msg, "截断") {
		t.Errorf("非截断的残缺参数被误报为截断：%s", msg)
	}
	if !strings.Contains(msg, "合法 JSON") {
		t.Errorf("应指出 JSON 格式问题：%s", msg)
	}
}

// 截断必须**真的拦住工具调用**，而不是只改错误文案：
// 旧实现拿着空 map 去调 files_write，工具回 "path is required"，
// 模型据此原样重试（实测连续 4 次）。这里断言 executeToolCallInner 的短路。
func TestTruncatedToolCallIsShortCircuited(t *testing.T) {
	tc := agentAPI.ToolCall{
		ID:   "call_1",
		Name: "files_write",
		Arguments: map[string]interface{}{
			"__arg_error": truncatedArgsError("files_write", 259, 4096),
		},
	}
	a := &Agent{}
	got := a.executeToolCallInner(tc, "webui", nil).Text

	if strings.Contains(got, "path is required") {
		t.Errorf("截断后仍走了工具分派（模型会原样重试）：%s", got)
	}
	for _, want := range []string{"截断", "max_tokens=4096", "拆成多次调用"} {
		if !strings.Contains(got, want) {
			t.Errorf("指引缺少 %q：%s", want, got)
		}
	}
}

// 回归（2026-09-19 线上实测）：真 invalid JSON 有 11/11 是同一成因 ——
// 模型把 timeout 写成 `"timeout": 20s`（值缺引号，schema 示例是 "10s, 1m, 30s"
// 而声明是 string 类型），而 command 部分一字节没错。
//
// 旧行为：解析失败 → 静默降级成空 map → 整个 command 被丢 → 工具报
// "command is required"，模型只能原样重试 ⇒ 实测 cmd_run 失败率 34%（34 败/64 成）。
func TestRepairUnquotedUnitNumberInArgs(t *testing.T) {
	// 全部取自日志原文（未被我自己的日志截断的那些）
	real := []string{
		`{"command": "ls -lt /tmp/*.xlsx /tmp/*.py 2>/dev/null | head -20; echo \"=== home ===\"; ls -lt ~ 2>/dev/null | head -20", "timeout": 20s}`,
		`{"command": "sleep 45; cat /tmp/run_szce.log; ls -la /tmp/szce_run_raw.json 2>/dev/null", "timeout": 90s}`,
		`{"command": "echo \"=== 上一轮 raw (471B) ===\"; cat /tmp/szce_run_raw.json; echo; echo \"=== 后台进程 ===\"; ps aux | grep -c \"[r]un_szce.py\"; echo \"=== log ===\"; cat /tmp/run_szce.log", "timeout": 30s}`,
		`{"command": "sleep 60; cat /tmp/probe_out.txt; echo \"=== alive ===\"; ps aux | grep -c \"[p]robe_models.py\"", "timeout": 120s}`,
		`{"command": "cat /tmp/probe_out.txt; echo \"--- alive ---\"; ps aux | grep -c \"[p]robe_models.py\"", "timeout": 30s}`,
	}
	for i, s := range real {
		m, ok := parseToolArgsJSON(s)
		if !ok {
			t.Errorf("case %d 仍解析失败", i)
			continue
		}
		if cmd, _ := m["command"].(string); cmd == "" {
			t.Errorf("case %d 完好的 command 丢失", i)
		}
		if to, _ := m["timeout"].(string); to == "" {
			t.Errorf("case %d timeout 未补成字符串: %#v", i, m["timeout"])
		}
	}
}

// 修复必须保守：不能碰合法 JSON，尤其不能改到字符串**正文里**的 “20s”。
func TestRepairKeepsValidArgsIntact(t *testing.T) {
	m, ok := parseToolArgsJSON(`{"command": "ls", "timeout": "20s"}`)
	if !ok {
		t.Fatal("合法 JSON 被判非法")
	}
	if m["timeout"] != "20s" {
		t.Errorf("合法 timeout 被改: %#v", m["timeout"])
	}
	m2, ok := parseToolArgsJSON(`{"content": "wait 20s then go"}`)
	if !ok {
		t.Fatal("含 20s 的正文被判非法")
	}
	if m2["content"] != "wait 20s then go" {
		t.Errorf("正文里的 20s 被误改: %#v", m2["content"])
	}
}

// 真截断（JSON 从中间断掉）绝不能被“修好”，否则会拿残缺参数去执行 —— 更危险。
func TestRepairDoesNotFabricateTruncatedArgs(t *testing.T) {
	for _, s := range []string{
		`{"command": "ls -la /tmp && echo done"`,
		`{"command": "echo hi", "timeout": 30`,
		`{"path": "/tmp/x", "content": "unterminated`,
	} {
		if _, ok := parseToolArgsJSON(s); ok {
			t.Errorf("截断参数被误判为可修复（危险）: %s", s)
		}
	}
}

// 端到端：修好 JSON 之后，字段必须**真的能被插件用上**。
// cmd 插件走 args["timeout"].(string) 再 time.ParseDuration ——
// 若修复把 20s 变成数字或丢了引号，插件会静默忽略 timeout，等于换个姿势失败。
func TestRepairedTimeoutUsableByPlugin(t *testing.T) {
	m, ok := parseToolArgsJSON(`{"command": "ls -lt /tmp | head -20", "timeout": 20s}`)
	if !ok {
		t.Fatal("解析失败")
	}
	to, isStr := m["timeout"].(string)
	if !isStr {
		t.Fatalf("timeout 必须是 string，否则 cmd 插件读不到: %#v", m["timeout"])
	}
	if to != "20s" {
		t.Errorf("timeout 值不对: %q", to)
	}
	if d, err := time.ParseDuration(to); err != nil || d.Seconds() != 20 {
		t.Errorf("cmd 插件下一步 ParseDuration(%q) 会失败: %v", to, err)
	}
}
