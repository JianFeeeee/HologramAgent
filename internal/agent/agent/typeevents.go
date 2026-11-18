package agent

// typeevents.go —— **类型注册事件的订阅方**（内核侧，用户裁定 2026-11-18）。
//
// ## 这条链路完整形状
//
//	内置插件（线程化，同地址空间）
//	    s.Events().Publish("agent.type.register", {"plugin":…, "descriptor": Descriptor{…go funcs…}})
//	    ↓  security.EventPolicy（允许发布）
//	裸总线 publishAs（过闸门 + 载荷脱敏）
//	    ↓  eventGuard.AllowPublish
//	本文件 Subscribe（身份 = 内核，无需授权）
//	    ↓  registry.HandleRegisterEvent（再查一次 IsBuiltinPlugin）
//	registry.Register（唯一那张表）
//	    ↓
//	admin_child 工具表 / 实例管理器 / 持久化恢复  全部自动认得新类型
//
// ## 为什么订阅在内核而不是某个插件里
//
// 注册表是**内核的**（类型决定"能创建什么实例"）。让某个插件去订阅，
// 就等于把"谁能扩类型"交给一个插件 —— 那与"内核拥有类型表"矛盾。
//
// ## 为什么需要 IsBuiltinPlugin 再查一次
//
// security 的策略表只能表达「这个事件名可发布」；而"谁是内置插件"是
// **运行期事实**（插件可以在运行期被装/卸）。所以订阅侧必须再查一遍，
// 这是本文件存在的一个直接理由。

import (
	"log"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	"github.com/JianFeeeee/HologramAgent/internal/events"
)

// SubscribeTypeEvents 订阅类型注册/撤销事件（根 agent 装配期一次）。
//
// 返回退订函数（内核退出时调；不调也无害 —— 进程结束时整张总线一起消失）。
//
// ★ 幂等：重复调用只装一次（返回同一个退订函数）。装配点可能因重载被跑
//
//	两遍，重复订阅会让同一条注册事件被处理两次 —— 第二次的"已注册"报错
//	会变成一条误导性的日志。
func (a *Agent) SubscribeTypeEvents() func() {
	if a == nil || a.publisher.bus == nil {
		return func() {}
	}
	typeEventUnsubMu.Lock()
	if fn, ok := typeEventUnsub[registry.EventTypeRegister]; ok {
		typeEventUnsubMu.Unlock()
		return fn
	}
	typeEventUnsubMu.Unlock()

	pluginBuiltin := a.isBuiltinPluginFn()

	regFn := a.publisher.bus.Subscribe(registry.EventTypeRegister, func(evt *events.Event) {
		if evt == nil {
			return
		}
		if err := registry.HandleRegisterEvent(registry.EventTypeRegister, evt.Payload, pluginBuiltin); err != nil {
			// ★ 拒绝**必须留痕**：静默丢弃会让"插件注册了但没生效"
			//   变成一条查不出的谜。
			log.Printf("[typeevents] register rejected: %v", err)
			return
		}
		log.Printf("[typeevents] agent type registered by %q",
			strPayload(evt.Payload, "plugin"))
	})

	unregFn := a.publisher.bus.Subscribe(registry.EventTypeUnregister, func(evt *events.Event) {
		if evt == nil {
			return
		}
		// 存活实例由实例管理器给出（撤销仍有实例的类型必须被拒）。
		var alive []string
		if m := a.EnsureInstanceManager(); m != nil {
			for _, b := range m.ListChildren(strPayload(evt.Payload, "type")) {
				alive = append(alive, b.ID)
			}
		}
		if err := registry.HandleUnregisterEvent(evt.Payload, alive); err != nil {
			log.Printf("[typeevents] unregister rejected: %v", err)
			return
		}
		log.Printf("[typeevents] agent type %q unregistered", strPayload(evt.Payload, "type"))
	})

	typeEventUnsubMu.Lock()
	typeEventUnsub[registry.EventTypeRegister] = regFn
	typeEventUnsub[registry.EventTypeUnregister] = unregFn
	typeEventUnsubMu.Unlock()

	return func() {
		regFn()
		unregFn()
		typeEventUnsubMu.Lock()
		delete(typeEventUnsub, registry.EventTypeRegister)
		delete(typeEventUnsub, registry.EventTypeUnregister)
		typeEventUnsubMu.Unlock()
	}
}

// isBuiltinPluginFn 返回"某个插件名是不是内置插件"的判据。
//
// ★ 为什么做成闭包而不是直接判：装配点可能在 pluginReg 就绪前就调本函数
//
//	（内核各组件的初始化顺序并不保证）。闭包在**事件到达时**才读，
//	那时插件早已加载完。
//
// 没接 pluginReg 时（单测/独立工具）返回 nil ⇒ registry 侧的判据被跳过。
// 那不会放宽安全：没有真实插件宿主就不存在跨进程插件。
func (a *Agent) isBuiltinPluginFn() func(string) bool {
	if a.MainState().pluginReg == nil {
		return nil
	}
	reg := a.MainState().pluginReg
	return func(name string) bool {
		return reg.IsBuiltinPlugin(name)
	}
}

// strPayload 取载荷里的字符串字段。
func strPayload(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

// typeEventUnsubMu 保护 Agent.typeEventUnsub（订阅是装配期一次，读是事件到达时）。
//
// ★ 用包级 map 而不是给 Agent 加字段：订阅状态是**进程级**事实
//
//	（一条总线只订阅一次），而且它不该出现在 Agent 的字段面上
//	（那会让"Agent 有哪些状态"变成越来越长的一串）。
var typeEventUnsubMu sync.Mutex

// typeEventUnsub 是进程级的订阅状态（key = 事件名，value = 退订函数）。
var typeEventUnsub = map[string]func(){}
