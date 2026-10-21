package core

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 阶段 2d：批次调度与保序。
//
// 三条规则：
//  1. 全批 ParallelSafe ⇒ 并发；否则**整批**降级串行（不做部分并发——
//     收益不抵不可预测性）。
//  2. 同一 output_send__<通道> 的多次发送**保序**（用户可见消息顺序敏感）。
//  3. 并发时每个工具只写自己的 per-tool ctx（这同时闭合 2c 的判据缺口）。

// slowDevice 是一个"可并发"的测试设备：每个 Execute 阻塞到被显式放行，
// 用来观察多个工具是否**同时**在执行中。
type slowDevice struct {
	name      string
	toolNames []string
	safe      []string // 声明为 ParallelSafe 的工具名
	entered   *int32
	active    *int32
	maxActive *int32
	release   chan struct{}
	delay     time.Duration
}

func (d *slowDevice) Name() string             { return d.name }
func (d *slowDevice) Type() agentIO.DeviceType { return agentIO.DeviceOutput }
func (d *slowDevice) Description() string      { return "slow test device" }
func (d *slowDevice) Tools() []agentIO.ToolDef {
	safe := map[string]bool{}
	for _, n := range d.safe {
		safe[n] = true
	}
	out := make([]agentIO.ToolDef, 0, len(d.toolNames))
	for _, n := range d.toolNames {
		out = append(out, agentIO.ToolDef{Name: n, ParallelSafe: safe[n]})
	}
	return out
}

func (d *slowDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	atomic.AddInt32(d.entered, 1)
	cur := atomic.AddInt32(d.active, 1)
	// 记录并发峰值
	for {
		old := atomic.LoadInt32(d.maxActive)
		if cur <= old || atomic.CompareAndSwapInt32(d.maxActive, old, cur) {
			break
		}
	}
	if d.release != nil {
		<-d.release // 阻塞，直到测试放行
	} else if d.delay > 0 {
		time.Sleep(d.delay)
	}
	atomic.AddInt32(d.active, -1)
	return "ran:" + tool, nil
}
func (d *slowDevice) Start() error                                 { return nil }
func (d *slowDevice) Stop() error                                  { return nil }
func (d *slowDevice) OutputCapabilities() agentIO.OutputCapability { return agentIO.CapText }
func (d *slowDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{} }

// ① 并发：全批 ParallelSafe ⇒ 同一时刻有多个工具在执行。
func TestBatchParallelWhenAllToolsParallelSafe(t *testing.T) {
	var entered, active, maxActive int32
	release := make(chan struct{})
	names := []string{"p_a", "p_b", "p_c"}

	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{
			{ID: "c1", Name: "p_a", Arguments: map[string]interface{}{}},
			{ID: "c2", Name: "p_b", Arguments: map[string]interface{}{}},
			{ID: "c3", Name: "p_c", Arguments: map[string]interface{}{}},
		}},
		{Content: "final"},
	}}
	a := newParallelAgent(t, sp, names, names, &entered, &active, &maxActive, release)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if out := a.runTaskSteps(a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))); out != outcomeDone {
			t.Errorf("runTaskSteps=%v", out)
		}
	}()

	// 等到**至少两个**同时进入执行；若串行则永远只有一个，会超时。
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&maxActive) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	<-done

	if got := atomic.LoadInt32(&maxActive); got < 2 {
		t.Errorf("全批 ParallelSafe 却未并发（并发峰值=%d，应 >=2）", got)
	}
	if entered != int32(len(names)) {
		t.Errorf("应执行 %d 个工具，实际 %d", len(names), entered)
	}
}

// ② 整批降级：只要有一个**非** ParallelSafe ⇒ 整批串行（不做部分并发）。
func TestBatchFallsBackToSerialIfAnyToolNotParallelSafe(t *testing.T) {
	var entered, active, maxActive int32
	release := make(chan struct{})
	names := []string{"s_a", "s_b", "s_c"}

	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{
			{ID: "c1", Name: "s_a", Arguments: map[string]interface{}{}},
			{ID: "c2", Name: "s_b", Arguments: map[string]interface{}{}},
			{ID: "c3", Name: "s_c", Arguments: map[string]interface{}{}},
		}},
		{Content: "final"},
	}}
	// 只声明前两个可并发 —— 第三个不声明 ⇒ 整批必须串行
	a := newParallelAgent(t, sp, names, names[:2], &entered, &active, &maxActive, release)

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.runTaskSteps(a.newTaskFrame("go", a.stageCtxFromInput("go", "", "")))
	}()
	// 给串行留出充分时间：让第一个工具走完并进入第二个
	time.Sleep(150 * time.Millisecond)
	observed := atomic.LoadInt32(&maxActive)
	close(release)
	<-done

	if observed > 1 {
		t.Errorf("存在非 ParallelSafe 工具时不应部分并发（并发峰值=%d）", observed)
	}
	if atomic.LoadInt32(&entered) != int32(len(names)) {
		t.Errorf("整批仍应全部执行，实际 %d", entered)
	}
}

// ③ 保序：同一 output_send__<通道> 的多次发送**必须**按声明顺序到达。
//
// 这是用户可见的语义：同一条通道连发 3 条消息，顺序颠倒用户就读错了。
func TestBatchSameOutputChannelKeepsOrder(t *testing.T) {
	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{
			{ID: "c1", Name: "output_send__testch", Arguments: map[string]interface{}{"payload": "first"}},
			{ID: "c2", Name: "output_send__testch", Arguments: map[string]interface{}{"payload": "second"}},
			{ID: "c3", Name: "output_send__testch", Arguments: map[string]interface{}{"payload": "third"}},
		}},
		{Content: "final"},
	}}
	a := newPreemptAgent(t, sp)

	var mu sync.Mutex
	var got []string
	dev := &recordingDevice{name: "testch", caps: agentIO.CapText,
		tools: []agentIO.ToolDef{{Name: "output_send__testch"}}}
	dev.onExec = func(payload string) {
		mu.Lock()
		got = append(got, payload)
		mu.Unlock()
		// 每条之间加延迟：若并发执行，顺序会被打乱
		time.Sleep(20 * time.Millisecond)
	}
	if err := a.io.RegisterDevice(dev); err != nil {
		t.Fatalf("注册设备失败: %v", err)
	}

	if out := a.runTaskSteps(a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))); out != outcomeDone {
		t.Fatalf("runTaskSteps=%v", out)
	}
	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("应发送 %d 条，实际 %d（%v）", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条应是 %q，实际 %q —— 同通道发送未保序（完整 %v）", i, want[i], got[i], got)
		}
	}
}

// recordingDevice 记录 output 工具的 payload 顺序。
type recordingDevice struct {
	name   string
	caps   agentIO.OutputCapability
	tools  []agentIO.ToolDef
	onExec func(payload string)
}

func (d *recordingDevice) Name() string             { return d.name }
func (d *recordingDevice) Type() agentIO.DeviceType { return agentIO.DeviceOutput }
func (d *recordingDevice) Description() string      { return "recording test device" }
func (d *recordingDevice) Tools() []agentIO.ToolDef { return d.tools }
func (d *recordingDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	if d.onExec != nil {
		p, _ := args["payload"].(string)
		d.onExec(p)
	}
	return map[string]interface{}{"status": "sent"}, nil
}
func (d *recordingDevice) Start() error                                 { return nil }
func (d *recordingDevice) Stop() error                                  { return nil }
func (d *recordingDevice) OutputCapabilities() agentIO.OutputCapability { return d.caps }
func (d *recordingDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{} }

// newParallelAgent 建一个带 slowDevice 的 agent。
func newParallelAgent(t *testing.T, sp agentAPI.Provider, names, safe []string,
	entered, active, maxActive *int32, release chan struct{}) *Agent {
	t.Helper()
	a := New(AgentConfig{
		ID:              "paragent",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	if err := a.io.RegisterDevice(&slowDevice{
		name: "slowdev", toolNames: names, safe: safe,
		entered: entered, active: active, maxActive: maxActive, release: release,
	}); err != nil {
		t.Fatalf("注册设备失败: %v", err)
	}
	return a
}

// 阶段 2c 判据的**并发版**（闭合此前"串行下测不出差别"的缺口）。
//
// 此前两条 2c 判据在串行路径下无法区分「per-tool ctx」与「单槽」——
// 变体验证（toolCtxFor 退回单槽）后仍然全绿。差别只在并发下显现。
// 本判据在**真实并发批次**下断言：
//
//	· 每个工具的 before_toolcall ctx 只带自己的 ToolCalls[0].Name
//	· after_toolcall 读到的 Result 属于当前工具，不是批内另一个的
//	· 全程 -race 无数据竞争
func TestBatchConcurrentEachToolSeesOwnContext(t *testing.T) {
	names := []string{"k_a", "k_b", "k_c", "k_d"}
	var entered, active, maxActive int32
	release := make(chan struct{})

	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{
			{ID: "c1", Name: names[0], Arguments: map[string]interface{}{}},
			{ID: "c2", Name: names[1], Arguments: map[string]interface{}{}},
			{ID: "c3", Name: names[2], Arguments: map[string]interface{}{}},
			{ID: "c4", Name: names[3], Arguments: map[string]interface{}{}},
		}},
		{Content: "final"},
	}}
	a := newParallelAgent(t, sp, names, names, &entered, &active, &maxActive, release)

	var mu sync.Mutex
	beforeNames := map[string]string{}
	crossTalk := map[string]string{}

	a.stageHost.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error {
		if len(ctx.ToolCalls) != 1 {
			t.Errorf("并发下 before_toolcall 的 ctx 应只带 1 个 ToolCall，实际 %d", len(ctx.ToolCalls))
		}
		name := ""
		if len(ctx.ToolCalls) > 0 {
			name = ctx.ToolCalls[0].Name
		}
		mu.Lock()
		if prev, dup := beforeNames[name]; dup {
			crossTalk["dup:"+name] = "与 " + prev + " 撞名"
		}
		beforeNames[name] = name
		mu.Unlock()
		return nil
	})
	a.stageHost.RegisterStage(sdk.StageAfterToolcall, func(ctx *sdk.StageContext) error {
		if len(ctx.ToolResults) == 0 {
			return nil
		}
		name := ctx.ToolResults[0].Name
		res := fmt.Sprint(ctx.ToolResults[0].Result)
		// 结果必须含**自己**的名字（"ran:k_a"），否则读到的是别人的
		if !strings.Contains(res, name) {
			mu.Lock()
			crossTalk["after:"+name] = res
			mu.Unlock()
		}
		return nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		if out := a.runTaskSteps(a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))); out != outcomeDone {
			t.Errorf("runTaskSteps=%v", out)
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&maxActive) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	<-done

	if len(crossTalk) > 0 {
		t.Errorf("并发下 StageContext 串味: %v", crossTalk)
	}
	if len(beforeNames) != len(names) {
		t.Errorf("每个工具应各看到自己的 ctx，实际看到 %v（期望 %v）", beforeNames, names)
	}
}
