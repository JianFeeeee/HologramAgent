package security

import "testing"

// ── 工具级敏感词（用户裁定 2026-11-03） ──

// TestToolSensitiveWordsAppendOnlyForThatTool 钉住两件事：
//
//	① 档位为某工具追加的词，只在该工具的结果上生效
//	② 语义是**追加**：整轮 SensitiveWords 仍然生效（不覆盖）
//
// 用户裁定原话：
//
//	「敏感词策略也应该是插件声明啊，每个插件声明自己轮次以及自己工具的
//	  敏感词策略」
func TestToolSensitiveWordsAppendOnlyForThatTool(t *testing.T) {
	rt := NewRuntime(nil)
	if err := rt.DeclareProfile(Profile{
		Name: "qq.guest", Owner: "qq",
		SensitiveWords: []string{"整轮词"},
		ToolSensitiveWords: map[string][]string{
			"web_fetch": {"网页专用词"},
			"qq_get_*":  {"聊天专用词"},
		},
	}); err != nil {
		t.Fatalf("声明档位: %v", err)
	}

	// ① web_fetch 命中自己的词
	f, _, _ := rt.ScanContentForTool(Inspection{Stage: "tool_result"}, "这里有网页专用词", "qq.guest", "web_fetch")
	if len(f) == 0 {
		t.Fatal("web_fetch 没命中自己声明的工具级敏感词")
	}

	// ② 换一个工具就不该命中（词表是工具属性，不能全局泄漏）
	f, _, _ = rt.ScanContentForTool(Inspection{Stage: "tool_result"}, "这里有网页专用词", "qq.guest", "cmd_run")
	if len(f) != 0 {
		t.Fatalf("cmd_run 命中了 web_fetch 的词 —— 工具级词表泄漏到别的工具：%v", f)
	}

	// ③ 通配符模式生效（qq_get_* 命中 qq_get_message）
	f, _, _ = rt.ScanContentForTool(Inspection{Stage: "tool_result"}, "聊天专用词", "qq.guest", "qq_get_message")
	if len(f) == 0 {
		t.Fatal("通配符模式 qq_get_* 没有匹配 qq_get_message")
	}

	// ④ 追加语义：整轮词在任何工具上都生效
	for _, tool := range []string{"web_fetch", "cmd_run", ""} {
		f, _, _ = rt.ScanContentForTool(Inspection{Stage: "tool_result"}, "这里有整轮词", "qq.guest", tool)
		if len(f) == 0 {
			t.Fatalf("整轮敏感词在 tool=%q 上没生效 —— 工具级词表不该覆盖整轮词表", tool)
		}
	}
}
