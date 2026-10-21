package plugin

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/plugin/proc"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// TestEventRing_CloseUnsubscribesFromBus 钉死：EventRing.Close 必须把
// 自己注册到 Bus 的 handler 全部撤掉。
//
// 为什么关键：那些 handler 会 ring.WritePush —— 也就是**写共享内存**。
// Host.Close 会 munmap 整块区域；若 handler 还挂在 Bus 上，munmap 之后
// 任意一条事件经过 Publish 都会让它写已解除映射的内存 ⇒ SIGSEGV。
// Bus.safeCall 虽有 recover，但 SIGSEGV 是 runtime 致命错误、recover 捕不到，
// 后果是整个 homed 进程被杀。
//
// 判据用「Publish 之后共享内存内容是否被改动」：这是端到端的可观察后果，
// 比断言内部计数器更接近真实危害。
func TestEventRing_CloseUnsubscribesFromBus(t *testing.T) {
	host, err := proc.NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	bus := events.NewBus()
	er := NewEventRing(host.EvtRing(), int(host.Evtfd().Fd()), bus)
	er.EvtRingSubscribeTracked([]pubsdk.EventType{pubsdk.EventSystem})

	// 订阅生效：Publish 一条事件应写入事件环。
	before := host.EvtRing().Written()
	bus.Publish(&events.Event{Type: events.EventSystem, Source: "test", Payload: map[string]interface{}{"a": 1}})
	if host.EvtRing().Written() == before {
		t.Fatal("订阅后 Publish 未写入事件环（测试前提不成立）")
	}

	// 关停：退订
	er.Close()

	// 退订后 Publish 不应再写入事件环（即不再触碰共享内存）。
	after := host.EvtRing().Written()
	bus.Publish(&events.Event{Type: events.EventSystem, Source: "test", Payload: map[string]interface{}{"b": 2}})
	if host.EvtRing().Written() != after {
		t.Fatal("EventRing.Close 未从 Bus 退订：\n" +
			"  munmap 后 handler 仍会写已解除映射的内存 ⇒ SIGSEGV。")
	}

	// 幂等：重复 Close 不 panic
	er.Close()
}
