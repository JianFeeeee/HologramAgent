package security

// EventPolicy 作为**总线闸门**的实现（events.Guard）。
//
// ## 为什么这一层必须存在（用户 2026-10-28 的指正）
//
// 「eventbus 上所有事件都要接受安全模块的监控。」
//
// 在此之前，EventPolicy 只被 internal/sdk/events.go 调用 —— 而那是**四条**
// 事件路径中的**一条**。其余三条（s.Publish / s.Subscribe / s.EventBus()）
// 拿的是裸 *events.Bus，策略完全不参与。于是「安全监控」只对合规者生效。
//
// 现在闸门挂在总线本体上（见 internal/events/bus.go 的 publishAs/subscribeAs），
// 本文件把 security 的策略与脱敏接上去。
//
// ## 分工（与既有设计一致）
//
//	internal/security   策略 + 载荷过闸（**本文件**）
//	internal/events     总线：每次发布/订阅都问一遍 Guard
//	internal/sdk        适配：把「谁在调用」翻译成 events.Publisher
//	sdk（公开）          只给接口与事件名

import (
	"github.com/JianFeeeee/HologramAgent/internal/events"
)

// eventGuard 把 Runtime 的策略适配成 events.Guard。
type eventGuard struct {
	rt *Runtime
}

// AsEventGuard 返回总线闸门实现。
//
// rt 为 nil 时返回 nil（调用方据此不装闸门）。**不**返回一个
// "永远放行"的假实现 —— 那会让"安全未装配"看起来像"安全已通过"。
func AsEventGuard(rt *Runtime) events.Guard {
	if rt == nil {
		return nil
	}
	return &eventGuard{rt: rt}
}

// AllowPublish 判定能否发布。
//
// ★ 内核身份（空 Name）一律放行：内核模块发布系统事件不需要授权，
// 它们本身就是可信执行体。但**载荷脱敏仍然执行**（见 ScrubPayload）——
// 内核也可能把敏感值拼进事件。
func (g *eventGuard) AllowPublish(p events.Publisher, eventType events.EventType) (bool, string) {
	if p.IsKernel() {
		return true, ""
	}
	d := g.rt.CheckEventPublish(p.Name, string(eventType))
	return d.Allowed, d.Reason
}

// AllowSubscribe 判定能否订阅。
func (g *eventGuard) AllowSubscribe(p events.Publisher, eventType events.EventType) (bool, string) {
	if p.IsKernel() {
		return true, ""
	}
	d := g.rt.CheckEventSubscribe(p.Name, string(eventType))
	return d.Allowed, d.Reason
}

// ScrubPayload 对出向载荷逐字段脱敏。
//
// ★ 对**所有**发布者生效（含内核）：凭据闸门与身份无关。
// 内核模块把 api_key 拼进事件，和插件拼进去一样危险。
func (g *eventGuard) ScrubPayload(eventType events.EventType, payload map[string]interface{}) map[string]interface{} {
	if g.rt == nil || payload == nil {
		return payload
	}
	out := make(map[string]interface{}, len(payload))
	for k, v := range payload {
		if s, ok := v.(string); ok && s != "" {
			clean, n := g.rt.PrepareOutbound(Inspection{
				Direction: DirectionOutbound,
				Stage:     "event:" + string(eventType),
			}, s)
			if n > 0 {
				out[k] = clean
				continue
			}
		}
		out[k] = v
	}
	return out
}

// ScrubInbound 对**入向**载荷逐字段清洗（投递给订阅者之前）。
//
// 与 ScrubPayload 对称，但用的是 Inbound 方向：
// 外部模块写进载荷的明文凭据在这里被换回别名。
//
// 为何必须在总线上做（而不是订阅方的适配层）：
// 总线是**唯一的投递点**，而订阅入口有四五条。在适配层做就只好每条
// 各自实现一遍（实际就是这么发生的：sdk 侧曾有一份 scrubIn），
// 漏一条就是漏一条路径。安全模块持有总线 ⇒ 在这里做一次就覆盖全部。
func (g *eventGuard) ScrubInbound(eventType events.EventType, payload map[string]interface{}) map[string]interface{} {
	if g.rt == nil || payload == nil {
		return payload
	}
	out := make(map[string]interface{}, len(payload))
	for k, v := range payload {
		if s, ok := v.(string); ok && s != "" {
			clean, n := g.rt.ScrubInbound(Inspection{
				Direction: DirectionInbound,
				Stage:     "event:" + string(eventType),
			}, s)
			if n > 0 {
				out[k] = clean
				continue
			}
		}
		out[k] = v
	}
	return out
}

// 编译期确认实现了 events.Guard。
var _ events.Guard = (*eventGuard)(nil)
