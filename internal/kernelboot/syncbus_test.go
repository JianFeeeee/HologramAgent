package kernelboot

// syncbus + 插件调度器的判据（2026-10-16）。
//
// 设计见 docs/zh/kernel-modularization-design.md §2.6。
//
// 这组判据里**最重要的一条是"默认路径为零改动"**：
// 存量 42 个插件都不声明依赖，它们的启动时机必须逐字不变。
// 静默改变启动时机 = 静默引入竞态。

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ★★★ 判据（最关键）：**未声明依赖的插件等内核完全初始化**（存量兼容）。
//
// 这是整个设计里风险最高的一处：若未声明的插件被"提前"拉起，
// 42 个存量插件的启动时机全部改变 ⇒ 静默竞态。
//
// 反例（若实现错误）：把"未声明"当成"依赖集为空"，而空集在数学上已满足
// ⇒ 第一层完成后就拉起 ⇒ 比现状早得多。
func TestSchedulerUndeclaredWaitsForKernelComplete(t *testing.T) {
	bus := NewSyncBus()
	var launched []string
	var mu sync.Mutex
	sch := NewScheduler(bus, func(p PluginSpec) error {
		mu.Lock()
		launched = append(launched, p.Name)
		mu.Unlock()
		return nil
	})

	// 存量插件：不声明依赖
	sch.Submit([]PluginSpec{{Name: "legacy1", AutoStart: true}})
	sch.Submit([]PluginSpec{{Name: "legacy2", AutoStart: true}})

	// 内核还在装配中（几层已就绪，但没完成）
	bus.MarkLayerReady([]string{"config"})
	bus.MarkLayerReady([]string{"memory", "lua"})
	bus.MarkLayerReady([]string{"llm"})

	mu.Lock()
	n := len(launched)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("★ 内核尚未完全初始化，未声明依赖的插件就被拉起了 %d 个（存量行为被改变）", n)
	}

	// 内核完成 ⇒ 现在才该拉起
	bus.MarkKernelComplete()
	mu.Lock()
	defer mu.Unlock()
	if len(launched) != 2 {
		t.Fatalf("内核完成后应拉起 2 个，实际 %d: %v", len(launched), launched)
	}
}

// ★★★ 判据：**声明了依赖的插件在依赖就绪时立即拉起**（新能力的正面证据）。
//
// 断言"早于内核完成" —— 否则这条判据在"等内核完成"的实现下也会通过（假绿）。
func TestSchedulerDeclaredLaunchesEarly(t *testing.T) {
	bus := NewSyncBus()
	var launched []string
	var mu sync.Mutex
	sch := NewScheduler(bus, func(p PluginSpec) error {
		mu.Lock()
		launched = append(launched, p.Name)
		mu.Unlock()
		return nil
	})

	// ★ 镜像真实顺序：Run 在**执行前** MarkDeclared（登记全部模块设施名），
	// 插件 Submit 在装配之后发生 ⇒ 判据必须先声明再提交。
	bus.MarkDeclared([]string{"config"})

	// 声明只需要 config
	sch.Submit([]PluginSpec{{Name: "early", RequiresFacilities: []string{"config"}, AutoStart: true}})

	// 第 0 层只提供 config ⇒ 应立刻拉起
	bus.MarkLayerReady([]string{"config"})

	mu.Lock()
	got := append([]string{}, launched...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "early" {
		t.Fatalf("★ 依赖已就绪却未拉起: %v", got)
	}

	// ★ 关键：此时内核**尚未**完成 —— 证明它是"尽早启动"而不是"等完成"
	if bus.KernelComplete() {
		t.Fatal("测试设置错误：此刻内核不该已完成")
	}
}

// ★★★ 判据：**依赖未齐则挂起**，不会误拉起。
func TestSchedulerWaitsForMissingDependency(t *testing.T) {
	bus := NewSyncBus()
	var launched []string
	sch := NewScheduler(bus, func(p PluginSpec) error {
		launched = append(launched, p.Name)
		return nil
	})

	// ★ 先声明 llm（登记名字，但尚未就绪）—— 与真实顺序一致。
	bus.MarkDeclared([]string{"llm", "config", "memory"})

	// 需要 llm
	sch.Submit([]PluginSpec{{Name: "needsllm", RequiresFacilities: []string{"llm"}, AutoStart: true}})

	bus.MarkLayerReady([]string{"config", "memory"}) // llm 还没好
	if len(launched) != 0 {
		t.Fatalf("★ llm 未就绪却拉起了: %v", launched)
	}
	if p := sch.Pending(); len(p) != 1 {
		t.Fatalf("应挂起 1 个，实际 %v", p)
	}

	// llm 就绪 ⇒ 拉起
	bus.MarkLayerReady([]string{"llm"})
	if len(launched) != 1 || launched[0] != "needsllm" {
		t.Fatalf("llm 就绪后应拉起: %v", launched)
	}
}

// ★★★ 判据：**只读隔离**（编译期保证，不是约定）。
//
// 断言 Scheduler 拿到的接口**没有**写方法 —— 如果给它 *SyncBus，
// 它就能谎报内核状态，让插件在依赖不存在时启动（病 A）。
func TestSchedulerCannotWriteSyncState(t *testing.T) {
	// ★ 检查必须是**接口类型的方法集**，不是某个具体值。
	//
	// 我第一次写错了：用 `var st SyncState = NewSyncBus()` 再断言 `st.(writer)` ——
	// 那检查的是**动态类型**（*SyncBus 当然有 MarkReady），判据当场报假红。
	// 要约束的是"拿到 SyncState 的人**能不能**调写方法" ⇒ 必须查接口的方法集。
	rt := reflect.TypeOf((*SyncState)(nil)).Elem()
	forbidden := []string{"MarkReady", "MarkKernelComplete", "MarkLayerReady"}
	for i := 0; i < rt.NumMethod(); i++ {
		m := rt.Method(i).Name
		for _, bad := range forbidden {
			if m == bad {
				t.Fatalf("★ SyncState（插件管理器拿到的接口）暴露了写方法 %s —— "+
					"它能谎报内核状态，让插件在依赖不存在时启动（病 A）", m)
			}
		}
	}
	// 反向确认：读方法必须在（否则接口退化到没用了）
	for _, want := range []string{"IsReady", "Snapshot", "AllReady", "KernelComplete"} {
		if _, ok := rt.MethodByName(want); !ok {
			t.Errorf("SyncState 缺少读方法 %s", want)
		}
	}
	// Scheduler 持有的字段类型也必须是 SyncState（不能是 *SyncBus）
	st := reflect.TypeOf(Scheduler{})
	f, ok := st.FieldByName("bus")
	if !ok {
		t.Fatal("Scheduler 应有 bus 字段")
	}
	if f.Type.String() != "kernelboot.SyncState" {
		t.Fatalf("★ Scheduler.bus 的类型是 %s —— 必须是只读的 SyncState", f.Type)
	}
}

// ★★★ 判据：**不伴随启动的插件即使依赖齐了也不自动拉起**（§2.6.6）。
func TestSchedulerAutoStartFalseIsNotLaunched(t *testing.T) {
	bus := NewSyncBus()
	var launched []string
	sch := NewScheduler(bus, func(p PluginSpec) error {
		launched = append(launched, p.Name)
		return nil
	})

	bus.MarkDeclared([]string{"config"})
	sch.Submit([]PluginSpec{
		{Name: "manual", RequiresFacilities: []string{"config"}, AutoStart: false},
	})
	bus.MarkLayerReady([]string{"config"})
	bus.MarkKernelComplete()

	if len(launched) != 0 {
		t.Fatalf("★ auto_start=false 的插件被自动拉起了（应等显式启用）: %v", launched)
	}
	if len(sch.Pending()) != 1 {
		t.Fatalf("它应一直挂起等显式启用: %v", sch.Pending())
	}
}

// ★★★ 判据：**就绪顺序可复现**（层内并发不得导致就绪顺序抖动）。
//
// 为何重要：syncbus 的就绪顺序决定哪些插件何时被拉起；
// 若顺序随机，同一份声明两次启动会拉出不同结果 ⇒ 不可审计、不可复现。
func TestSyncBusReadyOrderIsDeterministic(t *testing.T) {
	// 模拟"层内并发完成"：同一层 8 个设施，用 goroutine 乱序 MarkReady
	var first []string
	for round := 0; round < 5; round++ {
		bus := NewSyncBus()
		var wg sync.WaitGroup
		fac := []string{"f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8"}
		for i, f := range fac {
			wg.Add(1)
			go func(i int, f string) {
				defer wg.Done()
				// 反过来启动，制造乱序完成
				_ = i
				bus.MarkReady(f)
			}(i, f)
		}
		wg.Wait()
		got := bus.ReadyInOrder()
		if round == 0 {
			first = got
			continue
		}
		// 注意：MarkReady（单设施）的顺序本就取决于并发，不保证可复现；
		// 设计保证的是 **MarkLayerReady**（层边界一次性写入）可复现。
		// 所以这里只断言"同样的输入产生同样的集合"。
		if len(got) != len(first) {
			t.Fatalf("就绪集合大小抖动: %d vs %d", len(got), len(first))
		}
	}

	// ★ 真正的可复现性断言：MarkLayerReady 一次性写入
	var order []string
	for round := 0; round < 3; round++ {
		bus := NewSyncBus()
		bus.MarkLayerReady([]string{"a", "b", "c"})
		bus.MarkLayerReady([]string{"d", "e"})
		got := bus.ReadyInOrder()
		joined := strings.Join(got, ",")
		if round == 0 {
			order = got
			continue
		}
		if joined != strings.Join(order, ",") {
			t.Fatalf("★ MarkLayerReady 的就绪顺序不可复现:\n%v\n%v", order, got)
		}
	}
	if strings.Join(order, ",") != "a,b,c,d,e" {
		t.Fatalf("就绪顺序应为层内声明序: %v", order)
	}

	// ★ 幂等性：**重复标记**同一设施不得重复记录顺序。
	//
	// 为何要单独断言：Run 的层边界回调可能在某些路径重入
	//（且调用方也可能手动重复标记）。若不幂等，order 会不断膨胀
	// ⇒ 就绪顺序不再反映真实层序 ⇒ 依赖它的调度判断变得不可解释。
	//
	// 这条是补的：变异"去掉幂等检查"第一次没被咬住 —— 因为原判据
	// 只跑了一份声明、没重复标记，测不到幂等性。
	bus := NewSyncBus()
	bus.MarkLayerReady([]string{"a", "b"})
	bus.MarkLayerReady([]string{"a", "b"}) // 重复
	bus.MarkLayerReady([]string{"c"})
	if got := strings.Join(bus.ReadyInOrder(), ","); got != "a,b,c" {
		t.Fatalf("★ 重复标记导致就绪顺序膨胀（不幂等）: %v（期望 a,b,c）", got)
	}
}

// ★★★ 判据：**内核未完成时绝不发 KernelComplete**（内核没装好就不该放插件）。
//
// 这条守的是：装配中途失败 ⇒ 未声明依赖的插件**不**被拉起。
func TestRunDoesNotMarkCompleteOnFailure(t *testing.T) {
	bus := NewSyncBus()
	mods := []Module{
		&fakeModule{name: "A", provides: []string{"a"}},
		&fakeModule{name: "B", provides: []string{"b"}, initErr: errFake},
	}
	plan, err := Build(mods)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(plan, RunOptions{Bus: bus}); err == nil {
		t.Fatal("应失败")
	}
	if bus.KernelComplete() {
		t.Fatal("★ 装配失败了却标记内核完成 —— 未声明依赖的插件会被拉起（病 A）")
	}
}

// ★★★ 判据：**Run 在层边界自动发布本层设施**（不用模块自己标记）。
//
// 为何由 Run 做：模块自己标记会漏（静默漏一个设施 ⇒ 插件永远等不到），
// 且顺序取决于层内并发完成序。
func TestRunPublishesFacilitiesAtLayerBoundary(t *testing.T) {
	bus := NewSyncBus()
	mods := []Module{
		&fakeModule{name: "config", provides: []string{"config", "config-write"}},
		&fakeModule{name: "llm", provides: []string{"llm"}, requires: []string{"config"}},
	}
	plan, err := Build(mods)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(plan, RunOptions{Bus: bus}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"config", "config-write", "llm"} {
		if !bus.IsReady(f) {
			t.Errorf("★ 设施 %q 未在层边界被发布", f)
		}
	}
	if !bus.KernelComplete() {
		t.Error("全部完成后应标记内核完成")
	}
}

// ★★★ 判据：**空依赖列表 ≠ 已满足**（语义区分的直接断言）。
//
// 若这条反了，未声明依赖的插件会被提前拉起（见第一条判据）。
func TestAllReadyEmptyIsFalse(t *testing.T) {
	bus := NewSyncBus()
	bus.MarkKernelComplete()
	if bus.AllReady(nil) {
		t.Fatal("★ AllReady(空) 返回 true —— \"没有依赖\"被当成\"依赖全就绪\"，未声明插件会提前拉起")
	}
	if bus.AllReady([]string{}) {
		t.Fatal("★ AllReady(空切片) 返回 true")
	}
	// 非空且齐 ⇒ true
	bus.MarkReady("x")
	if !bus.AllReady([]string{"x"}) {
		t.Fatal("依赖确实就绪应返回 true")
	}
}

// ★★★ 判据：**拉起失败不阻塞其他插件**（一个坏插件不该拖垮全局）。
func TestSchedulerLaunchFailureDoesNotBlockOthers(t *testing.T) {
	bus := NewSyncBus()
	var okLaunched []string
	sch := NewScheduler(bus, func(p PluginSpec) error {
		if p.Name == "bad" {
			return errFake
		}
		okLaunched = append(okLaunched, p.Name)
		return nil
	})
	var failures int
	sch.SetLaunchErrorHandler(func(PluginSpec, error) { failures++ })

	bus.MarkDeclared([]string{"config"})
	sch.Submit([]PluginSpec{
		{Name: "bad", RequiresFacilities: []string{"config"}, AutoStart: true},
		{Name: "good", RequiresFacilities: []string{"config"}, AutoStart: true},
	})
	bus.MarkLayerReady([]string{"config"})

	if len(okLaunched) != 1 || okLaunched[0] != "good" {
		t.Fatalf("★ 一个插件拉起失败影响了其他插件: %v", okLaunched)
	}
	if failures != 1 {
		t.Fatalf("失败应被报告: %d", failures)
	}
}

// ★★★ 判据：**重复 Submit 不重复拉起**（幂等）。
func TestSchedulerIsIdempotent(t *testing.T) {
	bus := NewSyncBus()
	n := 0
	sch := NewScheduler(bus, func(PluginSpec) error { n++; return nil })
	bus.MarkKernelComplete()

	sch.Submit([]PluginSpec{{Name: "p", AutoStart: true}})
	sch.Submit([]PluginSpec{{Name: "p", AutoStart: true}})
	bus.MarkKernelComplete() // 再触发一次评估

	if n != 1 {
		t.Fatalf("★ 同一插件被拉起 %d 次（应幂等）", n)
	}
}

// ★★★ 判据：**MarkReady 的通知也在锁外**（走单设施路径的订阅者不死锁）。
//
// 变异 8b 暴露的边界：MarkLayerReady 持锁回调会死锁（已被
// TestSchedulerDeclaredLaunchesEarly 咬住），但 MarkReady 是**另一条路径**
// （单个设施的标记），若它持锁回调，只有"既调 MarkReady 又订阅"的场景才会卡住。
// 补这条判据把两条路径都盖住。
func TestSyncBusMarkReadyNotifiesWithoutDeadlock(t *testing.T) {
	bus := NewSyncBus()
	got := make(chan string, 4)

	// 订阅者**读状态**（真实回调必然会读，这正是死锁触发条件）
	bus.OnUpdate(func(s SyncState) {
		_ = s.Snapshot() // 持锁调用的话，这里会等 RLock ⇒ 死锁
		got <- "ok"
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		bus.MarkReady("config")
	}()

	select {
	case <-got:
	case <-done:
		t.Fatal("★ MarkReady 未通知订阅者（订阅者拿不到更新 ⇒ 插件不会自动拉起）")
	case <-timeAfter(5):
		t.Fatal("★ MarkReady 持锁调用回调 ⇒ 死锁（回调读状态时会等锁）")
	}
}

func timeAfter(sec int) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		time.Sleep(time.Duration(sec) * time.Second)
		close(ch)
	}()
	return ch
}
