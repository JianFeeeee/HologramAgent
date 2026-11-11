package test

// reasoning_content 的**协议契约**判据（2026-11-11）。
//
// ## 为什么要单独钉协议层
//
// 生产故障：
//
//	all 2 providers failed, last error: api error 400:
//	"The reasoning_content in the thinking mode must be passed back to the API."
//
// DeepSeek 官方规则（api-docs.deepseek.com/guides/thinking_mode）：
//
//	请求带 tools 时，**所有历史轮**的 reasoning_content 都要传回，并拼进上下文。
//
// 而内核每轮都带工具定义，所以这条必然触发。修复横跨三层：
//
//	L1  存（ContextEvent.ReasoningContent）+ 渲染回传
//	L2  落块（RoleReasoning 独立块）
//	L3  进蒸馏原文
//
// 本文件**只钉最外面那一层**：内核发给上游的 JSON 里，
// reasoning_content 是不是 assistant 消息的**顶层字段**。
//
// ## 为什么必须是顶层字段（而不是塞进 content）
//
// OpenAI 兼容格式（DeepSeek 走这条）规定：
//
//	{"role":"assistant","content":"回复","reasoning_content":"思考"}
//
// 把思考拼进 content 会同时坏两件事：
//   · 上游认为"没回传"，照样 400
//   · 模型下一轮会把自己的内心独白读成"我说过这话"
//
// 对照：Anthropic 才是 content block（thinking block）。我们走 OpenAI 兼容，
// 所以是顶层字段 —— 两者不能混。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// 判据：reasoning_content 是 assistant 消息的顶层字段，与 content 平级。
//
// 变异验证（实测）：把 Message.MarshalJSON 里写 reasoning_content 的那段删掉
// ⇒ 本判据红（字段消失，上游就会 400）。
func TestReasoningContentIsTopLevelField(t *testing.T) {
	msg := api.Message{
		Role:             "assistant",
		Content:          "已查到：上月在 12 号出账",
		ReasoningContent: "先确认问的是账单不是订单",
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	v, ok := got["reasoning_content"]
	if !ok {
		t.Fatalf("★ 消息里没有 reasoning_content 顶层字段（上游会 400）：%s", raw)
	}
	if v != "先确认问的是账单不是订单" {
		t.Fatalf("★ reasoning_content = %v", v)
	}
	// content 必须保持原样，不被思考污染。
	if got["content"] != "已查到：上月在 12 号出账" {
		t.Fatalf("★ content 被污染：%v", got["content"])
	}
}

// 判据：无思考时**不发**该字段（不伪造空字段）。
//
// 为什么：三个层次都要区分「没有思考」与「思考是空的」。
// 无条件发空串会让每条消息都带一个无意义字段，也会让上游
// 无从判断"这一轮到底有没有思考过"。
func TestNoReasoningOmitsField(t *testing.T) {
	msg := api.Message{Role: "assistant", Content: "普通回复"}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "reasoning_content") {
		t.Fatalf("★ 无思考时不该出现该字段：%s", raw)
	}
}

// 判据：带着 tool_calls 的 assistant 消息，reasoning_content 仍必须出现。
//
// 这条最贴近真实故障形态：DeepSeek 的触发条件是"**带 tools** 的请求历史"，
// 而实际报错的具体消息正是 **assistant(tool_calls)** 那条 ——
// 模型先思考、再决定调工具，工具结果回来时那条 assistant 必须带着思考。
func TestReasoningWithToolCalls(t *testing.T) {
	msg := api.Message{
		Role:             "assistant",
		Content:          "",
		ReasoningContent: "需要查一下 invoice 表",
		ToolCalls: []api.ToolCall{{
			ID:        "call_1",
			Name:      "sql_query",
			Arguments: map[string]interface{}{"sql": "select * from invoice"},
		}},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["reasoning_content"] != "需要查一下 invoice 表" {
		t.Fatalf("★ 带 tool_calls 的 assistant 丢了 reasoning_content：%s", raw)
	}
	// content 为空的 assistant(tool_calls) 应发 null（OpenAI 语义），
	// 不能变成空串 —— 有些上游对两者处理不同。
	if c, ok := got["content"]; !ok || c != nil {
		t.Errorf("content 期望 null，实际 %v", c)
	}
	if _, ok := got["tool_calls"]; !ok {
		t.Error("★ tool_calls 丢了")
	}
}
