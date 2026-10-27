package core

// L2 预填充必须同时给出「发生了什么」与「内容是什么」。
//
// 现场：召回命中「已回复老大：原来是「魔芋爽」这个谐音梗」——模型读得懂
// "我回复过"，却不知道梗本身，于是答"不知道"。真实内容在事件的文本块与
// 工具回执里，摘要行永远不含它。

import (
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/document"
)

func TestDocBodyExcerptPicksToolResult(t *testing.T) {
	d := &document.Doc{
		Events: []document.DocEvent{{
			Timestamp: time.Now(),
			Blocks: []memory.MemoryBlock{
				{Modality: "text", Text: "已回复老大：原来是这个谐音梗。"},
			},
			ToolResults: []document.ToolResultItem{
				{Name: "qq_get_message", Output: `{"content":"因为魔芋爽","message_id":123}`},
			},
		}},
	}
	got := docBodyExcerptForPrompt(d, 80)
	if !strings.Contains(got, "魔芋爽") {
		t.Fatalf("正文摘录应来自工具回执并含真实内容，实际: %q", got)
	}
	if strings.Contains(got, "{") {
		t.Fatalf("JSON 回执应被压成可读文本，实际: %q", got)
	}
}

func TestToolResultExcerptPlainText(t *testing.T) {
	if got := toolResultExcerptForPrompt("第一行\n第二行"); got != "第一行" {
		t.Fatalf("非 JSON 回执应取首行，实际: %q", got)
	}
	if got := toolResultExcerptForPrompt(""); got != "" {
		t.Fatalf("空回执应为空，实际: %q", got)
	}
}
