package core

// L1（工作记忆区）送给模型的形态由用户裁定（2026-10-27）：
//
//  1. **时序排列的对话**：用户行 / 助手行，助手回复全文（不截 120 字）；
//  2. 中断轮的"用户原话"从聊天工具回执里取（Input 是注入样板，不是人话）；
//  3. 工具结果**展开全文**，不截断、不限条数；
//  4. 只有**超长**结果（已落盘进 toolresult_store）折叠成句柄。

import (
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

func newTimelineCtx() *RelevanceContext {
	return NewRelevanceContext("", memory.NewStaticEmbedder(""))
}

func TestTimelineRendersTemporalDialogue(t *testing.T) {
	rc := newTimelineCtx()
	// 中断轮：Input 是样板，人话在 qq_get_message 的回执里
	rc.Append(ContextEvent{
		Timestamp: time.Date(2026, 10, 27, 16, 5, 44, 0, time.Local),
		Source:    "agent",
		NoMemory:  true,
		Input:     "来自「—/」的群消息(message_id=1)。先用qq_get_message取正文…",
		Response:  "好的，我去找月租低于400、水电常价、带独卫的单间。",
		ToolResults: []ToolResultItem{
			{Name: "qq_get_message", Output: `{"content":"[CQ:at,qq=1] 帮我找一套月租＜400 水电常价带独卫的单间我就去"}`},
		},
	})
	a := &Agent{context: rc}
	out := a.formatMergedTimeline(4000)

	if strings.Contains(out, "先用qq_get_message取正文") {
		t.Fatalf("中断样板不得作为用户原话出现:\n%s", out)
	}
	if !strings.Contains(out, "用户: [CQ:at,qq=1] 帮我找一套月租＜400 水电常价带独卫的单间我就去") {
		t.Fatalf("用户行必须是聊天工具回执里的原话:\n%s", out)
	}
	if !strings.Contains(out, "助手: 好的，我去找月租低于400、水电常价、带独卫的单间。") {
		t.Fatalf("助手行必须是完整回复（不截断）:\n%s", out)
	}
}

func TestTimelineExpandsToolResultsInFull(t *testing.T) {
	rc := newTimelineCtx()
	long := strings.Repeat("行内容", 200) // 1000 rune，远超旧 160/400 字截断
	rc.Append(ContextEvent{
		Timestamp: time.Now(),
		Source:    "agent",
		Input:     "查一下",
		Response:  "查到了。",
		ToolResults: []ToolResultItem{
			{Name: "cmd_run", Output: long},
		},
	})
	a := &Agent{context: rc}
	out := a.formatMergedTimeline(200000)
	if !strings.Contains(out, long) {
		t.Fatalf("普通工具结果必须展开全文（长度 %d）", len(long))
	}
	if strings.Contains(out, "…") && strings.Contains(out, "工具(cmd_run): 行内容行内容…") {
		t.Fatal("不得对普通工具结果做截断")
	}
}

func TestTimelineFoldsOnlyOversizedResults(t *testing.T) {
	rc := newTimelineCtx()
	rc.Append(ContextEvent{
		Timestamp: time.Now(),
		Source:    "agent",
		ID:        "evt_1",
		Input:     "跑大查询",
		Response:  "结果太长，已落盘。",
		ToolResults: []ToolResultItem{
			{Name: "cmd_run", Output: "预览：..."},
			{Name: "qq_get_message", Output: `{"content":"短回执"}`},
		},
		ToolResultStores: []ToolResultStore{
			{Tool: "cmd_run", TotalBytes: 36783, TotalLines: 900},
		},
	})
	a := &Agent{context: rc}
	out := a.formatMergedTimeline(200000)
	if !strings.Contains(out, `toolresult_read(event_id="evt_1")`) {
		t.Fatalf("超长结果必须折叠成句柄:\n%s", out)
	}
	if strings.Contains(out, "预览：...") {
		t.Fatalf("超长结果不得渲染正文:\n%s", out)
	}
	if !strings.Contains(out, "短回执") {
		t.Fatalf("未超长的结果必须展开:\n%s", out)
	}
}
