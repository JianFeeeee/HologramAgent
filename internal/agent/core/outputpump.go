package core

// outputpump.go 是输出通道的**异步壳**。
//
// # 它是什么（用户裁定 2026-11-04）
//
// 「就是在正常输出的情况下套一个异步的壳，rtchild 的所有调用都应该是异步的」
//
// 所以它**不改输出的语义**（还是把 text 交给同一个 outputch 设备），
// 只是把"调用"这一步从调用方的 goroutine 里摘出去：
//
//	rtStream 的流式循环:  pump.Push(chunk)  ← 立即返回
//	                        ↓ 有界队列（满则丢最旧）
//	                    pump 的消费 goroutine → dev.Execute(...)
//
// # 为什么必须异步（实测缺陷）
//
// 原 rtEmit 直接 `dev.Execute(...)`，于是**插件侧的延迟直接节流模型的生成**：
//
//	插件做 TTS / 网络编码 / 字幕渲染，慢 50ms 一片
//	→ rtStream 每读一个 chunk 就卡 50ms
//	→ 模型的下一个 token 迟 50ms 才开始消费
//	→ 用户听到的首字延迟被下游放大
//
// 「不攒整段」（契约 2）的收益全被这里吃掉。
//
// # 为什么用"丢最旧的"而不是"阻塞"或"无界"
//
//	阻塞     → 退化成现在的同步（等于没做）
//	无界     → 插件卡死会吃光内存
//	丢最旧   → 符合 rt 的"最新者胜"语义：过期的音频/字幕本来就不该播
//
// 丢弃必须**可观测**（dropped 计数 + 首次日志），否则"用户听不到最后几句"
// 会变成一个没有任何痕迹的现象。
//
// # 为什么不复用 IOManager.outputCh
//
// 那条通道是给 `output_send__*` 工具用的（`EmitOutputTo` → OutputEvent →
// 由 SDK 的 OutputChan 消费者取）。它的语义是"一条完整的输出事件"，
// 而 rt 需要的是"逐 chunk 的增量流"，且需要 per-child 的队列与丢弃统计。
// 混用会让 rt 的背压污染主通道。所以单独一个壳，但复用同一套纪律。

import (
	"log"
	"sync"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// outputPumpQueueSize 是每个 rtchild 输出队列的上限。
//
// 取值依据：rt 的 chunk 通常是几个到几十字节，64 片 ≈ 数百字节到数 KB，
// 足以吸收一次网络抖动；再大就只是在缓存"用户已经听不到"的过期内容。
const outputPumpQueueSize = 64

// outputPump 是一个输出通道的异步壳。
//
// ★ 生命周期：SpawnRTChild 时创建，StopRTChild 时 Close（排空后退出）。
type outputPump struct {
	// resolve 每次**重新**解析设备。
	//
	// ★ 必须是闭包而不是缓存的 dev 指针：插件崩溃时 releasePluginChannels
	// 会 UnregisterDevice，恢复后重新注册。缓存指针会一直指向死设备 ——
	// 现象是"插件重启后 rt 再也说不出话"，且日志里只有一行 execute 失败。
	resolve func() agentIO.Device

	mu      sync.Mutex
	q       []string
	notify  chan struct{}
	closing bool
	wg      sync.WaitGroup

	// dropped 是队列满时丢弃的片数（可观测）。
	dropped   uint64
	firstDrop sync.Once
	name      string // 输出通道名（日志用）
}

func newOutputPump(name string, resolve func() agentIO.Device) *outputPump {
	p := &outputPump{
		name:    name,
		resolve: resolve,
		notify:  make(chan struct{}, 1),
	}
	p.wg.Add(1)
	go p.loop()
	return p
}

// Push 投递一段增量文本，**立即返回**，不等待插件。
func (p *outputPump) Push(text string) {
	if p == nil || text == "" {
		return
	}
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return
	}
	p.q = append(p.q, text)
	// 满了：丢最旧（rt 的语义是"最新者胜"，过期内容不该再播）。
	if len(p.q) > outputPumpQueueSize {
		p.q = p.q[1:]
		p.dropped++
		p.mu.Unlock()
		p.firstDrop.Do(func() {
			log.Printf("[rtchild] output pump %q queue full (%d), dropping oldest chunks — "+
				"插件消费慢于模型生成，用户可能听不到最早的一段", p.name, outputPumpQueueSize)
		})
		return
	}
	p.mu.Unlock()
	// 非阻塞唤醒（电平语义，不计数）。
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// Close 关闭壳：**排空队列后**退出，避免把最后半句吞掉。
func (p *outputPump) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return
	}
	p.closing = true
	p.mu.Unlock()
	select {
	case p.notify <- struct{}{}:
	default:
	}
	p.wg.Wait()
}

// Stats 返回（剩余队列深度，丢弃片数）—— 诊断与判据用。
func (p *outputPump) Stats() (depth int, dropped uint64) {
	if p == nil {
		return 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.q), p.dropped
}

// loop 是唯一的消费者。
func (p *outputPump) loop() {
	defer p.wg.Done()
	for {
		<-p.notify

		for {
			p.mu.Lock()
			if len(p.q) == 0 {
				closing := p.closing
				p.mu.Unlock()
				if closing {
					return
				}
				break // 回外层等下一次唤醒
			}
			text := p.q[0]
			p.q = p.q[1:]
			p.mu.Unlock()

			p.emit(text)
		}
	}
}

// emit 真正把一片交给设备。★ 每次重新解析设备（见 resolve 的注释）。
func (p *outputPump) emit(text string) {
	dev := p.resolve()
	if dev == nil {
		// ★ 设备缺失必须留痕（一次性）：插件崩溃期间会走到这里，
		// 静默丢弃会让"用户听不到回应"变成一个没有任何线索的现象。
		p.firstDrop.Do(func() {
			log.Printf("[rtchild] output pump %q: 输出设备不存在（插件可能已崩溃/未注册），"+
				"后续输出将被丢弃直到设备重新出现", p.name)
		})
		return
	}
	if _, err := dev.Execute(p.name, map[string]interface{}{"text": text}); err != nil {
		log.Printf("[rtchild] output pump %q emit failed: %v", p.name, err)
	}
}

// pumpWaitDrain 等一个 pump 排空（判据用，避免用 sleep）。
func pumpWaitDrain(p *outputPump, within time.Duration) bool {
	if p == nil {
		return true
	}
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if depth, _ := p.Stats(); depth == 0 {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}
