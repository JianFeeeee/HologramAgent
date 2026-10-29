package events

// 总线闸门的判据（用户 2026-10-28：「eventbus 上所有事件都要接受安全模块的监控」）。
//
// ## 这组判据要钉住什么
//
// 在此之前，授权只存在于 internal/sdk/events.go 那**一条**路径。
// 其余三条（s.Publish / s.Subscribe / s.EventBus()）拿的是裸 *events.Bus，
// 任何插件都能绕过策略发任意事件 —— 包括伪造 kernel 事件。
//
// 现在闸门在 Bus.Publish / Bus.Subscribe 本体上，绕不过去。
// 所以判据的重点是「**绕过尝试必须失败**」，而不是「正常路径能跑」。

import (
	"errors"
	"testing"
)

// fakeGuard 是一个只放行白名单的闸门。
type fakeGuard struct {
	allowPublish   map[string]bool
	allowSubscribe map[string]bool
	scrubCalls     int
	inboundCalls   int
}

func (g *fakeGuard) AllowPublish(p Publisher, t EventType) (bool, string) {
	if p.IsKernel() {
		return true, ""
	}
	if g.allowPublish[string(t)] {
		return true, ""
	}
	return false, "not allowed"
}

func (g *fakeGuard) AllowSubscribe(p Publisher, t EventType) (bool, string) {
	if p.IsKernel() {
		return true, ""
	}
	if g.allowSubscribe[string(t)] {
		return true, ""
	}
	return false, "not allowed"
}

func (g *fakeGuard) ScrubPayload(_ EventType, payload map[string]interface{}) map[string]interface{} {
	g.scrubCalls++
	if _, ok := payload["secret"]; ok {
		out := map[string]interface{}{}
		for k, v := range payload {
			if k == "secret" {
				out[k] = "@{redacted}"
				continue
			}
			out[k] = v
		}
		return out
	}
	return payload
}

// ScrubInbound 模拟入向清洗：把 secret 字段换成 [inbound]。
//
// 计数器与出向分开，便于判据区分“洗了几次、洗的哪个方向”。
func (g *fakeGuard) ScrubInbound(_ EventType, payload map[string]interface{}) map[string]interface{} {
	g.inboundCalls++
	if _, ok := payload["secret"]; ok {
		out := map[string]interface{}{}
		for k, v := range payload {
			if k == "secret" {
				out[k] = "[inbound]"
				continue
			}
			out[k] = v
		}
		return out
	}
	return payload
}

// ① 插件发布未授权事件必须被**拒绝**，且不送达订阅者。
//
// 这是最核心的一条：能发 raw_input / agent_output 等于能伪造内核事件。
func TestGuardRejectsUnauthorizedPublish(t *testing.T) {
	bus := NewBus()
	bus.SetGuard(&fakeGuard{allowPublish: map[string]bool{"skill_detected": true}})

	got := 0
	bus.Subscribe(EventRawInput, func(*Event) { got++ })

	err := bus.PublishAs(Publisher{Name: "evil"}, &Event{Type: EventRawInput})
	if err == nil {
		t.Fatal("★ 未授权发布必须返回错误（静默丢弃会让插件以为发了）")
	}
	var de *DeniedError
	if !errors.As(err, &de) {
		t.Fatalf("应返回 DeniedError，实际 %T", err)
	}
	if got != 0 {
		t.Errorf("★ 被拒事件不应送达订阅者，实际送达 %d 次", got)
	}
}

// ② 授权事件正常送达，且来源是插件名。
func TestGuardAllowsAuthorizedPublish(t *testing.T) {
	bus := NewBus()
	bus.SetGuard(&fakeGuard{allowPublish: map[string]bool{"skill_detected": true}})

	var got *Event
	bus.Subscribe(EventSkillDetected, func(e *Event) { got = e })

	if err := bus.PublishAs(Publisher{Name: "clawhubadapter"},
		&Event{Type: EventSkillDetected, Payload: map[string]interface{}{"path": "/x"}}); err != nil {
		t.Fatalf("白名单事件应放行: %v", err)
	}
	if got == nil {
		t.Fatal("订阅者应收到事件")
	}
}

// ③ 内核身份不受授权限制（内核模块发布系统事件不需要策略）。
func TestGuardAllowsKernelPublish(t *testing.T) {
	bus := NewBus()
	bus.SetGuard(&fakeGuard{allowPublish: map[string]bool{}})

	if err := bus.PublishAs(Publisher{}, &Event{Type: EventSystem}); err != nil {
		t.Fatalf("内核身份应放行: %v", err)
	}
}

// ④ 内核身份**也要过载荷脱敏**。
//
// 凭据闸门与身份无关：内核把 api_key 拼进事件，和插件拼进去一样危险。
//
// ★ 2026-10-29 加入入向清洗后，本判据要区分两个方向：
//
//	出向（发布时）：secret "sk-123" → "@{redacted}"（别名）
//	入向（投递前）：再把载荷洗一遍 → "[inbound]"（本测试替身的标记）
//
// 所以**订阅者看到的是入向结果**，而不是出向结果。这不是回归：
// 两面都必须发生，旧的断言只钉了第一面，现在两面都钉。
func TestGuardScrubsKernelPayload(t *testing.T) {
	bus := NewBus()
	g := &fakeGuard{allowPublish: map[string]bool{}}
	bus.SetGuard(g)

	var got *Event
	bus.Subscribe(EventSystem, func(e *Event) { got = e })

	bus.Publish(&Event{Type: EventSystem, Payload: map[string]interface{}{"secret": "sk-123"}})
	if got == nil {
		t.Fatal("应收到事件")
	}

	// 出向跑过（原文不再以明文到达订阅者）
	if g.scrubCalls == 0 {
		t.Error("ScrubPayload（出向）应被调用 —— 内核载荷同样要过凭据闸门")
	}
	// 入向也跑过（投递前再洗一次）
	if g.inboundCalls == 0 {
		t.Error("ScrubInbound（入向）应被调用 —— 投递前必须清洗")
	}
	// 订阅者拿到的不能是原文
	if got.Payload["secret"] == "sk-123" {
		t.Fatalf("★ 订阅者收到了明文凭据（出向/入向都没生效）: %v", got.Payload["secret"])
	}
}

// ④b 入向清洗对**所有订阅入口**生效。
//
// 这正是把清洗从 sdk 适配层搬到总线上的理由：
// 以前只有 s.Events().Subscribe 那条路会洗，其余入口拿到原始载荷。
func TestGuardScrubsInboundForAllSubscribers(t *testing.T) {
	bus := NewBus()
	g := &fakeGuard{allowPublish: map[string]bool{}, allowSubscribe: map[string]bool{"system": true}}
	bus.SetGuard(g)

	// 三种不同身份的订阅者：内核（Subscribe）、插件 A（SubscribeAs）。
	var kernelGot, pluginGot *Event
	bus.Subscribe(EventSystem, func(e *Event) { kernelGot = e })
	if _, err := bus.SubscribeAs(Publisher{Name: "qq"}, EventSystem, func(e *Event) { pluginGot = e }); err != nil {
		t.Fatalf("订阅应放行: %v", err)
	}

	bus.Publish(&Event{Type: EventSystem, Payload: map[string]interface{}{"secret": "sk-999"}})

	for name, ev := range map[string]*Event{"kernel": kernelGot, "plugin": pluginGot} {
		if ev == nil {
			t.Fatalf("%s 订阅者应收到事件", name)
		}
		if ev.Payload["secret"] == "sk-999" {
			t.Errorf("★ %s 订阅者收到了明文 —— 入向清洗未覆盖该入口", name)
		}
	}
}

// ⑤ 未授权订阅必须被拒。
//
// 若能订阅 raw_input，插件就能读到用户的全部输入。
func TestGuardRejectsUnauthorizedSubscribe(t *testing.T) {
	bus := NewBus()
	bus.SetGuard(&fakeGuard{allowSubscribe: map[string]bool{"tools_ready": true}})

	if _, err := bus.SubscribeAs(Publisher{Name: "evil"}, EventRawInput, func(*Event) {}); err == nil {
		t.Fatal("★ 未授权订阅必须报错")
	}
	// 白名单内的可订阅
	if _, err := bus.SubscribeAs(Publisher{Name: "p1"}, EventToolsReady, func(*Event) {}); err != nil {
		t.Errorf("白名单订阅应放行: %v", err)
	}
}

// ⑥ 没有闸门时行为不变（开发/测试场景，且保证既有部署不被意外破坏）。
func TestGuardAbsentBehavesAsBefore(t *testing.T) {
	bus := NewBus()
	got := 0
	bus.Subscribe(EventRawInput, func(*Event) { got++ })
	if err := bus.PublishAs(Publisher{Name: "any"}, &Event{Type: EventRawInput}); err != nil {
		t.Fatalf("无闸门时不应报错: %v", err)
	}
	if got != 1 {
		t.Errorf("无闸门时应正常送达，实际 %d", got)
	}
}

// ⑦ 闸门可以被后装（启动顺序：总线先建、security 后装配）。
//
// 这条钉住真实的装配顺序：kernelapi 创建 Bus，security 模块在更晚的层
// 才注入 Guard。先发的 security_ready 事件不受影响，之后全部受控。
func TestGuardCanBeInstalledLater(t *testing.T) {
	bus := NewBus()
	// 先无闸门发一条
	if err := bus.PublishAs(Publisher{Name: "p"}, &Event{Type: EventSystem}); err != nil {
		t.Fatal(err)
	}
	// 装闸门后必须生效
	bus.SetGuard(&fakeGuard{allowPublish: map[string]bool{}})
	if err := bus.PublishAs(Publisher{Name: "p"}, &Event{Type: EventSystem}); err == nil {
		t.Fatal("★ 后装闸门必须立即生效")
	}
}

// ⑧ 未授权发布不得污染 EventAll 通配订阅。
func TestGuardDeniesBeforeWildcardHandlers(t *testing.T) {
	bus := NewBus()
	bus.SetGuard(&fakeGuard{allowPublish: map[string]bool{}})
	got := 0
	bus.Subscribe(EventAll, func(*Event) { got++ })

	_ = bus.PublishAs(Publisher{Name: "evil"}, &Event{Type: EventAgentOutput})
	if got != 0 {
		t.Errorf("★ 被拒事件不得送到 EventAll 订阅者，实际 %d", got)
	}
}
