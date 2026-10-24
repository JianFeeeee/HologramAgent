package plugin

import (
	"encoding/json"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/plugin/proc"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// EventRing 是 Bus 与 proc.EvtRing 之间的适配层。
//
// 把内核的事件总线接到共享内存事件环：Bus.Publish → handler
// 把事件序列化写入 EvtRing slot → eventfd 通知子进程。
// 不改 Bus 自身结构（保护零 API 变动）。
type EventRing struct {
	ring *proc.EvtRing
	bus  *events.Bus
	efd  int

	// unsubs 保存全部已注册订阅的取消函数。
	//
	// ★ 为什么必须留着：这些 handler 会 ring.WritePush（写共享内存）。
	// 而 Host.Close() 会 freeShm 解除整块映射 —— 若那时 handler 还在 Bus 上，
	// 一条事件就会让 handler 写已解除映射的内存：SIGSEGV。
	// 注意 Bus.safeCall 的 recover **捕不到** SIGSEGV（它是 runtime 致命错误，
	// 不是 panic），所以这不是「最坏情况只丢一条事件」，而是整个内核进程被杀。
	//
	// 此前 handleEvents 把 EvtRingSubscribe 返回的取消函数直接丢弃
	// （且 EventsUnsubscribe 是空实现），于是每个订阅过的插件都在 Bus 上
	// 永久留了一个写共享内存的 handler —— 内核关停时必炸。
	// 现在改为在这里登记，由 Close 统一退订（内核关停、以及插件自己的
	// events.unsubscribe 都走这里）。
	mu     sync.Mutex
	unsubs []func()
}

func NewEventRing(ring *proc.EvtRing, efd int, bus *events.Bus) *EventRing {
	return &EventRing{ring: ring, bus: bus, efd: efd}
}

// Close 退订本适配层注册到 Bus 的全部 handler。
//
// 必须在 Host.Close()（munmap 共享段）**之前**调用；见 unsubs 的说明。
// 幂等：重复调用安全（退订函数本身在 Bus 侧是「找不到就什么都不做」）。
func (er *EventRing) Close() {
	er.mu.Lock()
	unsubs := er.unsubs
	er.unsubs = nil
	er.mu.Unlock()

	for _, fn := range unsubs {
		if fn != nil {
			fn()
		}
	}
}

// Subscribe 在 Bus 上注册一个把事件分发到事件环的 handler，返回取消函数。
//
// 不改 Bus 自身结构——handler 把事件序列化后写入环并 post eventfd，
// Bus 侧按 EventType 精确匹配分发（与现有逻辑完全一致）。
func (er *EventRing) Subscribe(eventType pubsdk.EventType) func() {
	return er.bus.Subscribe(events.EventType(eventType), func(evt *events.Event) {
		payload, err := json.Marshal(evt)
		if err != nil {
			return
		}
		er.ring.WritePush(pubsdk.EventType(evt.Type), payload)
		proc.EvtfdNotify(er.efd)
	})
}

// EvtRingSubscribe 实现 proc.EvtRingSubscriber 接口。
// 按事件类型列表订阅，返回统一取消函数。
func (er *EventRing) EvtRingSubscribe(types []pubsdk.EventType) func() {
	unsubscribes := make([]func(), 0, len(types))
	for _, t := range types {
		unsubscribes = append(unsubscribes, er.Subscribe(t))
	}
	return func() {
		for _, fn := range unsubscribes {
			fn()
		}
	}
}

// EvtRingSubscribeTracked 与 EvtRingSubscribe 相同，但把取消函数登记到
// unsubs，供 Close 统一退订。内核的 events.subscribe 走这条。
func (er *EventRing) EvtRingSubscribeTracked(types []pubsdk.EventType) func() {
	un := er.EvtRingSubscribe(types)
	if un == nil {
		return nil
	}
	er.mu.Lock()
	er.unsubs = append(er.unsubs, un)
	er.mu.Unlock()
	return un
}
