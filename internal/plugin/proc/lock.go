package proc

import (
	"fmt"
	safeclose "github.com/JianFeeeee/HomeAgent/internal/safeclose"
	"sync"
	"time"
)

// 跨进程锁：锁仲裁回归内核（§3.7 已裁定，实验 3 + 实验 9 支撑）。
//
// 为什么不用 robust pthread_mutex：
//   - PTHREAD_PROCESS_SHARED + ROBUST 属性 Go 标准库无等价物，引入它意味着
//     **为了一把锁保留 cgo**——与"C 整体退场"的目标冲突。
//   - 锁仲裁回内核后，持锁进程崩溃由 cmd.Wait()/stdio EOF 检测，内核代为释放；
//     实验 9 实测无死锁、**无需 EOWNERDEAD 处理**。
//   - 成本：一次 RPC 往返 19.4 µs（实验 3，20000 次测得）。stage handler 的加锁
//     频率很低（每次 stage 一两次，不是每字段一次），微秒级往返可忽略。
//
// 于是整个新架构可做到**完全无 cgo**。

// lockWaitTimeout 是插件申请 stage 锁的最长等待时间。
//
// 取 30s：stage handler 自身受 60s 工具超时约束（toolcall.go），锁等待
// 必须显著短于它，否则超时错误会指向错误的原因。超时返回错误而非
// 静默继续——**持锁失败下改写共享段会破坏并发正确性**。
const lockWaitTimeout = 30 * time.Second

// stageLock 是内核侧为单个 stage 执行持有的互斥体。
//
// 一次 RunStage 对应一个 stageLock 实例：同阶段并发扇出的所有插件
// （含跨进程的）在此排队，语义等价于今日内置插件共享
// *StageContext 的 sync.RWMutex——这正是"保留并发扇出原始设计"
// （§0.2 第 1 条：并发扇出是原始设计，不是缺陷）。
type stageLock struct {
	mu sync.Mutex

	// ownerMu 保护 owner/held，使 ForceRelease 能安全介入
	ownerMu sync.Mutex
	owner   string // 当前持锁的插件名，空表示未持有
	held    bool
	// depth 是**重入深度**：同一插件嵌套进入同一临界区的次数。
	//
	// 为什么需要它（2026-10-07 修）：见 Acquire 的注释 —— 同一插件
	// 可在同一 stage 注册多个 handler（内核 RegisterStageFor 用 append），
	// 它们并发扇出各发一次 stage.lock，第二个进来时就是同插件重入。
	depth int
}

func newStageLock() *stageLock { return &stageLock{} }

// Acquire 为 plugin 申请写锁，带超时。
//
// # 同插件重入：直接通过（2026-10-07 改）
//
// 三个版本的设计与代价（都实测过）：
//
//  1. 重入即报错 —— 生产 93 条 qq 报错，第二个 handler 永不执行
//     （对 qq 而言那是权限门失效）。
//  2. 重入即等待 —— 同进程重入是自己等自己，30s 超时死锁。**更糟**：
//     我先实现了这版，跑判据立刻挂 30s，教训是「等待」不是重入的正解。
//  3. 重入即通过（当前）—— 可重入锁语义。调用方是「插件的一个 stage
//     handler」，而内核 RegisterStageFor 用 append：**同一插件可以在同一
//     stage 注册多个 handler**，它们并发扇出、各发一次 stage.lock。
//     同一进程内第二个进来时已在临界区内（同一进程只能串行跑），
//     本就不该再取互斥量；Release 到 depth 0 才真正释放。
//
// 不同插件的互斥语义**完全不变**：仍需排队等待。
func (l *stageLock) Acquire(plugin string) error {
	// 可重入：已持有者直接进临界区（不取 mu、不排队）
	l.ownerMu.Lock()
	if l.held && l.owner == plugin {
		l.depth++
		l.ownerMu.Unlock()
		return nil
	}
	l.ownerMu.Unlock()

	acquired := make(chan struct{})
	var acquiredOnce sync.Once
	go func() {
		l.mu.Lock()
		safeclose.Safe(&acquiredOnce, acquired)
	}()

	select {
	case <-acquired:
		l.ownerMu.Lock()
		l.owner = plugin
		l.held = true
		l.depth = 1
		l.ownerMu.Unlock()
		return nil
	case <-time.After(lockWaitTimeout):
		// 等待超时：上面的 goroutine 可能随后拿到锁，必须让它能释放，
		// 否则锁永久泄漏。用一个补偿 goroutine 等它拿到后立刻放掉。
		go func() {
			<-acquired
			l.ownerMu.Lock()
			stillFree := !l.held
			l.ownerMu.Unlock()
			if stillFree {
				l.mu.Unlock()
			}
		}()
		// ★ 文案带持锁方：排查时要能直接知道在等谁。
		return fmt.Errorf("proc: 插件 %s 申请 stage 锁超时（%s，持锁方=%q）",
			plugin, lockWaitTimeout, l.Owner())
	}
}

// Release 释放写锁。非持锁者调用返回错误（防止串扰）。
//
// 可重入：depth 减到 0 才真正释放（见 stageLock.depth）。
func (l *stageLock) Release(plugin string) error {
	l.ownerMu.Lock()
	if !l.held {
		l.ownerMu.Unlock()
		return fmt.Errorf("proc: 插件 %s 释放未持有的 stage 锁", plugin)
	}
	if l.owner != plugin {
		owner := l.owner
		l.ownerMu.Unlock()
		return fmt.Errorf("proc: 插件 %s 试图释放 %s 持有的 stage 锁", plugin, owner)
	}
	l.depth--
	if l.depth > 0 {
		// 仍在嵌套层：锁继续归该插件，但**不释放 mu**
		// ——否则外层还在临界区里就被别人抢走。
		l.ownerMu.Unlock()
		return nil
	}
	l.owner = ""
	l.held = false
	l.depth = 0
	l.ownerMu.Unlock()
	l.mu.Unlock()
	return nil
}

// ForceRelease 在插件进程崩溃/退出时由内核代为释放其持有的锁（实验 9 的自愈机制）。
//
// 返回是否实际释放了锁。**这是"无需 robust mutex"的核心**：
// 锁的所有权在内核进程，插件死亡由 cmd.Wait()/stdio EOF 检测到，
// 内核直接解锁，不存在"持锁者死亡导致全局死锁"。
func (l *stageLock) ForceRelease(plugin string) bool {
	l.ownerMu.Lock()
	if !l.held || l.owner != plugin {
		l.ownerMu.Unlock()
		return false
	}
	l.owner = ""
	l.held = false
	l.depth = 0
	l.ownerMu.Unlock()
	l.mu.Unlock()
	return true
}

// Owner 返回当前持锁插件名（诊断用）。
func (l *stageLock) Owner() string {
	l.ownerMu.Lock()
	defer l.ownerMu.Unlock()
	if !l.held {
		return ""
	}
	return l.owner
}
