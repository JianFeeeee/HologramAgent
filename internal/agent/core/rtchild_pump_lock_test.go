package core

// 判据：rtChild.pump 的读写必须走 rc.mu。
//
// ## 守的是什么（2026-11-04 实测 data race）
//
//	pump 是 rtChild 里**唯一**一个"构造后被改写、又有并发读者"的字段：
//
//	    Dispose（StopRTChild 的 goroutine）  写 rc.pump = nil
//	    rtEmit （rtLoop 的 goroutine）        读 rc.pump
//
// 两者无同步 ⇒ -race 报 DATA RACE，栈是：
//
//	    Read at … core.(*Agent).rtEmit      rtchild.go:1275
//	    Previous write … core.(*rtChild).Dispose  rtchild.go:377
//
// ## 为什么必须有这条判据（e2e 不够）
//
// TestRTChild_E2E_Sustained 覆盖了同一个窗口，但它是**偶发命中**：
// 实测全量负载下 5 次里中 1 次，单跑 20 次 0 次。这种判据在 CI 里
// 等于没有 —— 它红了是运气，绿了也不代表安全。
//
// 所以这里直接构造**最高并发压力**：一边不停 rtEmit，一边 Dispose，
// 用 -race 判定。它不依赖时序巧合。
//
// ## 判据的边界（诚实说明）
//
// 这条判据的"红"依赖 -race 检测器。不带 -race 时它只能发现
// panic / 死锁，不能发现数据竞争。所以必须与 `go test -race` 一起跑
// —— 本仓的提交纪律第 4 条已经要求全量 -race，这里只是把
// 「哪个窗口最危险」钉成可复跑的形态。

import (
	"sync"
	"testing"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// TestRTPumpAccessIsLocked 用并发压力 + -race 钉住加锁。
//
// 变异：把 rtEmit 里的 rc.mu.Lock/Unlock 去掉（直接读 rc.pump）
// 或把 Dispose 里的置 nil 移出锁 ⇒ 本判据在 -race 下红。
func TestRTPumpAccessIsLocked(t *testing.T) {
	dev := &mockOutputDevice{
		name: "rtpumpdev",
		caps: agentIO.CapText,
		toolFn: func(string, map[string]interface{}) (interface{}, error) {
			return "ok", nil
		},
	}
	rc := &rtChild{
		Child:  nil, // 本判据只碰 pump，不经过骨架
		output: "rtpumpdev",
	}
	// 给 rtEmit 用的 Agent（它读 a.io 拿设备）。
	a := New(AgentConfig{
		ID:        "rtpumpagent",
		IO:        agentIO.NewIOManager(),
		StageHost: NewStageHost(),
	})
	if err := a.io.RegisterDevice(dev); err != nil {
		t.Fatalf("注册设备: %v", err)
	}
	rc.agent = a
	rc.mu.Lock()
	rc.pump = newOutputPump("rtpumpdev", func() agentIO.Device {
		return a.io.GetDevice("rtpumpdev")
	})
	rc.mu.Unlock()

	// ★ 读者必须**跑满整个写者窗口**，不能跑固定次数就先结束。
	//
	// 第一版是 `for i := 0; i < 500; i++`：500 次 emit 在微秒级就跑完，
	// 而写者 sleep 1ms 才动手 ⇒ 两者根本不重叠。实测变异验证时，
	// 把 Dispose 的置 nil 移出锁（裸写）**连跑 6 次都不红** —— 判据是假的。
	//
	// 现在改成"持续跑到写者宣布结束"，保证读写真正交错。
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				a.rtEmit(rc, "x")
			}
		}
	}()

	// 写者：等读者进入循环后 Dispose（pump 置 nil + Close）。
	time.Sleep(2 * time.Millisecond)
	rc.Dispose()
	// Dispose 之后再放一小段：此时读者仍可能读 rc.pump（应读到 nil 或被锁挡住），
	// 这一小段正是"裸写会与读者撞车"的窗口。
	time.Sleep(2 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestRTPumpAfterDisposeIsNil 钉住 Dispose 之后 emit 不再推送到已关的 pump。
//
// 这是加锁的**语义**副作用验证：只加锁但先 Close 后置 nil，
// 读者仍可能拿到一个已关闭的 pump 去 Push。顺序必须"取走并置 nil，
// 再在锁外 Close"。
func TestRTPumpAfterDisposeIsNil(t *testing.T) {
	a := New(AgentConfig{
		ID:        "rtpumpagent2",
		IO:        agentIO.NewIOManager(),
		StageHost: NewStageHost(),
	})
	dev := &mockOutputDevice{name: "d2", caps: agentIO.CapText}
	if err := a.io.RegisterDevice(dev); err != nil {
		t.Fatalf("注册设备: %v", err)
	}
	rc := &rtChild{agent: a, output: "d2"}
	rc.mu.Lock()
	rc.pump = newOutputPump("d2", func() agentIO.Device { return a.io.GetDevice("d2") })
	rc.mu.Unlock()

	rc.Dispose()

	rc.mu.Lock()
	p := rc.pump
	rc.mu.Unlock()
	if p != nil {
		t.Fatal("★ Dispose 之后 rc.pump 仍非 nil —— emit 会推送到已关闭的 pump")
	}
	// 兜底路径必须能工作（不 panic）。
	a.rtEmit(rc, "after-dispose")
}
