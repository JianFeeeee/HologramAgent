package core

// 时间线必须按**声明**渲染工具回执（2026-10-27 用户裁定）。
//
// 现场：`formatMergedTimeline` 只写「→ 调用工具: qq_get_message」，
// 工具取回的正文在下一轮彻底消失 —— 模型"刚说的话转头就忘"：
// 它记得自己查过，却不记得查到了什么。而 ToolDef 上的 Cleaner /
// NoMemory 声明早就存在，缺的一直是**消费端**。
//
// 判据（三条，对应三条规则）：
//  ① NoMemory=true 的工具不渲染；
//  ② 有 Cleaner 的走 Cleaner；
//  ③ 都没有渲染原文首段，并按条数/长度截断。

import (
	"strings"
	"testing"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func newTimelineTestAgent(defs []sdk.ToolDef) *Agent {
	h := &StageHost{toolDefs: defs}
	return &Agent{stageHost: h}
}

func TestTimelineRendersToolResultsWithCleaner(t *testing.T) {
	a := newTimelineTestAgent([]sdk.ToolDef{
		{
			Name:    "qq_get_message",
			Cleaner: func(string) string { return "因为魔芋爽" },
		},
		{Name: "secret_tool", NoMemory: true},
		{Name: "raw_tool"},
	})
	e := &ContextEvent{
		Input: "想报河南医药大学可以来dd",
		ToolResults: []ToolResultItem{
			{Name: "qq_get_message", Output: `{"content":"因为魔芋爽","raw":"…"}`},
			{Name: "secret_tool", Output: "SHOULD-NOT-APPEAR"},
			{Name: "raw_tool", Output: "第一行\n第二行"},
		},
	}
	got := strings.Join(a.renderedToolResultsOf(e), " | ")
	if !strings.Contains(got, "因为魔芋爽") {
		t.Fatalf("声明了 Cleaner 的工具必须按其清洗结果渲染，实际: %q", got)
	}
	if strings.Contains(got, "SHOULD-NOT-APPEAR") {
		t.Fatalf("NoMemory=true 的工具不得进时间线，实际: %q", got)
	}
	if !strings.Contains(got, "第一行 第二行") {
		t.Fatalf("无 Cleaner 的工具应渲染原文（压成单行），实际: %q", got)
	}
}

func TestTimelineToolResultsBounded(t *testing.T) {
	a := newTimelineTestAgent(nil)
	e := &ContextEvent{}
	for i := 0; i < toolResultTimelineMax+2; i++ {
		e.ToolResults = append(e.ToolResults, ToolResultItem{Name: "t", Output: "x"})
	}
	got := a.renderedToolResultsOf(e)
	if len(got) != toolResultTimelineMax+1 {
		t.Fatalf("应渲染 %d 条 + 1 条省略提示，实际 %d 条: %v", toolResultTimelineMax, len(got), got)
	}
	if !strings.Contains(got[len(got)-1], "+2 more") {
		t.Fatalf("省略提示应说明还有几条，实际: %q", got[len(got)-1])
	}
}
