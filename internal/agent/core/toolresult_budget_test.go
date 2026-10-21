package core

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// 方案 B：工具结果**只统计不裁剪**。
//
// 现状核实：工具结果进 f.Msgs 时**没有任何长度上限**（task.go 直接
// `Content: result`），内核也**不预检**是否超长 —— 超限由上游 API 报错。
// 时间线那一侧有预算（ContextTokens = 0.8×窗口，进消息前就裁过），
// 但那只管 a.context 的历史事件，**不管单条工具结果**。
// ⇒ 一条巨大工具结果可能直接冲破预算，而内核**不会提前发现**。
//
// 本阶段的取舍：**不裁剪用户数据**（截断会让模型拿到残缺信息，且
// 截断位置由内核武断决定），改为**统计 + 告警**，把处置权交回给
// 调度器/上层。这与本仓「显式才是特权」的取向一致。
//
// 判据钉住四件事：会计数、会超阈值、默认**不**裁剪、报告可执行。

// bigToolDevice 返回一个指定大小的工具结果。
type bigToolDevice struct {
	name     string
	toolName string
	size     int
}

func (d *bigToolDevice) Name() string             { return d.name }
func (d *bigToolDevice) Type() agentIO.DeviceType { return agentIO.DeviceOutput }
func (d *bigToolDevice) Description() string      { return "big result test device" }
func (d *bigToolDevice) Tools() []agentIO.ToolDef {
	return []agentIO.ToolDef{{Name: d.toolName}}
}
func (d *bigToolDevice) Execute(string, map[string]interface{}) (interface{}, error) {
	return strings.Repeat("x", d.size), nil
}
func (d *bigToolDevice) Start() error                                 { return nil }
func (d *bigToolDevice) Stop() error                                  { return nil }
func (d *bigToolDevice) OutputCapabilities() agentIO.OutputCapability { return agentIO.CapText }
func (d *bigToolDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{} }

// ① 统计必须真的发生：巨大工具结果要触发一次「超预算」报告。
func TestHugeToolResultIsReported(t *testing.T) {
	// 阈值刻意调小，让 200KB 的结果必然超限（不必真造 1M token）
	a := newPreemptAgent(t, newPreemptProvider())
	a.toolResultWarnTokens = 200 * 1024 // 200KB（EstimateTokens 为 rune×2）

	rep := &countingReporter{}
	a.toolResultReporter = rep

	if err := a.io.RegisterDevice(&bigToolDevice{
		name: "bigdev", toolName: "big_tool", size: 400 * 1024, // 400KB
	}); err != nil {
		t.Fatalf("注册设备失败: %v", err)
	}

	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{{ID: "c1", Name: "big_tool", Arguments: map[string]interface{}{}}}},
		{Content: "final"},
	}}
	a2, _ := newBatchAgent(t, sp)
	a2.toolResultWarnTokens = a.toolResultWarnTokens
	a2.toolResultReporter = rep
	if err := a2.io.RegisterDevice(&bigToolDevice{
		name: "bigdev", toolName: "big_tool", size: 400 * 1024,
	}); err != nil {
		t.Fatalf("注册设备失败: %v", err)
	}

	f := a2.newTaskFrame("go", a2.stageCtxFromInput("go", "", ""))
	if out := a2.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps=%v err=%v", out, f.Err)
	}
	if rep.n == 0 {
		t.Error("400KB 的工具结果未触发任何超限报告 —— 统计没生效")
	}
	if rep.lastTool != "big_tool" {
		t.Errorf("报告应指明是哪个工具，实际 %q", rep.lastTool)
	}
	if rep.lastTokens <= 0 {
		t.Errorf("报告应带上估算 token 数，实际 %d", rep.lastTokens)
	}
}

// ② ★ 方案 B 的核心：**默认不裁剪**。统计归统计，数据必须原样给模型。
func TestHugeToolResultIsNotTruncated(t *testing.T) {
	a, _ := newBatchAgent(t, &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{{ID: "c1", Name: "big_tool", Arguments: map[string]interface{}{}}}},
		{Content: "final"},
	}})
	const size = 200 * 1024
	a.toolResultWarnTokens = 10 // 阈值调到极小，必定触发

	rep := &countingReporter{}
	a.toolResultReporter = rep
	if err := a.io.RegisterDevice(&bigToolDevice{
		name: "bigdev", toolName: "big_tool", size: size,
	}); err != nil {
		t.Fatalf("注册设备失败: %v", err)
	}

	f := a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps=%v err=%v", out, f.Err)
	}
	// 模型看到的必须**原样**
	var got string
	for _, m := range f.Msgs {
		if m.Role == "tool" {
			got = m.Content
		}
	}
	if len(got) != size {
		t.Errorf("工具结果被裁剪了：得到 %d 字节，期望 %d（方案 B 只统计不裁剪）",
			len(got), size)
	}
	// 且确实报告过
	if rep.n == 0 {
		t.Error("未触发超限报告")
	}
}

// ③ 小结果不该误报（避免噪音淹没有效信号）。
func TestNormalToolResultNotReported(t *testing.T) {
	a, _ := newBatchAgent(t, &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{{ID: "c1", Name: "small_tool", Arguments: map[string]interface{}{}}}},
		{Content: "final"},
	}})
	a.toolResultWarnTokens = 100 * 1024 // 100KB
	rep := &countingReporter{}
	a.toolResultReporter = rep
	if err := a.io.RegisterDevice(&smallToolDevice{}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	f := a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps=%v err=%v", out, f.Err)
	}
	if rep.n != 0 {
		t.Errorf("小结果被误报 %d 次 —— 噪音会淹没有效信号", rep.n)
	}
}

// ④ 报告内容要可执行：说清是哪个工具、多大、占预算多少。
func TestReportIsActionable(t *testing.T) {
	a, _ := newBatchAgent(t, &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{{ID: "c1", Name: "big_tool", Arguments: map[string]interface{}{}}}},
		{Content: "final"},
	}})
	a.toolResultWarnTokens = 1024
	rep := &countingReporter{}
	a.toolResultReporter = rep
	if err := a.io.RegisterDevice(&bigToolDevice{
		name: "bigdev", toolName: "big_tool", size: 50 * 1024,
	}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	f := a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps=%v err=%v", out, f.Err)
	}

	if rep.lastMsg == "" {
		t.Fatal("报告文案为空")
	}
	for _, want := range []string{"big_tool", "token"} {
		if !strings.Contains(rep.lastMsg, want) {
			t.Errorf("报告缺少 %q：%s", want, rep.lastMsg)
		}
	}
}

// ---- 测试替身 ----

type countingReporter struct {
	n          int
	lastTool   string
	lastTokens int
	lastMsg    string
}

func (r *countingReporter) report(tool string, tokens, budget int, msg string) {
	r.n++
	r.lastTool = tool
	r.lastTokens = tokens
	r.lastMsg = msg
}

// smallToolDevice 返回一个很小的结果。
type smallToolDevice struct{}

func (d *smallToolDevice) Name() string             { return "smalldev" }
func (d *smallToolDevice) Type() agentIO.DeviceType { return agentIO.DeviceOutput }
func (d *smallToolDevice) Description() string      { return "small result test device" }
func (d *smallToolDevice) Tools() []agentIO.ToolDef {
	return []agentIO.ToolDef{{Name: "small_tool"}}
}
func (d *smallToolDevice) Execute(string, map[string]interface{}) (interface{}, error) {
	return "tiny", nil
}
func (d *smallToolDevice) Start() error                                 { return nil }
func (d *smallToolDevice) Stop() error                                  { return nil }
func (d *smallToolDevice) OutputCapabilities() agentIO.OutputCapability { return agentIO.CapText }
func (d *smallToolDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{} }
