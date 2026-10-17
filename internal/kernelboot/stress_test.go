package kernelboot

// 内核装配层（syncbus / scheduler / KernelState）的**压力测试**。
//
// ## 为什么必须有压力测试（不是"跑一遍判据就够了"）
//
// 这层代码的并发行为**只有压力才能暴露**：
//   · 判据用 2~3 个 goroutine 验证语义正确性
//   · 压力用数百 goroutine × 数千轮验证**没有数据竞争 / 死锁 / 状态丢失**
//
// 本仓已有两处真实教训支持这个判断：
//   ① ModuleView 第一版把「当前模块」放在共享对象上 ⇒ 判据**偶发**失败
//      （3 次里 1 次）—— 那是"偶发"而不是"必现"，正是压力测试的领域
//   ② syncbus 的 OnUpdate 若持锁回调 ⇒ **死锁**（只在回调读状态时触发）
//
// ## 压什么（本层真实的并发面）
//
//   · SyncBus：内核写（MarkDeclared/MarkLayerReady/MarkKernelComplete）
//     与插件读（Known/IsReady/Snapshot/AllReady/**订阅回调**）并发
//   · Scheduler：多 goroutine 并发 Submit（同一插件可能被重复提交）
//     + 状态变化并发触发 evaluate
//   · KernelState：同层模块**并行**写**各自**字段（不同字段，无竞争）
//   · PlanFromGraph：并发读图（只读，但验证无共享可变状态）
//   · Run：真实分层执行，验证有界并发真的没超

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ★★★ 压力：**SyncBus 读写并发**（内核写 + N 个插件读 + 订阅回调）。
//
// 用 -race 跑时，任何未保护的状态都会报数据竞争。
func TestStressSyncBusConcurrentReadWrite(t *testing.T) {
	bus := NewSyncBus()
	const readers = 64
	const facilities = 200

	var callbacks int64
	// 订阅者**读状态**（这正是死锁的触发条件：若 fire 持锁调用回调，
	// 回调里 Snapshot() 会等读锁 ⇒ 死锁。压力下必现）
	for i := 0; i < 8; i++ {
		bus.OnUpdate(func(s SyncState) {
			_ = s.Snapshot()
			_ = s.AllReady([]string{"llm"})
			atomic.AddInt64(&callbacks, 1)
		})
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读侧：N 个"插件"持续查询
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				f := fmt.Sprintf("fac%d", i%facilities)
				_ = bus.Known(f)
				_ = bus.IsReady(f)
				_ = bus.Snapshot()
				_ = bus.ReadyInOrder()
				_ = bus.KernelComplete()
				_ = bus.AllReady([]string{"fac1", "fac2"})
			}
		}(i)
	}

	// 写侧：内核持续标记
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < facilities; i++ {
			bus.MarkLayerReady([]string{fmt.Sprintf("fac%d", i)})
		}
	}()

	// 让它跑一会儿
	time.Sleep(400 * time.Millisecond)
	bus.MarkKernelComplete()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	if atomic.LoadInt64(&callbacks) == 0 {
		t.Fatal("订阅回调从未被调用 —— 压力没有真正覆盖通知路径")
	}
	if !bus.KernelComplete() {
		t.Fatal("KernelComplete 应已置位")
	}
	t.Logf("回调触发 %d 次，%d 个并发读者", atomic.LoadInt64(&callbacks), readers)
}

// ★★★ 压力：**多 goroutine 并发 Submit**（同一插件可能被重复提交）。
//
// 真实场景：插件加载器可能分批提交（发现新插件、重扫目录），
// 而每次提交都会触发状态评估。并发下要保证：
//
//	· 不重复拉起同一插件（幂等）
//	· 不丢插件（全部最终被拉起）
//	· 无数据竞争
func TestStressSchedulerConcurrentSubmit(t *testing.T) {
	bus := NewSyncBus()
	bus.MarkDeclared([]string{"config", "llm", "memory"})
	// ★ 必须 MarkLayerReady：MarkDeclared 只登记"名字存在"，
	// 不代表就绪。插件要求 config **就绪**才拉起。
	// （我第一版漏了这步 ⇒ 压力判据报"0 个拉起" —— 那是判据 bug，非缺陷。）
	bus.MarkLayerReady([]string{"config", "llm", "memory"})

	const plugins = 300
	var mu sync.Mutex
	launched := map[string]int{}

	sch := NewScheduler(bus, func(p PluginSpec) error {
		mu.Lock()
		launched[p.Name]++
		mu.Unlock()
		return nil
	})
	sch.SetLaunchErrorHandler(func(p PluginSpec, err error) {
		t.Errorf("不应有拉起错误: %v", err)
	})

	// 16 个 goroutine 并发提交同一批插件（模拟重复扫描）
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			var specs []PluginSpec
			for i := 0; i < plugins; i++ {
				specs = append(specs, PluginSpec{
					Name:               fmt.Sprintf("p%03d", i),
					RequiresFacilities: []string{"config"},
					AutoStart:          true,
				})
			}
			sch.Submit(specs)
		}(g)
	}
	bus.MarkKernelComplete()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(launched) != plugins {
		t.Fatalf("★ 应拉起 %d 个插件，实际 %d（并发提交丢了插件）", plugins, len(launched))
	}
	for name, n := range launched {
		if n != 1 {
			t.Fatalf("★ 插件 %s 被拉起 %d 次（应恰好 1 次 —— 并发下幂等性破坏）", name, n)
		}
	}
}

// ★★★ 压力：**状态变化 + 提交并发**（真实时序：内核在标记就绪的同时插件在提交）。
func TestStressSchedulerConcurrentStateChange(t *testing.T) {
	bus := NewSyncBus()
	bus.MarkDeclared([]string{"config", "llm", "memory", "stagehost"})

	var launched int64
	sch := NewScheduler(bus, func(p PluginSpec) error {
		atomic.AddInt64(&launched, 1)
		return nil
	})
	sch.SetLaunchErrorHandler(func(p PluginSpec, err error) {})

	const plugins = 200
	var wg sync.WaitGroup

	// 写侧：内核逐层标记就绪
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, f := range []string{"config", "llm", "memory", "stagehost"} {
			bus.MarkLayerReady([]string{f})
		}
		bus.MarkKernelComplete()
	}()

	// 提交侧：8 个并发提交者
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < plugins; i++ {
				sch.Submit([]PluginSpec{{
					Name:               fmt.Sprintf("q%03d", i),
					RequiresFacilities: []string{"config"},
					AutoStart:          true,
				}})
			}
		}()
	}
	wg.Wait()
	bus.MarkKernelComplete() // 再触发一次评估，确保挂起的也被拉起

	if n := atomic.LoadInt64(&launched); n != plugins {
		t.Fatalf("★ 并发 提交×状态变化 下应拉起 %d 个，实际 %d", plugins, n)
	}
}

// ★★★ 压力：**PlanFromGraph 并发读**（多插件/多协程同时读启动计划）。
func TestStressPlanFromGraphConcurrentRead(t *testing.T) {
	mods := planTestModules()
	built, err := Build(mods)
	if err != nil {
		t.Fatal(err)
	}
	gf := decodeGraphOrFatal(t, EncodeGraph(planToEdgesForTest(built)))

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				plan, err := PlanFromGraph(gf, mods)
				if err != nil {
					errs <- err
					return
				}
				if len(plan.Layers) != 3 {
					errs <- fmt.Errorf("层数不对: %d", len(plan.Layers))
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// ★★★ 压力：**Run 的并发上限真的生效**（大量模块同层时不得超出）。
//
// ★ 这条与判据的 TestRunRespectsMaxParallel 不同：那条 8 个模块，
// 这条 **128 个模块**同层 —— 验证信号量在高压下不漏（观察峰值）。
func TestStressRunRespectsMaxParallelAtScale(t *testing.T) {
	const n = 128
	const maxP = 8

	var active, peak int64
	var mu sync.Mutex
	var mods []Module
	for i := 0; i < n; i++ {
		mods = append(mods, &fakeModule{
			name:     fmt.Sprintf("m%03d", i),
			provides: []string{fmt.Sprintf("f%03d", i)},
			onInit: func() {
				cur := atomic.AddInt64(&active, 1)
				mu.Lock()
				if cur > peak {
					peak = cur
				}
				mu.Unlock()
				time.Sleep(2 * time.Millisecond)
				atomic.AddInt64(&active, -1)
			},
		})
	}
	plan, err := Build(mods)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(plan, RunOptions{MaxParallel: maxP})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	observed := peak
	mu.Unlock()
	if observed > maxP {
		t.Fatalf("★ 并发上限 %d 被突破：观测峰值 %d（%d 个模块同层）", maxP, observed, n)
	}
	if len(res.Order) != n {
		t.Fatalf("应执行 %d 个模块，实际 %d", n, len(res.Order))
	}
	t.Logf("%d 个模块同层，上限 %d，观测峰值 %d", n, maxP, observed)
}

// ★★★ 压力：**KernelState 同层并行写**（各写自己字段，无竞争）。
func TestStressKernelStateParallelWrites(t *testing.T) {
	ks := NewKernelState()
	const n = 256

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 每个"模块"只写自己字段 —— 这正是真实约定。
			// 若有人违反（写别人的字段），-race 会报。
			ks.AddCloser(func() {})
		}(i)
	}
	wg.Wait()
	if len(ks.Closers) != n {
		t.Fatalf("应记录 %d 个关闭函数，实际 %d（AddCloser 有丢失）", n, len(ks.Closers))
	}
	if err := ks.CloseAll(); err != nil {
		t.Fatal(err)
	}
	if len(ks.Closers) != 0 {
		t.Fatal("CloseAll 后应清空")
	}
}
