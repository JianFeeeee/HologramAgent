package core

// L1 送给模型的形态：**provider 原生消息序列**（用户裁定，2026-11-03）。
//
//	> 「我的设计是 l1 渲染出来就是传统式的上下文排列，
//	>   难怪指代消解能力这么差」
//
// 在此之前 buildMessages 把**全部历史事件拼成一条 system 消息**：
//
//	system: 系统提示词
//	system: [Conversation timeline]
//	        [15:04:05] 用户: 帮我找月租<400的房子
//	        [15:04:05] 助手: ...
//	        [15:04:05] 工具(qq_get_message): ...
//	user:   当前输入
//
// 说话人身份靠**文本前缀**表达。模型对它是一段"背景资料"的注意力结构，
// 而不是"我说过的对话"——指代消解因此失效（用户回"新乡"接不上上一句）。
//
// 正确形态是原生 role 序列：
//
//	system
//	user      帮我找月租<400的房子
//	assistant ...
//	tool      ...
//	user      新乡
//	user      当前输入
//
// ★ 这些判据钉的是**结构**（role / ToolCallID 配对），不是文案。

import (
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

func newL1Agent(t *testing.T) *Agent {
	t.Helper()
	return &Agent{context: NewRelevanceContext("", memory.NewStaticEmbedder(""))}
}

// rolesOf 抽出每条消息的 role（判据断言用，避免整段字符串比对）。
func rolesOf(msgs []agentAPI.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

// TestL1RendersNativeMessageSequence 钉住：历史对话按原生 role 排列。
func TestL1RendersNativeMessageSequence(t *testing.T) {
	a := newL1Agent(t)
	a.context.Append(ContextEvent{
		Timestamp: time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local),
		Source:    "qq",
		Input:     "帮我找一套月租＜400 水电常价带独卫的单间",
		Response:  "好的，我去找。",
	})
	a.context.Append(ContextEvent{
		Timestamp: time.Date(2026, 11, 3, 10, 5, 0, 0, time.Local),
		Source:    "qq",
		Input:     "新乡",
		Response:  "新乡这边有几套。",
	})

	msgs := a.buildMessages("sys", "当前输入", 200000)

	want := []string{"system", "user", "assistant", "user", "assistant", "user"}
	if got := rolesOf(msgs); !equalStrings(got, want) {
		t.Fatalf("L1 必须是原生消息序列\n got roles: %v\nwant roles: %v", got, want)
	}

	// ★ 历史对话不得再以文本标签形式混进 system 消息。
	for i, m := range msgs {
		if m.Role != "system" {
			continue
		}
		if containsStr(m.Content, "[Conversation timeline]") || containsStr(m.Content, "用户:") {
			t.Fatalf("system[%d] 仍把历史渲染成文本日志，而不是原生消息序列:\n%s", i, m.Content)
		}
	}

	// 上一句用户原话必须是**独立 user 消息**，模型据此消解"新乡"指代。
	if msgs[1].Role != "user" || msgs[1].Content != "帮我找一套月租＜400 水电常价带独卫的单间" {
		t.Fatalf("user[1] 应是首句用户原话，实际 role=%q content=%q", msgs[1].Role, msgs[1].Content)
	}
	if msgs[3].Role != "user" || msgs[3].Content != "新乡" {
		t.Fatalf("user[3] 应是第二句用户原话，实际 role=%q content=%q", msgs[3].Role, msgs[3].Content)
	}
	// 最后一条是当前输入。
	if last := msgs[len(msgs)-1]; last.Role != "user" || last.Content != "当前输入" {
		t.Fatalf("末条应是当前输入，实际 role=%q content=%q", last.Role, last.Content)
	}
}

// TestL1ExpandsToolCallsAsPairedMessages 钉住工具轮的协议配对：
// assistant(tool_calls) 后必须紧跟同 CallID 的 tool 消息，否则上游 400。
func TestL1ExpandsToolCallsAsPairedMessages(t *testing.T) {
	a := newL1Agent(t)
	a.context.Append(ContextEvent{
		Timestamp: time.Now(),
		Source:    "qq",
		Input:     "帮我看看天气",
		Response:  "我查一下。",
		ToolCalls: []ToolCallItem{
			{CallID: "c1", Name: "weather_query", Arguments: map[string]interface{}{"city": "新乡"}},
			{CallID: "c2", Name: "qq_get_message", Arguments: map[string]interface{}{"id": 1}},
		},
		ToolResults: []ToolResultItem{
			{CallID: "c1", Name: "weather_query", Output: "晴 26 度"},
			{CallID: "c2", Name: "qq_get_message", Output: "他说新乡"},
		},
	})

	msgs := a.buildMessages("sys", "继续", 200000)

	// system, user, assistant(calls), tool, tool, user
	want := []string{"system", "user", "assistant", "tool", "tool", "user"}
	if got := rolesOf(msgs); !equalStrings(got, want) {
		t.Fatalf("工具轮必须展开成 assistant + 配对 tool 消息\n got: %v\nwant: %v", got, want)
	}
	asst := msgs[2]
	if len(asst.ToolCalls) != 2 {
		t.Fatalf("assistant 应带 2 个 tool_call，实际 %d", len(asst.ToolCalls))
	}
	for i, id := range []string{"c1", "c2"} {
		if asst.ToolCalls[i].ID != id {
			t.Fatalf("assistant.ToolCalls[%d].ID = %q，want %q", i, asst.ToolCalls[i].ID, id)
		}
		// 紧跟其后的 tool 消息必须回指同一个 CallID
		if msgs[3+i].ToolCallID != id {
			t.Fatalf("tool[%d].ToolCallID = %q，want %q（配对断裂，上游会 400）",
				3+i, msgs[3+i].ToolCallID, id)
		}
	}
}

// TestL1InactivePageDropsWholePair 钉住页剔除的协议完整性：
// 剔除一个工具页 = assistant 的该 tool_call 与对应 tool 消息**一起消失**。
// 只剔一半会让 assistant 声明了 call 却没有 tool 回执 ⇒ 上游 400。
func TestL1InactivePageDropsWholePair(t *testing.T) {
	a := newL1Agent(t)
	ev := ContextEvent{
		Timestamp: time.Now(),
		Source:    "qq",
		Input:     "并发查两个",
		Response:  "好。",
		ToolCalls: []ToolCallItem{
			{CallID: "keep", Name: "tool_keep"},
			{CallID: "drop", Name: "tool_drop"},
		},
		ToolResults: []ToolResultItem{
			{CallID: "keep", Name: "tool_keep", Output: "保留的结果"},
			{CallID: "drop", Name: "tool_drop", Output: "应被剔除的结果"},
		},
	}
	// 第 3 页 = 第 2 个 tool_call（第 1 页是 llm_round）
	ev.InactivePages = []int{3}
	a.context.Append(ev)

	msgs := a.buildMessages("sys", "继续", 200000)

	var asst *agentAPI.Message
	toolIDs := map[string]bool{}
	for i := range msgs {
		switch msgs[i].Role {
		case "assistant":
			asst = &msgs[i]
		case "tool":
			toolIDs[msgs[i].ToolCallID] = true
			if containsStr(msgs[i].Content, "应被剔除") {
				t.Fatalf("被剔除页的 tool 消息仍在序列里：%q", msgs[i].Content)
			}
		}
	}
	if asst == nil {
		t.Fatal("没有 assistant 消息")
	}
	for _, tc := range asst.ToolCalls {
		if tc.ID == "drop" {
			t.Fatal("被剔除页的 tool_call 仍留在 assistant 上 —— 有 call 无 result，上游必 400")
		}
	}
	if !toolIDs["keep"] {
		t.Fatal("未剔除页的 tool 消息不该消失")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
