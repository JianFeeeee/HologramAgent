//go:build linux || darwin

package plugin

import (
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/plugin/proc"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// 事件环基础测试：Host 创建事件环 → EventRing 写入 → 消费者读到。
func TestEventRing_BasicWriteAndConsume(t *testing.T) {
	host, err := proc.NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	bus := events.NewBus()
	er := NewEventRing(host.EvtRing(), int(host.Evtfd().Fd()), bus)

	// 消费者：从事件环段读取事件
	received := make(chan *pubsdk.Event, 10)
	consumer := proc.NewEvtConsumer(
		host.EvtData(),
		host.EvtfdReadFile(),
		0, // typeMask = 0：接收全部事件
		func(evt *pubsdk.Event) error {
			received <- evt
			return nil
		},
	)
	go consumer.Run()
	// LIFO：先 Stop（打断阻塞的 Read）再 Wait（等 Run 退出），
	// 两者都必须在 host.Close（munmap 整个区域）之前完成。
	defer consumer.Wait()
	defer consumer.Stop()

	// 订阅 agent_output 事件
	unsub := er.Subscribe(pubsdk.EventAgentOutput)
	defer unsub()

	// 发布事件
	bus.Publish(&events.Event{
		Type:    events.EventAgentOutput,
		Payload: map[string]interface{}{"text": "hello"},
	})

	// 等待消费者读到
	select {
	case evt := <-received:
		if evt.Type != pubsdk.EventType(events.EventAgentOutput) {
			t.Errorf("事件类型 = %v，期望 %v", evt.Type, events.EventAgentOutput)
		}
	case <-time.After(2 * time.Second):
		t.Error("消费者在 2s 内未收到事件")
	}
}

// 事件环溢出测试：写入超过 cap 时消费者仍能读到最新事件。
func TestEventRing_OverflowStillDelivers(t *testing.T) {
	host, err := proc.NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	bus := events.NewBus()
	er := NewEventRing(host.EvtRing(), int(host.Evtfd().Fd()), bus)

	// 不启动消费者，直接写入超过 cap 的事件（需先订阅，否则 Bus 不会触发事件环写入）
	unsub := er.Subscribe(pubsdk.EventSystem)
	defer unsub()
	for i := uint32(0); i < 8192+100; i++ {
		bus.Publish(&events.Event{
			Type:    events.EventSystem,
			Payload: map[string]interface{}{"seq": i},
		})
	}

	// 启动消费者，应能读到最新事件
	received := make(chan *pubsdk.Event, 10)
	consumer := proc.NewEvtConsumer(
		host.EvtData(),
		host.EvtfdReadFile(),
		0,
		func(evt *pubsdk.Event) error {
			// 非阻塞投递：本用例写入了 8292 条事件，若这里阻塞在
			// channel 上，drainEvents 会卡在 handler 里，Stop 就无法
			// 让 Run 退出。
			select {
			case received <- evt:
			default:
			}
			return nil
		},
	)
	go consumer.Run()
	defer consumer.Wait()
	defer consumer.Stop()

	select {
	case evt := <-received:
		if evt == nil {
			t.Error("收到 nil 事件")
		}
	case <-time.After(2 * time.Second):
		t.Error("溢出后消费者在 2s 内未收到事件")
	}
}

// typeMask 过滤测试：订阅者只收到匹配类型的事件。
func TestEventRing_TypeMaskFiltering(t *testing.T) {
	host, err := proc.NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	bus := events.NewBus()
	er := NewEventRing(host.EvtRing(), int(host.Evtfd().Fd()), bus)

	received := make(chan *pubsdk.Event, 10)
	// typeMask 只订阅 tool_call（bit 3 = 8）
	consumer := proc.NewEvtConsumer(
		host.EvtData(),
		host.EvtfdReadFile(),
		1<<3, // tool_call
		func(evt *pubsdk.Event) error {
			received <- evt
			return nil
		},
	)
	go consumer.Run()
	defer consumer.Wait()
	defer consumer.Stop()

	unsub := er.Subscribe(pubsdk.EventToolCall)
	defer unsub()

	// 发一个 tool_call 和一个 agent_output
	bus.Publish(&events.Event{
		Type:    events.EventToolCall,
		Payload: map[string]interface{}{"tool": "test"},
	})
	bus.Publish(&events.Event{
		Type:    events.EventAgentOutput,
		Payload: map[string]interface{}{"text": "should be filtered"},
	})

	// 只应收到 tool_call
	select {
	case evt := <-received:
		if evt.Type != pubsdk.EventType(events.EventToolCall) {
			t.Errorf("收到错误类型 %v，期望 tool_call", evt.Type)
		}
	case <-time.After(2 * time.Second):
		t.Error("消费者在 2s 内未收到 tool_call 事件")
	}

	// agent_output 不应到达
	select {
	case evt := <-received:
		t.Errorf("不应收到 agent_output，实际收到 %v", evt)
	case <-time.After(200 * time.Millisecond):
		// 正确：agent_output 被过滤
	}
}
