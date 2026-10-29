package sdk

// 受限事件总线的判据。
//
// 设计前提（用户 2026-10-27）：现在有 security 模块兜底，可以给插件开事件总线，
// 但只能是**受限的一条缝**——白名单类型、自己的命名空间、载荷过闸。
// 每条判据都在问：插件能不能借事件总线做超出边界的事？

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

func newTestEvents(t *testing.T, name string) (pubsdk.EventsAPI, *events.Bus) {
	t.Helper()
	bus := events.NewBus()
	return newEventsAPI(name, bus), bus
}

// ① 白名单事件可发布，且 Source 被强制设成插件名（不能冒名）。
func TestEventsPublishAllowlisted(t *testing.T) {
	e, bus := newTestEvents(t, "clawhubadapter")
	var got *events.Event
	bus.Subscribe(events.EventSkillDetected, func(evt *events.Event) { got = evt })

	if err := e.Publish("skill_detected", map[string]interface{}{"path": "/x/SKILL.md"}); err != nil {
		t.Fatalf("白名单事件应允许发布: %v", err)
	}
	if got == nil {
		t.Fatal("订阅者没收到事件")
	}
	if got.Source != "clawhubadapter" {
		t.Fatalf("Source 必须是插件名（不能冒名），实际 %q", got.Source)
	}
}

// ② 越界类型必须**报错**（静默丢弃会让插件以为发成功）。
func TestEventsPublishRejectsNonAllowlisted(t *testing.T) {
	e, _ := newTestEvents(t, "evil")
	for _, bad := range []string{
		"raw_input",      // 用户输入通道
		"agent_output",   // 对外输出
		"update_request", // 能驱动内核升级流程
		"stage",          // 阶段管道内部事件
		"system",         // 系统事件
		"plugin.other.x", // 别的插件命名空间
		"plugin.evil",    // 缺少后缀
		"",               // 空类型
	} {
		if err := e.Publish(bad, nil); err == nil {
			t.Errorf("发布 %q 应被拒绝", bad)
		}
	}
}

// ③ 自己的命名空间允许（插件间协作），且带后缀。
func TestEventsPublishOwnNamespace(t *testing.T) {
	e, bus := newTestEvents(t, "seq")
	var seen bool
	bus.Subscribe(events.EventType("plugin.seq.done"), func(*events.Event) { seen = true })
	if err := e.Publish("plugin.seq.done", map[string]interface{}{"n": 1}); err != nil {
		t.Fatalf("自己的命名空间应允许: %v", err)
	}
	if !seen {
		t.Fatal("订阅者没收到自己的命名空间事件")
	}
}

// ④ 订阅只开白名单事件；越界订阅报错。
//
// ★ 2026-10-28 语义变更：raw_input / agent_output 改为可订阅（与
// security.EventPolicy 同步）。理由是它们是**通道出口的必需品**：
// WebUI/CLI 的对话界面靠订阅它们渲染用户消息与 agent 回复；
// 不开就等于“装监控把界面打瘫”。订阅是只读观测，真正需要收紧的是**发布**。
func TestEventsSubscribeAllowlistedOnly(t *testing.T) {
	e, bus := newTestEvents(t, "p1")
	for _, ok := range []string{
		"tools_ready", "security_ready", "device_ready",
		"raw_input", "agent_output",
		"tool_call", "stage", "reasoning", "terminal_output",
	} {
		if _, err := e.Subscribe(ok, func(map[string]interface{}) {}); err != nil {
			t.Errorf("%q 应可订阅: %v", ok, err)
		}
	}

	// ★ update_request 可订阅（e2e 实测：内核执行器靠它收请求，
	// 不开会让升级请求静默丢失）。安全边界在**发布**侧。
	if _, err := e.Subscribe("update_request", func(map[string]interface{}) {}); err != nil {
		t.Errorf("update_request 应可订阅: %v", err)
	}
	// 完全不存在的事件名也要拒
	if _, err := e.Subscribe("no_such_event", func(map[string]interface{}) {}); err == nil {
		t.Error("订阅未登记事件应被拒绝")
	}
	// 解除订阅后不再收到
	cancel, err := e.Subscribe("security_ready", func(map[string]interface{}) {})
	if err != nil || cancel == nil {
		t.Fatalf("应能订阅 security_ready: %v", err)
	}
	cancel()
	if bus == nil {
		t.Fatal("bus 不应为 nil")
	}
}

// ⑤ 载荷出向过安全闸：明文凭据被换成 @{别名}，不会原样进事件。
func TestEventsPayloadIsScrubbed(t *testing.T) {
	dir := t.TempDir()
	store, err := security.InitGlobal(security.Options{
		Path:    dir + "/vars.json",
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { security.SetGlobal(nil) })
	security.SetRuntimeGlobal(security.NewRuntime(store))
	t.Cleanup(func() { security.SetRuntimeGlobal(nil) })

	e, bus := newTestEvents(t, "leaky")
	var payload map[string]interface{}
	bus.Subscribe(events.EventSkillDetected, func(evt *events.Event) { payload = evt.Payload })

	const secret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	if err := e.Publish("skill_detected", map[string]interface{}{"note": "token=" + secret}); err != nil {
		t.Fatal(err)
	}
	s, _ := payload["note"].(string)
	if strings.Contains(s, secret) {
		t.Fatalf("载荷里的明文凭据没有被拦下: %q", s)
	}
	if !strings.Contains(s, "@{sec_") {
		t.Fatalf("应换成变量别名，实际 %q", s)
	}
}

// ⑥ 键数上限：事件是通知，不是数据通道。
func TestEventsPayloadKeyLimit(t *testing.T) {
	e, _ := newTestEvents(t, "big")
	limit := security.DefaultEventPolicy().MaxPayloadKeys
	p := map[string]interface{}{}
	for i := 0; i < limit+1; i++ {
		p[string(rune('a'+i%26))+strings.Repeat("x", i)] = i
	}
	if err := e.Publish("skill_detected", p); err == nil {
		t.Fatal("超过键数上限应报错")
	}
}
