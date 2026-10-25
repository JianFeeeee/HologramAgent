package proc

import (
	"bufio"
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// newHeadlessProcess 构造一个**不 spawn 真进程**的 Process，供状态机单测使用。
//
// stdin 接到 io.Discard：abandon 会真的发 call.cancel 通知，
// 没有写端就会 nil 解引用。这样测试仍然走**真实代码路径**
// （包括发通知这一步），而不是把它 stub 掉——后者会让"取消到底有没有发出去"
// 变得无法被覆盖。
func newHeadlessProcess(name string) *Process {
	return &Process{
		name:     name,
		stdin:    bufio.NewWriter(io.Discard),
		inflight: make(map[uint64]*call),
		exited:   make(chan struct{}),
	}
}

// 取消超时必须：① 强制回收该调用名下的扩容块；② 上报插件失去响应。
//
// 判据要区分两类超时（这是本设计的核心，2026-10-25 用户裁定）：
//
//	业务超时 = 任务跑太久     → 正常，不作为不稳定依据
//	取消超时 = 喊停之后还不停 → 异常，是插件卡死的直接证据
//
// 变异：删掉 expireCanceled 里的 ReclaimRefs 调用 ⇒ 块泄漏判据变红。
// 变异：不调 onCancelIgnored ⇒ 上报判据变红。
func TestProcess_CancelIgnoredReclaimsBlocksAndReports(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	var (
		reportedCalls atomic.Uint64
		reclaimed     atomic.Int64
	)
	p := newHeadlessProcess("probe")
	p.SetReclaimBlocks(func(refs []SharedRef) int {
		n := host.ReclaimRefs(refs)
		reclaimed.Add(int64(n))
		return n
	})
	p.OnCancelIgnored(func(callID uint64, grace time.Duration) {
		reportedCalls.Add(1)
	})

	// 造一次"插件申请了扩容块、随后被取消"的调用。
	gen := host.Generation()
	ref, err := host.Arena().Alloc(42, 4096, gen) // owner 42 = 插件
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	c := newCall(7, "tool.invoke")
	c.trackBlock(ref)
	p.registerCallForTest(c)

	if used, _ := host.Arena().Stats(); used == 0 {
		t.Fatal("前置：块应已占用")
	}

	p.abandon(c, context.Canceled)

	// 宽限期未到：不应回收（插件可能还在收尾）。
	if reclaimed.Load() != 0 {
		t.Fatal("宽限期内不应强制回收")
	}

	// 模拟宽限期到期。
	p.expireCanceled(c)

	// ① 块必须被强制回收 —— 否则"取消了却瞒下内存"，无日志只涨不落。
	if used, _ := host.Arena().Stats(); used != 0 {
		t.Fatalf("取消超时后 arena 应归零，实际 used=%d（块泄漏）", used)
	}
	if reclaimed.Load() == 0 {
		t.Fatal("应调用强制回收")
	}

	// ② 必须上报（上层据此判定不稳定并重启）。
	if reportedCalls.Load() != 1 {
		t.Fatalf("应上报 1 次取消无响应，实际 %d", reportedCalls.Load())
	}

	// ③ 状态收敛到 failed（这次调用确实没正常完成）。
	if c.State() != callFailed {
		t.Fatalf("状态应为 failed，实际 %s", c.State())
	}
}

// 插件在宽限期内停手（回了应答）→ 不上报，且连续计数应被清零。
func TestProcess_CancelHonoredDoesNotReport(t *testing.T) {
	var reported, honored atomic.Int64
	p := newHeadlessProcess("probe")
	p.OnCancelIgnored(func(uint64, time.Duration) { reported.Add(1) })
	p.OnCancelHonored(func(uint64) { honored.Add(1) })

	c := newCall(11, "tool.invoke")
	p.registerCallForTest(c)
	p.abandon(c, context.Canceled)

	// 插件随后回了应答（说明它收到 cancel 并停手了）。
	if !p.deliverResponseForTest(&Response{ID: c.id, Result: []byte(`"x"`)}) {
		t.Fatal("取消后的迟到应答应被认领")
	}

	if honored.Load() != 1 {
		t.Fatalf("应记为已响应 1 次，实际 %d", honored.Load())
	}

	// 宽限期后再触发：已收敛，不应再上报。
	p.expireCanceled(c)
	if reported.Load() != 0 {
		t.Fatal("已响应的调用不应被上报为无响应")
	}
}

// 连续两次取消无响应才重启；中间成功响应一次即清零。
//
// 这是用户明确要求的判据："注意是 cancel 态超时，超过两次说明插件本身
// 处于崩溃/不稳定态，自动重启对应插件"。
func TestPlugin_CancelIgnoredThreshold(t *testing.T) {
	p := &Plugin{name: "flaky"}
	var restarts atomic.Int64
	p.onCrash = func(name string, err error) { restarts.Add(1) }

	// 第 1 次：不重启（可能是收尾慢）。
	p.noteCancelIgnored(1, cancelGrace)
	if restarts.Load() != 0 {
		t.Fatal("第 1 次取消无响应不应重启")
	}

	// 第 2 次：达到阈值，重启。
	p.noteCancelIgnored(2, cancelGrace)
	if restarts.Load() != 1 {
		t.Fatalf("连续第 %d 次应触发重启，实际 %d", cancelIgnoredLimit, restarts.Load())
	}

	// 计数清零后重新开始（"连续"而非"累计"）。
	p.cancelIgnoredMu.Lock()
	p.cancelIgnored = 0
	p.cancelIgnoredMu.Unlock()
	p.noteCancelIgnored(3, cancelGrace)
	if restarts.Load() != 1 {
		t.Fatal("清零后第一次不应再次重启（判据是连续）")
	}
}
