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
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	sched "github.com/JianFeeeee/HologramAgent/internal/agent/resident/sched"
)

// M 是驻留子类型的实现载体（每个父 agent 一个）。
//
// ★ `sched` 是**本类型自己的**输入调度器实例（不是基座字段、不是共享实例）：
//
//	驻留子与主 agent 现在调度行为相同，但那是**当前**的事实，不是未来的约束。
//	各自持有一份，任一方改自己的调度语义时不用回基座改、也不影响另一方。
type M struct {
	A     *agent.Agent
	sched *sched.Scheduler

	// st 是本类型的私有状态（inputch 处理表 / contextfull 去重 / 上报通路）。
	// ★ 不占基座的格子：曾经的 `subStates` + `SubKey*` 让基座成为
	//   "全类型登记处"，每加一种类型就要回来加一格 —— 已整体删除。
	st *state

	// ctx 是**本实例自己的**上下文实现（驻留子一套、主 agent 另一套）。
	// ★ 基座不持有它：上下文是 agentloop 的。
	ctx ContextMgr

	// currentInterrupt 标记本轮输入是否为中断（本类型自己的帧事实）。
	currentInterrupt bool
}

// mkResident 装配本类型的私有状态（父在创建子时调一次）。
func (m *M) mkResident(_ *agent.Agent, notifyParent func(string), onContextFull func()) {
	m.st = &state{notifyParent: notifyParent, onContextFull: onContextFull}
}

// carriers 是**本类型自己**的载体表（按 agent 指针索引）。
//
// ★ 与 mainagent 同一纪律：类型自己持有，基座一格都不留。
//
//	不能用"每次 Wrap 都新建" —— 那会让 `Of()` 每次取到不同的载体，
//	inputch 处理表 / contextfull 去重标记全部丢失（现场最典型的"看起来
//	能跑但状态不生效"）。
var carriers sync.Map // map[*agent.Agent]*M

// Wrap 为某个基座句柄取回它的载体（不存在则新建并登记）。
func Wrap(a *agent.Agent) *M {
	if a == nil {
		return nil
	}
	if v, ok := carriers.Load(a); ok {
		return v.(*M)
	}
	m := &M{A: a, sched: sched.New(0), st: &state{}}
	v, _ := carriers.LoadOrStore(a, m)
	return v.(*M)
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

// wrapOf 取回**这个子实例自己**的载体。
//
// ★ 原先返回 `Wrap(rc.parentAgent)` —— **父的**载体（2026-11-26 实测缺陷）。
//
//	名字读起来像"子的载体"，实际拿的是父的，于是 `wrapOf(rc)` 与
//	handle 所在的 `m` 是同一个对象。凡是"读这个子的内部状态"的地方
//	（`ResidentTable` 的处理表）都静默读到父的（空表），
//	而子其实处理完了（它的 `context.json` 里有 resp）。
//
//	子的 agent 就在 `rc.agent`（`childpkg.Child` 之外的私有字段），
//	`Wrap` 按 agent 指针索引载体表 ⇒ 拿到的正是该实例自己的 `M`。
func wrapOf(rc *residentChild) *M {
	if rc == nil || rc.agent == nil {
		return nil
	}
	return Wrap(rc.agent)
}
