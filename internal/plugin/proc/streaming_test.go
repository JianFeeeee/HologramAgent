package proc

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 流式输出压测（§4.3 标记「风险高」的那一项）。
//
// 担忧的原文：「Bus.Publish 路径禁用任何锁/阻塞——流式输出逐 token 发布，
// 任何等待都会卡顿」。实验 4 的数据：同步 Publish + 一个 20µs 慢订阅者，
// 5000 token 耗时 5.07s；改为写环 + post 后 2.29ms（加速比 2218x）。
//
// 这里验证事件环侧的 post-and-forget 性质在实现中成立。

// 慢消费者不拖慢 Publish。
//
// 判据：若 Publish 等消费者，5000 × 20µs = 100ms 是理论下限。
// post-and-forget 应远低于此。
func TestStreaming_SlowConsumerDoesNotBlockPublish(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}

	ring := host.EvtRing()

	var consumed atomic.Int64
	consumer := NewEvtConsumer(host.EvtData(), host.EvtfdReadFile(), 0,
		func(evt *pubsdk.Event) error {
			time.Sleep(20 * time.Microsecond)
			consumed.Add(1)
			return nil
		})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		consumer.Run()
	}()
	// defer LIFO：Stop → 关 evtfd 打断 Read → 等协程退出 → Close host
	defer host.Close()
	defer wg.Wait()
	defer host.EvtfdReadFile().Close()
	defer consumer.Stop()

	const tokens = 5000
	payload := []byte(`{"type":"content_delta","payload":{"text":"t"}}`)

	start := time.Now()
	for i := 0; i < tokens; i++ {
		ring.WritePush(pubsdk.EventContentDelta, payload)
		EvtfdNotify(host.EvtNotifyFd())
	}
	elapsed := time.Since(start)
	perToken := elapsed / tokens

	t.Logf("%d 次 Publish 耗时 %v，均摊 %v/token（消费者每条睡 20µs）",
		tokens, elapsed, perToken)
	t.Logf("同步语义下的理论下限：%v", tokens*20*time.Microsecond)

	if elapsed > 100*time.Millisecond {
		t.Errorf("Publish 疑似被慢消费者阻塞：耗时 %v ≥ 同步下限 100ms", elapsed)
	}
	if perToken > 20*time.Microsecond {
		t.Errorf("均摊 %v/token ≥ 消费者处理时间 20µs，说明存在等待", perToken)
	}
}

// 订阅者增多不使 Publish 线性恶化。
//
// §4.3 的具体要求：「长回复下 Publish 单次耗时不随订阅者数线性恶化」。
func TestStreaming_PublishLatencyFlatAcrossSubscribers(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	// defer LIFO：先停消费者再 Close host
	defer host.Close()
	defer host.EvtfdReadFile().Close()

	ring := host.EvtRing()
	payload := []byte(`{"type":"content_delta","payload":{"text":"t"}}`)
	const rounds = 3000

	measure := func(consumers int) time.Duration {
		var wg sync.WaitGroup
		active := make([]*EvtConsumer, 0, consumers)
		for i := 0; i < consumers; i++ {
			c := NewEvtConsumer(host.EvtData(), host.EvtfdReadFile(), 0,
				func(evt *pubsdk.Event) error {
					time.Sleep(10 * time.Microsecond)
					return nil
				})
			active = append(active, c)
			wg.Add(1)
			go func(cc *EvtConsumer) {
				defer wg.Done()
				cc.Run()
			}(c)
		}
		// defer LIFO：Stop → 等协程退出
		defer wg.Wait()
		defer func() {
			for _, c := range active {
				c.Stop()
			}
		}()

		time.Sleep(10 * time.Millisecond)

		start := time.Now()
		for i := 0; i < rounds; i++ {
			ring.WritePush(pubsdk.EventContentDelta, payload)
			EvtfdNotify(host.EvtNotifyFd())
		}
		return time.Since(start)
	}

	d1 := measure(1)
	d8 := measure(8)

	t.Logf("1 个消费者：%v（均摊 %v/次）", d1, d1/rounds)
	t.Logf("8 个消费者：%v（均摊 %v/次）", d8, d8/rounds)

	// 线性恶化的判据：8 倍订阅者不应接近 8 倍耗时。
	// 阈值取 4 倍——测量噪声与调度抖动都会影响。
	if d8 > d1*4 {
		t.Errorf("订阅者 1→8，Publish 从 %v 涨到 %v（>4 倍），疑似线性恶化", d1, d8)
	}
}

// 事件环溢出时 Publish 不退化。
//
// 消费者完全停摆时写端会覆盖最旧 slot。这条路径必须仍是 O(1)，
// 否则「消费者卡住」会连带拖慢内核主循环。
func TestStreaming_PublishStaysFastWhenRingOverflows(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	ring := host.EvtRing()
	payload := []byte(`{"type":"content_delta","payload":{"text":"t"}}`)

	// 无消费者：环必然溢出（cap=8192）
	const rounds = 30000

	start := time.Now()
	for i := 0; i < rounds; i++ {
		ring.WritePush(pubsdk.EventContentDelta, payload)
	}
	elapsed := time.Since(start)
	perPush := elapsed / rounds

	t.Logf("无消费者写入 %d 次（环 cap=%d，必然溢出）：%v，均摊 %v/次",
		rounds, evtRingCap, elapsed, perPush)

	// 溢出路径仍应是亚微秒级
	if perPush > 5*time.Microsecond {
		t.Errorf("溢出时均摊 %v/次，超出预期（应亚微秒级）", perPush)
	}
}
