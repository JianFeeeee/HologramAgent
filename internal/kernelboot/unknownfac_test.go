package kernelboot

// "插件拼错设施名 ⇒ 静默永不启动" 的判据（2026-10-16）。
//
// ## 修的缺口
//
// 插件声明 `requires_facilities` 是**字符串**。拼错时原行为：
// 永远等不到就绪 ⇒ **永不拉起**，且**无任何报错**、无超时兜底。
// 这正是本仓反复吃亏的"静默降级"形态。
//
// 修法：Submit 时校验"声明的设施 ⊆ 内核注册过的"，不符即报错并列出可选值。

import (
	"fmt"
	"strings"
	"testing"
)

// ★★★ 判据：**拼错的设施名在 Submit 时被拒绝**（不再静默挂起）。
func TestSchedulerRejectsUnknownFacility(t *testing.T) {
	bus := NewSyncBus()
	bus.MarkDeclared([]string{"config", "llm", "memory"}) // 内核注册过这些

	var launched []string
	var reported []string
	sch := NewScheduler(bus, func(p PluginSpec) error {
		launched = append(launched, p.Name)
		return nil
	})
	sch.SetLaunchErrorHandler(func(p PluginSpec, err error) {
		reported = append(reported, err.Error())
	})

	// 拼错：写 "llmservice" 而不是 "llm"
	sch.Submit([]PluginSpec{{Name: "typo", RequiresFacilities: []string{"llmservice"}, AutoStart: true}})
	bus.MarkKernelComplete()

	if len(launched) != 0 {
		t.Fatalf("★ 拼错设施名的插件被拉起了: %v", launched)
	}
	if len(reported) == 0 {
		t.Fatal("★ 拼错设施名却**没有任何报错** —— 插件会静默永不启动（这正是要修的缺口）")
	}
	msg := reported[0]
	if !strings.Contains(msg, "llmservice") {
		t.Errorf("错误应点出**是哪个名字**错了: %s", msg)
	}
	// 必须列出内核已知的设施名（"拼错了"当场可修）
	if !strings.Contains(msg, "llm") || !strings.Contains(msg, "config") {
		t.Errorf("错误应列出内核已知的设施名供对照: %s", msg)
	}
	// 不得把它留在挂起队列（否则会永远占位）
	if p := sch.Pending(); len(p) != 0 {
		t.Errorf("被拒的插件不该留在挂起队列: %v", p)
	}
}

// ★★★ 判据：**正确的设施名不受影响**（不能把正常路径也拦了）。
func TestSchedulerAcceptsKnownFacility(t *testing.T) {
	bus := NewSyncBus()
	bus.MarkDeclared([]string{"config", "llm"})
	// ★ 必须同时 MarkLayerReady —— 真实路径里 Run 两者都会做：
	// MarkDeclared（执行前，登记名字）与 MarkLayerReady（每层完成，标记就绪）。
	// 只声明不就绪 ⇒ 插件会一直挂起（那是"未就绪"而非"拼错"，两者不同）。
	bus.MarkLayerReady([]string{"config", "llm"})

	var launched []string
	sch := NewScheduler(bus, func(p PluginSpec) error {
		launched = append(launched, p.Name)
		return nil
	})
	sch.SetLaunchErrorHandler(func(p PluginSpec, err error) {
		t.Errorf("不应报错: %v", err)
	})

	sch.Submit([]PluginSpec{{Name: "ok", RequiresFacilities: []string{"config", "llm"}, AutoStart: true}})
	bus.MarkKernelComplete()

	if len(launched) != 1 || launched[0] != "ok" {
		t.Fatalf("声明正确的插件应被拉起: %v", launched)
	}
}

// ★★★ 判据：**"注册过但未就绪"是合法中间状态**（不能误判为拼错）。
//
// ★ 这条很重要：llm 声明了但还在第 1 层时，Known 为 true、Ready 为 false。
// 若把"未就绪"当"拼错"，所有多依赖插件都会在提交时被误拒。
func TestSchedulerAcceptsDeclaredButNotYetReady(t *testing.T) {
	bus := NewSyncBus()
	bus.MarkDeclared([]string{"config", "llm"}) // 两者都已声明
	bus.MarkLayerReady([]string{"config"})      // 只有 config 就绪

	var reported []string
	sch := NewScheduler(bus, nil)
	sch.SetLaunchErrorHandler(func(p PluginSpec, err error) { reported = append(reported, err.Error()) })

	sch.Submit([]PluginSpec{{Name: "needsllm", RequiresFacilities: []string{"llm"}, AutoStart: true}})
	if len(reported) != 0 {
		t.Fatalf("★ '已声明但未就绪'被误判为拼错: %v", reported)
	}
	if p := sch.Pending(); len(p) != 1 {
		t.Fatalf("应挂起等 llm 就绪: %v", p)
	}
	// llm 就绪后应被拉起
	bus.MarkLayerReady([]string{"llm"})
}

// ★★★ 判据：**Run 执行前登记全部模块设施**（装配中途失败也能校验）。
//
// ★ 为何需要：插件提交发生在装配之后。若某层失败、后面的模块没跑，
// 那些设施名就从未被 MarkLayerReady 登记 ⇒ 会**误报**"内核从未注册过"。
// 所以 Run 必须**执行前**就 MarkDeclared。
func TestRunDeclaresAllFacilitiesUpfront(t *testing.T) {
	bus := NewSyncBus()
	mods := []Module{
		&fakeModule{name: "a", provides: []string{"fa"}},
		&fakeModule{name: "b", provides: []string{"fb"}, requires: []string{"fa"},
			initErr: fmt.Errorf("故意失败")},
		&fakeModule{name: "c", provides: []string{"fc"}, requires: []string{"fb"}},
	}
	plan, err := Build(mods)
	if err != nil {
		t.Fatal(err)
	}
	// 这个 Run 会**失败**（b 的 Init 报错）⇒ c 那一层不会跑
	if _, err := Run(plan, RunOptions{Bus: bus}); err == nil {
		t.Fatal("前置：应失败")
	}
	// ★ 但全部设施名都应已登记（因为执行前就 MarkDeclared 了）
	for _, f := range []string{"fa", "fb", "fc"} {
		if !bus.Known(f) {
			t.Errorf("★ 设施 %q 未被登记 —— 装配中途失败后，插件提交会误报\"内核从未注册过\"", f)
		}
	}
	// 而未跑到的层不该是"就绪"
	if bus.IsReady("fc") {
		t.Error("未执行的模块不该标记就绪")
	}
}
