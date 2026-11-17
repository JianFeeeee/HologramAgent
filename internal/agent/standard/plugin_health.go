package standard

import (
	"log"
	"sync"
	"time"
)

const (
	maxPluginCrashes = 3
	crashWindow      = 5 * time.Minute
	reloadCooldown   = 30 * time.Second
)

type PluginHealthTracker struct {
	mu      sync.Mutex
	records map[string]*PluginHealthRecord
}

type PluginHealthRecord struct {
	CrashCount int
	LastCrash  time.Time
	Unhealthy  bool
	LastReload time.Time
}

func NewPluginHealthTracker() *PluginHealthTracker {
	return &PluginHealthTracker{
		records: make(map[string]*PluginHealthRecord),
	}
}

// RecordCrash 记录一次崩溃，返回 true 表示需要触发重载
func (t *PluginHealthTracker) RecordCrash(plugin string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	r, ok := t.records[plugin]
	if !ok {
		r = &PluginHealthRecord{}
		t.records[plugin] = r
	}

	if now.Sub(r.LastCrash) > crashWindow {
		r.CrashCount = 0
	}

	r.CrashCount++
	r.LastCrash = now

	if r.CrashCount >= maxPluginCrashes {
		r.Unhealthy = true
		log.Printf("[plugin] %s: %d crashes within %v, marking unhealthy", plugin, r.CrashCount, crashWindow)
		return true
	}

	log.Printf("[plugin] %s: crash #%d", plugin, r.CrashCount)
	return false
}

// IsHealthy 检查插件是否健康。
//
// ## 冷却期必须从**最近一次崩溃**起算（2026-11-04 修的实测缺陷）
//
// 原实现是 `time.Since(r.LastReload) > reloadCooldown`，而 LastReload
// **只在 MarkReloaded 里被赋值**（即只在"重载成功"之后）。于是：
//
//	RecordCrash 标记 Unhealthy=true
//	但 LastReload 仍是零值
//	time.Since(零值) = 约 2000 年 ⇒ 远大于 30s
//	⇒ 判定"冷却期早已过去" ⇒ 立刻置回 healthy
//
// 实测：连续 3 次 RecordCrash 后 needReload=true、Unhealthy=true，
// 紧接着 IsHealthy 返回 **true**。即**崩溃熔断从不生效**，
// task.go 与 toolbatch.go 里三处「插件不健康则跳过执行」全是死分支。
//
// 修法：冷却期以 LastCrash 为锚点 —— "刚崩完就先别用它，等 30s 再说"。
// 这才是熔断的语义（原意是给插件一个恢复窗口，不是给重载计时的）。
func (t *PluginHealthTracker) IsHealthy(plugin string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	r, ok := t.records[plugin]
	if !ok {
		return true
	}
	if !r.Unhealthy {
		return true
	}
	if time.Since(r.LastCrash) > reloadCooldown {
		r.Unhealthy = false
		r.CrashCount = 0
		log.Printf("[plugin] %s: cooldown passed (%v since last crash), restored to healthy",
			plugin, reloadCooldown)
		return true
	}
	return false
}

// MarkReloaded 标记插件已重载
func (t *PluginHealthTracker) MarkReloaded(plugin string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	r, ok := t.records[plugin]
	if ok {
		r.Unhealthy = false
		r.CrashCount = 0
		r.LastReload = time.Now()
	}
}

// PendingReloads 返回已过冷却期、需要重载的插件列表
func (t *PluginHealthTracker) PendingReloads() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	var result []string
	now := time.Now()
	for name, r := range t.records {
		if !r.Unhealthy {
			continue
		}
		// 与 IsHealthy 同一锚点（LastCrash）：刚崩完先等冷却，再尝试重载。
		// 原实现用 LastReload（零值 ⇒ 恒为真）⇒ 每次 autoReloadPlugins 都会
		// 尝试重载一个**刚崩的**插件，可能立刻再崩。
		if now.Sub(r.LastCrash) > reloadCooldown {
			result = append(result, name)
		}
	}
	return result
}
