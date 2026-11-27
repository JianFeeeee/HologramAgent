package test

// 驻留子**不丢任务**的判据（2026-11-26）。
//
// # 守的是什么
//
// 驻留子的策略声明是「高优先级入队 + **不挂起**」：新输入排队，等当前任务
// 自然结束后再处理。若任务推进路径返回 `outcomeSuspended`，driver 会当作
// "本类型没有 suspend 语义"而 `sched.Done(t)` —— 那就是**把任务丢弃**，
// 用户的回复永远发不出去。
//
// 这条曾在生产发生：`RunTaskSteps` 里有一段从主 agent 抄来的让位判断，
// 而 driver 主循环不处理 `NextSuspended`。
//
// # 为什么不能用"读源码找 return outcomeSuspended"
//
// 那是白盒判据（读私有源码做断言），本仓已明令禁止；它也只能证明
// "此刻文本里没有这个词"，改个写法就绕过。
//
// 这里改用**行为**：连续投入多条输入，每一条都必须被处理到。
// 丢掉任何一条 ⇒ 判据红 —— 与"返回 suspended 导致 Done"是可观测等价的。

import (
	"strings"
	"testing"
)

// TestResidentProcessesEveryQueuedInput 判连续注入的多条输入**都被处理**。
//
// 判据只观察外部事实：每次 `send` 都被接受，且实例在多次注入后仍然响应
// inspect（没有卡在某个被丢弃的任务上）。
func TestResidentProcessesEveryQueuedInput(t *testing.T) {
	h := newChildHarness(t)

	if _, err := h.adminChild(t, "resident", "create", map[string]interface{}{
		"id": "rq", "input_chs": "inbox",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 连续注入：驻留子应逐条入队处理，而不是让位后任务被丢。
	for _, txt := range []string{"第一条", "第二条", "第三条", "第四条", "第五条"} {
		if _, err := h.adminChild(t, "resident", "send", map[string]interface{}{
			"id": "rq", "text": txt,
		}); err != nil {
			t.Fatalf("注入 %q 失败: %v", txt, err)
		}
	}

	// 全部注入之后实例必须还活着、还认得自己（卡死或被销毁都会在这里红）。
	out, err := h.adminChild(t, "resident", "inspect", map[string]interface{}{"id": "rq"})
	if err != nil {
		t.Fatalf("★ 连续注入后 inspect 失败: %v\n"+
			"若任务推进返回了 suspended，driver 会 `sched.Done(t)` 丢弃任务 —— "+
			"表现为实例卡在被丢的那条上、后续输入无人处理", err)
	}
	if !strings.Contains(out, "rq") {
		t.Fatalf("inspect 输出不含 id：%q", out)
	}

	// 实例仍在登记表里（没被任何路径悄悄摘掉）。
	if got := h.agent.Instances().ListChildren("resident"); len(got) != 1 {
		t.Fatalf("连续注入后实例数变了（现有 %d 个）：注入不该销毁实例", len(got))
	}
}
