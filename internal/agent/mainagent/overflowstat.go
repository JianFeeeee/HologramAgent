package mainagent

// overflowstat.go —— 超页处置的**观测账本**（策略自己的记账）。
//
// ## 为什么它跟着策略走，而不是留在基座
//
// 计数的每一项都在回答"**我的策略做了什么**"：
//
//	Checked    我判了几次
//	Triggered  我裁了几次
//	Aborted    我裁不动、终止了几次
//	Upstream   我收到几次上游的明确报告
//
// 这是策略的账，不是基座的事实。基座连"水位多少算超"都不知道，自然也无从
// 记"我判了几次"。主 agent 的记忆整理/媒体/插件面在 I 批会整体搬进
// `mainagent`，这份账本随之一起走 —— 所以现在就不该在基座里留它。
//
// ## 并发
//
// 判定可能从两个 goroutine 进来（内核触发 / 类型的自主巡检），所以账本自带锁。
// 读侧（状态面）拿的是快照，不与写侧互相阻塞。

import (
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

// OverflowStat 是一次观测快照。
type OverflowStat struct {
	Checked    int
	Triggered  int
	Aborted    int
	Upstream   int
	LastPruned int
	LastRatio  float64
	LastEvents int
}

// OverflowLedger 是超页处置的累计账本。
type OverflowLedger struct {
	mu sync.Mutex
	st OverflowStat
}

// Snapshot 返回一份只读快照。
func (l *OverflowLedger) Snapshot() OverflowStat {
	if l == nil {
		return OverflowStat{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st
}

// Check 记一次"做了超页判定"（无论结论），返回累计判定次数。
//
// 返回值被日志用作"每 N 次打一行"的分母 —— 因此它是累计值而非增量。
func (l *OverflowLedger) Check() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.st.Checked++
	return l.st.Checked
}

// Pruned 记一次"触发并裁掉 N 条"。
func (l *OverflowLedger) Pruned(n int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.st.Triggered++
	l.st.LastPruned = n
	l.mu.Unlock()
}

// Aborted 记一次「超页但裁不出东西」的终止。
func (l *OverflowLedger) Aborted(ratio float64, events int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.st.Aborted++
	l.st.LastRatio = ratio
	l.st.LastEvents = events
	l.mu.Unlock()
}

// PruneRequested 记一次"发出了空中断、裁剪将在任务边界发生"。
//
// ★ 与 Pruned 的区别：Pruned 是"已经裁掉 N 条"，这里是"已请求、还没裁"。
// 分开记是因为两者回答不同问题：前者看效果，后者看触发。
func (l *OverflowLedger) PruneRequested() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.st.Triggered++
	l.mu.Unlock()
}

// Upstream 记一次"上游明确报 context_full"。
func (l *OverflowLedger) Upstream() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.st.Upstream++
	l.mu.Unlock()
}

// mainLedger 是主 agent 的类型账本（handler 记账与状态面读的是同一份）。
//
// ★ "同一份"很重要：状态面显示"触发了几次/裁了几条"，必须就是策略实际
// 发生的次数；两份额外账本会让两个数字各自漂移。
var mainLedger = &OverflowLedger{}

// ledgerFor 返回该 agent 的账本（当前单实例，故为进程内一份）。
//
// I 批把 mainState 搬进本包后，这里改成从 mainagent 自己的 state 取。
func ledgerFor(*agent.Agent) *OverflowLedger { return mainLedger }
