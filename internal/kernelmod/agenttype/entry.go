package agenttype

// entry.go —— 插件类型的**骨架登记项**（child.Entry 的实现）。
//
// 插件类型与内置类型走**同一套骨架**（`child.Registry` / `InstanceManager`）：
// 于是自动获得持久化、admin_child_list、StopAll、安全策略声明 ——
// 不需要为插件类型另写一套生命周期。

import (
	"context"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent/child"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// pluginChild 是一个插件类型的实例（child.Entry 的实现）。
type pluginChild struct {
	base *child.Child
	// def 是它的类型声明（Dispose/State 时可能需要）。
	def pubsdk.AgentTypeDef
	// a 是载体 agent。
	a *agent.Agent
}

func newEntry(def pubsdk.AgentTypeDef, a *agent.Agent, id string) *pluginChild {
	ctx, cancel := context.WithCancel(a.Ctx())
	return &pluginChild{
		base: &child.Child{
			LogIdentity: child.LogIdentity{ID: id, Kind: child.Kind(def.Type)},
			IO:          a.IO(),
			Ctx:         ctx,
			Cancel:      cancel,
			ParentID:    a.ParentSource(),
		},
		def: def,
		a:   a,
	}
}

func (e *pluginChild) Base() *child.Child { return e.base }
func (e *pluginChild) Kind() child.Kind   { return child.Kind(e.def.Type) }
func (e *pluginChild) Agent() any         { return e.a }
func (e *pluginChild) State() string      { return "running" }
func (e *pluginChild) Channels() []string { return nil } // 插件类型不独占通道

// Disposition 报告实例销毁时通道怎么处置（插件类型不独占通道 ⇒ 无处置）。
func (e *pluginChild) Disposition() string { return "" }

// OffloadOwned 报告本实例是否由内核为转投而拉起（插件类型恒 false）。
func (e *pluginChild) OffloadOwned() bool { return false }

// Dispose 按本类型语义收尾。
//
// ★ 不在这里停载体：骨架 Cleanup 在调用 Dispose **之前**已经 Stop + cancel，
//
//	这里只做类型自己的收尾（插件类型默认没有）。
func (e *pluginChild) Dispose() string {
	if e.a != nil {
		e.a.Stop()
	}
	return ""
}

var _ child.Entry = (*pluginChild)(nil)
var _ = types.AgentID("")
