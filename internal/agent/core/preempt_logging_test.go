package core

import (
	"testing"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// 抢占日志必须能说出**受害者是谁**（来源 + 类别 + 级别）。
// 此前 suspend/resume 不落日志，生产上无法回答"我的任务被谁打断了"。
func TestPreemptLogging_NamesVictimAndSource(t *testing.T) {
	sp := newPreemptProvider("intr-done", "low-done")
	a := newPreemptAgent(t, sp)

	lowEvt, _ := textEvent("qq", "低优先级任务")
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

	if a.sched.suspendDepth() != 0 {
		t.Fatalf("初始栈深应为 0，实际 %d", a.sched.suspendDepth())
	}

	intrEvt := &agentIO.InputEvent{
		Source: "cli", OutputChannel: "cli", Type: "text",
		Payload: map[string]interface{}{"content": "紧急打断", "interrupt": true},
	}
	a.sched.requestPreempt(intrEvt, LevelInteractive)
	a.cancelCurrentLLM()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("未挂起")
	}
	if d := a.sched.suspendDepth(); d != 1 {
		t.Fatalf("挂起后栈深应为 1，实际 %d", d)
	}
	// sourceOf 要能从任务/帧里取出可辨识来源（qq），而不是空串或 task#N。
	if got := sourceOf(lt, nil); got != "qq" {
		t.Fatalf("sourceOf(lt) = %q，期望 qq", got)
	}
}

func TestSourceOf_SelfAndNilAreSafe(t *testing.T) {
	if got := sourceOf(nil, nil); got != "?" {
		t.Fatalf("nil -> %q", got)
	}
	st := newSelfTask(selfInputMsg{text: "x", channel: "cli"})
	if got := sourceOf(st, nil); got != "self:cli" {
		t.Fatalf("self task -> %q", got)
	}
}

// 抢占日志必须把「入侵者」与「受害者」分开写，且受害者是**真正在跑的那个**。
//
// 镇的是一个自伤：第一版把日志打在 executeNewTask 里，而那时 nextRef 已经把
// s.running 换成了抢占者自己，于是日志写成 "victim = 入侵者"（实测输出过
// `preempt start: task#2 ... -> victim task#2 (cli)`）。判据必须落在
// registerInterrupt —— 那一刻 running 还没被换。
func TestSourceOf_DistinctVictimAndIntruder(t *testing.T) {
	if sourceOf(newInputTask(&agentIO.InputEvent{Source: "qq"}), nil) != "qq" {
		t.Fatal("queued qq 任务的来源应为 qq")
	}
	it := newInterruptTask(&agentIO.InputEvent{Source: "homeagent-mail-bridge"}, LevelMessage)
	if got := sourceOf(it, nil); got != "homeagent-mail-bridge" {
		t.Fatalf("中断来源应为 homeagent-mail-bridge，实际 %q", got)
	}
	if it.Class != TaskInterrupt || it.Level != LevelMessage {
		t.Fatalf("中断任务应 class=interrupt level=2，实际 %v/%v", it.Class, it.Level)
	}
	// 邮件若走排队注入则会变成 TaskQueued —— 那种情况下 canPreempt 必为假。
	q := &Task{Class: TaskQueued, Kind: TaskKindInput,
		Event: &agentIO.InputEvent{Source: "homeagent-mail-bridge"}}
	if canPreempt(q, newInputTask(&agentIO.InputEvent{Source: "qq"})) {
		t.Fatal("排队输入永远不得抢占另一个排队任务")
	}
}
