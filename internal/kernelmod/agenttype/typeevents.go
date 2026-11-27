package agenttype

// typeevents.go —— 内核订阅「类型注册 / 撤销」事件（用户裁定 2026-11-20）。
//
// eventbus 是内核模块之间的通信通道，与 agent 无关。类型表是内核的，
// 所以订阅与处理都必须在**内核模块**里，不在任何 agent 类型包。

import (
	"log"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
)

var (
	unsubMu sync.Mutex
	unsubs  []func()
)

// SubscribeTypeEvents 订阅类型注册/撤销事件（内核装配期一次，幂等）。
//
// aliveOf 给出某类型的存活实例 id（撤销仍有实例的类型必须被拒）。
// 实例表归内核，所以由内核注进来 —— 本包不持有实例表。
func SubscribeTypeEvents(ks *kstate.State, aliveOf func(typeName string) []string) func() {
	if ks == nil {
		return func() {}
	}
	busImpl, ok := ks.Require(kernelapi.FacEventBus)
	if !ok {
		return func() {}
	}
	bus, _ := busImpl.(kernelapi.EventBus)
	if bus == nil {
		return func() {}
	}
	builtinFn := builtinPluginFn(ks)

	assemblerAny, _ := ks.Require(kernelapi.FacAgentTypeHost)
	assembler, _ := assemblerAny.(*Host)
	if assembler == nil {
		log.Printf("[agenttype] type events not subscribed: 组装器未就绪")
		return func() {}
	}

	regFn := bus.Subscribe(registry.EventTypeRegister, func(evt *events.Event) {
		if evt == nil {
			return
		}
		pluginName, def, err := registry.HandleRegisterEvent(registry.EventTypeRegister, evt.Payload, builtinFn)
		if err != nil {
			log.Printf("[agenttype] register rejected: %v", err)
			return
		}
		if err := assembler.Assemble(pluginName, def); err != nil {
			log.Printf("[agenttype] register rejected by assembler: %v", err)
			return
		}
		log.Printf("[agenttype] agent type registered by %q", pluginName)
	})

	unregFn := bus.Subscribe(registry.EventTypeUnregister, func(evt *events.Event) {
		if evt == nil {
			return
		}
		var alive []string
		if aliveOf != nil {
			alive = aliveOf(strPayload(evt.Payload, "type"))
		}
		if err := registry.HandleUnregisterEvent(evt.Payload, alive); err != nil {
			log.Printf("[agenttype] unregister rejected: %v", err)
			return
		}
		log.Printf("[agenttype] agent type %q unregistered", strPayload(evt.Payload, "type"))
	})

	unsub := func() {
		regFn()
		unregFn()
	}
	unsubMu.Lock()
	unsubs = append(unsubs, unsub)
	unsubMu.Unlock()
	return unsub
}

// builtinPluginFn 返回"某插件名是否内置插件"的判据（运行期事实）。
func builtinPluginFn(ks *kstate.State) func(string) bool {
	if ks == nil {
		return nil
	}
	impl, ok := ks.Require(kernelapi.FacPluginRegistry)
	if !ok {
		return nil
	}
	reg, _ := impl.(*plugin.Registry)
	if reg == nil {
		return nil
	}
	return reg.IsBuiltinPlugin
}

func strPayload(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}
