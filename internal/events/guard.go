package events

// 事件总线的**统一监控闸门**（类型与接口部分）。
//
// ## 为什么必须做在总线本体上，而不是各适配层
//
// 在此之前，授权只存在于 `internal/sdk/events.go`（插件走 `s.Events()` 那条路）。
// 而实际有**四条**路径能发出/收到事件：
//
//	① s.Events().Publish/Subscribe     ← 过策略（唯一一条）
//	② s.Publish(evt) / s.Subscribe()   ← 裸总线，不过策略
//	③ s.EventBus() 直接拿 *Bus          ← 裸总线，连适配层都绕过
//	④ 内核模块 bus.Publish              ← 内核自身，不需要策略
//
// 于是 `EventPolicy` 只是"合规者才受约束"的一纸约定：任何插件调
// `s.EventBus().Publish(...)` 就能发任意事件（含伪造内核事件）。
//
// 现在把闸门放到 **Bus.Publish / Bus.Subscribe 本身**（见 bus.go）：
// 注册一个 Guard，所有发布与订阅都先问它。适配层不再持有策略，
// 只负责把「谁在调用」告诉总线。
//
// ## 为什么用接口注入而不是直接 import security
//
// events 是最底层的广播设施，security 依赖它（要发 security_ready）。
// 直接 import 会成环。所以这里定义**最小接口**，由装配方注入实现。
//
// 这与 syncbus 的「内核可写 / 插件只读」是同一思路：类型层面表达边界。

import "fmt"

// Publisher 是事件发布/订阅的主体身份。
//
// ★ 为什么必须显式传：没有身份就无法判定"这个 plugin 能不能发这个事件"。
// 空身份 = 内核内部（内核模块自己发布时不需要授权）。
type Publisher struct {
	// Name 是插件名；空串表示**内核内部**。
	//
	// 内核模块（update / device / pluginhost / agent）发布系统事件时
	// 不填 Name —— 它们本身就是可信的执行体，且没有"插件名"这个身份。
	Name string
}

// IsKernel 报告该发布者是否内核内部。
func (p Publisher) IsKernel() bool { return p.Name == "" }

// Guard 是总线闸门：判定某个主体能否发布/订阅某事件。
//
// 实现在 internal/security（策略与载荷脱敏都在那里），
// events 包只负责"每次发布/订阅都问一遍"。
type Guard interface {
	// AllowPublish 判定能否发布；allowed=false 时 reason 会进日志与错误。
	AllowPublish(p Publisher, eventType EventType) (allowed bool, reason string)
	// AllowSubscribe 判定能否订阅。
	AllowSubscribe(p Publisher, eventType EventType) (allowed bool, reason string)
	// ScrubPayload 对**出向**载荷做脱敏（凭据闸门）。
	//
	// 内核内部发布者同样要过：内核也可能把敏感值拼进事件
	//（例如把 api_key 放进 Stage 事件里）。
	ScrubPayload(eventType EventType, payload map[string]interface{}) map[string]interface{}

	// ScrubInbound 对**入向**载荷做清洗（投递给订阅者之前）。
	//
	// ## 为什么入向必须在总线上做（用户 2026-10-29 指正）
	//
	// 原先入向清洗在 `internal/sdk/events.go` 的订阅回调里手工做（scrubIn），
	// 而那条路径只盖住了 `s.Events().Subscribe` —— 其余路径
	//（`s.EventBus()`、`s.Subscribe`、内核模块自己的 Subscribe）
	// 拿到的都是**原始载荷**。同一个事件，订阅者看到的脏净程度取决于
	// 它当初用的是哪个入口 —— 策略存在两份口径，迟早漂移。
	//
	// 安全模块既然持有总线、而**投递本身就在总线内部**，
	// 入向清洗就该在那里做一次，而不是让每个适配层各做一遍。
	//
	// ## 与 ScrubPayload 的对称性
	//
	//	出向（发布时）：发布者 → [ScrubPayload] → 总线
	//	入向（投递时）：总线 → [ScrubInbound] → 每个订阅者
	//
	// 两者都是“凭据闸门”，与身份无关：内核发出的载荷也要洗，
	// 因为它同样可能拼进敏感值。
	ScrubInbound(eventType EventType, payload map[string]interface{}) map[string]interface{}
}

// DeniedError 表示事件操作被闸门拒绝。
//
// ★ 为什么返回 error 而不是静默丢弃：静默丢弃会让插件以为
// "发了、没反应"，那是最难排查的一类故障。
type DeniedError struct {
	Publisher string
	EventType string
	Reason    string
	Subscribe bool
}

func (e *DeniedError) Error() string {
	verb := "publish"
	if e.Subscribe {
		verb = "subscribe to"
	}
	return fmt.Sprintf("events: %q may not %s %q: %s", e.Publisher, verb, e.EventType, e.Reason)
}
