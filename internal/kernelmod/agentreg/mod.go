// Package agentreg 是**agent 登记模块**：把记录表挂到内核挂载点。
//
// ## 为什么它是最先装配的模块之一（不带任何前置）
//
// 安全模块要读 `AgentIndex` 判定"这个 agent 被授权了什么"，而安全模块
// 在装配层里很靠前（它必须早于任何读 core.llm.* 的动作）。所以登记表必须
// 更早存在 —— 它只有纯内存结构，没有任何依赖。
//
//	agentreg（本模块，零依赖）
//	   ↓ FacAgentRegistry / FacAgentIndex
//	security（Requires FacAgentIndex）
//	   ↓ FacSecurity
//	agent（Requires FacSecurity）
//
// ## 五表合一的落点
//
// 原先这些事实各在自己的包里、各有清理点：
//
//	security/outputsByAgent · security/principals.byID
//	agent/instances.recs    · （child.Registry 保留：它管对象，不是事实）
//
// 现在事实只有一份，`Remove(id)` 一次删干净。
package agentreg

import (
	agentregimpl "github.com/JianFeeeee/HologramAgent/internal/agent/agentreg"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// Module 提供 agent 记录表。
type Module struct{}

func (Module) Name() string       { return "agent-registry" }
func (Module) Requires() []string { return nil }
func (Module) Provides() []string {
	return []string{kernelapi.FacAgentRegistry, kernelapi.FacAgentIndex}
}

func (Module) Init(ks *kstate.State) error {
	store := agentregimpl.New()
	// 同一份实例挂两个名字：写面给内核实例生命周期，读面给安全模块。
	// 分开命名是因为**消费方需要的能力不同**（只读方拿不到 Remove）。
	ks.Provide(kernelapi.FacAgentRegistry, store)
	ks.Provide(kernelapi.FacAgentIndex, kernelapi.AgentIndex(store))
	return nil
}

func init() { kernelmod.Register(Module{}) }
