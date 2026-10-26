package core

// 插件本轮提示（SDK InjectOptions.TurnHint）的注入判据。
//
// 设计分工（用户 2026-10-26）：
//
//	人设（personal.md）  怎么说话      —— 静态
//	TurnHint             这一轮的事实  —— 动态，由插件声明
//
// 关键约束：提示必须作为**独立 system 消息**出现，绝不能拼进用户正文。

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// ① 有 TurnHint ⇒ 出现一条独立的 system 消息，用户消息原样不动。
func TestTurnHintInjectedAsSeparateSystemMessage(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	f := &TaskFrame{Input: "在吗"}
	f.StageCtx = &sdk.StageContext{Extra: map[string]interface{}{
		"turn_hint": "【本轮渠道】QQ 私聊 → 用 output_send__qq",
	}}
	f.Msgs = a.buildMessages(a.buildSystemPrompt("", "在吗"), f.Input, 4096)

	hint := stageHint(f.StageCtx, "turn_hint")
	if hint == "" {
		t.Fatal("没能从 stage 上下文取到 turn_hint")
	}
	f.Msgs = append(f.Msgs, []agentAPI.Message{{Role: "system", Content: "[Turn hints] " + hint}}...)

	last := f.Msgs[len(f.Msgs)-1]
	if last.Role != "system" {
		t.Fatalf("提示必须是 system 角色（否则模型会当成人话），实际 %s", last.Role)
	}
	if !strings.Contains(last.Content, "[Turn hints]") || !strings.Contains(last.Content, "output_send__qq") {
		t.Fatalf("提示内容不对: %q", last.Content)
	}
	// 用户消息不能被污染
	for _, m := range f.Msgs[:len(f.Msgs)-1] {
		if strings.Contains(m.Content, "[Turn hints]") {
			t.Fatalf("提示被拼进了非 system 消息（role=%s）: %q", m.Role, m.Content)
		}
	}
}

// ② 没有 TurnHint ⇒ 不追加任何消息（零行为变化）。
func TestNoTurnHintAppendsNothing(t *testing.T) {
	if got := stageHint(&sdk.StageContext{Extra: map[string]interface{}{}}, "turn_hint"); got != "" {
		t.Fatalf("没有提示时应返回空，实际 %q", got)
	}
	if got := stageHint(nil, "turn_hint"); got != "" {
		t.Fatalf("nil 上下文应返回空，实际 %q", got)
	}
	if got := stageHint(&sdk.StageContext{Extra: map[string]interface{}{"turn_hint": "   "}}, "turn_hint"); got != "" {
		t.Fatalf("纯空白应视为没有提示，实际 %q", got)
	}
}
