package proc

import (
	"context"
	"sync"
	"testing"
	"time"
)

// 状态机的基本性质：只能前进，不可回退。
func TestCallState_TransitionsAreMonotonic(t *testing.T) {
	c := newCall(1, "tool.invoke")
	if c.State() != callPending {
		t.Fatalf("初始应为 pending，实际 %s", c.State())
	}
	if !c.transitionTo(callPending, callCanceled) {
		t.Fatal("pending→canceled 应成功")
	}
	if c.State() != callCanceled {
		t.Fatalf("应为 canceled，实际 %s", c.State())
	}
	// 回退必须失败：canceled 之后不能变回 pending，也不能被当成"又一次取消"。
	if c.transitionTo(callPending, callDone) {
		t.Fatal("canceled 不得回退到 pending")
	}
	if c.transitionTo(callPending, callCanceled) {
		t.Fatal("重复取消不应再次赢得迁移（否则会重复发 cancel 通知）")
	}
	// canceled → done：允许（迟到应答到达时记录结果已到）。
	if !c.transitionTo(callCanceled, callDone) {
		t.Fatal("canceled→done 应成功（迟到应答仍要有归属）")
	}
	// 之后再无迁移可能。
	if c.transitionTo(callDone, callFailed) {
		t.Fatal("done 之后不得再迁移")
	}
}

// 并发竞争：无论应答与取消谁先到，最终状态必须是二者之一，且迁移只能成功一次。
//
// 这条守的是"取消与应答赛跑"这个真实场景（插件可能刚好在 cancel 生效前跑完）。
func TestCallState_ConcurrentTransitionWinsOnce(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := newCall(uint64(i), "tool.invoke")
		var wins int32
		var mu sync.Mutex
		var wg sync.WaitGroup
		// 一个 goroutine 声称"完成"，另一个声称"取消"。
		for _, to := range []callState{callDone, callCanceled} {
			wg.Add(1)
			go func(to callState) {
				defer wg.Done()
				if c.transitionTo(callPending, to) {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}(to)
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("第 %d 轮：应恰好一方赢得 pending 迁移，实际 %d 方", i, wins)
		}
	}
}

// 取消后到达的应答**必须能被认出来**，而不是被当成"未知 id"。
//
// 这是本次改造的核心判据。旧实现取消时直接 delete(pending, id)，
// 于是迟到的应答只能走"找不到 id ⇒ 丢弃 + 打日志"，它无法说明这条应答
// 属于哪次调用、那次调用为什么结束。现在状态保留，因此可以精确归类。
//
// 变异：把 abandon 改回 delete(p.inflight, id)（不迁移状态）
// ⇒ 本判据变红（应答会被判为"未知 id"）。
func TestProcess_CanceledCallStillOwnsItsLateReply(t *testing.T) {
	p := startKillablePlugin(t)
	defer p.Kill()

	// ★ 必须走**真实的** abandon 路径，手工构造状态会绕过被检验的代码。
	//
	// 第一版我就是手工 transitionTo + registerCallForTest，结果
	// "变异：abandon 摘表" 依然全绿——测试根本没覆盖 abandon。
	c := newCall(p.allocCallID(t), "tool.invoke")
	p.registerCallForTest(c)
	p.abandon(c, context.Canceled) // ← 被检验的就是它摘不摘表
	if c.State() != callCanceled {
		t.Fatalf("abandon 后应为 canceled，实际 %s", c.State())
	}

	// 插件侧此时才回一个应答。按新逻辑：id 仍在表里、状态是 canceled，
	// 应答应被识别（canceled→done）而不是报"未知 id"。
	delivered := p.deliverResponseForTest(&Response{ID: c.id, Result: []byte(`"late"`)})
	if !delivered {
		t.Fatal(`取消后的迟到应答应被识别为属于该调用，而不是「未知 id」`)
	}
	if c.State() != callDone {
		t.Fatalf("迟到应答应把状态推进到 done，实际 %s", c.State())
	}
}

// 已经结束并摘表的调用再来应答，才是真正的未知 id。
func TestProcess_UnknownIDIsReported(t *testing.T) {
	p := startKillablePlugin(t)
	defer p.Kill()

	if p.deliverResponseForTest(&Response{ID: 999999}) {
		t.Fatal("表里没有的 id 不应被投递")
	}
}

// ---- 测试辅助 ----

func (p *Process) allocCallID(t *testing.T) uint64 {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inflight == nil {
		p.inflight = make(map[uint64]*call)
	}
	p.nextID++
	return p.nextID
}

func (p *Process) registerCallForTest(c *call) {
	p.mu.Lock()
	if p.inflight == nil {
		p.inflight = make(map[uint64]*call)
	}
	p.inflight[c.id] = c
	p.mu.Unlock()
}

// deliverResponseForTest 直接走**真实的**分发实现（deliverResponse）。
//
// 第一版我在这里复制了一份分发逻辑，结果 onCancelHonored 只在真路径里
// 被调用，测试却显示通过——假绿。现在只做一行转发，保证测的就是线上代码。
func (p *Process) deliverResponseForTest(resp *Response) bool {
	before := p.inflightCountForTest()
	p.deliverResponse(resp)
	return p.inflightCountForTest() < before
}

func (p *Process) inflightCountForTest() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inflight)
}

func startKillablePlugin(t *testing.T) *Process {
	t.Helper()
	bin := buildTestPlugin(t, "echoplugin.go")
	p, err := Spawn("state", bin, Options{Handler: noopHandler})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = p.Kill() })
	// 等它就绪，避免后续测试与握手竞争。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.CallContext(ctx, "plugin.init", map[string]interface{}{"name": "state"}); err != nil {
		// echoplugin 可能不支持 plugin.init，忽略——本测试只用表状态。
		t.Logf("plugin.init（可忽略）: %v", err)
	}
	return p
}

// 宽限期到期后，已取消且对端始终不回答的条目必须被回收。
//
// 守的是"取消频繁 + 对端从不回答"这个组合：条目若永久保留，
// 内存会随取消次数单调增长。
//
// 这里把宽限期临时缩短到 ms 级——否则测试要等一分钟。
func TestProcess_CanceledCallReclaimedAfterGrace(t *testing.T) {
	p := startKillablePlugin(t)

	c := newCall(p.allocCallID(t), "tool.invoke")
	p.registerCallForTest(c)
	p.abandon(c, context.Canceled)

	// 宽限期内应仍在表里（这样迟到的应答才认得出归属）。
	if !p.hasInflight(c) {
		t.Fatal("取消后应立即保留条目（供迟到应答认领）")
	}

	// 直接触发回收：等价于宽限期到期时 time.AfterFunc 做的事。
	p.retire(c, callFailed)

	if p.hasInflight(c) {
		t.Fatal("宽限期后条目必须被回收，否则内存随取消次数单调增长")
	}
	if c.State() != callFailed {
		t.Fatalf("回收后应为 failed，实际 %s", c.State())
	}
}

func (p *Process) hasInflight(c *call) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight[c.id] == c
}
