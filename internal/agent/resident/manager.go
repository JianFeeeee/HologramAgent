package resident

// manager.go —— 驻留子类型的**实现载体**（组合基座句柄 + 本类型自己的状态）。
//
// ## 形态（用户裁定 2026-11-17 / 2026-11-19）
//
//	type M struct{ A *agent.Agent }
//
// 组合，不内嵌。需要基座什么就直接调 `m.A.Xxx()` —— **没有 `Host` 接口，
// 没有 `a.X → h.X` 的映射层**（那种层被判定为「平庸设计稀烂抽象」）。
//
// ## 为什么载体不挂在基座的格子里
//
// 曾经的 `subStates` 五格 + `SubKey*` 让基座成为"全类型登记处"：
// 每加一种类型就要回来给基座加一格。现在载体由 `child.Entry.Agent()`
// 交给骨架（每个实例自带它的类型行为），基座一格都不留。
//
// ## 根 agent 没有 Entry
//
// 主 agent 是根，不是谁的子 —— 它的载体在自己的包里持有，基座不需要
// 一个 slot 给它。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

// M 是驻留子类型的实现载体（每个父 agent 一个）。
//
// ★ `sched` 是**本类型自己的**输入调度器实例（不是基座字段、不是共享实例）：
//
//	驻留子与主 agent 现在调度行为相同，但那是**当前**的事实，不是未来的约束。
//	各自持有一份，任一方改自己的调度语义时不用回基座改、也不影响另一方。
type M struct {
	A     *agent.Agent
	sched *agent.Scheduler

	// st 是本类型的私有状态（inputch 处理表 / contextfull 去重 / 上报通路）。
	// ★ 不占基座的格子：曾经的 `subStates` + `SubKey*` 让基座成为
	//   "全类型登记处"，每加一种类型就要回来加一格 —— 已整体删除。
	st *state
}

// mkResident 装配本类型的私有状态（父在创建子时调一次）。
func (m *M) mkResident(_ *agent.Agent, notifyParent func(string), onContextFull func()) {
	m.st = &state{notifyParent: notifyParent, onContextFull: onContextFull}
}

// Wrap 为某个基座句柄创建载体（由父在创建子时调用）。
func Wrap(a *agent.Agent) *M {
	if a == nil {
		return nil
	}
	return &M{A: a, sched: agent.NewScheduler(0), st: &state{}}
}

// Of 从实例管理器取回**根 agent 的**驻留子载体（类型自己的 Descriptor 用）。
//
// ★ 这是类型包内部的事：`Descriptor.Construct/Admin` 拿到的是内核给的
//
//	`registry.Host`，它们按约定断言回实例管理器并取根 agent 的载体。
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

// wrapOf 从一个子的父 agent 取回它的载体（子把自己的父存了下来）。
func wrapOf(rc *residentChild) *M {
	if rc == nil || rc.parentAgent == nil {
		return nil
	}
	return Wrap(rc.parentAgent)
}
