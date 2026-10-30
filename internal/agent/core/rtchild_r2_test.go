package core

// R2 判据：rtSlot 单槽覆盖语义（plan.md 十一.五）。
//
// 三条判据分别钉住三件事，删掉对应实现任一条都会红：
//  1. 新输入覆盖旧输入（旧值**作废**，不保留、不补偿）—— 契约 1
//  2. 永不排队（连续 N 次 Push 后待处理恒为 1）—— 契约 3
//  3. 覆盖是**有统计**的（dropped 单调前进）—— 否则"打断生效了几次"不可观测

import (
	"fmt"
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

func rtEvent(content string) *agentIO.InputEvent {
	return &agentIO.InputEvent{
		Source:  "voice",
		Type:    "text",
		Payload: map[string]interface{}{"content": content},
	}
}

func rtContent(evt *agentIO.InputEvent) string {
	if evt == nil || evt.Payload == nil {
		return ""
	}
	s, _ := evt.Payload["content"].(string)
	return s
}

// TestRTSlot_NewInputReplacesOld 钉住「最新者胜」：后到的输入**作废旧值**。
//
// 若把 Push 改成"入队"（append 到 slice），本判据必然红：Take 会先取到旧值。
func TestRTSlot_NewInputReplacesOld(t *testing.T) {
	s := newRTSlot()
	s.Push(rtEvent("第一句"))
	s.Push(rtEvent("第二句"))

	evt, _, ok := s.Take()
	if !ok {
		t.Fatal("槽里应有待处理输入")
	}
	if got := rtContent(evt); got != "第二句" {
		t.Fatalf("取到的输入 = %q，期望 %q —— 最新者胜未生效（旧值竟然先出队）",
			got, "第二句")
	}
	// 取走后必须清空：否则 rtLoop 会反复拿到同一条输入重跑同一轮。
	if _, _, ok := s.Take(); ok {
		t.Fatal("Take 之后槽未清空 —— rtLoop 会重复处理同一条输入")
	}
}

// TestRTSlot_NeverQueues 钉住「不吃队列」：无论 Push 多少次，待处理恒为 1 条。
//
// 这是契约 3 的直接判据。若实现改成有界队列（哪怕容量 8），
// 连续 Push 100 次后 Take 会连续取出 100 条 —— 本判据立刻红。
func TestRTSlot_NeverQueues(t *testing.T) {
	const n = 100
	s := newRTSlot()
	for i := 0; i < n; i++ {
		s.Push(rtEvent(fmt.Sprintf("第%d句", i)))
	}

	// 只应该能取出**一条**（最后那条）。
	evt, _, ok := s.Take()
	if !ok {
		t.Fatal("槽里应有待处理输入")
	}
	if got, want := rtContent(evt), fmt.Sprintf("第%d句", n-1); got != want {
		t.Fatalf("取到 %q，期望最后一条 %q", got, want)
	}
	if _, _, ok := s.Take(); ok {
		t.Fatalf("连续 Push %d 次后能取出第二条 —— 这不是单槽，是队列（违反契约 3）", n)
	}
}

// TestRTSlot_CountsDropped 钉住可观测性：被覆盖的输入必须计数。
//
// 为什么这是判据而不只是日志：rt 场景下"打断"是常态，但如果
// dropped 恒为 0，说明要么插件攒批没生效、要么输入速率极低 ——
// 两种情况下阶段二的延迟数据都不可信。所以它必须能被读到。
func TestRTSlot_CountsDropped(t *testing.T) {
	s := newRTSlot()
	if got := s.Dropped(); got != 0 {
		t.Fatalf("初始 dropped = %d，期望 0", got)
	}
	s.Push(rtEvent("a")) // 第一次不丢弃（槽是空的）
	if got := s.Dropped(); got != 0 {
		t.Fatalf("首次 Push 不应计丢弃，得到 %d", got)
	}
	s.Push(rtEvent("b")) // 覆盖 a ⇒ +1
	s.Push(rtEvent("c")) // 覆盖 b ⇒ +1
	if got := s.Dropped(); got != 2 {
		t.Fatalf("dropped = %d，期望 2（两次覆盖）", got)
	}

	// 取走之后槽为空 ⇒ 下一次 Push 不算覆盖。
	s.Take()
	s.Push(rtEvent("d"))
	if got := s.Dropped(); got != 2 {
		t.Fatalf("取空后 Push 不应计丢弃，dropped = %d", got)
	}
}

// TestRTSlot_SeqMonotonicAndMatchesTake 钉住 seq 单调递增且 Take 取到的是**最新 seq**。
//
// seq 是契约 1 的可验证凭据（plan.md 十一.四 空白 3）：
// 输出要回传它，判据才能证明"这一轮是用最新输入跑的，而不是旧输入的残余"。
func TestRTSlot_SeqMonotonicAndMatchesTake(t *testing.T) {
	s := newRTSlot()
	var last uint64
	for i := 0; i < 5; i++ {
		seq := s.Push(rtEvent(fmt.Sprintf("v%d", i)))
		if seq <= last {
			t.Fatalf("seq 未单调递增：%d 之后是 %d", last, seq)
		}
		last = seq
	}
	_, seq, ok := s.Take()
	if !ok {
		t.Fatal("应有待处理")
	}
	if seq != last {
		t.Fatalf("Take 返回 seq=%d，期望最新 %d —— 取到的不是最新输入", seq, last)
	}
}

// TestRTSlot_WakeIsNonBlocking 钉住唤醒是**电平**语义：
// 槽里已有信号时重复 Push 不得阻塞（cap=1，非阻塞写）。
//
// 若把 wake 改成无缓冲通道或阻塞写，本判据会因 Push 卡死而超时。
func TestRTSlot_WakeIsNonBlocking(t *testing.T) {
	s := newRTSlot()
	// 连推多次：第一次填满 wake(cap=1)，后续必须走 default 分支返回。
	for i := 0; i < 10; i++ {
		s.Push(rtEvent(fmt.Sprintf("w%d", i)))
	}
	// 至少要能收到一次唤醒信号。
	select {
	case <-s.Wake():
	default:
		t.Fatal("Push 之后 Wake 从未就绪 —— rtLoop 会永久阻塞（待机醒不来）")
	}
}
