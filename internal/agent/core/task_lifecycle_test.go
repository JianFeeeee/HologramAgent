package core

// M3a 验收测试：任务生命周期（prepare → run → finish）与「每任务恰一次终态」。
//
// 设计依据 docs/zh/input-scheduler-design.md §11.3（X2/X3/X4 的 M3a 形态）：
// 帧覆盖全生命周期后，提交（context.Append）与回执（emitResponse）只能在
// finish 段发生一次——挂起不会重复提交。

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func newLifecycleAgent(t *testing.T, sp agentAPI.Provider, bus *events.Bus, sh *StageHost) *Agent {
	t.Helper()
	return New(AgentConfig{
		ID:              "lifecycle",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       sh,
		EventBus:        bus,
	})
}

func textEvent(source, content string) (*agentIO.InputEvent, chan *agentIO.OutputEvent) {
	ch := make(chan *agentIO.OutputEvent, 1)
	return &agentIO.InputEvent{
		RequestID:     "req-1",
		Source:        source,
		Type:          "text",
		Payload:       map[string]interface{}{"content": content},
		OutputChannel: source,
		ResponseCh:    ch,
	}, ch
}

// X3（M3a 形态）：正常任务在 finish 段**恰好**提交一次并回执一次。
func TestLifecycle_NormalCommitsOnceAndReplies(t *testing.T) {
	bus := events.NewBus()
	var outputs, rawInputs int
	bus.Subscribe(events.EventAgentOutput, func(*events.Event) { outputs++ })
	bus.Subscribe(events.EventRawInput, func(*events.Event) { rawInputs++ })

	sp := &scriptProvider{script: []*agentAPI.CompletionResponse{{Content: "答复"}}}
	a := newLifecycleAgent(t, sp, bus, NewStageHost())

	evt, respCh := textEvent("cli", "你好")
	if _, out := a.runInputTask(evt); out != outcomeDone {
		t.Fatalf("runInputTask=%v，期望 outcomeDone", out)
	}

	select {
	case r := <-respCh:
		if got, _ := r.Payload["content"].(string); got != "答复" {
			t.Fatalf("回执内容=%q，期望 答复", got)
		}
		if !r.Done {
			t.Fatal("回执必须带 Done=true")
		}
	default:
		t.Fatal("同步回执缺失：finish 段必须写 ResponseCh")
	}

	if outputs != 1 {
		t.Fatalf("agent_output 事件=%d，期望恰好 1（每任务一次终态）", outputs)
	}
	if rawInputs != 1 {
		t.Fatalf("raw_input 事件=%d，期望 1", rawInputs)
	}
	if a.context.Len() != 2 {
		t.Fatalf("上下文事件=%d，期望 2（输入事件 + 本轮事件）", a.context.Len())
	}
}

// X2：被去重的输入以 skipped 终态结束——不提交、不回执、不发输出事件。
func TestLifecycle_DuplicateSkippedHasTerminal(t *testing.T) {
	bus := events.NewBus()
	outputs := 0
	bus.Subscribe(events.EventAgentOutput, func(*events.Event) { outputs++ })

	sp := &scriptProvider{script: []*agentAPI.CompletionResponse{
		{Content: "第一次"}, {Content: "第二次"},
	}}
	a := newLifecycleAgent(t, sp, bus, NewStageHost())

	e1, _ := textEvent("webui", "同样的消息")
	if _, out := a.runInputTask(e1); out != outcomeDone {
		t.Fatalf("首次输入=%v，期望 outcomeDone", out)
	}
	after1, outputs1 := a.context.Len(), outputs

	e2, ch2 := textEvent("webui", "同样的消息")
	if _, out := a.runInputTask(e2); out != outcomeDone {
		t.Fatalf("去重输入应正常返回（不挂起），实际 %v", out)
	}
	if a.context.Len() != after1 {
		t.Fatalf("去重命中不得提交上下文：%d → %d", after1, a.context.Len())
	}
	if len(ch2) != 1 {
		t.Fatal("去重命中必须回一个 skipped 终态，否则同步调用方永久挂起")
	}
	r := <-ch2
	if skipped, _ := r.Payload["skipped"].(bool); !skipped {
		t.Fatalf("去重回执必须带 skipped=true，实际 %+v", r.Payload)
	}
	if reason, _ := r.Payload["reason"].(string); reason != "duplicate" {
		t.Fatalf("reason=%q，期望 duplicate", reason)
	}
	if outputs != outputs1 {
		t.Fatalf("去重命中不得发输出事件：%d → %d", outputs1, outputs)
	}
}

// 被 on_input 阶段短路：回执阶段给的响应，且不提交上下文（与原实现一致）。
func TestLifecycle_OnInputShortCircuit(t *testing.T) {
	sh := NewStageHost()
	reply := "被插件短路"
	sh.RegisterStage(sdk.StageOnInput, func(ctx *sdk.StageContext) error {
		ctx.Response = &reply
		return nil
	})

	sp := &scriptProvider{} // 不应被调用到
	a := newLifecycleAgent(t, sp, events.NewBus(), sh)

	evt, respCh := textEvent("cli", "任意")
	if _, out := a.runInputTask(evt); out != outcomeDone {
		t.Fatalf("短路任务=%v，期望 outcomeDone", out)
	}
	select {
	case r := <-respCh:
		if got, _ := r.Payload["content"].(string); got != reply {
			t.Fatalf("短路响应=%q，期望 %q", got, reply)
		}
	default:
		t.Fatal("短路路径必须回执")
	}
	if a.context.Len() != 0 {
		t.Fatalf("短路路径不得提交上下文，实际 %d 条", a.context.Len())
	}
	if len(sp.reqs) != 0 {
		t.Fatal("短路路径不得调用 LLM")
	}
}

// 错误路径：以 error 终态结束，回执错误文本，且提交的是**错误事件**（无 turn 事件）。
func TestLifecycle_ErrorPathTerminal(t *testing.T) {
	bus := events.NewBus()
	outputs := 0
	bus.Subscribe(events.EventAgentOutput, func(*events.Event) { outputs++ })

	sp := &scriptProvider{err: &agentAPI.ProviderError{StatusCode: 401, Message: "bad key"}}
	a := newLifecycleAgent(t, sp, bus, NewStageHost())

	evt, respCh := textEvent("cli", "会失败")
	if _, out := a.runInputTask(evt); out != outcomeFailed {
		t.Fatalf("runInputTask=%v，期望 outcomeFailed", out)
	}
	select {
	case r := <-respCh:
		got, _ := r.Payload["content"].(string)
		if !strings.HasPrefix(got, "处理错误:") {
			t.Fatalf("错误回执=%q，期望以 处理错误: 开头", got)
		}
	default:
		t.Fatal("错误路径必须回执（否则同步调用方永久挂起）")
	}
	if outputs != 1 {
		t.Fatalf("错误路径的 agent_output 事件=%d，期望 1", outputs)
	}
	// 输入事件 + 错误事件 = 2；不得出现带 ToolsUsed 的 turn 事件。
	if a.context.Len() != 2 {
		t.Fatalf("错误路径上下文事件=%d，期望 2", a.context.Len())
	}
	recent := a.context.Recent(10)
	last := recent[len(recent)-1]
	if last.Response == "" {
		t.Fatal("错误事件必须带 Response")
	}
}

// _consolidation_ 走记忆整理专用路径：不回执、不提交上下文。
func TestLifecycle_ConsolidationRouted(t *testing.T) {
	bus := events.NewBus()
	outputs := 0
	bus.Subscribe(events.EventAgentOutput, func(*events.Event) { outputs++ })

	sp := &scriptProvider{script: []*agentAPI.CompletionResponse{{Content: "整理完毕"}}}
	a := newLifecycleAgent(t, sp, bus, NewStageHost())

	evt, respCh := textEvent("system", "整理任务")
	evt.OutputChannel = channelConsolidation
	if _, out := a.runInputTask(evt); out != outcomeDone {
		t.Fatalf("consolidation=%v，期望 outcomeDone", out)
	}
	if len(respCh) != 0 {
		t.Fatal("consolidation 路径不得回执")
	}
	if outputs != 0 {
		t.Fatalf("consolidation 路径不得发输出事件，实际 %d", outputs)
	}
	if a.context.Len() != 0 {
		t.Fatalf("consolidation 路径不得写用户上下文，实际 %d", a.context.Len())
	}
}
