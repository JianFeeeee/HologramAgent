package io

import (
	"testing"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// drainOne 取出一条注入事件；没有则 Fatal。
func drainOne(t *testing.T, ch <-chan *InputEvent) *InputEvent {
	t.Helper()
	select {
	case evt := <-ch:
		return evt
	default:
		t.Fatal("没有拿到注入事件")
		return nil
	}
}

// 零值 InjectOptions 必须与历史的三参数注入产出**完全一致**的 payload。
//
// 这是兼容性底线：任何按 payload 取字段的下游（事件订阅方、旧内核、
// 工具链测试）都不能因为这次改造而看到新键。
func TestInjectTextOpts_ZeroValueMatchesLegacyPayload(t *testing.T) {
	m := NewIOManager()
	m.InjectText("src", "hello")
	legacy := drainOne(t, m.InputChan())

	m2 := NewIOManager()
	m2.InjectTextOpts("src", "chan", "hello", InjectOptions{})
	withOpts := drainOne(t, m2.InputChan())

	if len(withOpts.Payload) != len(legacy.Payload) {
		t.Fatalf("零值注入多出了键：legacy=%v opts=%v", legacy.Payload, withOpts.Payload)
	}
	for k, v := range legacy.Payload {
		if withOpts.Payload[k] != v {
			t.Fatalf("键 %q 不一致：legacy=%v opts=%v", k, v, withOpts.Payload[k])
		}
	}
}

// 标志位必须出现在事件 payload 上——eventloop 就是从那里读的。
func TestInjectTextOpts_CarriesFlags(t *testing.T) {
	m := NewIOManager()
	m.InjectTextOpts("src", "chan", "hello", InjectOptions{
		NoMemory:      true,
		ContextPolicy: "prune",
		RecallPolicy:  "none",
		CleanerName:   "clean_me",
	})
	evt := drainOne(t, m.InputChan())

	if evt.Payload["no_memory"] != true {
		t.Errorf("no_memory 未传递: %v", evt.Payload["no_memory"])
	}
	if evt.Payload["context_policy"] != "prune" {
		t.Errorf("context_policy 未传递: %v", evt.Payload["context_policy"])
	}
	if evt.Payload["recall_policy"] != "none" {
		t.Errorf("recall_policy 未传递: %v", evt.Payload["recall_policy"])
	}
	if evt.Payload["cleaner_name"] != "clean_me" {
		t.Errorf("cleaner_name 未传递: %v", evt.Payload["cleaner_name"])
	}
	if evt.Payload["content"] != "hello" {
		t.Errorf("content 丢失: %v", evt.Payload["content"])
	}
	if evt.OutputChannel != "chan" {
		t.Errorf("输出通道 = %q，期望 chan", evt.OutputChannel)
	}
}

// 中断注入走另一条队列，标志位同样要带上（用户已确认中断允许声明 prune）。
func TestInjectInterruptTextOpts_CarriesFlags(t *testing.T) {
	m := NewIOManager()
	m.InjectInterruptTextOpts("src", "chan", "alert", InjectOptions{ContextPolicy: "prune", RecallPolicy: "none"})
	evt := drainOne(t, m.InputInterruptChan())

	if evt.Payload["context_policy"] != "prune" {
		t.Errorf("中断注入的 context_policy 未传递: %v", evt.Payload)
	}
	if evt.Payload["recall_policy"] != "none" {
		t.Errorf("中断注入的 recall_policy 未传递: %v", evt.Payload)
	}
	if evt.Payload["type"] != "text" || evt.Payload["content"] != "alert" {
		t.Errorf("中断注入的基本字段不对: %v", evt.Payload)
	}
	if _, has := evt.Payload["no_memory"]; has {
		t.Errorf("未声明的 no_memory 不应出现: %v", evt.Payload)
	}
}

// 带媒体的注入同样要带标志位。
func TestInjectInputMediaOpts_CarriesFlags(t *testing.T) {
	m := NewIOManager()
	blocks := []pubsdk.ContentBlock{{Type: "image_url", ImageURL: &pubsdk.ImageURL{URL: "data:image/png;base64,AA"}}}
	m.InjectInputMediaOpts("src", "chan", "看图", blocks, InjectOptions{NoMemory: true})
	evt := drainOne(t, m.InputChan())

	if evt.Payload["no_memory"] != true {
		t.Errorf("媒体的 no_memory 未传递: %v", evt.Payload)
	}
	if _, ok := evt.Payload["media_blocks"]; !ok {
		t.Errorf("媒体块丢失: %v", evt.Payload)
	}
}

// 旧方法必须继续等价工作（它们是 Opts 变体的零值糖）。
func TestLegacyNoMemoryMethodStillSetsFlag(t *testing.T) {
	m := NewIOManager()
	m.InjectTextNoMemoryTo("src", "chan", "quiet")
	evt := drainOne(t, m.InputChan())
	if evt.Payload["no_memory"] != true {
		t.Fatalf("旧 NoMemory 方法应置位: %v", evt.Payload)
	}
}
