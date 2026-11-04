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
//
// ## 这条判据曾经是假的（2026-11-04 修）
//
// 原实现自己在测试里**手动重放**了真实注入逻辑：
//
//	f.Msgs = a.buildMessages(...)
//	hint := stageHint(f.StageCtx, "turn_hint")
//	f.Msgs = append(f.Msgs, agentAPI.Message{Role: "system", Content: "[Turn hints] " + hint})
//	// 然后断言 "最后一条是 system、内容含 [Turn hints]"
//
// 它断言的是**它自己刚 append 的那一行** —— 把 task.go:656-661 的实现
// 抄进测试，再验证抄得对不对。真实代码里那段被删掉，这条判据照样绿。
//
// 正确做法：把 turn_hint 放进 StageCtx，跑**真实的 stepPrepare**
// （newTaskFrame 起始就是 StepPrepare，runTaskSteps 会执行它），
// 然后检查它产出的 Msgs。这样才测到 task.go 那段注入。
//
// 变异：删掉 task.go 里 `if hint := stageHint(f.StageCtx, "turn_hint")`
// 那一段 ⇒ 本例红。
func TestTurnHintInjectedAsSeparateSystemMessage(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())

	stageCtx := &sdk.StageContext{Extra: map[string]interface{}{
		"turn_hint": "【本轮渠道】QQ 私聊 → 用 output_send__qq",
	}}
	f := a.newTaskFrame("在吗", stageCtx)

	// 只跑到 stepPrepare 结束（下一步是 LLM 调用，本判据不关心）。
	a.stepPrepare(f)

	// ① 必须有一条独立的 system 消息承载提示
	var hintMsgs []agentAPI.Message
	for _, m := range f.Msgs {
		if strings.Contains(m.Content, "[Turn hints]") {
			hintMsgs = append(hintMsgs, m)
		}
	}
	if len(hintMsgs) != 1 {
		t.Fatalf("★ 含 [Turn hints] 的消息有 %d 条，期望恰好 1 条 ——\n"+
			"0 条 = 注入逻辑没生效；多条 = 被重复追加", len(hintMsgs))
	}
	if hintMsgs[0].Role != "system" {
		t.Fatalf("★ 提示必须是 system 角色（否则模型会当成人话），实际 %s", hintMsgs[0].Role)
	}
	if !strings.Contains(hintMsgs[0].Content, "output_send__qq") {
		t.Fatalf("★ 提示内容丢了（%q）", hintMsgs[0].Content)
	}

	// ② 用户消息不能被污染：user 角色的消息里不能出现 [Turn hints]
	for _, m := range f.Msgs {
		if m.Role == "user" && strings.Contains(m.Content, "[Turn hints]") {
			t.Fatalf("★ 提示被拼进了 user 消息: %q", m.Content)
		}
	}
}

// ①b 没有 TurnHint ⇒ 真实的 stepPrepare 不产出任何 [Turn hints] 消息。
//
// 与 ① 成对：只测"有"会放过"永远注入"的实现。
func TestNoTurnHintNoSystemMessageFromPrepare(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	f := a.newTaskFrame("在吗", &sdk.StageContext{Extra: map[string]interface{}{}})
	a.stepPrepare(f)

	for _, m := range f.Msgs {
		if strings.Contains(m.Content, "[Turn hints]") {
			t.Fatalf("★ 没给 turn_hint 却注入了提示: role=%s content=%q", m.Role, m.Content)
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
