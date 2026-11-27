package agent

// agentsregistry.go —— 记录表的取值（§3.2.2）。
//
// ## 两条取值路径，一个落点
//
//	① AgentConfig.Agents  —— **正路**（内核装配时传入，kernelmod/agent 接
//	   FacAgentRegistry）
//	② Gate().Registry()   —— 兼容路径（闸门与记录表是同一组装配事实；
//	   有些装配点只接了闸门）
//
// 两者都拿不到 ⇒ nil，调用方跳过登记（判定侧 fail-open，行为与改造前一致）。
//
// ★ 为什么不做包级单例：单例会让"哪个内核的表"取决于最后谁调了 Set，
//   而多实例/单测场景下那就是串台。表必须跟着**这次装配**走。

import "github.com/JianFeeeee/HologramAgent/internal/kernelapi"

// agentsRegistry 返回本 agent 的记录表（可能为 nil）。
func (a *Agent) agentsRegistry() kernelapi.AgentRegistry {
	if a == nil {
		return nil
	}
	if a.agents != nil {
		return a.agents
	}
	return kernelapi.AgentRegistryOf(a.Gate())
}

// AgentsRegistry 暴露记录表给装配侧（nil = 未接入）。
func (a *Agent) AgentsRegistry() kernelapi.AgentRegistry { return a.agentsRegistry() }

// SetAgentsRegistry 注入记录表（装配期一次；nil 无副作用）。
func (a *Agent) SetAgentsRegistry(reg kernelapi.AgentRegistry) {
	if a == nil || reg == nil {
		return
	}
	a.agents = reg
}

// kindName 返回本 agent 的**类型名**（记录表用；根 agent 为空）。
//
// ★ 为什么不加 AgentConfig.Kind 字段：类型名只是登记表的一个标注，
//
//	而类型模块本来就知道自己叫什么（注册表 key）。这里从"有父 + 是不是 rt"
//	这两个**已有事实**推出来，不引入第五个身份位。
func (a *Agent) kindName() string {
	if a == nil {
		return ""
	}
	if a.cfg.ParentID == "" {
		return ""
	}
	return string(agentLogIdentity(a.cfg).Kind)
}
