package test

// 思考内容（reasoning）在 L2/L3 的往返判据（2026-11-11）。
//
// ## 钉的是什么
//
// 用户裁定：**L1/L2/L3 都要包含 reasoning**。此前 L2 只落 Input/Response、
// L3 蒸馏只读 Input/Response/工具输出 —— 那是错误实现。
//
// 真因（生产 400）：
//
//	all 2 providers failed, last error: api error 400:
//	"The reasoning_content in the thinking mode must be passed back to the API."
//
// DeepSeek 规则：请求**带 tools** 时，所有历史轮的 reasoning_content 都要回传。
// 内核每轮都带工具定义 ⇒ 一旦历史里丢了思考内容，会话就永久 400。
//
// ## 判据走的是真实入口
//
// 用 ContextToColdContext（L1 → L2 的唯一入口）与 InOutWithReasoning
// （L2 → L1 的还原入口），断言"写进去的思考能不能原样读回来"。
// 不测内部函数、不碰私有字段。

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory/coldcontext"
)

// 判据：思考内容随事件进 L2，并能从块里读回。
//
// 变异验证（实测）：把 docBlocksForEntry 里生成 reasoning 块的那段删掉
// ⇒ 本判据红（reasoning 读回为空）。
func TestReasoningSurvivesL2RoundTrip(t *testing.T) {
	s := newStore(t)

	const thinking = "先确认用户问的是账单不是订单，再查 invoice 表"
	const reply = "已查到：上月在 12 号出账"

	doc, err := s.ContextToColdContext("agent", []coldcontext.ContextEntry{{
		Source:           "agent",
		Content:          "上个月账单什么时候出的？",
		Response:         reply,
		ReasoningContent: thinking,
	}}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("ContextToColdContext: %v", err)
	}
	if doc == nil {
		t.Fatal("★ ContextToColdContext 返回 nil")
	}
	if len(doc.Events) != 1 {
		t.Fatalf("★ 事件数 = %d，期望 1", len(doc.Events))
	}

	in, resp, reasoning, _ := doc.Events[0].InOutWithReasoning()
	if reasoning != thinking {
		t.Fatalf("★ L2 丢了思考内容：读回 %q，期望 %q", reasoning, thinking)
	}
	// 顺带钉住"没串味"：三者在 L2 里必须是三个独立的东西，
	// 否则回灌时会把思维链当成正式回复发给模型。
	if in != "上个月账单什么时候出的？" {
		t.Errorf("Input 串味：%q", in)
	}
	if resp != reply {
		t.Errorf("Response 串味：%q", resp)
	}
}

// 判据：思考块与回复块是**两个块**，不是合并成一个。
//
// 为什么单独钉：合并会让回灌时分不清哪个是 content、哪个是 reasoning，
// 而这个错误在"内容还在、只是位置不对"的形态下不会触发任何 panic。
func TestReasoningIsSeparateBlockFromResponse(t *testing.T) {
	s := newStore(t)

	doc, err := s.ContextToColdContext("agent", []coldcontext.ContextEntry{{
		Source:           "agent",
		Content:          "问题",
		Response:         "回复",
		ReasoningContent: "思考",
	}}, nil, nil, nil, nil)
	if err != nil || doc == nil {
		t.Fatalf("ContextToColdContext: %v / %v", err, doc)
	}

	var reasoningBlocks, responseBlocks int
	for _, b := range doc.Events[0].Blocks {
		switch string(b.BlockRole) {
		case string(coldcontext.TextBlockRoleReasoning):
			reasoningBlocks++
		case string(coldcontext.TextBlockRoleResponse):
			responseBlocks++
		}
	}
	if reasoningBlocks != 1 {
		t.Errorf("★ 思考块数 = %d，期望 1（合并进 response 会让回灌串味）", reasoningBlocks)
	}
	if responseBlocks != 1 {
		t.Errorf("★ 回复块数 = %d，期望 1", responseBlocks)
	}
}

// 判据：没有思考时不得凭空造一个空块。
//
// 反向保护：如果实现改成"无条件生成 reasoning 块"，上面两条仍会绿，
// 但每次归档都会多一个空块 —— L2 里堆满噪声，且空块的 ID 会随
// 时间戳变化而产生大量无效条目。
func TestNoReasoningMeansNoBlock(t *testing.T) {
	s := newStore(t)

	doc, err := s.ContextToColdContext("agent", []coldcontext.ContextEntry{{
		Source:   "agent",
		Content:  "问题",
		Response: "回复",
	}}, nil, nil, nil, nil)
	if err != nil || doc == nil {
		t.Fatalf("ContextToColdContext: %v / %v", err, doc)
	}
	for _, b := range doc.Events[0].Blocks {
		if string(b.BlockRole) == string(coldcontext.TextBlockRoleReasoning) {
			t.Fatal("★ 无思考时不应生成 reasoning 块")
		}
	}
	_, _, reasoning, _ := doc.Events[0].InOutWithReasoning()
	if reasoning != "" {
		t.Fatalf("★ 无思考时读回 %q，期望空串", reasoning)
	}
}

// 判据：思考里带特殊字符（换行、引号、中文标点）也能原样往返。
//
// 为什么值得钉：块 ID 由 sha256(时间戳|source|role|text) 派生，
// 而 L2 落盘是 JSON。任何一处转义或截断都会让 ID 与内容不匹配，
// 表现为"块存在但读不回来"——这正是 isEntryTextBlock 的判据会失配的地方。
func TestReasoningRoundTripWithSpecialChars(t *testing.T) {
	s := newStore(t)
	thinking := "第一行：检查 \"invoice\" 表\n第二行：注意 NULL 值\n第三行：结论「需要重试」"

	doc, err := s.ContextToColdContext("agent", []coldcontext.ContextEntry{{
		Source:           "agent",
		Content:          "查一下",
		Response:         "好",
		ReasoningContent: thinking,
	}}, nil, nil, nil, nil)
	if err != nil || doc == nil {
		t.Fatalf("ContextToColdContext: %v / %v", err, doc)
	}
	_, _, got, _ := doc.Events[0].InOutWithReasoning()
	if got != thinking {
		t.Fatalf("★ 特殊字符往返失真：\n got=%q\nwant=%q", got, thinking)
	}
}

// 判据：InOut 保持三返回值（不因新增 reasoning 而破坏既有调用方）。
//
// 这条是**兼容性契约**：InOut 有 8 个调用点，其中 6 个不需要思考内容。
// 如果实现图省事把 InOut 改成四返回值，那些调用点会被迫一起改 ——
// 改动量会掩盖真正的语义变化，且容易改错。
func TestInOutSignatureStillUsable(t *testing.T) {
	s := newStore(t)
	doc, err := s.ContextToColdContext("agent", []coldcontext.ContextEntry{{
		Source:           "agent",
		Content:          "问",
		Response:         "答",
		ReasoningContent: "思",
	}}, nil, nil, nil, nil)
	if err != nil || doc == nil {
		t.Fatalf("ContextToColdContext: %v / %v", err, doc)
	}
	// 三返回值必须仍然可用，且语义不变。
	in, resp, held := doc.Events[0].InOut()
	if in != "问" || resp != "答" {
		t.Fatalf("★ InOut 语义变了：in=%q resp=%q", in, resp)
	}
	_ = held // 无媒体块时为 nil
	if strings.Contains(resp, "思") {
		t.Fatal("★ InOut 的 response 里混进了思考内容")
	}
}
