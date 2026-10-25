package proc

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// stage 执行：把内核的 RunStage 并发扇出接到共享段（§3.4，风险 3.4 的落点）。
//
// 执行链路：
//
//	内核 RunStage（并发 go func，原始设计不变）
//	  └─ 外部插件 handler = coreHandler.runStage()
//	       ├─ Host.beginStage：首个到达者独占共享段并写入 StageContext
//	       ├─ stage.invoke RPC → 插件进程
//	       │    └─ 插件侧：stage.lock → 读共享段 → handler → 只写脏字段 → stage.unlock
//	       └─ Host.endStage：最后离开者把共享段读回内核 StageContext + 压实 arena
//
// 关键性质：
//   - **并发扇出保留**（§0.2 第 1 条：并发扇出是原始设计，不是缺陷）
//   - **无副本**：全部插件 mmap 同一 memfd，在同一份状态上读改写，锁仲裁串行化临界区
//   - **只读插件零写入**：脏字段集为空 → 不可能覆盖他人改写
//
// 对照今日 C ABI：每个插件拿到独立 JSON 副本，回传时无条件覆盖 10 个字段，
// 实测 35.8~36.8% lost update（§8.4），现网量级百分之几脏数据进 LLM（§8.6）。

// lockRegistry 持有当前进行中 stage 的锁，供插件的 stage.lock/unlock 路由。
type lockRegistry struct {
	mu   sync.Mutex
	lock *stageLock
}

func (r *lockRegistry) bind(l *stageLock) {
	r.mu.Lock()
	r.lock = l
	r.mu.Unlock()
}

// unbind 在协调器结束时摘除路由（2026-10-07 修）。
//
// ★ 为什么必须有：协调器结束时它就不该再被路由到。
//
//	旧实现只 bind 从不解绑，于是 locks.lock 永远指向**上一轮**的锁；
//	那一轮若有插件异常退出（defer unlock 未执行），它的 held 会一直挂着，
//	下一轮的 stage.lock 打过来就成了“重复申请”。
//	生产实测：本机 homed 93 条 qq 报错就是这么来的。
//
// 条件 compare 只在仍指向该锁时才摘（防止把新一轮的绑定误清）。
func (r *lockRegistry) unbind(l *stageLock) {
	r.mu.Lock()
	if r.lock == l {
		r.lock = nil
	}
	r.mu.Unlock()
}

// releaseIfLeaked 在协调器结束时释放可能没被释放的锁，并返回是否释放了。
//
// 自愈兜底：锁的所有权在内核进程（这正是 ForceRelease 存在的理由），
// 那么“持锁的插件已经不在了”这件事内核必须能收尾——否则一把泄漏的锁
// 会让整个 stage 通道逐轮卡死。
func (r *lockRegistry) releaseIfLeaked(l *stageLock) bool {
	if l == nil {
		return false
	}
	owner := l.Owner()
	if owner == "" {
		return false
	}
	if l.ForceRelease(owner) {
		log.Printf("[proc] stage coordinator found %s still holding stage lock at exit (abnormal path missed unlock), released on its behalf", owner)
		return true
	}
	return false
}

func (r *lockRegistry) current() *stageLock {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lock
}

func (r *lockRegistry) acquire(plugin string) error {
	l := r.current()
	if l == nil {
		return fmt.Errorf("stage.lock: 当前无进行中的 stage（插件 %s 在 stage 外加锁？）", plugin)
	}
	return l.Acquire(plugin)
}

func (r *lockRegistry) release(plugin string) error {
	l := r.current()
	if l == nil {
		return fmt.Errorf("stage.unlock: 当前无进行中的 stage（插件 %s）", plugin)
	}
	return l.Release(plugin)
}

// forceRelease 在插件进程崩溃时释放其可能持有的锁（实验 9 的自愈机制）。
func (r *lockRegistry) forceRelease(plugin string) bool {
	l := r.current()
	if l == nil {
		return false
	}
	return l.ForceRelease(plugin)
}

// stageInvokeTimeout 是单个插件执行 stage 的上限。
//
// 取 30s：与内核工具超时（60s，toolcall.go）留出差距，
// 使 stage 超时能被识别为 stage 问题而非工具问题。
// 超时后调用方返回错误，卡住的插件进程可由上层 Kill 回收——
// **对比 cgo 路径超时后 OS 线程永久泄漏（现网 26 次，§9.3）**。
const stageInvokeTimeout = 30 * time.Second

// runStage 是注册到内核 StageHost 的 handler（每个外部插件一个）。
func (h *coreHandler) runStage(stage string, sc *pubsdk.StageContext) error {
	if h.host == nil {
		return fmt.Errorf("插件 %s: stage %s 共享段未就绪", h.name, stage)
	}

	coord, err := h.host.beginStage(sc)
	if err != nil {
		return fmt.Errorf("插件 %s stage %s: %w", h.name, stage, err)
	}
	defer func() {
		if endErr := h.host.endStage(coord); endErr != nil {
			log.Printf("[proc] %s stage %s finalization failed: %v", h.name, stage, endErr)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), stageInvokeTimeout)
	defer cancel()

	if err := h.invokeStageWithCtx(ctx, stage, coord.seg.Seq()); err != nil {
		// 插件可能在持锁时失败（崩溃/超时）——强制释放，避免后续插件死锁。
		// 这正是"锁仲裁回内核"的自愈价值（实验 9）：无需 robust mutex。
		if h.host.ForceReleaseLock(h.name) {
			log.Printf("[proc] %s stage %s failed, force-releasing its stage lock", h.name, stage)
		}
		return err
	}
	return nil
}
