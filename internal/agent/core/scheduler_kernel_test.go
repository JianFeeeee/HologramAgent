package core

// L4 的内核独占性 + 两类别抢占规则。
//
// 模型（用户明确）：
//   - 类别由**用哪个注入 API** 决定，与通道名无关；
//   - L1..L3 由插件在 InjectOptions.Priority 声明；
//   - L4 只有内核持有（panic / 内核事件 selfip）；
//   - 排队输入无级别，可被**任何**中断打断。

import (
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 非内核级来源声明 L4 必须被夹到 L3；内核级来源（内置插件）可用到 L4。
func TestKernel_L4RequiresKernelLevelSource(t *testing.T) {
	if got := clampPluginLevel(LevelCritical); got != LevelInteractive {
		t.Fatalf("非特权声明 L4 应被夹到 L3，实际 %v", got)
	}
	cases := []struct {
		declared string
		want     Level
	}{
		{"L1", LevelBackground},
		{"L2", LevelMessage},
		{"L3", LevelInteractive},
		{"l2", LevelMessage},
		{"L4", LevelInteractive}, // 非特权 → 夹到 L3
		{"L7", DefaultLevel},     // 未知 → 默认级
		{"", DefaultLevel},       // 未声明 → 默认级
		{"紧急", DefaultLevel},     // 拼写错误 → 默认级（不得被静默当成别的级别）
	}
	for _, c := range cases {
		evt := &agentIO.InputEvent{Payload: map[string]interface{}{}}
		if c.declared != "" {
			evt.Payload["priority"] = c.declared
		}
		if got := interruptLevel(evt, false); got != c.want {
			t.Fatalf("非特权声明 %q → 级别 %v，期望 %v", c.declared, got, c.want)
		}
	}
	if got := interruptLevel(nil, false); got != DefaultLevel {
		t.Fatalf("无事件应为默认级，实际 %v", got)
	}

	// 特权（内核级插件）：L4 被承认，其余待遇不变。
	for _, c := range []struct {
		declared string
		want     Level
	}{
		{"L4", LevelCritical},
		{"L3", LevelInteractive},
		{"L1", LevelBackground},
		{"", DefaultLevel},
		{"L9", DefaultLevel},
	} {
		evt := &agentIO.InputEvent{Payload: map[string]interface{}{}}
		if c.declared != "" {
			evt.Payload["priority"] = c.declared
		}
		if got := interruptLevel(evt, true); got != c.want {
			t.Fatalf("特权声明 %q → 级别 %v，期望 %v", c.declared, got, c.want)
		}
	}
}

// 内核级 = 插件注册表里的**内置工厂**（编译期自注册），与插件自报名无关；
// source 约定 `插件名` 或 `插件名/实例`（如 webui/<deviceID>）。
func TestKernel_KernelLevelSource(t *testing.T) {
	plugin.RegisterFactory("core_test_builtin", func(string, map[string]interface{}) (sdk.Plugin, error) {
		return nil, nil
	})
	a := &Agent{pluginReg: plugin.NewRegistry()}

	cases := []struct {
		source string
		want   bool
	}{
		{"core_test_builtin", true},
		{"core_test_builtin/dev-1", true}, // 插件名/实例
		{"core_test_external", false},
		{"webui", false}, // 本测试注册表里没有 webui 工厂
		{"", false},
		{"core_test_builtinX", false}, // 不做前缀匹配
	}
	for _, c := range cases {
		if got := a.isKernelLevelSource(c.source); got != c.want {
			t.Fatalf("source=%q → %v，期望 %v", c.source, got, c.want)
		}
	}
	if (&Agent{}).isKernelLevelSource("core_test_builtin") {
		t.Fatal("没有插件注册表时不得授予内核级")
	}
}

// 排队任务可被**任何**中断打断——包括最低的 L1。
func TestKernel_QueuedTaskIsPreemptedByAnyInterrupt(t *testing.T) {
	s := newScheduler(8)
	q := newInputTask(&agentIO.InputEvent{Source: "plugin", OutputChannel: "plugin"})
	s.enqueue(q)
	if task, _, kind := s.nextRef(); task != q || kind != nextReady {
		t.Fatalf("应取到排队任务，kind=%v", kind)
	}

	evt, _ := textEvent("qq", "最低级中断")
	if !s.registerInterrupt(newInterruptTask(evt, LevelBackground)) {
		t.Fatal("L1 中断也必须能打断排队任务（排队任务无级别）")
	}
	if s.immediate == nil || s.immediate.Level != LevelBackground {
		t.Fatalf("抢占者应进 immediate 槽，实际 %+v", s.immediate)
	}
}

// 排队输入从不抢占——它没有级别，也就没有“比谁高”。
func TestKernel_QueuedInputNeverPreempts(t *testing.T) {
	s := newScheduler(8)
	if !s.enqueue(newInputTask(&agentIO.InputEvent{Source: "a", OutputChannel: "a"})) {
		t.Fatal("入队失败")
	}
	s.nextRef() // running = 第一个排队任务
	if s.registerInterrupt(newInputTask(&agentIO.InputEvent{Source: "b", OutputChannel: "b"})) {
		t.Fatal("排队输入不得抢占任何任务")
	}
}

// 内核 L4 入口不受夹取影响，且能抢占中断（L3）。
func TestKernel_RequestKernelPreemptUsesL4(t *testing.T) {
	s := newScheduler(8)
	evt, _ := textEvent("cli", "L3 运行中")
	s.registerInterrupt(newInterruptTask(evt, LevelInteractive))
	s.nextRef() // running = L3 中断

	kevt, _ := textEvent("kernel", "panic 中断")
	if !s.requestKernelPreempt(kevt) {
		t.Fatal("内核 L4 应能抢占 L3 中断")
	}
	if s.immediate == nil || s.immediate.Level != LevelCritical {
		t.Fatalf("内核中断必须是 L4，实际 %+v", s.immediate)
	}
}

// 任务 panic → 内核 L4 中断（panic 是 L4 的来源之一）。
func TestKernel_PanicRaisesL4Interrupt(t *testing.T) {
	sp := &scriptProvider{script: []*agentAPI.CompletionResponse{{Content: "已收到内核事件"}}}
	a := newPreemptAgent(t, sp)

	// Event 为 nil：handleInput 解引用即 panic。
	bad := newInputTask(nil)
	if !a.sched.enqueue(bad) {
		t.Fatal("入队失败")
	}
	task, _, _ := a.sched.nextRef()
	a.executeTask(task) // panic 被隔离

	snap := a.DumpScheduler()
	if snap.Immediate == nil {
		t.Fatal("panic 必须产生一条内核 L4 中断")
	}
	if snap.Immediate.Level != LevelCritical {
		t.Fatalf("panic 中断级别=%v，期望 L4", snap.Immediate.Level)
	}
	text, _ := snap.Immediate.Event.Payload["content"].(string)
	if !strings.Contains(text, "panic") {
		t.Fatalf("panic 中断应说明发生了什么，实际 %q", text)
	}
	if snap.Immediate.Event.Payload["kernel"] != true {
		t.Fatal("内核中断必须带 kernel 标记，便于与插件中断区分")
	}
}

// 递归保护是结构性的：L4 内核中断自己 panic 时，不再产生新的 L4。
func TestKernel_PanicInsideL4DoesNotRecurse(t *testing.T) {
	sp := &scriptProvider{}
	a := newPreemptAgent(t, sp)

	evt, _ := textEvent("kernel", "内核事件")
	l4 := newKernelInterruptTask(evt)
	a.sched.immediate = l4
	task, _, _ := a.sched.nextRef()
	if task != l4 {
		t.Fatal("应取到 L4 内核中断")
	}
	a.executeTask(&Task{ID: task.ID, Class: TaskInterrupt, Level: LevelCritical, Kind: TaskKindInput, Event: nil})

	snap := a.DumpScheduler()
	if snap.Immediate != nil || len(snap.PendingInterrupts) != 0 {
		t.Fatalf("L4 自身 panic 不得再产生中断（否则自我放大），实际 immediate=%+v pending=%d",
			snap.Immediate, len(snap.PendingInterrupts))
	}
}

// 中断栈的 4 帧上界是**结构推论**：排队(L0) ← I(L1) ← I(L2) ← I(L3) ← I(L4 运行中)。
func TestKernel_StackBoundIsFullChain(t *testing.T) {
	s := newScheduler(16)
	frame := func() *TaskFrame { return &TaskFrame{} }
	chain := []struct {
		class TaskClass
		lv    Level
	}{
		{TaskQueued, 0},
		{TaskInterrupt, LevelBackground},
		{TaskInterrupt, LevelMessage},
		{TaskInterrupt, LevelInteractive},
	}
	for i := 0; i < len(chain)-1; i++ {
		s.suspend(&Task{ID: uint64(i + 1), Class: chain[i].class, Level: chain[i].lv}, frame())
	}
	if !s.canSuspend() {
		t.Fatal("3 帧挂起时仍应容得下 L3（第 4 级）继续下潜")
	}
	s.suspend(&Task{ID: 4, Class: TaskInterrupt, Level: LevelInteractive}, frame())
	if s.canSuspend() {
		t.Fatal("4 帧挂起 = 全链挂起（L4 运行中），不应再有下潜余量")
	}
}

// 端到端：插件声明 Priority → io.applyInjectOpts → payload → interruptLevel → 任务级别。
// 这条链路断在任何一环，插件声明的级别都会静默失效（降级到 L1）。
func TestKernel_PriorityFlowsThroughIOLayer(t *testing.T) {
	ioM := agentIO.NewIOManager()
	ioM.InjectInterruptTextOpts("qq", "cli", "通知", agentIO.InjectOptions{Priority: "L3"})

	select {
	case evt := <-ioM.InputInterruptChan():
		if got := interruptLevel(evt, false); got != LevelInteractive {
			t.Fatalf("经 io 层后的级别=%v，期望 L3（payload=%v）", got, evt.Payload)
		}
		task := newInterruptTask(evt, interruptLevel(evt, false))
		if task.Class != TaskInterrupt || task.Level != LevelInteractive {
			t.Fatalf("中断任务类别/级别=%v/%v，期望 interrupt/L3", task.Class, task.Level)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("中断未到达 interruptCh")
	}

	// 排队路径带 priority 也必须无效：排队输入没有级别。
	ioM.InjectTextOpts("qq", "cli", "普通输入", agentIO.InjectOptions{Priority: "L3"})
	select {
	case evt := <-ioM.InputChan():
		task := newInputTask(evt)
		if task.Class != TaskQueued || task.Level != 0 {
			t.Fatalf("排队任务类别/级别=%v/%v，期望 queued/无级别", task.Class, task.Level)
		}
		if effectiveLevel(task) != 0 {
			t.Fatalf("排队任务有效级=%v，期望 0", effectiveLevel(task))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("排队输入未到达 inputCh")
	}
}

// 内核级插件声明的 L4 必须一路到达调度器（“立即打断”能力，如 WebUI 终止按钮）。
func TestKernel_KernelLevelPluginCanRaiseL4(t *testing.T) {
	plugin.RegisterFactory("core_test_l4", func(string, map[string]interface{}) (sdk.Plugin, error) {
		return nil, nil
	})
	a := newPreemptAgent(t, &scriptProvider{})
	a.pluginReg = plugin.NewRegistry()

	// 先让一个排队任务跑起来（无级别），才能看到“抢占”。
	evt, _ := textEvent("qq", "长任务")
	if !a.sched.enqueue(newInputTask(evt)) {
		t.Fatal("入队失败")
	}
	a.sched.nextRef()

	// 内核级插件（内置）声明 L4 的终止通知。
	kevt, _ := textEvent("core_test_l4", "用户按了终止按钮")
	kevt.Payload["priority"] = "L4"
	level := interruptLevel(kevt, a.isKernelLevelSource(kevt.Source))
	if level != LevelCritical {
		t.Fatalf("内核级插件声明 L4 应得 L4，实际 %v", level)
	}
	if !a.sched.requestPreempt(kevt, level) {
		t.Fatal("L4 应能打断排队任务")
	}
	if a.sched.immediate == nil || a.sched.immediate.Level != LevelCritical {
		t.Fatalf("应有一条 L4 中断在 immediate，实际 %+v", a.sched.immediate)
	}

	// 反例：同样的声明来自外部插件 → 夹到 L3。
	eevt, _ := textEvent("core_test_external", "外部插件也想立即打断")
	eevt.Payload["priority"] = "L4"
	if got := interruptLevel(eevt, a.isKernelLevelSource(eevt.Source)); got != LevelInteractive {
		t.Fatalf("外部插件声明 L4 应被夹到 L3，实际 %v", got)
	}
}
