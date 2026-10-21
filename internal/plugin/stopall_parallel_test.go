package plugin

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// ===== StopAll 并行化 =====
//
// 串行停 23 个插件、每个最坏 5s(plugin.stop 调用)+5s(等退出)+2s(收割)
// = 最坏 276s，而 systemd 只给 90s ⇒ 关停几乎必然被 SIGKILL。
// 线上实测：每次 stop 都 "State 'stop-sigterm' timed out"，
// 进程组里 23 个插件全退完了，最后那条 [homed] stopped 仍打不出来。
//
// 并行后最坏约等于单个插件的预算（约 12s），而不是 N 倍。
//
// ★ 并行化最大的风险是死锁与重复释放：Stop 会触发 markExited →
//   onExit → ReclaimOwner，后者要读共享内存段。所以判据同时盯
//   "真的并行"与"不死锁、不错杀"。

// stubPlugin 是最小可用插件。
type stubPlugin struct {
	name        string
	delay       time.Duration
	stopped     atomic.Bool
	panicOnStop bool
	stopCount   atomic.Int32
}

func newStubPlugin(name string, delay time.Duration) *stubPlugin {
	return &stubPlugin{name: name, delay: delay}
}

func (s *stubPlugin) Name() string               { return s.name }
func (s *stubPlugin) Start(*sdk.PluginSDK) error { return nil }
func (s *stubPlugin) Stop() error {
	s.stopCount.Add(1)
	if s.panicOnStop {
		panic("stub: 故意 panic")
	}
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.stopped.Store(true)
	return nil
}

// StopAll 必须真的并行，否则退回串行 = 关停超时。
func TestStopAllStopsInParallel(t *testing.T) {
	const n = 8
	const each = 120 * time.Millisecond

	var plugins []sdk.Plugin
	var stubs []*stubPlugin
	for i := 0; i < n; i++ {
		s := newStubPlugin("p", each)
		_ = i
		stubs = append(stubs, s)
		plugins = append(plugins, s)
	}
	r := &Registry{instances: plugins}

	start := time.Now()
	r.StopAll()
	elapsed := time.Since(start)

	serial := n * each
	// 串行实现会耗时 serial；留一半余量仍能可靠区分。
	if elapsed > serial/2 {
		t.Errorf("StopAll 是串行的：%d 个插件各 %v 用了 %v（串行≈%v，并行应≈%v）",
			n, each, elapsed, serial, each)
	}
	for _, s := range stubs {
		if !s.stopped.Load() {
			t.Errorf("插件 %s 没被停掉", s.name)
		}
	}
}

// 一个插件 panic 不能带崩整个关停，也不能让其它插件停不掉。
func TestStopAllSurvivesPanickingPlugin(t *testing.T) {
	bad := newStubPlugin("bad", 0)
	bad.panicOnStop = true
	good := newStubPlugin("good", 0)
	r := &Registry{instances: []sdk.Plugin{bad, good}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if rec := recover(); rec != nil {
				t.Errorf("插件的 panic 不该冒到关停流程上：%v", rec)
			}
		}()
		r.StopAll()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("StopAll 卡死（可能死锁）")
	}
	if !good.stopped.Load() {
		t.Error("一个插件 panic 不该让其它插件停不掉")
	}
}

// 关停期间必须冻结自动重启，否则崩溃判定会在关停中把插件重新拉起
// （段已拆而进程还在 → SIGBUS）。
func TestStopAllFreezesAutoRestart(t *testing.T) {
	s := newStubPlugin("p", 0)
	r := &Registry{instances: []sdk.Plugin{s}}
	r.StopAll()
	if !r.shuttingDown.Load() {
		t.Error("StopAll 之后 shuttingDown 应为 true")
	}
	if r.instances != nil {
		t.Error("StopAll 之后 instances 应被清空")
	}
}

// 每个插件恰好 Stop 一次：重复调用会二次释放共享段/重复跑 stop handler。
func TestStopAllStopsEachPluginExactlyOnce(t *testing.T) {
	var plugins []sdk.Plugin
	var stubs []*stubPlugin
	for i := 0; i < 5; i++ {
		s := newStubPlugin("p", 0)
		stubs = append(stubs, s)
		plugins = append(plugins, s)
	}
	r := &Registry{instances: plugins}
	r.StopAll()
	for _, s := range stubs {
		if c := s.stopCount.Load(); c != 1 {
			t.Errorf("Stop 被调 %d 次，应恰好 1 次", c)
		}
	}
}

// stop handler 必须在 Stop 之前跑完（handler 负责解绑通道等），
// 且并行化后这个顺序不能被破坏。
//
// handler 挂在 PluginSDK 上（RegisterStopHandler），由 runStopHandlers
// 通过 r.sdkRefs 取出执行 —— 所以判据必须真的构造一个 PluginSDK，
// 否则测的是一条不存在的注册路径。
func TestStopAllRunsStopHandlerBeforeStop(t *testing.T) {
	var mu sync.Mutex
	var order []string

	s := newStubPlugin("p", 0)
	r := &Registry{
		instances: []sdk.Plugin{s},
		sdkRefs:   map[string]*sdk.PluginSDK{},
	}
	sdkn := sdk.New("p", sdk.SDKConfig{})
	sdkn.RegisterStopHandler(func() {
		mu.Lock()
		order = append(order, "handler")
		mu.Unlock()
	})
	r.sdkRefs["p"] = sdkn

	probe := &orderProbePlugin{stubPlugin: s, onStop: func() {
		mu.Lock()
		order = append(order, "stop")
		mu.Unlock()
	}}
	r.instances = []sdk.Plugin{probe}

	r.StopAll()
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "handler" || order[1] != "stop" {
		t.Errorf("stop handler 必须先于 Stop 执行，实际顺序 %v", order)
	}
}

type orderProbePlugin struct {
	*stubPlugin
	onStop func()
}

func (o *orderProbePlugin) Stop() error {
	o.onStop()
	return o.stubPlugin.Stop()
}
