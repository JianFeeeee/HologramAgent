package agent

import agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"

// childfacts.go —— 子 agent 的**命名规则与共享事实**（基座侧）。
//
// 这些不是类型策略，是"子怎么被寻址"的事实：
//
//	parent/<id>   子往父投递时用的 source
//	child/<id>    父为子分配的入站 inputch
//
// 四类 agent 都用同一条规则（底层复用父的 inputch + InjectInterruptOpts），
// 所以它是基座的事实，不是任何类型的所有权。

// ParentSource 返回本 agent 在**父侧**的入站通道名（"parent/<id>"）。
func (a *Agent) ParentSource() string {
	if a == nil {
		return ""
	}
	return "parent/" + string(a.id)
}

// ChildInboundChannel 返回给某个子分配的入站通道名（"child/<id>"）。
//
// ★ 只拼名字，不产生副作用：注销路径要用它算出同一个名字，
//
//	不能再走登记（那会把刚摘掉的登记又写回去）。
func ChildInboundChannel(childID string) string {
	return "child/" + childID
}

// RegisterInboundChannel 登记父侧的入站 inputch（Owner = 父自己）。
//
// ★ 归属父自己：它是**父的**入站通道 —— 子往父投递时落在这条通道上，
//
//	父的 agentloop 从自己的 io 里读到它。
func (a *Agent) RegisterInboundChannel(ch, plugin string) {
	if a == nil || ch == "" || a.io == nil {
		return
	}
	reg := a.io.ChannelRegistry()
	if reg == nil {
		return
	}
	_ = reg.Register(agentIO.InputChannel{Name: ch, Plugin: plugin, Owner: string(a.id)})
}

// FirstNonEmpty 返回第一个非空字符串（类型装配参数的常见缺省链）。
func FirstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// DefaultChannelForBuiltin 是内置工具族执行时用的**稳定来源标识**
// （工具执行不产出用户可见输出，但记忆写入需要一个固定 source）。
func DefaultChannelForBuiltin() string { return "core.builtin" }
