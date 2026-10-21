package core

// M4 验收测试：临界区语义显式化 + 抢占延迟到安全点 + 批次不再被中断放弃。
//
// 设计依据 docs/zh/input-scheduler-design.md §4.3（临界区）、§11.1（P5/P6）。
//
// 关键结构事实：让位检查**只在 step 之间**进行，因此任何正在执行的 step
// （工具 RPC、ONNX、CAS 落盘）天然不可抢占——中断只能等它返回。

import (
	"sync"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// P5/P6：工具执行期间到达的高优先级中断不得立即抢占；必须等工具返回后的安全点。
func TestPreempt_DeferredDuringToolExec(t *testing.T) {
	sh := NewStageHost()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	sh.RegisterTool("t_slow", sdk.ToolDef{Name: "t_slow", Plugin: "t"}, func(args map[string]interface{}) (interface{}, error) {
		once.Do(func() { close(entered) })
		<-release
		return "slow-done", nil
	})

	sp := &scriptProvider{script: []*agentAPI.CompletionResponse{
		{Content: "", ToolCalls: []agentAPI.ToolCall{tc("c1", "t_slow")}}, // 低优先级任务调用慢工具
		{Content: "low-done"},  // 恢复后收尾
		{Content: "intr-done"}, // 中断任务
	}}
	a := New(AgentConfig{
		ID:              "crit",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       sh,
	})

	if _, _ = enqueueQueued(t, a, "qq", "低优先级任务"); true {
	}
	lt, _, _ := a.sched.nextRef()
	done := make(chan struct{})
	go func() { a.executeNewTask(lt); close(done) }()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("慢工具未被调用")
	}

	// 工具执行中注入 L4 中断。
	intrEvt, _ := textEvent("cli", "紧急打断")
	intrEvt.Payload["interrupt"] = true
	if !a.sched.requestKernelPreempt(intrEvt) {
		t.Fatal("L4 应 arm 让位信号")
	}
	// 关键断言：信号已 arm，但任务仍在工具里 —— 绝不能挂起。
	if !a.sched.preemptGrantedFor() {
		t.Fatal("让位信号应已 arm")
	}
	if a.DumpScheduler().Running == nil {
		t.Fatal("工具执行中不得挂起（StepToolExec 是临界区）")
	}
	if len(a.DumpScheduler().SuspendStack) != 0 {
		t.Fatal("工具执行中 suspendStack 应为空")
	}

	// 放行工具 → 工具返回后的安全点才挂起。
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("工具返回后未挂起")
	}

	snap := a.DumpScheduler()
	if len(snap.SuspendStack) != 1 {
		t.Fatalf("工具返回后 suspendStack=%d，期望 1", len(snap.SuspendStack))
	}
	if snap.SuspendStack[0].Frame.Step != StepToolAfter {
		t.Fatalf("应在工具执行后的安全点挂起（StepToolAfter），实际 %v", snap.SuspendStack[0].Frame.Step)
	}
	if len(snap.PendingInterrupts) != 1 {
		t.Fatalf("中断请求不得丢失，pendingInterrupts=%d", len(snap.PendingInterrupts))
	}
}

// _consolidation_ 整任务视为不可抢占（它直接改图库）。
func TestCriticalSection_ConsolidationMarked(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())

	// 判定：只有记忆整理通道是整任务临界区。
	if isCriticalChannel("cli") {
		t.Fatal("普通通道不应被判为临界区")
	}
	if !isCriticalChannel(channelConsolidation) {
		t.Fatal("记忆整理必须是不可抢占临界区")
	}

	// 集成：标志的推导链「输入事件 → 通道 → isCriticalChannel → scheduler.critical」
	// 必须成立（N0 之后通道只从事件推导，不再有内核可变字段）。
	for _, c := range []struct {
		channel string
		want    bool
	}{
		{"cli", false},
		{channelConsolidation, true},
	} {
		evt, _ := textEvent("tc", "x")
		evt.OutputChannel = c.channel
		a.sched.setCritical(isCriticalChannel(outputChannelOf(evt)))
		if got := a.sched.inCritical(); got != c.want {
			t.Fatalf("通道 %q → critical=%v，期望 %v", c.channel, got, c.want)
		}
	}
}

// 新语义：没有抢占时，同批的多个工具必须全部执行——不再有「中断放弃剩余批」。
func TestBatch_NotAbandonedWithoutPreemption(t *testing.T) {
	sh := NewStageHost()
	var mu sync.Mutex
	var ran []string
	reg := func(name string) {
		sh.RegisterTool(name, sdk.ToolDef{Name: name, Plugin: "t"}, func(args map[string]interface{}) (interface{}, error) {
			mu.Lock()
			ran = append(ran, name)
			mu.Unlock()
			return name + "-out", nil
		})
	}
	reg("t_a")
	reg("t_b")

	sp := &scriptProvider{script: []*agentAPI.CompletionResponse{
		{Content: "", ToolCalls: []agentAPI.ToolCall{tc("c1", "t_a"), tc("c2", "t_b")}},
		{Content: "全部完成"},
	}}
	a := New(AgentConfig{
		ID:              "batch",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       sh,
	})

	if _, _ = enqueueQueued(t, a, "cli", "跑两个工具"); true {
	}
	tt, _, _ := a.sched.nextRef()
	a.executeNewTask(tt)

	if len(ran) != 2 || ran[0] != "t_a" || ran[1] != "t_b" {
		t.Fatalf("同批工具应全部按序执行，实际 %v", ran)
	}
	snap := a.DumpScheduler()
	if len(snap.SuspendStack) != 0 || len(snap.PendingInterrupts) != 0 {
		t.Fatalf("无抢占时不应有挂起或待处理中断：%+v", snap)
	}
	if snap.Stats.Executed != 1 {
		t.Fatalf("Executed=%d，期望 1", snap.Stats.Executed)
	}
}

// TestSchedulerStatusExposesLevelsAndStack 钉住调度器状态面**按级别可读**：
// 四级队列深度、各级累计计数、立即抢占项、中断栈帧。
//
// 起因：此前 SchedulerStatus 只给 total（pending_interrupts / suspend_stack），
// 看不出"堵在 L1 还是 L4"、"栈里压的是谁"。WebUI 想画运行态就无数据可用。
//
// 这个用例直接摆好调度器内部状态、只验**映射**（同包白盒）：调度行为本身
// 由 scheduler_e2e_test / scheduler_critical_test 覆盖，这里不该重复它们的时序。
func TestSchedulerStatusExposesLevelsAndStack(t *testing.T) {
	a := New(AgentConfig{ID: "rt", ProviderManager: agentAPI.NewProviderManager(), IO: agentIO.NewIOManager()})
	if a.sched == nil {
		t.Fatal("agent 应带调度器")
	}
	a.sched.mu.Lock()
	a.sched.queue = append(a.sched.queue, &Task{ID: 1, Class: TaskQueued, Kind: TaskKindInput})
	a.sched.interruptQueues[LevelCritical] = append(a.sched.interruptQueues[LevelCritical],
		&Task{ID: 2, Class: TaskInterrupt, Level: LevelCritical, Kind: TaskKindInput},
		&Task{ID: 3, Class: TaskInterrupt, Level: LevelCritical, Kind: TaskKindInput})
	a.sched.interruptQueues[LevelBackground] = append(a.sched.interruptQueues[LevelBackground],
		&Task{ID: 4, Class: TaskInterrupt, Level: LevelBackground, Kind: TaskKindSelf})
	a.sched.immediate = &Task{ID: 5, Class: TaskInterrupt, Level: LevelCritical, Kind: TaskKindInput}
	a.sched.suspendStack = append(a.sched.suspendStack, &suspendedTask{
		Task:  &Task{ID: 6, Class: TaskInterrupt, Level: LevelInteractive, Kind: TaskKindInput},
		Frame: &TaskFrame{},
	})
	a.sched.stats.InterruptsByLevel[LevelCritical] = 7
	a.sched.stats.PreemptsByLevel[LevelCritical] = 3
	a.sched.mu.Unlock()

	got := a.schedulerStatus()
	if got.ReadyQueueDepth != 1 {
		t.Fatalf("ready_queue_depth 应为 1，实际 %d", got.ReadyQueueDepth)
	}
	// 按级别的队列深度：下标即级别，下标 0 恒为 0
	if got.InterruptQueues[LevelCritical] != 2 || got.InterruptQueues[LevelBackground] != 1 {
		t.Fatalf("按级别队列深度不对：%v", got.InterruptQueues)
	}
	if got.InterruptQueues[0] != 0 {
		t.Fatalf("下标 0（无级别）必须恒为 0，实际 %v", got.InterruptQueues)
	}
	// pending = 两条队列 + immediate
	if got.PendingInterrupts != 4 {
		t.Fatalf("pending_interrupts 应为 4，实际 %d", got.PendingInterrupts)
	}
	// 立即抢占项要能单独看到
	if got.Immediate == nil || got.Immediate.ID != 5 || got.Immediate.Level != int(LevelCritical) {
		t.Fatalf("immediate 未正确映射：%+v", got.Immediate)
	}
	// 中断栈帧（栈底→栈顶）要带任务标识
	if len(got.SuspendFrames) != 1 || got.SuspendFrames[0].Task.ID != 6 ||
		got.SuspendFrames[0].Task.Level != int(LevelInteractive) {
		t.Fatalf("中断栈帧未正确映射：%+v", got.SuspendFrames)
	}
	// 累计计数按级别透传
	if got.InterruptsByLevel[LevelCritical] != 7 || got.PreemptsByLevel[LevelCritical] != 3 {
		t.Fatalf("各级计数未透传：interrupts=%v preempts=%v", got.InterruptsByLevel, got.PreemptsByLevel)
	}
	// 未使用的级别必须是 0（不能把别的级别串进来）
	if got.InterruptQueues[LevelMessage] != 0 || got.InterruptQueues[LevelInteractive] != 0 {
		t.Fatalf("未使用的级别应为 0：%v", got.InterruptQueues)
	}
}

// TestChannelInfoCarriesTopology 钉住通道状态面不再丢信息：
// 方向、描述、工具名、输出能力都要能被前端画出来。
func TestChannelInfoCarriesTopology(t *testing.T) {
	ch := channelInfoFromIO(agentIO.ChannelInfo{
		Name:        "webui",
		Type:        agentIO.DeviceOutput,
		Description: "Web 控制台",
		Tools:       []agentIO.ToolDef{{Name: "output_send"}},
		OutputCaps:  agentIO.CapText | agentIO.CapImage,
	})
	if ch.Direction != "out" {
		t.Fatalf("方向应为 out，实际 %q", ch.Direction)
	}
	if ch.Description != "Web 控制台" {
		t.Fatalf("描述丢失：%q", ch.Description)
	}
	if len(ch.Tools) != 1 || ch.Tools[0] != "output_send" {
		t.Fatalf("工具名丢失：%v", ch.Tools)
	}
	if ch.OutputCaps != int(agentIO.CapText|agentIO.CapImage) || ch.CapsText == "" {
		t.Fatalf("输出能力丢失：caps=%d text=%q", ch.OutputCaps, ch.CapsText)
	}
	if d := channelInfoFromIO(agentIO.ChannelInfo{Name: "mic", Type: agentIO.DeviceInput}).Direction; d != "in" {
		t.Fatalf("输入通道方向应为 in，实际 %q", d)
	}
	if d := channelInfoFromIO(agentIO.ChannelInfo{Name: "both", Type: agentIO.DeviceIO}).Direction; d != "io" {
		t.Fatalf("双向通道方向应为 io，实际 %q", d)
	}
}
