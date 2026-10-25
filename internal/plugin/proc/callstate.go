package proc

import (
	"sync"
	"sync/atomic"
)

// 单次跨进程调用的**状态机**（SDK / 传输层内部逻辑，插件作者看不到）。
//
// ## 为什么需要显式状态而不是一个 map + delete
//
// 旧实现把「调用在进行中」这件事只表达为 `pending[id] 存在`。取消时
// 直接 `delete(pending, id)` —— 于是状态的**全部痕迹**都消失了，之后
// 插件返回的结果无法与任何状态对上，只能走「未知 id，丢弃 + 打日志」。
//
// 那条路径能跑，但它是**副作用式的正确**：正确性依赖"恰好没人再读这个
// 通道"，而不是依赖某个明确规则。真实后果有三个：
//
//  1. 取消后插件仍会**回一个应答**（它不知道已经没人要了），内核每次都要
//     打一条"丢弃迟到应答"日志——在取消频繁的场景里这就是噪声，且很像是
//     故障信号（第一版我确实把它误判成管道问题）。
//  2. 无法区分「取消后插件认输」与「取消后插件其实跑完了」——两者对
//     上层（是否该重试、是否该记日志）意义完全不同。
//  3. 没有任何地方能回答「这次调用现在处于什么状态」，诊断只能靠猜。
//
// ## 状态迁移
//
//	pending ──应答到达────────→ done
//	   │
//	   ├──取消/超时──→ canceled ──迟到应答到达──→ done（结果被丢弃，但状态被记录）
//	   │
//	   └──进程退出──→ failed
//
// 关键规则：**状态只前进一步，且不可回退**。cancelled 之后再收到的应答
// 不会把状态改回 done（"完成"），只是把"结果到了"这件事记下来。
//
// 所有迁移都是 CAS，因此并发（应答与取消赛跑）时谁先到谁赢，且只有一方
// 会得到迁移成功的返回值——这正是"该由谁负责收尾"的判据。
type callState int32

const (
	// callPending 调用已发出，等待应答。
	callPending callState = iota
	// callDone 已收到应答（或确定的失败），结果已交回调用方。
	callDone
	// callCanceled 调用方已放弃（超时/取消/抢占）。
	//
	// 与 callFailed 分开：取消是**主动**的（我方不再要结果），
	// 进程退出是**被动**的（对端没了）。上层的处置不同——取消不该报警，
	// 进程退出该触发插件健康计数。
	callCanceled
	// callFailed 对端进程退出，调用无法完成。
	callFailed
)

func (s callState) String() string {
	switch s {
	case callPending:
		return "pending"
	case callDone:
		return "done"
	case callCanceled:
		return "canceled"
	case callFailed:
		return "failed"
	}
	return "unknown"
}

// call 是一次跨进程调用的**全部状态**。
//
// 所有与该调用相关的状态都放在这里，而不是散落到几张平行 map：
//
//	state  —— 生命周期（pending/done/canceled/failed）
//	ch     —— 应答投递通道
//	blocks —— 该调用申请过的扩容块（取消超时时要强制回收）
//
// 放在一起的理由：这几样东西的**生命周期完全相同**（都随调用开始、随调用收敛）。
// 拆成多张表就必须各自维护增删与过期，且任何一处漏删都是泄漏——
// 第一版我确实这么写过（独立的 callblocks 表 + 自己的 TTL），
// 结果是同一件事记在两处、两个过期策略要同时正确。现在只有一处。
type call struct {
	id     uint64
	ch     chan *Response
	kind   string       // 调用类型（tool.invoke / stage.invoke …），仅用于日志与诊断
	state  atomic.Int32 // callState
	cancel atomic.Bool  // 是否已向对端发过 call.cancel（避免重复通知）

	// blocksMu 保护 blocks（分配来自插件 RPC 的另一个 goroutine）。
	blocksMu sync.Mutex
	blocks   []SharedRef
}

// trackBlock 记下一块属于本次调用的扩容块。
//
// 为什么要记：插件为大结果申请扩容块后，若调用被取消，插件会静默退出
// （正确行为），内核**从未见过**那个 ResultRef —— 块就永远还不回来了，
// 且全程没有任何日志。记在这里，取消超时时内核才能自己把账清了。
func (c *call) trackBlock(ref SharedRef) {
	if c == nil || ref.IsZero() {
		return
	}
	c.blocksMu.Lock()
	c.blocks = append(c.blocks, ref)
	c.blocksMu.Unlock()
}

// takeBlocks 取出并清空本次调用名下的块。
func (c *call) takeBlocks() []SharedRef {
	if c == nil {
		return nil
	}
	c.blocksMu.Lock()
	defer c.blocksMu.Unlock()
	out := c.blocks
	c.blocks = nil
	return out
}

func newCall(id uint64, kind string) *call {
	c := &call{id: id, ch: make(chan *Response, 1), kind: kind}
	c.state.Store(int32(callPending))
	return c
}

// legalTransition 是**全局唯一**的合法迁移表。
//
// 为什么要有它，而不是让每个调用方自己挑 from/to：
// transitionTo 是 CAS，它只校验"当前值是 from"，**不校验 to 是否合适**。
// 于是 `transitionTo(callDone, callFailed)` 这种明显非法的调用会静默成功，
// 把终态改回去——状态机的单向性就白写了。实测中这确实出现过一次
// （retire 曾写成 `pending→to || canceled→to`，导致 done 可以被改成 failed）。
//
// 现在把"能不能迁移"集中在一处判定：新增迁移必须在这里登记，
// 写错的 from/to 组合会直接被拒（并留下日志线索）。
var legalTransition = map[callState][]callState{
	callPending:  {callDone, callCanceled, callFailed},
	callCanceled: {callDone, callFailed}, // 迟到应答 / 进程随后退出
	callDone:     nil,                    // 终态
	callFailed:   nil,                    // 终态
}

func canTransition(from, to callState) bool {
	for _, ok := range legalTransition[from] {
		if ok == to {
			return true
		}
	}
	return false
}

// transitionTo 尝试把状态从 from 迁移到 to（CAS）。
// 返回 true 表示**本次调用**赢得了这次迁移，负责后续动作。
//
// 非法组合（不在 legalTransition 里）一律返回 false 且状态不变——
// 让错误尽早暴露成"迁移没发生"，而不是悄悄把终态改回去。
func (c *call) transitionTo(from, to callState) bool {
	if !canTransition(from, to) {
		return false
	}
	return c.state.CompareAndSwap(int32(from), int32(to))
}

// State 返回当前状态（诊断用）。
func (c *call) State() callState { return callState(c.state.Load()) }
