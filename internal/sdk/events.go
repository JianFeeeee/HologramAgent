package sdk

import (
	"fmt"
	"strings"

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
// 边界由三件事决定，缺一不可：
//  1. 类型白名单 + 插件自己的命名空间（`plugin.<name>.*`）
//  2. 载荷出向经 security 审查（明文凭据换成 @{别名}）+ 键数上限
//  3. 订阅只开只读事件（tools_ready / security_ready / device_ready）
//
// 为什么不直接把 *events.Bus 交给插件：Bus.Publish 能发 EventAll 通配类型、
// 能伪造任意 Source、载荷不做任何审查。那不是"多给一个便利"，而是把内核
// 内部同步面变成没有边界的公共写口。

const (
	// eventPayloadMaxKeys 限制载荷顶层键数，防止"把一个对象当事件"塞进来。
	// 事件是**通知**，不是数据通道：大块数据该走插件自己的通道或工具返回值。
	eventPayloadMaxKeys = 64
)

type eventsAPI struct {
	pluginName string
	bus        *events.Bus
	sec        *security.Runtime // 可 nil（安全模块未装配时零成本跳过）
	unsubs     []func()
}

// newEventsAPI 构造插件的受限事件面（bus 为 nil 时返回 nil，调用方按 nil 处理）。
func newEventsAPI(pluginName string, bus *events.Bus) pubsdk.EventsAPI {
	if bus == nil {
		return nil
	}
	return &eventsAPI{pluginName: pluginName, bus: bus, sec: security.RuntimeGlobal()}
}

func (e *eventsAPI) AllowedPublish() []string {
	out := pubsdk.PluginPublishableEvents()
	return append(out, "plugin."+e.pluginName+".*")
}

func (e *eventsAPI) AllowedSubscribe() []string { return pubsdk.PluginSubscribableEvents() }

// publishAllowed：白名单精确匹配，或本插件命名空间前缀（plugin.<name>.<后缀>）。
func (e *eventsAPI) publishAllowed(t string) bool {
	for _, a := range pubsdk.PluginPublishableEvents() {
		if t == a {
			return true
		}
	}
	prefix := "plugin." + e.pluginName + "."
	return strings.HasPrefix(t, prefix) && len(t) > len(prefix)
}

func (e *eventsAPI) subscribeAllowed(t string) bool {
	for _, a := range pubsdk.PluginSubscribableEvents() {
		if t == a {
			return true
		}
	}
	return false
}

func (e *eventsAPI) Publish(eventType string, payload map[string]interface{}) error {
	if eventType == "" {
		return fmt.Errorf("events: event type must not be empty")
	}
	if !e.publishAllowed(eventType) {
		return fmt.Errorf("events: plugin %q may not publish %q (allowed: %v)",
			e.pluginName, eventType, e.AllowedPublish())
	}
	if payload == nil {
		payload = map[string]interface{}{}
	}
	if len(payload) > eventPayloadMaxKeys {
		return fmt.Errorf("events: payload has %d keys, limit is %d", len(payload), eventPayloadMaxKeys)
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
	if !e.subscribeAllowed(eventType) {
		return nil, fmt.Errorf("events: plugin %q may not subscribe to %q (allowed: %v)",
			e.pluginName, eventType, e.AllowedSubscribe())
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
