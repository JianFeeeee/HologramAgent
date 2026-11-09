package test

// internal/events 的黑盒判据：NewBus → Subscribe → Publish → handler 收到的事件。
// 只调导出方法，不碰 b.subs、不读源码。

import (
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/events"
)

// TestPublishDeliversPayload：订阅后发布，handler 必须收到原事件内容。
func TestPublishDeliversPayload(t *testing.T) {
	bus := events.NewBus()
	got := make(chan *events.Event, 1)
	unsub := bus.Subscribe(events.EventType("contract.test"), func(e *events.Event) {
		got <- e
	})
	defer unsub()

	bus.Publish(&events.Event{
		Type:    events.EventType("contract.test"),
		Source:  "contract",
		Payload: map[string]interface{}{"value": 42},
	})
	select {
	case e := <-got:
		if e.Source != "contract" {
			t.Fatalf("★ Source = %q", e.Source)
		}
		if e.Payload["value"] != 42 {
			t.Fatalf("★ Payload = %v", e.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("★ 订阅者没收到事件")
	}
}

// TestUnsubscribeStopsDelivery：退订后不得再收到。
func TestUnsubscribeStopsDelivery(t *testing.T) {
	bus := events.NewBus()
	n := 0
	unsub := bus.Subscribe(events.EventType("contract.off"), func(e *events.Event) { n++ })
	unsub()
	bus.Publish(&events.Event{Type: events.EventType("contract.off")})
	if n != 0 {
		t.Fatalf("★ 退订后仍收到 %d 次", n)
	}
}

// TestOnlyMatchingTypeReceives：不同事件类型之间不串台。
func TestOnlyMatchingTypeReceives(t *testing.T) {
	bus := events.NewBus()
	a, b := 0, 0
	bus.Subscribe(events.EventType("type.a"), func(e *events.Event) { a++ })
	bus.Subscribe(events.EventType("type.b"), func(e *events.Event) { b++ })
	bus.Publish(&events.Event{Type: events.EventType("type.a")})
	if a != 1 || b != 0 {
		t.Fatalf("★ 串台：a=%d b=%d", a, b)
	}
}

// TestPublisherIdentity：Name 为空即内核，非空即插件。
func TestPublisherIdentity(t *testing.T) {
	if !((events.Publisher{}).IsKernel()) {
		t.Fatal("★ 空 Name 应判为内核")
	}
	if (events.Publisher{Name: "qq"}).IsKernel() {
		t.Fatal("★ 插件被判成内核")
	}
}

// TestSetGuardRoundTrip：SetGuard → Guard 拿回同一个守卫。
type stubGuard struct{}

func (g *stubGuard) AllowPublish(p events.Publisher, et events.EventType) (bool, string) {
	return true, ""
}
func (g *stubGuard) AllowSubscribe(p events.Publisher, et events.EventType) (bool, string) {
	return true, ""
}
func (g *stubGuard) ScrubPayload(et events.EventType, m map[string]interface{}) map[string]interface{} {
	return m
}
func (g *stubGuard) ScrubInbound(et events.EventType, m map[string]interface{}) map[string]interface{} {
	return m
}

func TestSetGuardRoundTrip(t *testing.T) {
	bus := events.NewBus()
	g := &stubGuard{}
	bus.SetGuard(g)
	var got events.Guard = bus.Guard()
	if got != g {
		t.Fatal("★ Guard() 没拿回 SetGuard 设置的守卫")
	}
}
