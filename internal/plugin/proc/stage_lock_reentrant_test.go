package proc

import (
	"strings"
	"sync"
	"testing"
	"time"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// qq 插件「重复申请 stage 锁」的生产诊断与修复（2026-10-07）
//
// ## 生产现象
//
// 本机 homed（10-07 11:28~21:58）日志里 93+1 条：
//
//	[stage] before_toolcall handler error: proc: qq.stage.invoke:
//	  申请 stage 锁: proc: 插件 qq 重复申请 stage 锁（handler 内不应嵌套加锁）
//
// sanitizer 各 1 条。qq 是唯一「每次工具调用都跑 before_toolcall」的插件，
// 所以把这个缺陷放大到天天可见。
//
// ## 我走过的两条错路（记录以免重犯）
//
// ❌ 错路一：「内核没释放锁」。——错。模板 handleStageInvoke 的加解锁是
//    完整配对的（unlocked 标志保证只解一次），不会漏解。
//
// ❌ 错路二：「同一插件注册了两个 before_toolcall handler，扇出并发进入」。
//    ——错。实测线上 qq 是**单进程（pid 恒定）**，且
//    `strings plugin.bin | grep beforeOwnToolcall` = 0，
//    说明线上跑的是 example/qq 那份（只注册一次 beforeToolcall）。
//    内核 RegisterStageFor 用 append 但插件侧只有一个 entry。
//
// ## 真正的根因：lockRegistry 换锁时不做交接
//
// stageLock 是**每个协调器新建一个**（newStageCoordinator → newStageLock），
// 而 lockRegistry 只在 beginStage 里 bind，**endStage 从不解绑**。
//
// 于是存在这样一个窗口：
//
//	第 N 轮：beginStage → bind(lockN)
//	第 N 轮：endStage  → h.coord=nil, stageMu.Unlock()
//	★ locks.lock 仍指向 lockN；lockN.held 取决于最后一个插件有没有 unlock
//	第 N+1 轮：某插件在 bind(lockN+1) 之前发来 stage.lock
//	         → 打到 lockN 上
//
// 若 lockN.held 仍为 true 且 owner 恰是同名插件，就是「重复申请」。
//
// ★ 为什么不释放 lockN.held 就是漏洞：插件异常路径（handler panic 被
//   runStage 的 recover 吞掉、RPC 响应写失败等）会让 defer unlock 之前
//   就 return，锁就永久挂着。ForceRelease 只在 invokeStageWithCtx
//   **返回错误**时触发，正常路径没有兜底。
//
// ## 修法
//
// 1. endStage 的「最后离开者」分支里显式解绑（locks.bind(nil)）——
//    协调器结束时它就不该再被路由到。
// 2. 更根本：同一插件对同一把锁的**重复申请应当是幂等的**，
//    而不是报错。因为一个插件在同一 stage 内注册多个 handler 时，
//    第二次进入是合法的（同一进程、同一临界区、串行执行即可）。
//    Acquire 改成：已持有者**等待**而不是报错。
// 3. runStage 的 defer 里核对 Owner()，仍被持有则记日志 + ForceRelease，
//    让异常路径不会把锁泄漏到下一轮。

// ---- 判据 1：协调器结束后不得再被路由 ----

// TestLockRegistry_UnbindOnLastDepart 钉住 endStage 后锁不再可路由。
//
// ★ 这条判据描述的是「上一轮的锁泄漏到下一轮」那个生产缺陷：
//
//	若不解绑，lockN 仍持有 true 时，下一轮的插件会打到旧锁上。
func TestLockRegistry_UnbindOnLastDepart(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	sc := &pubsdk.StageContext{RawMessage: "round-A"}
	coord, err := host.beginStage(sc)
	if err != nil {
		t.Fatalf("beginStage: %v", err)
	}
	// 模拟插件持锁后异常退出（defer unlock 没跑到）
	if err := host.locks.acquire("qq"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := host.endStage(coord); err != nil {
		t.Fatalf("endStage: %v", err)
	}

	// ★ 协调器已结束：此后再来的 stage.lock 不得再打到它的锁上。
	if got := host.locks.current(); got != nil {
		t.Errorf("最后离开后应解绑锁路由，实际仍指向 %p（held=%v owner=%q）",
			got, got.held, got.owner)
	}
}

// ---- 判据 2：同一插件的重复申请必须幂等，不能报错 ----

// TestStageLock_SameOwnerReentryIsIdempotent 是 qq 那 93 条报错的正解。
//
// 一个插件在同一 stage 内注册多个 handler 时，第二次进入是**合法**的
// （同一进程、同一个临界区，串行执行即可）。报错会让它的第二个 handler
// 永远跑不到 —— 对 qq 而言那意味着权限门失效。
func TestStageLock_SameOwnerReentryIsIdempotent(t *testing.T) {
	l := newStageLock()
	if err := l.Acquire("qq"); err != nil {
		t.Fatalf("首次 Acquire: %v", err)
	}
	// 同一插件再次进入同一临界区：必须成功，且不得死锁
	done := make(chan error, 1)
	go func() { done <- l.Acquire("qq") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("★ 同插件重入不应报错（会让第二个 handler 永不执行）: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("★ 同插件重入死锁了 —— 比报错更糟，会把整个 stage 卡住")
	}
	// 重入后仍应由该插件持有
	if l.Owner() != "qq" {
		t.Errorf("Owner 应仍为 qq，实际 %q", l.Owner())
	}
	// ★ 可重入的关键性质：depth 减到 0 才真正释放。
	//   本函数共 Acquire 了 2 次（外层 + 重入），所以必须 Release 两次。
	//   只放一次时锁仍归 qq —— 这不是 bug，是外层还没出临界区。
	if err := l.Release("qq"); err != nil {
		t.Errorf("第一次 Release: %v", err)
	}
	if l.Owner() != "qq" {
		t.Errorf("深度未归零时锁仍应归 qq（外层还在临界区），实际 %q", l.Owner())
	}
	if err := l.Release("qq"); err != nil {
		t.Errorf("第二次 Release: %v", err)
	}
	if l.Owner() != "" {
		t.Errorf("全部释放后 Owner 应为空，实际 %q", l.Owner())
	}
}

// ---- 判据 3：不同插件仍必须互斥（不能为了幂等把锁做成摆设）----

func TestStageLock_DifferentPluginsStillMutuallyExclusive(t *testing.T) {
	l := newStageLock()
	if err := l.Acquire("qq"); err != nil {
		t.Fatalf("acquire qq: %v", err)
	}
	// 别的插件必须**等**在门外，而不是报错
	done := make(chan error, 1)
	go func() { done <- l.Acquire("sanitizer") }()
	select {
	case err := <-done:
		t.Fatalf("★ 不同插件不得同时进入临界区（err=%v）—— 加锁失效", err)
	case <-time.After(200 * time.Millisecond):
		// 正确：仍在等待
	}
	if err := l.Release("qq"); err != nil {
		t.Fatalf("release qq: %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("qq 释放后 sanitizer 应能进入: %v", err)
	}
	if l.Owner() != "sanitizer" {
		t.Errorf("Owner 应为 sanitizer，实际 %q", l.Owner())
	}
}

// ---- 判据 4：异常路径不得把锁泄漏到下一轮 ----

// TestRunStage_LeakedLockRecoveredOnNextRound 钉住自愈兜底。
//
// 模拟：插件持锁后 handler 崩溃（defer unlock 未执行），
// 下一轮 stage 必须能正常开始，而不是一直撞「重复申请」。
func TestRunStage_LeakedLockRecoveredOnNextRound(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	// 直接构造「协调器结束时锁仍被持有」的状态
	sc := &pubsdk.StageContext{RawMessage: "leaky"}
	coord, err := host.beginStage(sc)
	if err != nil {
		t.Fatalf("beginStage: %v", err)
	}
	if err := host.locks.acquire("qq"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// endStage 不抛错（生产里它也不抛），但锁泄漏了
	if err := host.endStage(coord); err != nil {
		t.Fatalf("endStage: %v", err)
	}

	// 下一轮：协调器必须先真正开始（beginStage 才会 bind 新锁），
	// 而不是还挂在上一轮的旧锁上。
	c2, err := host.beginStage(&pubsdk.StageContext{RawMessage: "round-B"})
	if err != nil {
		t.Fatalf("下一轮 beginStage 应成功（上一轮泄漏不该阻塞）: %v", err)
	}
	// 绑的是**新**锁，不是上一轮那只
	if host.locks.current() == coord.lock {
		t.Error("★ 下一轮绑的仍是上一轮的锁 —— 泄漏会继续污染")
	}
	if err := host.locks.acquire("qq"); err != nil {
		t.Fatalf("下一轮加锁应成功: %v", err)
	}
	if err := host.locks.release("qq"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := host.endStage(c2); err != nil {
		t.Fatalf("endStage: %v", err)
	}
}

// ---- 判据 5：并发下同插件重入不许崩 ----

func TestStageLock_ConcurrentSameOwnerNoPanic(t *testing.T) {
	l := newStageLock()
	if err := l.Acquire("qq"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Acquire("qq"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("并发重入报错: %v", err)
	}
}

// ---- 判据 6：文案与自愈行为 ----

// TestStageLock_ReleaseNamesRealOwner 钉住释放失败时的文案。
//
// 生产排查时「试图释放 X 持有的 stage 锁」能直接指出串扰源；
// 而旧的「重复申请 stage 锁（handler 内不应嵌套加锁）」把排查者
// 引向「插件写错了」，而真因在内核的锁管理 —— 文案会固化错误认知。
func TestStageLock_ReleaseNamesRealOwner(t *testing.T) {
	l2 := newStageLock()
	if err := l2.Acquire("qq"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() { _ = l2.ForceRelease("qq") }()

	// 非持有者释放：必须报错且说清真正的持有者
	err := l2.Release("other")
	if err == nil {
		t.Fatal("释放非持有者必须报错")
	}
	if !strings.Contains(err.Error(), "qq") {
		t.Errorf("错误应指明真正的持有者 qq，实际: %v", err)
	}
	// 无人持有时释放也要报错
	if err := newStageLock().Release("ghost"); err == nil {
		t.Error("无人持有时释放必须报错")
	}
}

// ---- 判据 7：泄漏自愈必须真的发生（上一版判据抓不住）----

// TestEndStage_ReleasesLeakedLock 钉住 releaseIfLeaked。
//
// ★ 这条判据是补写的：变异测试时发现「去掉 releaseIfLeaked」**判据不红**，
//
//	因为当时只断言了「解绑后路由为 nil」，没断言锁本身被释放。
//	——判据没覆盖到，就是判据的缺口，不是实现的缺口。
func TestEndStage_ReleasesLeakedLock(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	coord, err := host.beginStage(&pubsdk.StageContext{RawMessage: "leak"})
	if err != nil {
		t.Fatalf("beginStage: %v", err)
	}
	// 模拟插件持锁后异常退出：defer unlock 没跑到
	if err := host.locks.acquire("qq"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if coord.lock.Owner() != "qq" {
		t.Fatalf("前置条件：锁应被 qq 持有，实际 %q", coord.lock.Owner())
	}

	// endStage 必须代为释放（否则这把锁毒化之后所有 stage）
	if err := host.endStage(coord); err != nil {
		t.Fatalf("endStage: %v", err)
	}
	if got := coord.lock.Owner(); got != "" {
		t.Errorf("★ 协调器结束时仍被 %q 持有 —— 泄漏的锁会毒化后续每一轮 stage", got)
	}
}

// ---- 判据 8：depth 归零前不得放掉外层的锁 ----

// TestStageLock_InnerReleaseKeepsOuterLock 钉住可重入的关键性质。
//
// ★ 这条也是补写的：变异「内层 Release 就直接放锁」时判据不红，
//
//	因为我只断言了最终状态（都放完了 Owner 为空），
//	没断言**中间状态**——而 bug 恰恰在中间：
//	外层还在临界区，锁却已被放掉，别人可以抢进去。
func TestStageLock_InnerReleaseKeepsOuterLock(t *testing.T) {
	l := newStageLock()
	if err := l.Acquire("qq"); err != nil {
		t.Fatalf("外层 Acquire: %v", err)
	}
	if err := l.Acquire("qq"); err != nil { // 重入
		t.Fatalf("重入 Acquire: %v", err)
	}
	// 内层释放：外层还在临界区，锁必须仍归 qq
	if err := l.Release("qq"); err != nil {
		t.Fatalf("内层 Release: %v", err)
	}
	if l.Owner() != "qq" {
		t.Fatalf("★ 内层释放后锁被放掉了（Owner=%q）—— 外层仍在临界区，别人能抢进去", l.Owner())
	}
	// 别的插件此刻必须进不来
	done := make(chan error, 1)
	go func() { done <- l.Acquire("sanitizer") }()
	select {
	case err := <-done:
		t.Fatalf("★ 外层仍在临界区，别的插件却进来了（err=%v）", err)
	case <-time.After(200 * time.Millisecond):
	}
	// 外层释放后才可以进
	if err := l.Release("qq"); err != nil {
		t.Fatalf("外层 Release: %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("外层释放后 sanitizer 应能进入: %v", err)
	}
}
