package test

// return_pending_test.go —— 基座「任务退回」原语的判据。
//
// 语义（用户裁定 2026-11-27）：**调度器取走事件 → 通过基座接口退回**。
// 接口只负责**路由**（退到哪个通道 / 退给哪个 agent），不负责取。
//
// ★ 判据只看外部可见事实：退回后通道上的暂存条数（`StashLen`）
// 与目标实例收到了什么。不读源码、不读日志文本。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// returnHarness 造一个带通道登记表的 agent，并登记两条入站通道。
func returnHarness(t *testing.T, channels ...string) (*agent.Agent, *agentIO.ChannelRegistry) {
	t.Helper()
	iom := agentIO.NewIOManager()
	reg := iom.ChannelRegistry()
	for _, ch := range channels {
		if err := reg.Register(agentIO.InputChannel{Name: ch, Plugin: ch}); err != nil {
			t.Fatalf("Register(%q): %v", ch, err)
		}
	}
	a := faces{IO: iom}.mount(newMainAgent(t, agent.AgentConfig{
		TypeName: agent.TypeNameMain, ID: "return-probe",
	}))
	t.Cleanup(a.Stop)
	return a, reg
}

// 退回到**指定通道**：所有任务落到那一个通道的暂存区。
func TestReturnPendingToExplicitChannel(t *testing.T) {
	a, reg := returnHarness(t, "qq", "webui")
	evts := []*agentIO.InputEvent{
		{RequestID: "r1", Source: "qq", OutputChannel: "qq"},
		{RequestID: "r2", Source: "webui", OutputChannel: "webui"},
	}

	moved, _, err := a.ReturnPending(evts, agent.ReturnTarget{Channel: "webui"})
	if err != nil {
		t.Fatalf("ReturnPending: %v", err)
	}
	if moved != 2 {
		t.Fatalf("应退回 2 条，实际 %d", moved)
	}
	// ★ 指定通道 ⇒ 全部落到 webui，包括本来从 qq 进来的那条。
	if got := reg.StashLen("webui"); got != 2 {
		t.Fatalf("webui 暂存应为 2，实际 %d", got)
	}
	if got := reg.StashLen("qq"); got != 0 {
		t.Fatalf("qq 不应有暂存，实际 %d", got)
	}
}

// 退回时**不指定通道** ⇒ 逐条按各自 Source 归位（不能混存）。
//
// ★ 这条钉的是"一个实例同时收多条通道的消息"这个真实场景：
// 混存会让接回 qq 的人拿到本该给 webui 的活。
func TestReturnPendingWithoutChannelUsesEachEventSource(t *testing.T) {
	a, reg := returnHarness(t, "qq", "webui", "cli")
	evts := []*agentIO.InputEvent{
		{RequestID: "r1", Source: "qq"},
		{RequestID: "r2", Source: "webui"},
		{RequestID: "r3", Source: "cli"},
	}

	moved, _, err := a.ReturnPending(evts, agent.ReturnTarget{})
	if err != nil {
		t.Fatalf("ReturnPending: %v", err)
	}
	if moved != 3 {
		t.Fatalf("应退回 3 条，实际 %d", moved)
	}
	for _, ch := range []string{"qq", "webui", "cli"} {
		if got := reg.StashLen(ch); got != 1 {
			t.Fatalf("通道 %q 暂存应为 1（逐条按 Source 归位），实际 %d", ch, got)
		}
	}
}

// 原事件必须被**原样**暂存：`RequestID` 与 `ResponseCh` 不能丢。
//
// ★ 这是仓库实测过的坑：重建事件会丢掉 ResponseCh，同步调用方
// （cli / a2a）于是永久挂起。
func TestReturnPendingKeepsOriginalEventIdentity(t *testing.T) {
	a, reg := returnHarness(t, "qq")
	respCh := make(chan *agentIO.OutputEvent, 1)
	evt := &agentIO.InputEvent{RequestID: "keep-me", Source: "qq", ResponseCh: respCh}

	if _, _, err := a.ReturnPending([]*agentIO.InputEvent{evt}, agent.ReturnTarget{}); err != nil {
		t.Fatalf("ReturnPending: %v", err)
	}

	got := reg.TakeStash("qq")
	if len(got) != 1 {
		t.Fatalf("应取回 1 条暂存，实际 %d", len(got))
	}
	if got[0].RequestID != "keep-me" {
		t.Fatalf("RequestID 丢了：%q", got[0].RequestID)
	}
	if got[0].ResponseCh == nil {
		t.Fatal("★ ResponseCh 丢了 —— 同步调用方会永久挂起")
	}
}

// 目标通道不存在 ⇒ 不静默吞掉：必须给同步调用方终态，且返回错误。
func TestReturnPendingReportsGoneChannel(t *testing.T) {
	a, reg := returnHarness(t, "qq")
	respCh := make(chan *agentIO.OutputEvent, 1)
	evts := []*agentIO.InputEvent{
		{RequestID: "r1", Source: "gone-channel", ResponseCh: respCh},
	}

	moved, _, err := a.ReturnPending(evts, agent.ReturnTarget{})
	if moved != 0 {
		t.Fatalf("通道不存在时不应报告退回成功，实际 moved=%d", moved)
	}
	if err == nil {
		t.Fatal("通道不存在应返回错误（不许静默）")
	}
	// 终态必须送到：否则 cli/a2a 永久挂起。
	select {
	case out := <-respCh:
		if out == nil {
			t.Fatal("终态事件为空")
		}
	default:
		t.Fatal("★ 通道不存在时没有给调用方终态 —— 同步调用方会永久挂起")
	}
	_ = reg
}

// 子 agent 不能转投给其他 agent（只有主 agent 是"父"）。
func TestChildCannotReturnToAnotherAgent(t *testing.T) {
	iom := agentIO.NewIOManager()
	reg := iom.ChannelRegistry()
	_ = reg.Register(agentIO.InputChannel{Name: "qq", Plugin: "qq"})
	child := faces{IO: iom}.mount(newMainAgent(t, agent.AgentConfig{
		TypeName: agent.TypeNameMain, ID: "kid", ParentID: "return-probe",
	}))
	t.Cleanup(child.Stop)

	if !child.IsChildAgent() {
		t.Fatal("前提不成立：构造出的实例应当是子 agent")
	}

	_, _, err := child.ReturnPending(
		[]*agentIO.InputEvent{{RequestID: "r1", Source: "qq"}},
		agent.ReturnTarget{AgentID: "other"},
	)
	if err == nil {
		t.Fatal("★ 子 agent 不该能把任务转投给其他 agent（那是绕过父做调度）")
	}

	// 但**退回到通道**是允许的：那是"还给用户"，任何实例都能做。
	moved, _, err := child.ReturnPending(
		[]*agentIO.InputEvent{{RequestID: "r2", Source: "qq"}},
		agent.ReturnTarget{},
	)
	if err != nil || moved != 1 {
		t.Fatalf("子 agent 应能退回到通道：moved=%d err=%v", moved, err)
	}
}

// 空事件列表是合法输入（类型可能取完发现没有待办）。
func TestReturnPendingEmptyIsNoop(t *testing.T) {
	a, _ := returnHarness(t, "qq")
	moved, summary, err := a.ReturnPending(nil, agent.ReturnTarget{})
	if err != nil {
		t.Fatalf("空列表不该报错: %v", err)
	}
	if moved != 0 {
		t.Fatalf("空列表应退回 0 条，实际 %d", moved)
	}
	if summary == "" {
		t.Fatal("空列表也应给出摘要（调用方要能向模型解释）")
	}
}
