package core

// 判据：rt 的输出调用是**异步**的（用户裁定 2026-11-04）。
//
// 「就是在正常输出的情况下套一个异步的壳，rtchild 的所有调用都应该是异步的」
//
// ## 这条缺陷的原貌
//
// rtEmit 曾直接 `dev.Execute(...)`，于是插件侧延迟（TTS / 网络编码 / 字幕渲染）
// **直接节流模型的 token 消费**：rtStream 每读一个 chunk 就等插件一次。
// 「不攒整段」（契约 2）的收益全被这里吃掉。
//
// ## 判据为什么必须测"时序关系"而不是"有没有 pump"
//
// 断言 `rc.pump != nil` 是结构判据，改回同步但保留字段时仍然绿
// （engineering-lessons.md §1.1）。所以这里测**可观测的时间差**：
// 模型吐完的时刻 必须早于 插件收完的时刻。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// pumpSlowDevice 是一个**慢**的输出设备：每片 sleep，模拟 TTS / 网络编码。
type pumpSlowDevice struct {
	name  string
	delay time.Duration

	mu    sync.Mutex
	texts []string
	// lastAt 是最后一次 Execute 完成的时刻（用于和"模型吐完"比较）。
	lastAt atomic.Int64
	calls  atomic.Int64
}

func (d *pumpSlowDevice) Name() string                   { return d.name }
func (d *pumpSlowDevice) Type() agentIO.DeviceType       { return agentIO.DeviceOutput }
func (d *pumpSlowDevice) Description() string            { return "slow output" }
func (d *pumpSlowDevice) Tools() []agentIO.ToolDef       { return nil }
func (d *pumpSlowDevice) Start() error                   { return nil }
func (d *pumpSlowDevice) Stop() error                    { return nil }
func (d *pumpSlowDevice) ChannelDef() agentIO.ChannelDef { return agentIO.ChannelDef{} }
func (d *pumpSlowDevice) OutputCapabilities() agentIO.OutputCapability {
	return agentIO.CapText | agentIO.CapRT
}

func (d *pumpSlowDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	d.calls.Add(1)
	time.Sleep(d.delay)
	d.mu.Lock()
	if s, ok := args["text"].(string); ok {
		d.texts = append(d.texts, s)
	}
	d.mu.Unlock()
	d.lastAt.Store(time.Now().UnixNano())
	return "ok", nil
}

func (d *pumpSlowDevice) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.texts...)
}

// burstProvider 一次性吐 n 片然后结束，并记录"吐完的时刻"。
type burstProvider struct {
	n      int
	doneAt atomic.Int64
	mu     sync.Mutex
	calls  int
}

func (p *burstProvider) Name() string          { return "burst" }
func (p *burstProvider) MaxContextTokens() int { return 8192 }
func (p *burstProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return &agentAPI.CompletionResponse{Content: "x"}, nil
}

// ★ doneAt 必须记在**最后一片被消费之后**，而不是 ChatStream 返回时。
//
// 第一版把 channel 预填满再记时间戳 ⇒ doneAt 是"函数返回时刻"，
// 与真正的"模型吐完"无关。于是变异验证（把 rtEmit 改回同步）**不会变红** ——
// 判据测的是一个恒真的量。这是 engineering-lessons.md §1.1
// 「结构判据不能替代质量判据」的又一例。
//
// 现在用无缓冲 channel + goroutine 逐片发送，最后一片**送出后**才记时间戳，
// 这样 doneAt 反映的是"消费侧真正读走最后一片"的时刻。
func (p *burstProvider) ChatStream(ctx context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	ch := make(chan agentAPI.StreamChunk)
	go func() {
		defer close(ch)
		for i := 0; i < p.n; i++ {
			select {
			case <-ctx.Done():
				return
			case ch <- agentAPI.StreamChunk{Content: "片"}:
			}
		}
		select {
		case <-ctx.Done():
			return
		case ch <- agentAPI.StreamChunk{Done: true}:
		}
		// 最后一片（Done）已被消费方读走 ⇒ 这才是"模型吐完"的真实时刻。
		p.doneAt.Store(time.Now().UnixNano())
	}()
	return ch, nil
}

// TestRTChild_OutputIsAsync 是本阶段的核心判据。
//
// 慢设备（每片 40ms）× 8 片 = 串行需 ≥320ms。
// 断言：模型"吐完"的时刻 早于 设备"收完"的时刻 —— 即生成没有被插件节流。
//
// 变红条件：rtEmit 改回同步 dev.Execute ⇒ 两个时刻几乎相等 ⇒ 红。
func TestRTChild_OutputIsAsync(t *testing.T) {
	const chunks = 8
	const delay = 40 * time.Millisecond

	dev := &pumpSlowDevice{name: "slowout", delay: delay}
	p := &burstProvider{n: chunks}

	pio := agentIO.NewIOManager()
	host := New(AgentConfig{ID: "async-host", IO: pio, Provider: p})
	if err := pio.RegisterDevice(dev); err != nil {
		t.Fatalf("注册慢设备: %v", err)
	}
	for _, n := range []string{"slowout", "slowin"} {
		if err := pio.ChannelRegistry().Register(agentIO.InputChannel{
			Name: n, Plugin: "asynctest",
			Capabilities: agentIO.CapText | agentIO.CapRT,
		}); err != nil {
			t.Fatalf("注册通道 %s: %v", n, err)
		}
	}

	id, err := host.SpawnRTChild(RTChildOptions{ID: "a-rt", Input: "slowin", Output: "slowout"})
	if err != nil {
		t.Fatalf("SpawnRTChild: %v", err)
	}
	defer host.StopRTChildren()

	if _, err := host.PushRTInput(id, "说话"); err != nil {
		t.Fatalf("PushRTInput: %v", err)
	}

	// 等设备收完最后一片（或用 pump 排空作为更快的前置）。
	waitForWithin(t, "设备收完所有片", 10*time.Second, func() bool {
		return len(dev.snapshot()) >= chunks
	})

	gen := p.doneAt.Load()
	consumed := dev.lastAt.Load()
	if gen == 0 || consumed == 0 {
		t.Fatalf("时序采样缺失：gen=%d consumed=%d", gen, consumed)
	}
	if gen >= consumed {
		t.Fatalf("模型吐完(%v) 不早于 插件收完(%v) —— 生成被插件节流了，"+
			"说明输出调用仍是同步的（异步壳没生效）",
			time.Unix(0, gen).Format("15:04:05.000"),
			time.Unix(0, consumed).Format("15:04:05.000"))
	}

	// 顺带确认内容没丢、顺序没乱。
	got := dev.snapshot()
	if len(got) < chunks {
		t.Fatalf("只收到 %d 片，期望 %d（异步不该丢片）", len(got), chunks)
	}
}

// TestOutputPump_DropsOldestOnFull 钉住背压语义：满则丢**最旧**。
//
// 变红条件：改成阻塞 Push（退化为同步）或无限队列（吃内存）⇒ 红。
func TestOutputPump_DropsOldestOnFull(t *testing.T) {
	// 设备永远阻塞，逼队列堆满。
	block := make(chan struct{})
	dev := &blockingDevice{name: "blk", gate: block}
	pump := newOutputPump("blk", func() agentIO.Device { return dev })
	defer func() { close(block); pump.Close() }()

	// 推入远超容量的片数。
	total := outputPumpQueueSize * 3
	for i := 0; i < total; i++ {
		pump.Push("x")
	}
	// 给消费 goroutine 一点时间卡在第一片上。
	time.Sleep(20 * time.Millisecond)

	depth, dropped := pump.Stats()
	if dropped == 0 {
		t.Fatalf("推入 %d 片（容量 %d）却没有丢弃 —— 队列是无界的？"+
			"（depth=%d）", total, outputPumpQueueSize, depth)
	}
	if depth > outputPumpQueueSize {
		t.Fatalf("队列深度 %d 超过上限 %d —— 背压没生效", depth, outputPumpQueueSize)
	}
}

// blockingDevice 的 Execute 阻塞到 gate 关闭。
type blockingDevice struct {
	name string
	gate chan struct{}
}

func (d *blockingDevice) Name() string                   { return d.name }
func (d *blockingDevice) Type() agentIO.DeviceType       { return agentIO.DeviceOutput }
func (d *blockingDevice) Description() string            { return "blocking" }
func (d *blockingDevice) Tools() []agentIO.ToolDef       { return nil }
func (d *blockingDevice) Start() error                   { return nil }
func (d *blockingDevice) Stop() error                    { return nil }
func (d *blockingDevice) ChannelDef() agentIO.ChannelDef { return agentIO.ChannelDef{} }
func (d *blockingDevice) OutputCapabilities() agentIO.OutputCapability {
	return agentIO.CapText | agentIO.CapRT
}
func (d *blockingDevice) Execute(string, map[string]interface{}) (interface{}, error) {
	<-d.gate
	return "ok", nil
}

// TestOutputPump_CloseDrains 钉住"最后半句不被吞"。
//
// 变红条件：Close 直接丢弃队列 ⇒ 红。
func TestOutputPump_CloseDrains(t *testing.T) {
	dev := &pumpSlowDevice{name: "drain", delay: 5 * time.Millisecond}
	pump := newOutputPump("drain", func() agentIO.Device { return dev })

	const n = 5
	for i := 0; i < n; i++ {
		pump.Push("片")
	}
	pump.Close() // 必须等队列排空

	if got := len(dev.snapshot()); got != n {
		t.Fatalf("Close 后设备收到 %d 片，期望 %d —— 队列没排空，最后几句被吞了", got, n)
	}
}

// TestOutputPump_ResolvesDeviceEachTime 钉住设备每片重新解析。
//
// 插件崩溃时设备会 Unregister，恢复后重新注册。缓存指针会一直指向死设备。
// 变红条件：把 resolve 的结果缓存下来 ⇒ 恢复后仍然丢 ⇒ 红。
func TestOutputPump_ResolvesDeviceEachTime(t *testing.T) {
	var mu sync.Mutex
	var cur agentIO.Device // nil 表示"当前没有设备"

	pump := newOutputPump("flap", func() agentIO.Device {
		mu.Lock()
		defer mu.Unlock()
		return cur
	})

	// ① 设备不存在时推入：不 panic、不阻塞。
	pump.Push("早")
	time.Sleep(10 * time.Millisecond)

	// ② 设备出现后推入：必须能送达。
	dev := &pumpSlowDevice{name: "flap", delay: 0}
	mu.Lock()
	cur = dev
	mu.Unlock()
	pump.Push("晚")
	waitForWithin(t, "设备出现后输出送达", 3*time.Second, func() bool {
		return len(dev.snapshot()) >= 1
	})

	pump.Close()
	if got := dev.snapshot(); len(got) == 0 {
		t.Fatal("设备恢复后仍收不到输出 —— resolve 被缓存成了死指针")
	}
}
