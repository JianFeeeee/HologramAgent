package core

// R11 判据：待机零消耗 + 长时 e2e（契约 4 / 契约 1 的持续正确性）。
//
//	契约 4：无输入即静默，**不烧轮次、不烧 token、不轮询**
//
// "不轮询"这条容易被忽略但同样重要：轮询会烧 CPU，且在静默期产生
// 无意义的唤醒。实现上 rtLoop 必须 block 在 Wake() 上（见 rtchild.go）。

import (
	"context"
	"sync"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// rtCountingProvider 统计调用次数与总 token 量（近似：正文长度）。
type rtCountingProvider struct {
	mu    sync.Mutex
	calls int
	chars int
}

func (p *rtCountingProvider) Name() string          { return "rtCounting" }
func (p *rtCountingProvider) MaxContextTokens() int { return 8192 }
func (p *rtCountingProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &agentAPI.CompletionResponse{Content: "ok"}, nil
}
func (p *rtCountingProvider) ChatStream(_ context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	ch := make(chan agentAPI.StreamChunk, 2)
	ch <- agentAPI.StreamChunk{Content: "答"}
	p.mu.Lock()
	p.chars++
	p.mu.Unlock()
	ch <- agentAPI.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}
func (p *rtCountingProvider) stats() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.chars
}

// TestRTChild_IdleZeroTokens 钉住契约 4：**静默期零模型调用**。
//
// 若 rtLoop 改成轮询（select 带 default 或定时 tick），本判据会看到
// calls > 0 —— 那意味着空转烧钱。
func TestRTChild_IdleZeroTokens(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "idle")

	// 给 rtLoop 足够时间"如果它要空转的话"必然已经转了。
	time.Sleep(300 * time.Millisecond)

	if calls, _ := p.stats(); calls != 0 {
		t.Fatalf("静默 300ms 期间模型被调用 %d 次 —— 待机必须零消耗（契约 4）", calls)
	}
	// 槽也必须仍是空的（没有凭空产生输入）。
	if st, ok := host.RTChildStats(id); !ok {
		t.Fatal("实例不在登记表")
	} else if st.Pending {
		t.Fatal("静默期槽里出现了待处理输入")
	} else if st.Rounds != 0 {
		t.Fatalf("静默期跑了 %d 轮", st.Rounds)
	}

	// 再等一段，确认不是"刚启动还没轮到"。
	time.Sleep(200 * time.Millisecond)
	if calls, _ := p.stats(); calls != 0 {
		t.Fatalf("静默 500ms 累计调用 %d 次", calls)
	}
}

// TestRTChild_IdleStopsWhenInputArrives 是配套正例：来输入必须**真的**跑起来。
//
// 只有"静默零调用"这一半的判定条件是坏的 —— "永不调用模型"的实现也能通过，
// 但那是死进程。正例是必需品。
func TestRTChild_IdleStopsWhenInputArrives(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "wakeup")
	time.Sleep(100 * time.Millisecond)
	if calls, _ := p.stats(); calls != 0 {
		t.Fatalf("静默期不该有调用，得到 %d", calls)
	}

	host.PushRTInput(id, "你好")
	waitFor(t, "输入后跑了一轮", func() bool {
		calls, _ := p.stats()
		return calls >= 1
	})
}

// TestRTChild_E2E_Sustained 是长时 e2e：连续灌入输入，验证
//
//	· 每一批都被处理（不丢活）
//	· 无 goroutine 泄漏 / 无 panic
//	· history 持续增长（普通上下文）
//	· 全程 -race 干净（由调用方加 -race）
//
// 用 -short 跳过：默认用例集不该被一段秒级压测拖慢。
func TestRTChild_E2E_Sustained(t *testing.T) {
	if testing.Short() {
		t.Skip("长时 e2e；-short 跳过")
	}
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "e2e")

	// 定速灌入（模拟插件攒批后的节奏）。总数取 60：足够暴露累积问题，
	// 又不至于让默认用例集跑太久。
	const total = 60
	for i := 0; i < total; i++ {
		host.PushRTInput(id, "第 N 句话")
		// 给 rtLoop 一点时间消费；不追求"每条都跑完"，要的是压力。
		if i%10 == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}

	// 收尾：等最后一条被处理。
	waitFor(t, "所有输入都被消费", func() bool {
		st, ok := host.RTChildStats(id)
		return ok && !st.Pending
	})

	tr, ok := host.RTTranscript(id)
	if !ok {
		t.Fatal("取不到 transcript")
	}
	if len(tr) == 0 {
		t.Fatal("跑了多轮但 history 为空 —— 普通上下文没在累积")
	}
	calls, _ := p.stats()
	if calls == 0 {
		t.Fatal("全程零模型调用 —— rtLoop 没工作")
	}
	t.Logf("e2e: 灌入 %d 条，模型调用 %d 次，history %d 条", total, calls, len(tr))

	// 关闭必须干净。
	if err := host.StopRTChild(id); err != nil {
		t.Fatalf("停止: %v", err)
	}
	if got := len(host.RTChildren()); got != 0 {
		t.Fatalf("停止后仍有 %d 个实例", got)
	}
}

// TestRTChild_RapidInputCollapsesToLatest 钉住契约 1+3 的**联合语义**：
// 输入到达得比处理快时，只有最新一条会被处理，其余被覆盖丢弃。
//
// 这不是缺陷，是设计：rt 场景下"用户刚说的话"才是当前态，旧语音已经过期。
// 若有人把 rtSlot 改成队列（让每条都跑），本判据会看到 rounds > 1 —— 那时
// 通话会变成"逐条追赶"，用户说完很久机器才反应到，违反实时性。
//
// ★ 同时钉住 dropped 计数器：它是"打断真的发生了"的唯一可观测凭据。
func TestRTChild_RapidInputCollapsesToLatest(t *testing.T) {
	p := &rtCountingProvider{}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "collapse")
	time.Sleep(30 * time.Millisecond)

	// 一口气推 50 条（比 rtLoop 处理得快得多）。
	const burst = 50
	for i := 0; i < burst; i++ {
		host.PushRTInput(id, "连发")
	}
	waitFor(t, "槽被消费", func() bool {
		st, ok := host.RTChildStats(id)
		return ok && !st.Pending
	})

	st, ok := host.RTChildStats(id)
	if !ok {
		t.Fatal("实例不存在")
	}
	if st.Rounds > 3 {
		t.Fatalf("连发 %d 条却跑了 %d 轮 —— rtSlot 退化成了队列（契约 1/3：最新者胜、不排队）",
			burst, st.Rounds)
	}
	if st.Dropped == 0 {
		t.Fatal("连发 50 条但 dropped=0 —— 覆盖没有发生，或计数器没接线")
	}
	if st.Dropped+st.Rounds < burst/2 {
		t.Fatalf("dropped=%d rounds=%d，合计远小于连发数 %d —— 有输入凭空消失了（既没被处理也没计入丢弃）",
			st.Dropped, st.Rounds, burst)
	}
}
