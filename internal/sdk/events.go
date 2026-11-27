package sdk

import (
	"fmt"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 事件类型/事件类型别名：插件经 SDK 订阅/发布内核事件，
// 无需直接 import internal/events（内核事件总线仅通过 SDK 暴露）。

type EventType = events.EventType
type Event = events.Event

const (
	EventAgentLLMChain  = events.EventAgentLLMChain
	EventReasoning      = events.EventReasoning
	EventSystem         = events.EventSystem
	EventTerminalOutput = events.EventTerminalOutput
	EventAll            = events.EventAll

	// EventPluginsChanged 是插件可见面变化事件（订阅者重新拉取虚拟插件管理器）。
	EventPluginsChanged = events.EventPluginsChanged

	// ★ 已删（2026-11-25，docs §9.5）：
	//
	//	EventRawInput        → on_input 阶段钩子
	//	EventAgentOutput     → before_output 阶段 + EmitOutputTo 本身
	//	EventStage           → 插件自己的 stage handler 通知自己的客户端
	//	EventContentDelta    → 输出通道逐 chunk 调用
	//	EventReasoningDelta  → 同上（kind=reasoning）
	//
	// 判据是归属：这些是 **agent 的状态**，总线只服务内核模块之间。

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
	bus        kernelapi.EventBus
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
func newEventsAPI(pluginName string, bus kernelapi.EventBus, rt *security.Runtime) pubsdk.EventsAPI {
	// sec 由内核注入（不再读 RuntimeGlobal 包级单例）。
	api := &eventsAPI{pluginName: pluginName, bus: bus, sec: rt}
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

// Publish 以**本插件身份**发布事件。
//
// ## ★ 为什么必须走 PublishAs（2026-10-29 收敛）
//
// 此前这里调的是 `bus.Publish`，而它的实现是 `publishAs(Publisher{}, ...)` ——
// 空 Publisher 的 `IsKernel()` 为真，于是 security 的 `AllowPublish`
// **第一行就直接放行**。也就是说：总线上的事件策略对这条路径从未生效过，
// 它之所以“安全”，只是因为本函数自己又查了一遍策略。
//
// 后果是**同一条策略有两份实现**（本文件 + security.eventGuard），
// 一旦口径漂移就会出现“总线放行、适配层拦截”或反之的分裂。
//
// 现在只保留一份：
//
//	e.bus.PublishAs(Publisher{Name: e.pluginName}, ...)
//	  → security.eventGuard.AllowPublish   （授权，唯一一处）
//	  → security.eventGuard.ScrubPayload   （出向脱敏）
//	  → 投递时 security.eventGuard.ScrubInbound（入向清洗）
//
// 本文件只剩“把身份如实告诉总线”这一件职责 —— 插件模块不再持有策略判断
// （用户裁定：所有安全部分归安全模块）。
func (e *eventsAPI) Publish(eventType string, payload map[string]interface{}) error {
	if e.unavailable != "" {
		return fmt.Errorf("events: %s", e.unavailable)
	}
	if payload == nil {
		payload = map[string]interface{}{}
	}
	// 载荷键数上限仍在这里判：它是**当前唯一实现**（eventGuard.AllowPublish
	// 只拿到 eventType、看不到 payload，故无法在那里做）。
	// 删掉它 = 直接失去这项保护，所以不属“重复”而不应一并丢弃。
	if d := e.checkSize(len(payload)); !d.Allowed {
		return fmt.Errorf("%s", d.Reason)
	}
	return e.bus.PublishAs(events.Publisher{Name: e.pluginName}, &events.Event{
		Type:    events.EventType(eventType),
		Source:  e.pluginName,
		Payload: payload,
	})
}
func (e *eventsAPI) checkSize(keys int) security.Decision {
	if e.sec != nil {
		return e.sec.CheckEventPayloadSize(keys)
	}
	return security.DefaultEventPolicy().CheckEventPayloadSize(keys)
}

func (e *eventsAPI) Subscribe(eventType string, handler func(map[string]interface{})) (func(), error) {
	if e.unavailable != "" {
		return nil, fmt.Errorf("events: %s", e.unavailable)
	}
	if handler == nil {
		return nil, fmt.Errorf("events: handler must not be nil")
	}
	// 走 SubscribeAs：授权交给 security.eventGuard.AllowSubscribe（唯一一处），
	// 入向清洗已由总线在投递前统一执行 —— 这里不再自己洗一遍。
	unsub, err := e.bus.SubscribeAs(events.Publisher{Name: e.pluginName},
		events.EventType(eventType), func(evt *events.Event) {
			handler(evt.Payload)
		})
	if err != nil {
		return nil, err
	}
	e.unsubs = append(e.unsubs, unsub)
	return unsub, nil
}

// stop 解除本插件建立的全部订阅（插件卸载时由宿主调用）。
func (e *eventsAPI) stop() {
	for _, u := range e.unsubs {
		u()
	}
	e.unsubs = nil
}
