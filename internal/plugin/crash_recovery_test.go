package plugin

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// fakeCleaner 记录内核侧摘除动作，用于断言「插件死后注册面被摘干净」。
//
// 同时实现 PluginToolCleaner 与 PluginStageCleaner——生产里 StageHost 两者都实现。
type fakeCleaner struct {
	mu     sync.Mutex
	tools  []string
	stages []string
}

func (f *fakeCleaner) UnregisterPluginTools(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools = append(f.tools, name)
}

func (f *fakeCleaner) UnregisterPluginStages(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stages = append(f.stages, name)
	return 1
}

func (f *fakeCleaner) toolCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tools...)
}

func (f *fakeCleaner) stageCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stages...)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// newTestRegistry 造一个可用于 detach/崩溃路径测试的最小 Registry。
func newTestRegistry(t *testing.T) (*Registry, *fakeCleaner, *agentIO.IOManager) {
	t.Helper()
	cleaner := &fakeCleaner{}
	iom := agentIO.NewIOManager()
	r := &Registry{
		plugins:           make(map[string]sdk.Plugin),
		factories:         make(map[string]NativeFactory),
		pluginAutoRestart: make(map[string]bool),
		sdkRefs:           make(map[string]*sdk.PluginSDK),
		knownDisabled:     make(map[string]bool),
		pluginHashes:      make(map[string]string),
		pluginChannels:    make(map[string]*pluginChannelSet),
		plgDir:            t.TempDir(),
		toolCleaner:       cleaner,
		iom:               iom,
	}
	return r, cleaner, iom
}

// detachPlugin 必须同时摘工具、stage handler、IO 通道。
//
// 此前各卸载路径只调 UnregisterPluginTools，漏了后两项：
// 插件的工具没了但 stage handler 还在每轮 RunStage 里被调用并失败，
// output device 还留在 IOManager 里让模型看到一个永远发不出去的通道。
func TestDetachPlugin_RemovesToolsStagesAndChannels(t *testing.T) {
	r, cleaner, iom := newTestRegistry(t)

	// 模拟插件注册过通道
	if err := iom.RegisterDevice(&channelDevice{name: "demo_out"}); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	iom.RegisterInputChannel("demo_in", agentIO.ChannelDef{})
	r.noteChannel("demo", "demo_out", true)
	r.noteChannel("demo", "demo_in", false)

	r.detachPlugin("demo")

	if !contains(cleaner.toolCalls(), "demo") {
		t.Error("应摘除插件工具")
	}
	if !contains(cleaner.stageCalls(), "demo") {
		t.Error("应摘除插件 stage handler（否则每轮 RunStage 都会打向已死插件）")
	}
	if iom.GetDevice("demo_out") != nil {
		t.Error("output device 应被摘除，否则模型仍看到一个必然失败的通道")
	}
	if _, ok := iom.GetInputChannelDef("demo_in"); ok {
		t.Error("input channel 定义应被摘除")
	}
}

// 通道台账在 detach 后清空，使插件重启时能重新注册同名通道。
//
// 不清空的后果：RegisterDevice 撞上同名旧 device 直接报 already registered，
// 新进程的通道注册不上——插件“重启成功”了但通道永久指向已死进程。
func TestReleasePluginChannels_AllowsReRegistrationAfterRestart(t *testing.T) {
	r, _, iom := newTestRegistry(t)

	if err := iom.RegisterDevice(&channelDevice{name: "qq"}); err != nil {
		t.Fatalf("首次注册: %v", err)
	}
	r.noteChannel("qq", "qq", true)

	r.detachPlugin("qq")

	// 重启后同名通道必须能重新注册
	if err := iom.RegisterDevice(&channelDevice{name: "qq"}); err != nil {
		t.Fatalf("摘除后应可重新注册同名通道，实际: %v", err)
	}
	// 台账已清空，重复 detach 不应再摘掉新注册的那个
	r.releasePluginChannels("qq")
	if iom.GetDevice("qq") == nil {
		t.Error("台账已清空，重复 detach 不应摘掉重启后新注册的通道")
	}
}

// 崩溃回调必须摘注册面 + 排重启，且不阻塞调用方（它跑在 readLoop 的 goroutine 里）。
func TestOnProcCrash_DetachesImmediately(t *testing.T) {
	r, cleaner, _ := newTestRegistry(t)
	// 没有插件目录 → ReloadOne 必然失败，但摘除动作应已完成
	r.pluginAutoRestart["ghost"] = false // 关掉自动重启，只验摘除

	done := make(chan struct{})
	go func() {
		r.onProcCrash("ghost", errors.New("signal: killed"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("onProcCrash 不应阻塞（它在 readLoop 的 goroutine 上）")
	}

	if !contains(cleaner.toolCalls(), "ghost") {
		t.Error("崩溃后应立即摘除工具，否则模型继续调用一个必然失败的工具")
	}
	if !contains(cleaner.stageCalls(), "ghost") {
		t.Error("崩溃后应摘除 stage handler")
	}
}

// 声明了不自动重启的插件，崩溃后不得被拉起。
func TestScheduleProcRestart_RespectsAutoRestartOff(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	r.pluginAutoRestart["noauto"] = false

	r.scheduleProcRestart("noauto", errors.New("boom"))

	if n := r.crashCount("noauto"); n != 0 {
		t.Errorf("禁用自动重启时不该记崩溃计数，实际 %d", n)
	}
}

// 窗口内连续崩溃超过上限后停止自动重启，避免崩溃循环打满 CPU。
func TestScheduleProcRestart_StopsAfterThreshold(t *testing.T) {
	r, _, _ := newTestRegistry(t)

	for i := 0; i < procMaxRestarts+2; i++ {
		r.noteCrash("loopy")
	}
	if got := r.crashCount("loopy"); got != procMaxRestarts+2 {
		t.Fatalf("崩溃计数应累计，实际 %d", got)
	}

	// 超阈值后再调不应尝试重启（无插件目录时重启必然失败并留日志，
	// 这里只验它提前返回：计数不再增长）。
	before := r.crashCount("loopy")
	r.scheduleProcRestart("loopy", errors.New("again"))
	if after := r.crashCount("loopy"); after != before+1 {
		t.Errorf("应只记一次计数即返回，before=%d after=%d", before, after)
	}
}

// 崩溃计数在窗口外自动归零，避免偶发崩溃永久累积成“不可重启”。
func TestNoteCrash_WindowExpiry(t *testing.T) {
	r, _, _ := newTestRegistry(t)

	r.noteCrash("old")
	r.crashMu.Lock()
	r.procCrashes["old"].last = time.Now().Add(-procCrashWindow - time.Second)
	r.crashMu.Unlock()

	if got := r.crashCount("old"); got != 0 {
		t.Errorf("窗口外计数应归零，实际 %d", got)
	}
	if got := r.noteCrash("old"); got != 1 {
		t.Errorf("窗口外应重新从 1 计，实际 %d", got)
	}
}

// 关停途中不得再拉起插件：段已拆而进程还在会直接 SIGBUS。
func TestScheduleProcRestart_SkippedDuringShutdown(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	r.shuttingDown.Store(true)

	r.scheduleProcRestart("any", errors.New("boom"))

	if n := r.crashCount("any"); n != 0 {
		t.Errorf("关停中应直接返回，不记计数，实际 %d", n)
	}
}

// PluginRuntime 对未安装插件返回 false，对有目录的插件报告加载通道。
func TestPluginRuntime_ReportsChannelAndInstallState(t *testing.T) {
	r, _, _ := newTestRegistry(t)

	if _, ok := r.PluginRuntime("nope"); ok {
		t.Error("未安装插件应返回 false")
	}

	dir := filepath.Join(r.plgDir, "procplug")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, binEntry)
	if err := os.WriteFile(bin, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	info, ok := r.PluginRuntime("procplug")
	if !ok {
		t.Fatal("有插件目录应视为已安装")
	}
	if info.Channel != "proc" {
		t.Errorf("应识别为 proc 通道，实际 %q", info.Channel)
	}
	if info.Loaded || info.Alive {
		t.Error("未加载的插件不应报告 loaded/alive")
	}
}
