package proc

import (
	"sync/atomic"
	"testing"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// stubSubscriber 是最小 EvtRingSubscriber 实现，用于验证「关停时统一退订」。
//
// 为什么必须桩掉 Host.evtSubscriber：真实实现是 internal/plugin.EventRing，
// 它依赖 Bus（另一个包），在 proc 包内会造成循环依赖。这里只关心
// Host.Close 是否**调用了** closer —— 真实实现拿到 Close 后的行为
// 由 internal/plugin 侧测试覆盖。
type stubSubscriber struct {
	closed atomic.Bool
	// subCount 记录被登记的订阅数（供断言 tracked 语义）
	subCount atomic.Int32
}

func (s *stubSubscriber) EvtRingSubscribe(types []pubsdk.EventType) func() {
	s.subCount.Add(1)
	return func() {}
}

func (s *stubSubscriber) EvtRingSubscribeTracked(types []pubsdk.EventType) func() {
	s.subCount.Add(1)
	return func() {}
}

func (s *stubSubscriber) Close() { s.closed.Store(true) }

// TestHost_CloseUnsubscribesEventRing 钉死不变量：
//
//	Host.Close() 必须在 munmap 共享段**之前**退订事件环。
//
// 为什么这是安全不变量：事件环订阅的 handler 会 ring.WritePush（写共享内存）。
// Host.Close 会 unmap 那块内存；若订阅还在 Bus 上，munmap 后任意一条事件经过
// Publish 都会让 handler 写已解除映射的内存 ⇒ SIGSEGV。
// Bus.safeCall 虽有 recover，但 SIGSEGV 是 runtime 致命错误、recover 捕不到，
// 后果是整个内核进程被杀。
//
// 修复前：handleEvents 丢弃取消函数、EventsUnsubscribe 是 no-op、
// Host.Close 也从不停订阅 —— 每个订阅过的插件都在 Bus 上永久留了一个
// 写共享内存的 handler，内核关停时必炸。
func TestHost_CloseUnsubscribesEventRing(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}

	sub := &stubSubscriber{}
	host.SetEvtSubscriber(sub)

	// 模拟插件订阅（走 tracked 路径，与 handleEvents 一致）
	host.evtSubscriber.EvtRingSubscribeTracked([]pubsdk.EventType{pubsdk.EventSystem})
	if sub.subCount.Load() != 1 {
		t.Fatalf("订阅登记数 = %d, want 1", sub.subCount.Load())
	}

	if err := host.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !sub.closed.Load() {
		t.Fatal("Host.Close 未退订事件环：\n" +
			"  munmap 之后 handler 仍挂在 Bus 上，一条事件就会写已解除映射的内存\n" +
			"  ⇒ SIGSEGV（recover 捕不到，内核进程被杀）。\n" +
			"  修法：Host.Close 在 freeShm 之前调用 evtCloser.Close。")
	}
}

// TestHost_CloseWithoutSubscriber 确认没设订阅时 Close 不 panic
// （evtSubscriber 为 nil 是合法状态：未注册任何事件的部署）。
func TestHost_CloseWithoutSubscriber(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if err := host.Close(); err != nil {
		t.Fatalf("无订阅者时 Close 应成功: %v", err)
	}
}
