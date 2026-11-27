package rtchild

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

// M 是 rt 类型的实现载体（每个父 agent 一个）。
//
// ★ `st` 是本类型自己的（单槽覆盖 + 单轮工具上限）。rt 不吃队列（无 sched）。
//
// ★ 这里**没有**"父侧留档"：已回收实例的上下文交接走**父子通信协议**
//
//	（`CallParent(handover)`）—— 父收到就是一条正常上报，不再需要父侧
//	另存一张表（`rtArchiveState` 已删）。
type M struct {
	A *agent.Agent

	st *rtState
}

// Wrap 取回（不存在则创建）本类型载体。
func Wrap(a *agent.Agent) *M {
	if a == nil {
		return nil
	}
	return &M{A: a, st: &rtState{}}
}

// Of 从实例管理器取回根 agent 的 rt 载体（本类型 Descriptor 用）。
func Of(host any) *M {
	im, ok := host.(*agent.InstanceManager)
	if !ok || im == nil {
		return nil
	}
	root, _ := im.Root().(*agent.Agent)
	if root == nil {
		return nil
	}
	return Wrap(root)
}
