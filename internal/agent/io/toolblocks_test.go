package io

import (
	"strconv"
	"sync"
	"testing"
)

// 阶段 2b：`ConsumeToolBlocks` 从 **IOManager 级单队列** 改为 **per-call**。
//
// 现状（channel.go:1010-1021）：SetToolBlocks 写入一个共享的
// toolPendingBlocks，ConsumeToolBlocks 取走并清空。它是**全局单槽**，
// 没有 call_id 维度。
//
// 并行化的直接后果：同批多个工具各自注入媒体时，后执行的
// ConsumeToolBlocks 会**抢走**前一个的块 ⇒ 媒体挂到错误的 tool 消息上。
// 而多模态插件在 3 处调用 SetToolBlocks（multimodal/plugin.go:136,246,320），
// 这是真实使用面。
//
// 这条判据钉死「谁注入的归谁」——当前实现必然失败。

// 并发：两个工具各自注入媒体，各自取回，要求**互不串味**。
//
// 不用共享 map 收集结果：goroutine 内写 map 需要额外同步，而按
// 「各 goroutine 只写自己的下标」写切片即可，无需任何锁。
func TestConsumeToolBlocks_PerCallIsolation(t *testing.T) {
	m := NewIOManager()

	// 模拟两个工具按并行序注入：alpha 先注入，beta 后注入，
	// 但取回顺序不定（调度决定，这正是并行下的真实情况）。
	m.SetToolBlocksFor("call_alpha", []interface{}{"alpha-block"})
	m.SetToolBlocksFor("call_beta", []interface{}{"beta-block"})

	var alpha, beta []interface{}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		alpha = m.ConsumeToolBlocksFor("call_beta")
	}()
	go func() {
		defer wg.Done()
		beta = m.ConsumeToolBlocksFor("call_alpha")
	}()
	wg.Wait()

	// alpha 的槽里装的应是 beta 抢到的结果？不——上面故意交叉，
	// 目的是让两个 goroutine 真正并发取回；此处只断言**各自取到的内容对得上**。
	if len(alpha) != 1 || alpha[0] != "beta-block" {
		t.Errorf("取 call_beta 的 goroutine 拿到 %#v，期望 [beta-block]", alpha[0])
	}
	if len(beta) != 1 || beta[0] != "alpha-block" {
		t.Errorf("取 call_alpha 的 goroutine 拿到 %#v，期望 [alpha-block]", beta[0])
	}
}

// 取走是**消费**语义：同一 call_id 取第二次必须为空。
func TestConsumeToolBlocksFor_IsConsuming(t *testing.T) {
	m := NewIOManager()
	m.SetToolBlocksFor("c1", []interface{}{"x"})
	if b := m.ConsumeToolBlocksFor("c1"); len(b) != 1 {
		t.Fatalf("首次取回应得 1 块，实际 %#v", b)
	}
	if b := m.ConsumeToolBlocksFor("c1"); len(b) != 0 {
		t.Errorf("二次取回应为空（消费语义），实际 %#v", b)
	}
}

// 未知 call_id 取回必须为空，且**不得**影响他人的块。
func TestConsumeToolBlocksFor_UnknownCallIsEmpty(t *testing.T) {
	m := NewIOManager()
	m.SetToolBlocksFor("c1", []interface{}{"mine"})
	if b := m.ConsumeToolBlocksFor("no_such_call"); len(b) != 0 {
		t.Errorf("未知 call 应返回空，实际 %#v", b)
	}
	if b := m.ConsumeToolBlocksFor("c1"); len(b) != 1 {
		t.Errorf("取未知 call 误伤了 c1 的块: %#v", b)
	}
}

// 并发压力：N 个 call 各自注入并取回，要求**零串味**。
// 单槽实现在此必然大面积失败——这正是判据的价值。
func TestConsumeToolBlocksFor_ConcurrentNoCrossTalk(t *testing.T) {
	m := NewIOManager()
	const n = 32

	// ★ 每个 goroutine 必须用**唯一**的 id（2026-11-04 修）。
	//
	// 原来写的是 `id := string(rune('A' + i%26))` —— n=32 而字母只有 26 个，
	// 于是 i=0 与 i=26 都得到 'A'，6 个 id 各被两个 goroutine 共用。
	// 两者共享同一个 key：后 Set 的覆盖先 Set 的、先 Consume 的拿到空或
	// 别人的值 ⇒ 判据约 1/8 概率假红。
	//
	// 实测该失败在**本次改动之前**的基线上同样复现（12 次内中 1 次），
	// 报错 `call E 期望 [block-4-E]，实际 []`。这是**测试自身的缺陷**，
	// 不是 SetToolBlocksFor / ConsumeToolBlocksFor 的并发缺陷 ——
	// 两个 goroutine 抢同一个 key 本就该串话（那是覆盖语义，不是串话）。
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		// id 唯一 ⇒ 每个调用有自己独立的 key，断言才有意义。
		id := "call-" + strconv.Itoa(i)
		want := "block-" + strconv.Itoa(i%10) + "-" + id
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.SetToolBlocksFor(id, []interface{}{want})
			b := m.ConsumeToolBlocksFor(id)
			if len(b) != 1 || b[0] != want {
				errs <- "call " + id + " 期望 [" + want + "]，实际 " + toStr(b)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestConsumeToolBlocksFor_KeysAreIsolated 钉住"不同 key 互不干扰"这条契约。
//
// ## 为什么单开一条（2026-11-04）
//
// 上面那条并发判据原先用 `i%26` 生成 id，n=32 ⇒ 6 个 id 重复，两个 goroutine
// 抢同一个 key ⇒ 约 1/8 概率假红。它红了半年没人管，因为"偶发"通常被当成
// 环境问题。
//
// 根因是判据**自己违反了它想验证的前提**：想测"不同 key 不串话"，
// 却让多个 goroutine 共用 key。
//
// 这条判据把前提本身钉住：一次性写入 N 个**互不相同**的 key，
// 逐个读回必须各得其所。它不涉及并发，所以失败一定是真缺陷。
func TestConsumeToolBlocksFor_KeysAreIsolated(t *testing.T) {
	m := NewIOManager()
	const n = 64
	want := make([]string, n)
	for i := 0; i < n; i++ {
		id := "call-" + strconv.Itoa(i)
		want[i] = "block-" + strconv.Itoa(i)
		m.SetToolBlocksFor(id, []interface{}{want[i]})
	}
	for i := 0; i < n; i++ {
		id := "call-" + strconv.Itoa(i)
		got := m.ConsumeToolBlocksFor(id)
		if len(got) != 1 || got[0] != want[i] {
			t.Fatalf("★ key %s 期望 [%s]，实际 %s ——\n"+
				"不同 key 之间串话了（或写入被覆盖）", id, want[i], toStr(got))
		}
	}
}

// TestConsumeToolBlocksFor_ConsumeIsOneShot 钉住消费语义：读一次后清空。
func TestConsumeToolBlocksFor_ConsumeIsOneShot(t *testing.T) {
	m := NewIOManager()
	m.SetToolBlocksFor("c", []interface{}{"b"})
	if got := m.ConsumeToolBlocksFor("c"); len(got) != 1 {
		t.Fatalf("首次消费应得 1 条，实际 %d", len(got))
	}
	if got := m.ConsumeToolBlocksFor("c"); len(got) != 0 {
		t.Fatalf("★ 二次消费应得空（block 是一次性的），实际 %s", toStr(got))
	}
}

func toStr(b []interface{}) string {
	s := "["
	for i, v := range b {
		if i > 0 {
			s += " "
		}
		s += v.(string)
	}
	return s + "]"
}
