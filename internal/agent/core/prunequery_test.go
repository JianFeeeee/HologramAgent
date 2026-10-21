package core

import (
	"testing"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// ContextPolicy=prune 的查询向量必须取**清洗后**的有效内容。
//
// 裁剪的入参是相关性查询向量，它决定保留/归档哪些上下文事件。原始工具输出里
// 混着 ANSI 转义、base64、JSON 包装等噪声，直接向量化会让打分失真，裁掉本该
// 保留的事件。ToolDef.Cleaner 的契约本就写着「仅在向量化/jieba/蒸馏时调用」，
// 裁剪正是在向量化——此前只在构建事件向量时用了它，裁剪查询漏了。
func TestToolOutputForQueryAppliesCleaner(t *testing.T) {
	host := NewStageHost()
	called := 0
	if err := host.RegisterTool("demo_tool", sdk.ToolDef{
		Name: "demo_tool",
		Cleaner: func(s string) string {
			called++
			return "cleaned:" + s
		},
	}, func(map[string]interface{}) (interface{}, error) { return nil, nil }); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	a := &Agent{stageHost: host}
	raw := "\x1b[31mresult\x1b[0m"

	got := a.toolOutputForQuery("demo_tool", raw)
	if called != 1 {
		t.Fatalf("Cleaner 应被调用恰好一次，实际 %d", called)
	}
	if got != "cleaned:"+raw {
		t.Fatalf("查询应使用清洗结果，实际 %q", got)
	}

	// 未注册 Cleaner 的工具：回退原文。
	if got := a.toolOutputForQuery("no_such_tool", raw); got != raw {
		t.Fatalf("无 Cleaner 应回退原文，实际 %q", got)
	}

	// 无 StageHost（如裸 Agent）：不能 panic，回退原文。
	if got := (&Agent{}).toolOutputForQuery("demo_tool", raw); got != raw {
		t.Fatalf("nil stageHost 应回退原文，实际 %q", got)
	}
}

// Cleaner 返回空串时必须回退原文：空串会让查询向量退化成零向量，
// 所有事件相关性相同，裁剪就失去判据（等于随机裁）。
func TestToolOutputForQueryEmptyCleanFallsBack(t *testing.T) {
	host := NewStageHost()
	if err := host.RegisterTool("t", sdk.ToolDef{
		Name:    "t",
		Cleaner: func(string) string { return "" },
	}, func(map[string]interface{}) (interface{}, error) { return nil, nil }); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	a := &Agent{stageHost: host}
	if got := a.toolOutputForQuery("t", "raw"); got != "raw" {
		t.Fatalf("Cleaner 返回空应回退原文，实际 %q", got)
	}
}
