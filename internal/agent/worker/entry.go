package worker

// entry.go —— 工作式子实例的**骨架登记项**（`child.Entry` 实现）。
//
// ## 为什么必须补这一层（2026-11-20）
//
// 原先 worker 根本没有实例：`Construct` 只起一个裸 goroutine
// （`go m.runChildTask(...)`），借用**父** agent 的 provider / 工具表 /
// `RunToolBatch`。实证：
//
//	worker 包内 agent.New( )   → 0 处
//	worker 包内 Children().Add → 0 处
//
// 后果（都是真缺陷）：
//
//	· `RegistryToolDefs()` 按父的侧别取表（SideParent）⇒ 子侧工具漏给它
//	· `RunToolBatch` 用父的 Host 跑工具 ⇒ 安全闸按父身份判
//	· `InjectSelfChannel` 注入父的队列 ⇒ 完成通知串到自己身上
//	· `admin_child_list` 看不到它 / `StopAll` 收不掉它
//
// 现在与 resident 同构：`agent.New` 造**自己的**实例 + `Children().Add` 登记。

import (
	"context"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent/child"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// workerEntry 是一个工作式子实例。
//
// 形态照 `resident/resident.go` 的 `residentChild`：嵌入 `*child.Child`
// 承载公共部分（ID / Ctx / IO / 状态），载体 agent 自己持有。
type workerEntry struct {
	*child.Child
	// a 是本实例的载体（**它自己的** agent，不是父的）。
	a any
	// m 是父侧的载体（读任务表用）。
	m *M
	// taskID 是任务 id（与实例 id 相同）。
	taskID string
}

func (e *workerEntry) Base() *child.Child {
	if e == nil {
		return nil
	}
	return e.Child
}

func (e *workerEntry) Kind() child.Kind { return child.KindWorker }

// Agent 返回载体（骨架用它读"这个子 agent 本身"）。
func (e *workerEntry) Agent() any { return e.a }

func (e *workerEntry) State() string {
	if e == nil || e.Child == nil {
		return "stopped"
	}
	if e.Child.Stopped() {
		return "stopped"
	}
	return "running"
}

// Channels 返回持有的 inputch：工作式子**不接通道**（一次性执行体）。
func (e *workerEntry) Channels() []string { return nil }

// Disposition 返回通道处置策略：无通道 ⇒ 空。
func (e *workerEntry) Disposition() string { return "" }

// OffloadOwned 恒 false（不是 kernel 为转投拉起的实例）。
func (e *workerEntry) OffloadOwned() bool { return false }

// Dispose 按本类型语义收尾。
//
// ★ worker 没有图记忆/临时库句柄（它只用自己的上下文），
//
//	所以这里**只**做骨架要求的公共收尾：停载体 + 标记停止。
//
// 返回给父看的摘要（可为空）。
func (e *workerEntry) Dispose() string {
	if e == nil {
		return ""
	}
	if e.Child != nil {
		e.Child.Stop()
	}
	return ""
}

// newWorkerEntry 构造一个登记项。
func newWorkerEntry(id string, a any, m *M, parentID string, ioM *agentIO.IOManager, ctx context.Context, cancel context.CancelFunc) *workerEntry {
	base := &child.Child{
		LogIdentity: child.LogIdentity{ID: id},
		IO:          ioM,
		Ctx:         ctx,
		Cancel:      cancel,
		ParentID:    parentID,
	}
	// ★ Cancel 由调用方在 WithCancel 之后填（这里不造 ctx）。
	return &workerEntry{Child: base, a: a, m: m, taskID: id}
}
