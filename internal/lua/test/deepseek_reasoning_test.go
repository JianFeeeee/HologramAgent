package test

// deepseek 适配器的 reasoning_content 回传判据（2026-11-11）。
//
// ## 钉的故障
//
//	all 2 providers failed, last error: api error 400:
//	"The reasoning_content in the thinking mode must be passed back to the API."
//
// DeepSeek 规则：请求带 tools 时，所有历史轮的 reasoning_content 都要传回。
//
// 内核侧三层已修（L1 存+回传、L2 落块、L3 进蒸馏），但**存量会话**的
// context.json 是修复前写的，那些历史事件根本没有该字段。不兜底就是死结：
// 历史缺字段 ⇒ 400 ⇒ 会话永久不可用（重启也救不回来，缺的字段在磁盘上）。
//
// 所以适配器必须补字段。本文件钉住"兜底补得对、且不覆盖真实值"。
//
// ## 为什么要补**空串**而不是跳过
//
// 字段语义是"上一轮的思考内容"。空串如实表达"这一轮没有可回传的思考"。
// 其他主流实现的同款修复也是补空串（langchain #37179、n8n #34924、
// opencode #24722）—— 上游接受，且比删掉整条消息更保真。

import (
	"encoding/json"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/lua"
)

// newStartedVM 起一个加载了内置适配器的 VM（与运行时同一套落盘逻辑）。
func newStartedVM(t *testing.T) *lua.VM {
	t.Helper()
	v := lua.NewVM(t.TempDir())
	if err := v.Start(); err != nil {
		t.Fatalf("VM.Start: %v", err)
	}
	t.Cleanup(v.Stop)
	return v
}

// transform 调真实内置适配器的 transform_request，返回解码后的请求体。
func transform(t *testing.T, v *lua.VM, body string) map[string]interface{} {
	t.Helper()
	out, err := v.CallTransformRequest("deepseek", body)
	if err != nil {
		t.Fatalf("CallTransformRequest: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("适配器输出不是合法 JSON: %v\n%s", err, out)
	}
	return got
}

// assistantMsgs 取请求里所有 assistant 消息。
func assistantMsgs(req map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	msgs, _ := req["messages"].([]interface{})
	for _, m := range msgs {
		if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "assistant" {
			out = append(out, mm)
		}
	}
	return out
}

// 判据：带 tools + assistant 缺 reasoning_content ⇒ 必须补上空串。
//
// 变异验证（实测）：把 transform_request 里补字段那段删掉 ⇒ 本判据红。
func TestAdapterBackfillsReasoningWhenToolsPresent(t *testing.T) {
	v := newStartedVM(t)
	// 存量会话形态：assistant(tool_calls) 没有 reasoning_content
	body := `{
		"model":"deepseek-chat",
		"tools":[{"type":"function","function":{"name":"sql_query","parameters":{"type":"object"}}}],
		"messages":[
			{"role":"user","content":"上个月账单什么时候出的？"},
			{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"sql_query","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"12号"}
		]
	}`

	got := transform(t, v, body)
	as := assistantMsgs(got)
	if len(as) != 1 {
		t.Fatalf("★ assistant 消息数 = %d，期望 1", len(as))
	}
	if _, ok := as[0]["reasoning_content"]; !ok {
		t.Fatalf("★ 带 tools 的请求里 assistant 缺 reasoning_content（上游会 400）：%v", as[0])
	}
	// 补的是空串，不是凭空造内容
	if s, _ := as[0]["reasoning_content"].(string); s != "" {
		t.Errorf("★ 兜底应补空串，实际 %q（不得伪造思考内容）", s)
	}
}

// 判据：**不能覆盖**内核已经带来的真实思考内容。
//
// 这条比上面那条更要紧：覆盖 = 把真实思维链换成空串，
// 上游虽然不报 400 了，但多轮上下文里的推理链条被抹掉，
// 属于"错误从显式变隐式"。
func TestAdapterDoesNotOverwriteRealReasoning(t *testing.T) {
	v := newStartedVM(t)
	const realThinking = "先确认问的是账单不是订单"
	body := `{
		"model":"deepseek-chat",
		"tools":[{"type":"function","function":{"name":"sql_query","parameters":{"type":"object"}}}],
		"messages":[
			{"role":"user","content":"问"},
			{"role":"assistant","content":"答","reasoning_content":"` + realThinking + `"}
		]
	}`

	got := transform(t, v, body)
	as := assistantMsgs(got)
	if len(as) != 1 {
		t.Fatalf("assistant 数 = %d", len(as))
	}
	if s, _ := as[0]["reasoning_content"].(string); s != realThinking {
		t.Fatalf("★ 真实思考被覆盖：%q（期望 %q）", s, realThinking)
	}
}

// 判据：不带 tools 时不补（上游不要求，也不必给上下文塞空字段）。
func TestAdapterLeavesReasoningAloneWithoutTools(t *testing.T) {
	v := newStartedVM(t)
	body := `{
		"model":"deepseek-chat",
		"messages":[
			{"role":"user","content":"你好"},
			{"role":"assistant","content":"你好，有什么可以帮你"}
		]
	}`

	got := transform(t, v, body)
	as := assistantMsgs(got)
	if len(as) != 1 {
		t.Fatalf("assistant 数 = %d", len(as))
	}
	if _, ok := as[0]["reasoning_content"]; ok {
		t.Fatalf("★ 无 tools 时不该补该字段：%v", as[0])
	}
}

// 判据：多轮历史里**每一条** assistant 都要有，不能只补最后一条。
//
// DeepSeek 的要求是"所有历史轮"。只补最后一条会让第二次工具回环
// 再次 400 —— 而那种"修一次好一次、第三轮又坏"最难查。
func TestAdapterBackfillsEveryAssistantTurn(t *testing.T) {
	v := newStartedVM(t)
	body := `{
		"model":"deepseek-chat",
		"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object"}}}],
		"messages":[
			{"role":"user","content":"a"},
			{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"r1"},
			{"role":"user","content":"b"},
			{"role":"assistant","content":"","tool_calls":[{"id":"c2","type":"function","function":{"name":"t","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c2","content":"r2"}
		]
	}`

	got := transform(t, v, body)
	as := assistantMsgs(got)
	if len(as) != 2 {
		t.Fatalf("assistant 数 = %d，期望 2", len(as))
	}
	for i, m := range as {
		if _, ok := m["reasoning_content"]; !ok {
			t.Errorf("★ 第 %d 条 assistant 缺 reasoning_content（要补全部历史轮）", i+1)
		}
	}
}

// 判据：适配器不能顺手弄坏别的字段（tools / messages / model）。
//
// 反向保护：兜底逻辑若写成"重建整个请求体"，很容易漏字段。
// 这条钉住"只补一个字段，其余原样"。
func TestAdapterPreservesOtherFields(t *testing.T) {
	v := newStartedVM(t)
	body := `{
		"model":"deepseek-chat",
		"temperature":0.7,
		"max_tokens":1024,
		"stream":true,
		"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object"}}}],
		"messages":[{"role":"user","content":"hi"}]
	}`

	got := transform(t, v, body)
	if got["model"] != "deepseek-chat" {
		t.Errorf("★ model 被改：%v", got["model"])
	}
	if temp, _ := got["temperature"].(float64); temp != 0.7 {
		t.Errorf("★ temperature 被改：%v", got["temperature"])
	}
	if mt, _ := got["max_tokens"].(float64); mt != 1024 {
		t.Errorf("★ max_tokens 被改：%v", got["max_tokens"])
	}
	if got["stream"] != true {
		t.Errorf("★ stream 被改：%v", got["stream"])
	}
	if _, ok := got["tools"]; !ok {
		t.Error("★ tools 丢了")
	}
	// 无思考内容的 user 消息不该被加上该字段
	msgs, _ := got["messages"].([]interface{})
	for _, m := range msgs {
		if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "user" {
			if _, has := mm["reasoning_content"]; has {
				t.Errorf("★ 给 user 消息加了 reasoning_content（只该给 assistant）")
			}
		}
	}
}

// 判据：disable_thinking 必须转成**顶层** thinking 字段。
//
// ## 为什么这条最关键（它是 400 的第一根因）
//
// 官方文档示例全用 OpenAI SDK：extra_body={"thinking": {...}}。
// 那是 **SDK 层参数** —— SDK 会摊平进请求体顶层。而适配器是裸发 HTTP，
// 修正前写成 req.extra_body.thinking，服务端不认识这个包，直接忽略
// ⇒ thinking 保持默认（开启）⇒ 带 tools 的请求强制要求 reasoning_content
// ⇒ 400。也就是说：配置 thinking_enabled=false 也从未真正关掉过思考。
//
// 变异验证（实测）：把 req.thinking 改回 req.extra_body.thinking ⇒ 本判据红。
func TestAdapterDisablesThinkingAtTopLevel(t *testing.T) {
	v := newStartedVM(t)
	got := transform(t, v, `{"model":"deepseek-chat","disable_thinking":true,"messages":[{"role":"user","content":"x"}]}`)

	if _, ok := got["disable_thinking"]; ok {
		t.Error("★ disable_thinking 应被消费掉，不该透传给上游")
	}
	// 必须是顶层
	th, ok := got["thinking"].(map[string]interface{})
	if !ok {
		t.Fatalf("★ 缺少顶层 thinking 字段（上游会忽略 ⇒ 思考关不掉）：%v", got)
	}
	if th["type"] != "disabled" {
		t.Fatalf("★ thinking.type = %v，期望 disabled", th["type"])
	}
	// 不得留 extra_body 壳（SDK 层的参数名，裸 HTTP 上游不认识）
	if _, ok := got["extra_body"]; ok {
		t.Error("★ 不应把 extra_body 壳透传给上游（SDK 层参数）")
	}
}

// 判据：不带 disable_thinking 时，不得凭空加 thinking 字段。
//
// 反向保护：默认不改上游行为。上游默认是**开启**思考，
// 我们只在用户明确要求关闭时才动。
func TestAdapterNoThinkingFieldByDefault(t *testing.T) {
	v := newStartedVM(t)
	got := transform(t, v, `{"model":"deepseek-chat","messages":[{"role":"user","content":"x"}]}`)
	if _, ok := got["thinking"]; ok {
		t.Fatalf("★ 未要求关闭时不该加 thinking：%v", got["thinking"])
	}
}
