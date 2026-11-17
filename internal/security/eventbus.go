package security

// eventbus.go —— 安全模块**注册的事件总线代理面**（用户 2026-11-17 第⑥次裁定）。
//
// ## 用户原话
//
// > 「是让安全模块代理的接口成为唯一选择」
//
// ## 它解决什么
//
// 在此之前，能发/收事件的地方有四条路径，其中三条拿的是**裸 `*events.Bus`**
// （`sdk.Publish` / `sdk.Subscribe` / `sdk.EventBus()`），闸门只盖住了
// `sdk.Events()` 那一条。后来把闸门挂到 `Bus.Publish`/`Bus.Subscribe` 本身上，
// 于是**绕过策略**的路被堵住了 —— 但"裸总线仍能被拿到"这件事没有变：
// 全仓仍有 47 处 `*events.Bus` 引用，每个消费方都能调 `PublishAs` 之外的
// 一切（包括 `SetGuard` 本身，那等于把闸门换成自己的）。
//
// 现在：`FacEventBus` 上放的是**本文件的代理面**，裸总线退回 `FacEventBusRaw`
// （只有装配侧的 security 取，用来装闸门并把它挡在消费方可达面之外）。
//
// ## 与"不改 publish/subscribe 的调用写法"如何同时成立
//
// 代理沿用的方法与总线的既有 API **同形、同语义**（Publish/PublishAs/
// Subscribe/SubscribeAs）。调用方写 `bus.Publish(evt)` 还是同一行，
// 只是它现在拿到的是代理而不是裸总线 —— 换实现，不换写法。
//
// ## 闸门位置（不能搬）
//
// 授权判定与载荷脱敏仍发生在**发布时刻**（`events.Bus.publishAs` 里），
// 因为被拒必须能当场返回 `DeniedError` 给调用方。投递（投给谁、按什么顺序）
// 才由事件循环推动。所以本代理只做两件事：**限定身份**、**限定可达面**
// （不给 SetGuard / SetDispatcher —— 那两个是装配侧才能碰的）。

import (
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// AsEventBus 把裸总线包成**消费方唯一的取用面**（安全模块的代理）。
//
// bus 为 nil 时返回 nil：装配期缺总线是装配错误，由调用方响亮报错，
// 不在这里造一个"永远成功"的假实现（那会让"没装总线"看起来像"事件都发出去了"）。
//
// pluginName 是**内核身份之外**的主体名（插件 / agent）；空 = 内核内部。
// 生产装配下由安全模块统一注册一个内核身份的代理；插件侧仍走 sdk 的
// 带身份视图（它需要按插件名逐条过策略）。
func AsEventBus(bus *events.Bus, pluginName string) kernelapi.EventBus {
	if bus == nil {
		return nil
	}
	return &eventBusProxy{bus: bus, name: pluginName}
}

// eventBusProxy 是安全模块注册的代理面。
//
// ★ 它**不暴露** SetGuard / SetDispatcher / Guard / Dispatcher：
// 那四个是装配侧的旋钮，消费方拿到就等于能拆掉闸门或换掉驱动者。
type eventBusProxy struct {
	bus  *events.Bus
	name string
}

func (p *eventBusProxy) Publish(evt *model.Event) {
	p.bus.Publish(evt)
}

func (p *eventBusProxy) PublishAs(pub model.Publisher, evt *model.Event) error {
	// 身份以代理自己的为准：调用方不能靠传一个空 Publisher 冒充内核。
	if p.name != "" {
		pub = model.Publisher{Name: p.name}
	}
	return p.bus.PublishAs(pub, evt)
}

func (p *eventBusProxy) Subscribe(eventType model.EventType, handler model.Handler) func() {
	return p.bus.Subscribe(eventType, handler)
}

func (p *eventBusProxy) SubscribeAs(pub model.Publisher, eventType model.EventType, handler model.Handler) (func(), error) {
	if p.name != "" {
		pub = model.Publisher{Name: p.name}
	}
	return p.bus.SubscribeAs(pub, eventType, handler)
}

// 编译期确认：代理满足接口层的消费面。
var _ kernelapi.EventBus = (*eventBusProxy)(nil)

// ── 投递驱动者的注入点 ──

// AsEventDispatchSink 返回**投递驱动者的注入点**。
//
// ★ 为什么注入点归安全模块（2026-11-17 第⑤次裁定）：裸总线不出 security
//
//	的手，而"投递由谁推动"是运行期事件循环的决定。两者要能连上，就必须
//	有一个由 security 掌管的接口 —— 它就是本类型。事件循环只拿到这一个
//	动作（SetDispatcher），拿不到总线本身，也拿不到 Guard。
func AsEventDispatchSink(bus *events.Bus) kernelapi.EventDispatchSink {
	if bus == nil {
		return nil
	}
	return &eventDispatchSink{bus: bus}
}

type eventDispatchSink struct{ bus *events.Bus }

func (s *eventDispatchSink) SetDispatcher(d kernelapi.EventDispatcher) {
	if d == nil {
		s.bus.SetDispatcher(nil)
		return
	}
	s.bus.SetDispatcher(dispatchAdapter{d: d})
}

// dispatchAdapter 把接口层的驱动者适配成事件包的 Dispatcher。
//
// 两者的值类型是同一套（model 声明、events 别名），所以这里只有转发。
type dispatchAdapter struct{ d kernelapi.EventDispatcher }

func (a dispatchAdapter) Dispatch(evt *events.Event, handlers []events.Handler) {
	a.d.Dispatch(evt, handlers)
}

var _ kernelapi.EventDispatchSink = (*eventDispatchSink)(nil)
