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

import "sync"

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

// Upstream 记一次"上游明确报 context_full"。
func (l *OverflowLedger) Upstream() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.st.Upstream++
	l.mu.Unlock()
}

// defaultLedger 是未注入账本时的进程内默认账本。
//
// ★ 为什么留一个默认值（而基座不留默认实现）：账本只影响**观测**，
// 不影响任何行为判定；类型模块注入自己那份之后，读取者拿到的是它自己的账。
// 与"基座不许有默认 Loop / 默认裁剪"不冲突 —— 那两类默认会改变行为。
var defaultLedger = &OverflowLedger{}
