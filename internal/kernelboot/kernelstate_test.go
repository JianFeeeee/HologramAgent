package kernelboot

// KernelState（公共数据结构体）的判据。
//
// ## ★ 这个文件为什么是被重建的（一个真实的覆盖漏洞）
//
// `CloseAll` 的逆序判据原先在 `bootctx_test.go` 里。我后来用 `KernelState`
// 取代 `BootCtx` 时**删掉了那个文件**，而新文件里**没有**补回这条判据。
//
// 是**变异测试**发现的：把 `CloseAll` 改成正序执行，测试**全绿**
// （没有任何判据在检查它）⇒ 覆盖漏洞。
//
// 教训：删文件/重写实现时，必须**逐条核对原判据是否都补回了**
// （判据的"数量"不是指标，"每条原判据都有对应"才是）。

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// ★★★ 判据：**CloseAll 逆序执行**（LIFO，与 defer 一致）。
//
// 为何逆序：先初始化的一般是底层（config → memory → llm），
// 停机应**反向拆**，否则可能拆掉别人还在用的东西。
func TestKernelStateCloseAllIsReverseOrder(t *testing.T) {
	ks := NewKernelState()
	var mu sync.Mutex
	var order []string

	for _, name := range []string{"config", "memory", "llm"} {
		n := name
		ks.AddCloser(func() {
			mu.Lock()
			order = append(order, n)
			mu.Unlock()
		})
	}
	if err := ks.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	want := []string{"llm", "memory", "config"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("★ CloseAll 应逆序（LIFO，与 defer 一致）: 期望 %v 实际 %v", want, order)
	}
}

// ★★★ 判据：**CloseAll 不因单个关闭函数 panic 而中断**。
func TestKernelStateCloseAllSurvivesPanic(t *testing.T) {
	ks := NewKernelState()
	closed := false
	ks.AddCloser(func() { panic("关闭时炸了") })
	ks.AddCloser(func() { closed = true })

	_ = ks.CloseAll()
	if !closed {
		t.Fatal("★ 一个关闭函数 panic 导致其他模块没被关闭（资源泄漏）")
	}
}

// ★★★ 判据：**CloseAll 幂等**（清空后再次调用不重复执行）。
func TestKernelStateCloseAllIsIdempotent(t *testing.T) {
	ks := NewKernelState()
	var n int32
	ks.AddCloser(func() { atomic.AddInt32(&n, 1) })

	_ = ks.CloseAll()
	_ = ks.CloseAll() // 第二次不应再执行
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("★ 关闭函数被执行 %d 次（应恰好 1 次）", got)
	}
}

// ★★★ 判据：**AddCloser 并发安全**（同层模块并行 Init 都会调它）。
//
// ★ 这条是压力测试+race 抓到的**真缺陷**的回归判据：
// 第 1 层有 8 个模块并行，其中 5 个注册关闭函数 ⇒ 并发 append
// ⇒ 数据竞争 + 可能丢失元素（某个模块的关闭函数永不调用 ⇒ 资源泄漏）。
func TestKernelStateAddCloserConcurrent(t *testing.T) {
	ks := NewKernelState()
	const n = 500

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ks.AddCloser(func() {})
		}(i)
	}
	wg.Wait()

	if len(ks.Closers) != n {
		t.Fatalf("★ 并发 AddCloser 丢失了关闭函数：应 %d 个，实际 %d（slice append 竞争）",
			n, len(ks.Closers))
	}
}

// ★★★ 判据：**nil 关闭函数被忽略**（不 panic、不占位）。
func TestKernelStateAddCloserIgnoresNil(t *testing.T) {
	ks := NewKernelState()
	ks.AddCloser(nil)
	if len(ks.Closers) != 0 {
		t.Fatalf("nil 关闭函数应被忽略，实际记录 %d 个", len(ks.Closers))
	}
	if err := ks.CloseAll(); err != nil {
		t.Fatalf("空表 CloseAll 应成功: %v", err)
	}
}
