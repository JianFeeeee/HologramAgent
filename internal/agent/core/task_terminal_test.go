package core

// M6 验收测试：任务级回执与断链点统一为终态事件。
//
// 设计依据 docs/zh/input-scheduler-design.md §7（不变量 I5）、§11.3（X1–X4）。
//
// 问题背景：回执原先由全局 emitResponse 写（无任务归属），且 processInput 有多条
// 「提前 return 而不 emit」的路径（解析失败、去重、consolidation）——同步调用方
// 若不自带超时（cli、clawhubadapter）就会永久挂起。

import (
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// X1：回执按任务归属，中断的回执绝不投给被挂起的等待者。
func TestTerminal_TaskScopedReplyNotMisrouted(t *testing.T) {
	sp := newPreemptProvider("intr-done", "low-done")
	a := newPreemptAgent(t, sp)

	lowEvt, lowCh := textEvent("qq", "低优先级任务")
	lowTask := newInputTask(lowEvt)
	if !a.sched.enqueue(lowTask) {
		t.Fatal("入队失败")
	}
	lt, _, _ := a.sched.nextRef()

	done := make(chan struct{})
	go func() { a.executeNewTask(lt); close(done) }()
	select {
	case <-sp.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider 未被调用")
	}

	intrEvt, intrCh := textEvent("cli", "紧急打断")
	intrEvt.Payload["interrupt"] = true
	if !a.sched.requestKernelPreempt(intrEvt) {
		t.Fatal("内核 L4 应抢占排队任务")
	}
	a.cancelCurrentLLM()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("未挂起")
	}

	// 执行中断任务 → 只应写它自己的回执通道。
	it, _, k := a.sched.nextRef()
	if k != nextImmediate {
		t.Fatalf("应取到立即运行的中断，kind=%v", k)
	}
	a.executeNewTask(it)
	if len(intrCh) != 1 {
		t.Fatalf("中断任务应回执到自己的通道，实际 %d", len(intrCh))
	}
	if len(lowCh) != 0 {
		t.Fatal("中断的回执绝不能被投给被挂起的等待者")
	}

	// 恢复并结束后，原任务才拿到自己的回执。
	rt, rf, k2 := a.sched.nextRef()
	if k2 != nextSuspended {
		t.Fatalf("应恢复被抢占任务，kind=%v", k2)
	}
	a.resumeTask(rt, rf)
	if len(lowCh) != 1 {
		t.Fatalf("恢复任务结束后应恰好回执一次，实际 %d", len(lowCh))
	}
	if got, _ := (<-lowCh).Payload["content"].(string); got != "low-done" {
		t.Fatalf("原任务回执内容=%q，期望 low-done", got)
	}
}

// X2：空输入（解析失败）也必须有终态回执。
func TestTerminal_EmptyInputGetsSkippedReply(t *testing.T) {
	a := newLifecycleAgent(t, &scriptProvider{}, nil, NewStageHost())
	ch := make(chan *agentIO.OutputEvent, 1)
	evt := &agentIO.InputEvent{
		RequestID:     "r-empty",
		Source:        "cli",
		Type:          "text",
		Payload:       map[string]interface{}{}, // 无 content，无媒体块
		OutputChannel: "cli",
		ResponseCh:    ch,
	}
	if _, out := a.runInputTask(evt); out != outcomeDone {
		t.Fatalf("空输入应正常返回，实际 %v", out)
	}
	if len(ch) != 1 {
		t.Fatal("空输入必须回 skipped 终态")
	}
	if reason, _ := (<-ch).Payload["reason"].(string); reason != "empty_input" {
		t.Fatalf("reason=%q，期望 empty_input", reason)
	}
	if a.context.Len() != 0 {
		t.Fatal("空输入不得写入上下文")
	}
}

// X4：无超时的同步调用方（cli / clawhubadapter）在断链路径上不再永久挂起。
//
// 这是回归判据：修复前 `InjectTextSync` 遇到去重命中会永久阻塞。
func TestTerminal_NoTimeoutSyncCallerDoesNotHang(t *testing.T) {
	a := newLifecycleAgent(t, &scriptProvider{script: []*agentAPI.CompletionResponse{
		{Content: "第一次"}, {Content: "第二次"},
	}}, nil, NewStageHost())

	// 第一次成功
	e1, ch1 := textEvent("cli", "重复内容")
	if _, out := a.runInputTask(e1); out != outcomeDone {
		t.Fatalf("首次=%v", out)
	}
	if len(ch1) != 1 {
		t.Fatal("首次应有回执")
	}

	// 第二次（去重命中）：模拟同步调用方阻塞等待——必须在 1s 内拿到终态。
	e2, ch2 := textEvent("cli", "重复内容")
	go a.runInputTask(e2)
	select {
	case r := <-ch2:
		if skipped, _ := r.Payload["skipped"].(bool); !skipped {
			t.Fatalf("应为 skipped 终态，实际 %+v", r.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("去重命中让同步调用方永久挂起（X4 回归）")
	}
}
