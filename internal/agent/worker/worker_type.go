package worker

// worker_type.go —— 工作式子的**实现载体**（组合基座句柄 + 本包状态）。
//
// 按 §1.3：组合，不内嵌。需要基座的什么就直接调 `M.A.Xxx()` ——
// 没有 Host 接口，没有映射层。

import (
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

// M 是工作式子类型的实现载体（每个父 agent 一个）。
type M struct {
	A *agent.Agent
	// st 是本类型自己的任务表（不占基座格子）。
	st *State
}

// carriers 是**本类型自己**的载体表（按父 agent 指针索引）。
//
// ★ 不再占基座的格子（`subStates` + `SubKeyWorker`）：基座曾是"全类型
//
//	登记处"，每加一种类型就要回来加一格 —— 那些格子已删。
//	类型自己持有自己的东西，这是"每个 agent 持有自己的全部内容"的落点。
var carriers sync.Map // map[*agent.Agent]*M

// Of 取回某个父 agent 的工作式子载体（未装配 ⇒ nil）。
func Of(a *agent.Agent) *M {
	if a == nil {
		return nil
	}
	if v, ok := carriers.Load(a); ok {
		return v.(*M)
	}
	return nil
}

// ensure 取回或创建载体。
func ensure(a *agent.Agent) *M {
	if a == nil {
		return nil
	}
	if m := Of(a); m != nil {
		return m
	}
	m := &M{A: a, st: NewState()}
	v, _ := carriers.LoadOrStore(a, m)
	return v.(*M)
}

// state 返回本载体的任务表。
func (m *M) state() *State {
	if m == nil {
		return nil
	}
	return m.st
}
