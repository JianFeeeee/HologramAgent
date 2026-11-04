// Package child 是**子 agent 的类型抽象层**。
//
// # 为什么要有这个包（用户裁定 2026-11-04）
//
// 此前每新增一种子 agent，都要往 `internal/agent/core` 的同一个文件里追加代码：
//
//	agent.go      加一组类型专属字段（锁 + 表 + 专属状态），约 20 行/类型
//	inputroute.go 加一段"查我这张表 → 分流"的分支
//	toolcall.go   加若干 `case tc.Name == "<该类型的控制工具>":`
//	以及独立的 <type>.go 放整份生命周期实现
//
// 实测：新增一种子 agent 需要改 **18 个生产文件**。已经因此漂移过一次
// （`StopRTChildren` 漏注销入站登记，见 docs/zh/engineering-lessons.md §2.3）。
//
// # 抽象的形状
//
// 分成两半：
//
//	**骨架（本包提供，只写一遍）**
//	    spawnChild  校验 → 去重 → 独立 ctx → 独立 IO(共享通道登记表 + 父视图回退)
//	                → 构造载体 → 绑定 → 声明子策略 → 登记 → 启动
//	    stopChild   撤销策略 → 注销入站登记 → 取消 ctx → kind.Dispose
//	    stopAll     对全部子执行 stopChild
//
//	**差异（由 ChildKind 实现，每种类型一份）**
//	    Deliver       输入策略：队列（不丢活）还是单槽（最新者胜）
//	    BuildContext  上下文策略：相关性裁剪 还是 跨轮 history + 窗口
//	    Emit          输出策略（含异步批大小）
//	    RunTools      工具策略（并发批 还是 串行）
//	    Dispose       销毁语义（残余 keep/drop；通道释放还是悬空）
//	    ParentTools   暴露给父的控制工具
//
// # 已确认的边界（不要为了"更统一"而抹掉）
//
//   - 驻留子的 `DestroyResident` 有三种 ChannelDisposition（释放/保留/归还）
//   - rtchild 的 rt 通道**销毁后悬空**，不归还（用户裁定，见 rt 的 Dispose 实现）
//   - 驻留子 `child.Start()` 起 7 个 goroutine；rtchild **只起 1 个** rtLoop
//     （rt 明确不要 schedulerLoop / interceptLoop / 记忆整理循环）
//
// 这三条是**语义差异**，属于 ChildKind 的职责，不属于骨架。
package child

import (
	"context"
	"errors"
	"sync"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// Kind 是子 agent 的类型标签。
//
// 为什么用字符串而不是 iota 枚举：它要出现在状态面、日志、以及
// ChannelRegistry 的归属判定里；字符串在日志里直接可读，
// 且新增类型不需要改一个中心 switch（这与包的目的正好一致）。
type Kind string

const (
	// KindResident 是长期派驻的子 agent（有任务队列、有残余可回灌、不丢活）。
	KindResident Kind = "resident"
	// KindRT 是实时通话子 agent（单槽覆盖、最新者胜、可饿死、丢现场）。
	KindRT Kind = "rt"
)

// Child 是一个**已建立**的子 agent 实例的**公共部分**。
//
// ★ 用法是**嵌入**：类型专属状态放在各实现自己的结构体里，
// 公共部分靠嵌入提升字段：
//
//	type rtChild struct {
//	    *child.Child          // ID / Kind / Ctx / IO / InboundCh / 状态 / 轮次
//	    agent   *Agent        // 载体（类型专属，不进公共结构）
//	    slot    *rtSlot
//	}
//
// 为什么不把 agent *Agent 放进本结构：本包不能 import core（否则成环），
// 而 core 可以 import 本包。嵌入让公共部分与载体指针各归各位，
// 同时避免为此造一个“只有 ID 的假 Agent” —— 那种结构既不能用又会误导后来者。
type Child struct {
	// ID 是登记表的键，也是通道 Owner 记的值。
	//
	// ★ 必须与载体 Agent 的 ID **逐字相同**：AssignRealtime 会把通道 Owner
	// 记成 carrier.id，而路由按 entry.Owner 在本表里查 —— 两处各拼一次名字
	// 就会永远查不到（曾因写成 "rt/"+id 而让插件输入全打到父身上）。
	ID string

	// Kind 是本实例的类型标签。
	Kind Kind

	// IO 是本子自己的 IOManager（独立输入入口 + 共享通道登记表 + 父视图回退）。
	IO *agentIO.IOManager

	// Ctx / Cancel 是本子**自己的**取消域。
	//
	// 必须是 WithCancel(parentCtx) 派生：父停则子停（不留孤儿），
	// 但单独 cancel 只影响本子，不波及父与兄弟。
	Ctx    context.Context
	Cancel context.CancelFunc

	// InboundCh 是本子在父侧注册的入站通道名（"child/<id>"），
	// stopChild 负责注销它。空 = 本类型不需要入站通道。
	InboundCh string

	// ParentID 是父的 agent id（策略撤销、日志用）。
	ParentID string

	// CreatedAt 是 Unix 纳秒，供状态面排序。
	CreatedAt int64

	mu     sync.Mutex
	state  State
	rounds uint64
	// stopped 标记已 Stop。rtLoop 取到输入后先查它，防止
	// “Wake 信号先于 cancel 到达 select”时白跑一轮（D8）。
	stopped bool
}

// Stop 原子地标记停止并取消本子的 ctx。可重复调用（幂等）。
func (c *Child) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.stopped = true
	c.state = StateStopped
	c.mu.Unlock()
	if c.Cancel != nil {
		c.Cancel()
	}
}

// Stopped 报告是否已被 Stop。
func (c *Child) Stopped() bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

// State 是子 agent 的运行态。
type State string

const (
	StateRunning     State = "running"
	StateContextFull State = "contextfull"
	StateStopped     State = "stopped"
)

// String 让状态能直接进日志与状态面。
func (s State) String() string { return string(s) }

// SetState 设置运行态（并发安全）。
func (c *Child) SetState(s State) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
}

// State 返回当前运行态。
func (c *Child) State() State {
	if c == nil {
		return StateStopped
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == "" {
		return StateRunning
	}
	return c.state
}

// BumpRound 累加轮次（可观测性用）。
func (c *Child) BumpRound() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.rounds++
	c.mu.Unlock()
}

// Rounds 返回已跑轮次。
func (c *Child) Rounds() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rounds
}

// ── 错误 ──

var (
	// ErrChildExists 表示同名子已存在。
	//
	// 为什么不允许"同名覆盖"：登记表与通道 Owner 都以 id 为键，
	// 覆盖会让上一代的残留（通道归属、策略、留档）被当成这一代的。
	// 使用者必须先显式 stop。
	ErrChildExists = errors.New("同名子 agent 已存在")
	// ErrChildNotFound 表示 id 未登记。
	ErrChildNotFound = errors.New("子 agent 不存在")
	// ErrUnknownKind 表示 Kind 没有注册实现。
	ErrUnknownKind = errors.New("子 agent 类型未注册")
)

// 差异点接口见 kind.go。

// ToolResult 是一条工具执行结果。
type ToolResult struct {
	CallID string
	Name   string
	Text   string
	Raw    interface{}
	Denied bool
}

// ToolSpec 是父控制工具的定义与派发。
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
	Required    []string
	// Handler 接收父发起的调用。c 是**被操作的子**（可能为 nil，如 list 动作）。
	Handler func(c *Child, args map[string]interface{}) string
	// Batchable 声明该工具在父侧是否可并发执行。
	Batchable bool
	// NeedsChild 为 true 时骨架会先按 args["id"] 查出子再传 Handler。
	NeedsChild bool
}

// ResidualPolicy 是销毁时对未处理输入的处置（驻留子用）。
type ResidualPolicy string

const (
	// ResidualKeep 把残余回灌给父（不丢活）。
	ResidualKeep ResidualPolicy = "keep"
	// ResidualDrop 丢弃残余。
	ResidualDrop ResidualPolicy = "drop"
)

// ChannelDisposition 是销毁时对通道归属的处置（驻留子用）。
type ChannelDisposition string

const (
	// ChannelRelease 注销通道登记。
	ChannelRelease ChannelDisposition = "release"
	// ChannelKeep 保留登记，Owner 仍指向已销毁的 id（悬空）。
	ChannelKeep ChannelDisposition = "keep"
	// ChannelHandBack 归还父（Owner 清空或改指父）。
	ChannelHandBack ChannelDisposition = "handback"
)
