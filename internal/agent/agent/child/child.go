package child

// child.go 是子 agent 的**公共部分**与类型标签。
//
// # 这个包只做一件事
//
// 把所有类型的子 agent 共有的、**最容易"改一处漏一处"**的生命周期收成一处：
//
//	单一登记表（Registry）        ← 替换掉 residents / rtChildren 两张表
//	单一清理路径（Cleanup）        ← 单数与复数共用，防"复数漏一步"
//	公共字段（Child：id/ctx/IO/入站通道）
//
// # 为什么这里没有 ChildKind 接口
//
// 曾设计过 `ChildKind`（Deliver / BuildContext / Emit / RunTools / Dispose /
// ParentTools），但在实现过程中发现它是**假抽象**：
//
//	· 各类型要用载体 Agent 的大半能力（provider / 工具执行 / 记忆 / 安全）
//	· 要满足它就得以 `Entry` 为参数、在实现里做类型断言回 `*Agent`
//	· 接口比被抽象的东西还大 ⇒ 只是把复杂度换个地方放
//
// 实测结果是它零实现、零调用（26 个导出符号里 20 个无人使用）。
// 按"不留技术债"的要求，接口与其配套的 Host / ToolSpec / ToolResult 已删除，
// 类型差异直接写在各自的文件里（rtchild.go / resident.go / spawn.go）。
//
// # 已确认的边界（不要为了"更统一"而抹掉）
//
//   - 驻留子的 `DestroyResident` 有三种 ChannelDisposition（释放/挂起/接回）
//   - rtchild 的 rt 通道**销毁后悬空**，不归还
//   - 驻留子 `Start()` 起 7 个 goroutine；rtchild **只起 1 个** rtLoop
//   - 工作式子**不进**登记表（无 io/通道，结果缓存与存活登记语义不同）

import (
	"context"
	"errors"
	"log"
	"sync"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// Kind 是子 agent 的类型标签。
//
// 用字符串而不是 iota：它要出现在状态面与日志里，字符串直接可读，
// 且新增类型不需要改一个中心 switch。
type Kind string

const (
	// KindResident 是长期派驻的子 agent（任务队列、残余可回灌、不丢活）。
	KindResident Kind = "resident"
	// KindRT 是实时通话子 agent（单槽覆盖、最新者胜、可饿死、丢现场）。
	KindRT Kind = "rt"
)

// Child 是一个**已建立**的子 agent 实例的公共部分。
//
// ★ 用法是**嵌入**：类型专属状态放在各实现自己的结构体里，
// 公共部分靠嵌入提升字段：
//
//	type rtChild struct {
//	    *child.Child          // ID / Kind / Ctx / IO / InboundCh / stopped
//	    agent   *Agent        // 载体（类型专属，不进公共结构）
//	    slot    *rtSlot
//	}
//
// 为什么不把 agent *Agent 放进本结构：本包不能 import core（否则成环），
// 而 core 可以 import 本包。嵌入让公共部分与载体指针各归各位。
type Child struct {
	// LogIdentity 提供 ID / Kind 与日志身份标签（嵌入即提升 LogTag()）。
	//
	// ID 是登记表的键，也是通道 Owner 记的值。
	//
	// ★ 必须与载体 Agent 的 ID **逐字相同**：AssignRealtime 会把通道 Owner
	// 记成 carrier.id，而路由按 entry.Owner 在本表里查 —— 两处各拼一次名字
	// 就会永远查不到（曾因写成 "rt/"+id 而让插件输入全打到父身上）。
	//
	// Kind 是本实例的类型标签（Add 时由 Entry.Kind() 写入）。
	LogIdentity

	// IO 是本子自己的 IOManager（独立输入入口 + 共享通道登记表 + 父视图回退）。
	IO *agentIO.IOManager

	// Ctx / Cancel 是本子**自己的**取消域。
	//
	// 必须是 WithCancel(parentCtx) 派生：父停则子停（不留孤儿），
	// 但单独 cancel 只影响本子，不波及父与兄弟。
	Ctx    context.Context
	Cancel context.CancelFunc

	// InboundCh 是本子在父侧注册的入站通道名（"child/<id>"），
	// Cleanup 负责注销它。空 = 本类型不需要入站通道。
	InboundCh string

	// ParentID 是父的 agent id（日志用）。
	ParentID string

	// CreatedAt 是 Unix 纳秒，供状态面排序。
	CreatedAt int64

	mu      sync.Mutex
	stopped bool
}

// Stop 原子地标记停止并取消本子的 ctx。可重复调用（幂等）。
func (c *Child) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.stopped = true
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

// ── 日志身份（嵌入即提升）──

// LogIdentity 是「这个执行者在日志里是谁」的唯一推导点。
//
// ★ 为什么需要它：生产日志里「主 agent 调工具」与「驻留子调工具」写成
// **同一格式**的一行（`[agent] tool X result: ...`）。而主 agent、驻留子、
// rtchild 在同一个内核里**同进程并发**，出问题时无法回答「这一步是谁执行的」。
//
// ★ 为什么推导收在这里而不是各类型自己拼：
//
// 工具执行、超时、schema 拒绝这些日志是**共用代码路径**
// （见 core/toolcall.go 的 executeToolCallOutcome，三条路都经过它）。
// 让每个类型各自拼标签必然"改一处漏一处" —— 那正是本仓反复栽的形态。
//
// 所以：**嵌入它**就自动获得 LogTag()。两处嵌入点：
//
//	Child（各类型子 agent 的公共部分）
//	core.Agent（主 agent 没有 Child，但同样需要身份）
//
// 三者的标签形态：
//
//	[main]              主 agent（根，没有上级）
//	[resident:<id>]     驻留子 / 转投驻留子
//	[rt:<id>]           实时通话子 agent（rtchild）
//
// ⚠️ 工作式子（runChildTask）不嵌入它：它不是 agent，日志前缀已经是
// `[child] <taskID>`，自带身份（见 spawn.go）。
type LogIdentity struct {
	// Kind 是执行者类型（空 Kind ⇒ 主 agent）。
	Kind Kind
	// ID 是执行者 id（空 ⇒ 主 agent）。
	ID string
}

// LogTagMain 是主 agent 的身份标签。
const LogTagMain = "[main]"

// LogTag 返回日志身份标签。
//
// id 为空时退化为 LogTagMain：宁可少一个标签，也不要打出 `[rt:]` 这种
// 让人以为 id 被吞了的形态。nil 安全（未初始化/空指针不 panic）。
func (li *LogIdentity) LogTag() string {
	if li == nil || li.ID == "" {
		return LogTagMain
	}
	switch li.Kind {
	case KindResident:
		return "[resident:" + li.ID + "]"
	case KindRT:
		return "[rt:" + li.ID + "]"
	}
	return "[" + string(li.Kind) + ":" + li.ID + "]"
}

// Logf 写一行带身份标签的日志。
//
// ★ **这就是"基类自动记录"**：调用方只写业务内容，标签由基类补。
//
//	以前（每个调用点都得记得带）：
//	    log.Printf("%s tool %s result: %s", a.logTag(), name, res)
//	    ↑ 新加一处日志时忘了补 a.logTag() ⇒ 那一行又没有身份，且没人会发现
//
//	现在（嵌入即提升，调用方不必知道标签的存在）：
//	    a.Logf("tool %s result: %s", name, res)
//
// 与 log.Printf 逐个参数、逐条格式串等价，只是把身份前缀收进基类。
// nil 安全的：未初始化 / 空指针时按主 agent 记录，不 panic。
func (li *LogIdentity) Logf(format string, args ...any) {
	log.Printf(li.LogTag()+" "+format, args...)
}

// ── 错误 ──

var (
	// ErrChildExists 表示同名子已存在。
	//
	// 为什么不允许"同名覆盖"：登记表与通道 Owner 都以 id 为键，
	// 覆盖会让上一代的残留（通道归属、策略、留档）被当成这一代的。
	ErrChildExists = errors.New("同名子 agent 已存在")
	// ErrChildNotFound 表示 id 未登记。
	ErrChildNotFound = errors.New("子 agent 不存在")
)
