// Package agenttype 把**插件声明的类型**组装成内核可用的注册项。
//
// ## 用户裁定（2026-11-19）
//
// > 「sdk 暴露出去的是抽象接口即插件只是把对应的回调与每个事件对应的处理函数
// >    注册进去作为一个参数结构体传给 sdk 接口，由 plghost 通过 eventbus
// >    推到内核，内核再按照契约组装，而不是直接引用 agent 包」
//
// ## 它做什么
//
//	插件：填 sdk.AgentTypeDef（中立类型 + 三个回调）
//	  ↓ EventBus "agent.type.register"
//	本包：按 Def.Spec 组装 → registry.Descriptor
//	  ├─ Construct   内核造实例（插件不碰 IO/GraphOpener/Security）
//	  ├─ Initialize  挂 RuntimeHooks + 把 def.Loop 适配到 Hooks.Loop
//	  ├─ Admin       适配中立签名
//	  └─ Help        适配中立签名
//	  ↓
//	registry.Register（唯一那张表）
//
// ★ 它是**内核侧唯一的适配点**：中立面（sdk.*）↔ 内核实类型（agent.*）
//
//	只在这里转换一次。插件永远看不到 `*agent.Agent`。
package agenttype

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// Module 提供"插件类型组装器"。
type Module struct{}

func (Module) Name() string       { return "agent-type" }
func (Module) Requires() []string { return []string{kernelmod.FacAgent} }
func (Module) Provides() []string { return []string{kernelapi.FacAgentTypeHost} }

func (Module) Init(ks *kstate.State) error {
	ks.Provide(kernelapi.FacAgentTypeHost, New())
	// ★ 内核自己订阅类型注册/撤销事件（eventbus 是内核模块之间的通道）。
	//   存活实例由实例表给（实例表归内核），所以经闭包注入。
	SubscribeTypeEvents(ks, func(typeName string) []string {
		agentImpl, ok := ks.Require(kernelmod.FacAgent)
		if !ok {
			return nil
		}
		// 实例表归内核：从根 agent 的实例管理器取。
		lister, ok := agentImpl.(interface {
			ListChildren(string) []registry.ChildBrief
		})
		if !ok {
			return nil
		}
		var ids []string
		for _, b := range lister.ListChildren(typeName) {
			ids = append(ids, b.ID)
		}
		return ids
	})
	return nil
}

func init() { kernelmod.Register(Module{}) }
