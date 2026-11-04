package core

// 判据：插件崩溃熔断**真的会熔断**。
//
// ## 守的是什么（2026-11-04 实测缺陷）
//
// 原实现：recordCrash 标记 Unhealthy=true，isHealthy 却用
//
//	time.Since(r.LastReload) > reloadCooldown
//
// 而 LastReload **只在 markReloaded 里赋值**（重载成功后）。插件刚崩、
// 从未重载时它是零值，time.Since(零值) ≈ 2000 年 ⇒ 远大于 30s
// ⇒ 判"冷却期早已过去" ⇒ **立刻把 Unhealthy 置回 false**。
//
// 后果（这条比"少一个保护"严重）：
//
//	agent 判定插件不健康 ⇒ 跳过执行、拉起重载  —— 这道保护**从不生效**
//	recordCrash 返回 needReload=true 也没用：下一句 isHealthy 就说它健康了
//
// 实测证据：连续 3 次 recordCrash 后 needReload=true、Unhealthy=true，
// 紧接着 isHealthy 返回 true。
//
// ## 修法
//
// 冷却期锚点从 LastReload 改为 **LastCrash** —— "刚崩完先别用它，等 30s"。
// 这才是熔断语义：给插件一个观察窗口，不是给重载计时。
//
// ## 这些判据最容易写废的地方
//
// ① 只断言 recordCrash 返回值 —— 它本来就返回 true，测了等于没测。
//    必须断言 **isHealthy 的结论**，那才是被熔断逻辑消费的东西。
// ② 不测"冷却之后会恢复" —— 那样把代码改成"永不恢复"也能过，
//    而那就成了永久禁用插件。

import (
	"testing"
	"time"
)

// TestCircuitBreakerActuallyOpens 是主判据。
//
// 变异：把 isHealthy 的锚点改回 r.LastReload ⇒ 本例红（立刻判健康）。
func TestCircuitBreakerActuallyOpens(t *testing.T) {
	tr := newPluginHealthTracker()

	for i := 0; i < maxPluginCrashes; i++ {
		tr.recordCrash("p")
	}

	if tr.isHealthy("p") {
		t.Fatalf("★ 连续 %d 次崩溃后插件仍被判健康 ——\n"+
			"熔断从不生效，三处「插件不健康则跳过执行」全是死分支。\n"+
			"（原实现用 LastReload 作冷却锚点，零值让冷却显得早已过去）",
			maxPluginCrashes)
	}
}

// TestCircuitBreakerWaitsForCooldownBeforeReload 钉住重载队列也要等冷却。
//
// 原实现同样用 LastReload（零值）⇒ 每次 autoReloadPlugins 都会尝试重载
// 一个刚崩的插件，很可能立刻再崩（崩溃循环）。
//
// 变异：pendingReloads 锚点改回 LastReload ⇒ 本例红。
func TestCircuitBreakerWaitsForCooldownBeforeReload(t *testing.T) {
	tr := newPluginHealthTracker()
	for i := 0; i < maxPluginCrashes; i++ {
		tr.recordCrash("p")
	}
	if got := tr.pendingReloads(); len(got) != 0 {
		t.Fatalf("★ 刚崩完就进了重载队列（应等冷却期）：%v", got)
	}
}

// TestCircuitBreakerRecoversAfterCooldown 钉住"冷却之后要恢复"。
//
// 没有这条，把 isHealthy 改成恒返回 false 也能让主判据通过 ——
// 那就成了永久禁用插件（比不熔断更糟：一个抖动过的插件永远被停用）。
//
// 变异：删掉 isHealthy 里的恢复分支（恒 return false）⇒ 本例红。
func TestCircuitBreakerRecoversAfterCooldown(t *testing.T) {
	tr := newPluginHealthTracker()
	for i := 0; i < maxPluginCrashes; i++ {
		tr.recordCrash("p")
	}
	if tr.isHealthy("p") {
		t.Fatal("前置条件不成立：应当先处于熔断态")
	}

	// 把最近崩溃时间往前拨，模拟"冷却期已过"。
	tr.mu.Lock()
	tr.records["p"].LastCrash = time.Now().Add(-2 * reloadCooldown)
	tr.mu.Unlock()

	if !tr.isHealthy("p") {
		t.Fatal("★ 冷却期过后仍未恢复健康 —— 插件会被永久停用（比不熔断更糟）")
	}
	// 恢复后计数清零，下次崩溃重新从头计数。
	tr.mu.Lock()
	cnt := tr.records["p"].CrashCount
	tr.mu.Unlock()
	if cnt != 0 {
		t.Fatalf("★ 恢复后 CrashCount=%d，应清零（否则一次抖动就永久熔断）", cnt)
	}
}

// TestCircuitBreakerUnknownPluginIsHealthy 钉住边界：从没崩过的插件不记账。
func TestCircuitBreakerUnknownPluginIsHealthy(t *testing.T) {
	tr := newPluginHealthTracker()
	if !tr.isHealthy("never-seen") {
		t.Fatal("★ 从未崩溃的插件被判不健康")
	}
	if got := tr.pendingReloads(); len(got) != 0 {
		t.Fatalf("★ 空表却给出重载队列：%v", got)
	}
}

// TestCircuitBreakerWindowResets 钉住崩溃窗口：稀疏的旧崩溃不该累加成熔断。
func TestCircuitBreakerWindowResets(t *testing.T) {
	tr := newPluginHealthTracker()
	for i := 0; i < maxPluginCrashes-1; i++ {
		tr.recordCrash("p")
	}
	// 把上次崩溃拨到窗口之外：下一次崩溃应从 1 重新计数。
	tr.mu.Lock()
	tr.records["p"].LastCrash = time.Now().Add(-2 * crashWindow)
	tr.mu.Unlock()

	need := tr.recordCrash("p")
	if need {
		t.Fatal("★ 窗口外的旧崩溃被累加进本次计数（应重新从 1 开始）")
	}
	if !tr.isHealthy("p") {
		t.Fatal("★ 窗口重置后不该处于熔断态")
	}
}
