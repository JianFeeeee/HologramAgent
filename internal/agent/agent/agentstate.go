package agent

// agentstate.go —— **类型专属状态的存放处**（拆包的结构基础，2026-11-17）。
//
// ## 问题
//
// `Agent` 结构原先同时挂着四类 agent 的状态：
//
//	主 agent     记忆整理面 / 媒体 / 插件 / 预算统计（约 32 个字段）
//	rt            rtSlot / rtMaxTools / 归档留档
//	驻留子        inputch 处理表 / contextfull 标记 / 向父汇报的回调
//	工作式子      childTasks / 序号
//
// 于是读代码时无法回答"这个字段是谁的"，也无法把一个类型整块搬走。
//
// ## 解法：按类型分组持有，基座只留一个入口
//
//	Agent.sub  —— 一张**分区表**，每个类型一格；用不到就是 nil
//
// 基座因此只有 1 个字段而不是 88 个。各类型模块（以及基座里那些**只服务
// 某个类型**的方法）经 `a.rt()` / `a.resident()` / `a.worker()` 取自己那一格。
//
// ## 为什么不是"直接搬进类型模块的结构体"
//
// 那些方法仍在基座包里（例如 `rtLoop` 要读 `rtSlot`）。把它们连方法一起搬
// 是下一步（类型成包）；在那之前，状态先按类型归位 —— 这样搬的时候
// **只需改接收者**，字段已经不再散落。
//
// ## 为什么格子是 nil 而不是零值
//
// "这个 agent 不是 rtchild" 与 "它是 rtchild 但还没开始一轮" 是两件事。
// nil 表达前者（装配错误能响亮报出来），零值会把两者混成同一个。

import (
	"sync"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// rtState 是 rtchild 专属状态。
type rtState struct {
	// slot 是唯一输入入口（单槽覆盖：最新输入赢，旧输入被丢弃）。
	slot *rtSlot
	// maxTools 是单轮最多几批工具调用（0 = 不限）。
	maxTools int
}

// rtArchiveState 是父侧的 rt 留档（已回收实例的上下文交接数据）。
//
// ★ 它在**父**上，不在 rtchild 上：留档的意义是"回收后父按需取回原文"，
// 实例本身已经不存在了。
type rtArchiveState struct {
	mu     sync.Mutex
	closed map[string][]agentAPI.Message
}

// residentState 是驻留子专属状态。
type residentState struct {
	// notifyParent 是"子 → 父"的主动汇报回调（L3 中断）。
	notifyParent func(text string)
	// onContextFull 是 contextfull → 父侧内核级事件（L4）。
	onContextFull func()

	// ctxFullMu / ctxFullSignaled 保证 contextfull 只上报一次（防抖动刷屏）。
	ctxFullMu       sync.Mutex
	ctxFullSignaled bool

	// tableMu 保护处理表（子持有，父 pull）。
	tableMu        sync.Mutex
	table          []InputchRecord
	pending        *InputchRecord
	currentInputch string
}

// workerState 是工作式子（spawn_child）专属状态。
type workerState struct {
	mu     sync.Mutex
	nextID int64
	seq    int64
	tasks  map[string]*childTaskState
}

// subStates 是**类型分区表**（见文件头）。
type subStates struct {
	rt       *rtState
	archive  *rtArchiveState
	resident *residentState
	worker   *workerState
}

// ensure 返回分区表（`New` 里已建好；这里只是 nil 安全的兜底）。
func (a *Agent) ensureSub() *subStates {
	if a == nil {
		return nil
	}
	if a.sub == nil {
		a.sub = &subStates{}
	}
	return a.sub
}

// rt 取 rt 状态（不是 rtchild 时返回 nil）。
func (a *Agent) rt() *rtState {
	if a == nil || a.sub == nil {
		return nil
	}
	return a.sub.rt
}

// mkRT 为本次装配创建 rt 状态（rtchild 构造时调）。
func (a *Agent) mkRT(slot *rtSlot, maxTools int) {
	s := a.ensureSub()
	s.rt = &rtState{slot: slot, maxTools: maxTools}
}

// archive 取父侧的 rt 留档（不是 rt 父时返回 nil）。
func (a *Agent) archive() *rtArchiveState {
	if a == nil || a.sub == nil {
		return nil
	}
	return a.sub.archive
}

// mkArchive 创建留档区（父第一次拉起 rt 时调）。
func (a *Agent) mkArchive() *rtArchiveState {
	s := a.ensureSub()
	if s.archive == nil {
		s.archive = &rtArchiveState{closed: map[string][]agentAPI.Message{}}
	}
	return s.archive
}

// resident 取驻留子状态（不是驻留子时返回 nil）。
func (a *Agent) resident() *residentState {
	if a == nil || a.sub == nil {
		return nil
	}
	return a.sub.resident
}

// mkResident 为本次装配创建驻留子状态（驻留子构造时调）。
func (a *Agent) mkResident(notifyParent func(string), onContextFull func()) *residentState {
	s := a.ensureSub()
	s.resident = &residentState{notifyParent: notifyParent, onContextFull: onContextFull}
	return s.resident
}

// worker 取工作式子状态（不是工作式子父时返回 nil）。
func (a *Agent) workerState() *workerState {
	if a == nil || a.sub == nil {
		return nil
	}
	return a.sub.worker
}

// mkWorker 创建工作式子状态（父第一次 spawn_child 时调）。
//
// ★ 调用方持锁：并发 spawn 时不能各建一张表（两张表会丢一半任务）。
func (a *Agent) mkWorker() *workerState {
	s := a.ensureSub()
	if s.worker == nil {
		s.worker = &workerState{tasks: map[string]*childTaskState{}}
	}
	return s.worker
}
