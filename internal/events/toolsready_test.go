package events

import "testing"

// tools_ready 是"工具已全部注册、可以安全执行工具调用序列"的唯一信号。
//
// 语义只到"工具可调"：seq 的开机序列**不需要 agent 在跑**，返回值进队列
// 等 agent 起来后处理即可。把事件绑在 agent.Start 上会让时机被人为推后，
// 且把"何时可跑序列"这个内核判断外包给编排层。
//
// 为什么要有判据：这个事件缺失时**没有任何报错** —— 依赖它的订阅者
// （如 seq 的开机序列）只是永远不执行。静默失效正是最需要判据的一类。
func TestToolsReadyEventExistsAndIsDistinct(t *testing.T) {
	if EventToolsReady == "" {
		t.Fatal("EventToolsReady 未定义 —— 订阅者无法等待工具就绪")
	}
	if EventToolsReady == EventAll {
		t.Fatal("EventToolsReady 不能是 EventAll（会把所有事件都当成工具就绪）")
	}
	// 必须与相邻的生命周期事件互不冲突（拼错会变成"永不触发"）
	others := []EventType{
		EventSkillDetected, EventSystem, EventScheduler,
		EventAgentOutput, EventAgentLLMChain,
	}
	for _, o := range others {
		if EventToolsReady == o {
			t.Fatalf("EventToolsReady 与 %q 重名", o)
		}
	}
	if string(EventToolsReady) != "tools_ready" {
		t.Fatalf("事件名应为 tools_ready，实际 %q", EventToolsReady)
	}
}

// 总线必须能按类型精确分发 tools_ready（订阅者只想要这一个事件）。
func TestToolsReadyDispatch(t *testing.T) {
	bus := NewBus()
	var got []*Event
	bus.Subscribe(EventToolsReady, func(e *Event) { got = append(got, e) })

	// 发一条无关事件：不得误触发
	bus.Publish(&Event{Type: EventSystem})
	if len(got) != 0 {
		t.Fatalf("无关事件不应触发 tools_ready 订阅者，实际 %d 次", len(got))
	}

	bus.Publish(&Event{
		Type:    EventToolsReady,
		Source:  "kernel",
		Payload: map[string]interface{}{"plugin_count": 3},
	})
	if len(got) != 1 {
		t.Fatalf("tools_ready 应触发一次，实际 %d 次", len(got))
	}
	if n, _ := got[0].Payload["plugin_count"].(int); n != 3 {
		t.Fatalf("payload 应透传 plugin_count，实际 %v", got[0].Payload)
	}
}
