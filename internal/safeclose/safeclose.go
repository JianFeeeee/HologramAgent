// Package safeclose 提供**幂等**的 channel 关闭。
//
// ## 为什么需要它（本仓实测事故）
//
// 2026-10-17 真机 SIGTERM 实测崩溃：
//
//	panic: close of closed channel
//	main.startHeartbeat.func3()  bootstrap.go:259
//	main.main()                  main.go:293
//
// 根因：`startHeartbeat` 返回的 stop 是**裸的 `close(hbStop)`**，而它有
// **两个调用点**（`main.go` 的 `defer` + `waitForShutdown` 的显式调用）
// ⇒ 第二次 close 必然 panic。
//
// 危害形态很特殊：它崩在**退出路径的最后一步** —— 清理全做完了才崩 ⇒
// 退出码 0 → 2（panic）⇒ systemd/guard 判成"异常退出"并触发**不必要的
// 重启/告警**，而功能本身完全正常，排查方向会被引向"哪里崩了"。
//
// ## 为什么"前置检查"必须是**统一纪律**而不是逐处判断
//
// 全仓有 37 处 `close(chan)`。逐处人工判断"这里会不会被调两次"是**不可持续**的：
//
//	· 今天只被调一次，明天加一条错误路径就变成两次 —— 而那次改动看起来
//	  与 close 无关，评审不会注意；
//	· 并发场景下 `select { case <-ch: default: close(ch) }` **也不安全**
//	  （两个 goroutine 可同时落到 default，然后都 close）；
//	· 判断依据往往是"隐式的"（这个函数只在退出时调一次），这种隐式前提
//	  正是未来修改最容易破坏的东西。
//
// ⇒ 所以本包把"幂等"做成**唯一入口**：只要调 close 就走 Safe，
//
//	Safe 自带 `sync.Once`。新增的关闭路径无论怎么加，都不会 panic。
//
// ## 用法
//
//	// 方式一：就地声明 once（局部 channel、闭包返回值等）
//	var once sync.Once
//	return func() { safeclose.Safe(&once, hbStop) }
//
//	// 方式二：结构体里持有 once 字段（生命周期长的 channel）
//	type Foo struct { done chan struct{}; doneOnce sync.Once }
//	func (f *Foo) Close() { safeclose.Safe(&f.doneOnce, f.done) }
//
// ★ 判据 `internal/safeclose/close_discipline_test.go` 会强制：
//
//	除本包之外，全仓**不得**出现裸 `close(chan)`。
package safeclose

import "sync"

// Safe 幂等关闭 ch。第二次及以后的调用是 no-op。
//
// 返回 true 表示**本次真的执行了** close（首次调用），false 表示已被关闭过。
//
// ## 为什么用 sync.Once 而不是 `select { case <-ch: default: close(ch) }`
//
// select 写法在**并发**下不安全：两个 goroutine 都可能是"未关闭"，
// 都落到 default，然后都执行 close ⇒ 第二次 panic。
// （实测过这种形态：`healthcheck` 的 `Stop()` 就是用 select 判的，
//
//	单线程安全，但若将来 Stop 被并发调用就会炸。）
//
// sync.Once 是真正原子的：Do 内部由 runtime 保证只有一个执行者。
func Safe[T any](once *sync.Once, ch chan T) bool {
	if once == nil || ch == nil {
		return false
	}
	fired := false
	once.Do(func() {
		fired = true
		close(ch)
	})
	return fired
}

// SafeFunc 返回一个幂等的关闭函数（适合作为 stop 回调直接交出）。
//
//	go func() { ... case <-stop: return ... }()
//	return safeclose.SafeFunc(hbStop)
func SafeFunc[T any](ch chan T) func() {
	var once sync.Once
	return func() { Safe(&once, ch) }
}
