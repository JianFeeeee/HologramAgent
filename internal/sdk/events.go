package sdk

import (
	"fmt"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 事件类型/事件类型别名：插件经 SDK 订阅/发布内核事件，
// 无需直接 import internal/events（内核事件总线仅通过 SDK 暴露）。

type EventType = events.EventType
type Event = events.Event

const (
	EventRawInput       = events.EventRawInput
	EventAgentOutput    = events.EventAgentOutput
	EventAgentLLMChain  = events.EventAgentLLMChain
	EventToolCall       = events.EventToolCall
	EventReasoning      = events.EventReasoning
	EventStage          = events.EventStage
	EventSystem         = events.EventSystem
	EventTerminalOutput = events.EventTerminalOutput
	EventAll            = events.EventAll

	// 流式增量事件（token 级）：核心 process() 流式化后每收到一个增量块发布。
	// 客户端可选订做真逐 token 渲染；聚合事件仍照常发布，旧订阅者不受影响。
	EventReasoningDelta = events.EventReasoningDelta
	EventContentDelta   = events.EventContentDelta

	// EventMemoryAccess 是图记忆的真实读写事件（哪个块被读/写/删/合）。
	//
	// 星图靠它做「反馈」：此前只能拿 tool_call 的工具名猜节点，而工具名是英文、
	// 实体名是中文概念（实测命中 0），只能退化成点亮一批固定节点。
	EventMemoryAccess = events.EventMemoryAccess
)

// ───────────────────── 受限事件总线（插件可见） ─────────────────────
//
// **本层只做适配，不做策略**（用户 2026-10-27 的指正）：
//
//	internal/security    谁可以发/订阅什么事件（EventPolicy）+ 载荷过闸
//	internal/sdk（本文件）  查策略 → 发/订 → 过闸 → 落到 *events.Bus
//	sdk（公开）            只给接口与事件名
//
// 为什么策略必须住在 security：事件的边界就是**授权**。能发 skill_detected
// 等于能驱动 skillmgr 接管；能发 update_request 等于能触发内核升级流程。
// 这类判断与工具黑白名单是同一类，必须与它们同处一处，否则策略会有两份真相，
// 换个实现就换口径。

type eventsAPI struct {
	pluginName string
	bus        *events.Bus
	sec        *security.Runtime // 可 nil：未装配时按**默认策略**（最小集合）判定
	unsubs     []func()
	// unavailable 非空表示宿主没接总线：Publish/Subscribe 一律返回该原因。
	unavailable string
}

// newEventsAPI 构造插件的受限事件面。
//
// ★ bus 为 nil 时返回**不可用实现**而不是 nil：
// 返回 nil 会让 `s.Events().Publish(...)` 在未接总线的宿主里直接 panic
// （实测：internal/plugins 的集成测试在 loadOne 阶段崩在 clawhubadapter 上）。
// 插件不该为"宿主没接总线"写 nil 判断——那是一个该报错、而不是该崩溃的状态。
func newEventsAPI(pluginName string, bus *events.Bus) pubsdk.EventsAPI {
	api := &eventsAPI{pluginName: pluginName, bus: bus, sec: security.RuntimeGlobal()}
	if bus == nil {
		api.unavailable = "event bus is not wired in this host"
	}
	return api
}

func (e *eventsAPI) AllowedPublish() []string {
	pol := e.EventPolicy()
	out := make([]string, len(pol.Publishable))
	copy(out, pol.Publishable)
	return append(out, "plugin."+e.pluginName+".*")
}

func (e *eventsAPI) AllowedSubscribe() []string {
	pol := e.EventPolicy()
	out := make([]string, len(pol.Subscribable))
	copy(out, pol.Subscribable)
	return out
}

// EventPolicy 取当前策略：优先用 security Runtime 的（可热改），
// Runtime 为 nil 时退回 security 包的默认策略（同样是最小集合，失败方向收紧）。
func (e *eventsAPI) EventPolicy() security.EventPolicy {
	if e.sec != nil {
		return e.sec.EventPolicy()
	}
	return security.DefaultEventPolicy()
}

func (e *eventsAPI) Publish(eventType string, payload map[string]interface{}) error {
	if e.unavailable != "" {
		return fmt.Errorf("events: %s", e.unavailable)
	}
	if d := e.checkPublish(eventType); !d.Allowed {
		return fmt.Errorf("%s", d.Reason)
	}
	if payload == nil {
		payload = map[string]interface{}{}
	}
	if d := e.checkSize(len(payload)); !d.Allowed {
		return fmt.Errorf("%s", d.Reason)
	}

	// ★ 载荷出向审查：事件载荷会被其他订阅者（含 WebUI/状态面）读到，
	// 插件顺手把凭据放进去就等于绕过凭据闸门。
	e.bus.Publish(&events.Event{
		Type:    events.EventType(eventType),
		Source:  e.pluginName,
		Payload: e.scrubOut(eventType, payload),
	})
	return nil
}

func (e *eventsAPI) checkPublish(t string) security.Decision {
	if e.sec != nil {
		return e.sec.CheckEventPublish(e.pluginName, t)
	}
	return security.DefaultEventPolicy().CheckEventPublish(e.pluginName, t)
}

func (e *eventsAPI) checkSubscribe(t string) security.Decision {
	if e.sec != nil {
		return e.sec.CheckEventSubscribe(e.pluginName, t)
	}
	return security.DefaultEventPolicy().CheckEventSubscribe(e.pluginName, t)
}

func (e *eventsAPI) checkSize(keys int) security.Decision {
	if e.sec != nil {
		return e.sec.CheckEventPayloadSize(keys)
	}
	return security.DefaultEventPolicy().CheckEventPayloadSize(keys)
}

// scrubOut 对载荷做一次出向脱敏（安全模块未装配时原样返回）。
func (e *eventsAPI) scrubOut(eventType string, payload map[string]interface{}) map[string]interface{} {
	if e.sec == nil {
		return payload
	}
	out := make(map[string]interface{}, len(payload))
	for k, v := range payload {
		if s, ok := v.(string); ok {
			if clean, n := e.sec.PrepareOutbound(security.Inspection{
				Direction: security.DirectionOutbound,
				Stage:     "event:" + eventType,
				Source:    e.pluginName,
			}, s); n > 0 {
				out[k] = clean
				continue
			}
		}
		out[k] = v
	}
	return out
}

func (e *eventsAPI) Subscribe(eventType string, handler func(map[string]interface{})) (func(), error) {
	if e.unavailable != "" {
		return nil, fmt.Errorf("events: %s", e.unavailable)
	}
	if d := e.checkSubscribe(eventType); !d.Allowed {
		return nil, fmt.Errorf("%s", d.Reason)
	}
	if handler == nil {
		return nil, fmt.Errorf("events: handler must not be nil")
	}
	unsub := e.bus.Subscribe(events.EventType(eventType), func(evt *events.Event) {
		handler(e.scrubIn(eventType, evt.Payload))
	})
	e.unsubs = append(e.unsubs, unsub)
	return unsub, nil
}

// scrubIn 对入向载荷做清洗（事件可能带其他模块写进来的内容）。
func (e *eventsAPI) scrubIn(eventType string, p map[string]interface{}) map[string]interface{} {
	if e.sec == nil || p == nil {
		return p
	}
	out := make(map[string]interface{}, len(p))
	for k, v := range p {
		if s, ok := v.(string); ok {
			if clean, n := e.sec.ScrubInbound(security.Inspection{
				Direction: security.DirectionInbound,
				Stage:     "event:" + eventType,
			}, s); n > 0 {
				out[k] = clean
				continue
			}
		}
		out[k] = v
	}
	return out
}

// stop 解除本插件建立的全部订阅（插件卸载时由宿主调用）。
func (e *eventsAPI) stop() {
	for _, u := range e.unsubs {
		u()
	}
	e.unsubs = nil
}
