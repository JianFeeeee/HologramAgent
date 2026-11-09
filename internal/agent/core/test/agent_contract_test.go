package test

// internal/agent/core 的黑盒判据：只用 New(AgentConfig) + 公开方法，
// 输入 → 可观察输出。不构造 TaskFrame、不调 stepLLM/runStage、
// 不碰 a.sched、不读源码。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/core"
	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

func newAgent(cfg core.AgentConfig) *core.Agent {
	return core.New(cfg)
}

// TestAgentID：构造时给的 ID 必须能从 ID() 读回。
func TestAgentID(t *testing.T) {
	a := newAgent(core.AgentConfig{ID: types.AgentID("contract-agent")})
	if got := a.ID(); got != types.AgentID("contract-agent") {
		t.Fatalf("★ ID() = %q，期望 contract-agent", got)
	}
}

// TestOutputAllowlist：AllowedOutputs 为空 = 完整授权；非空 = 白名单。
func TestOutputAllowlist(t *testing.T) {
	all := newAgent(core.AgentConfig{})
	if !all.IsOutputAllowed("qq") || !all.IsOutputAllowed("webui") {
		t.Fatal("★ 空白名单应完整授权")
	}
	limited := newAgent(core.AgentConfig{
		AllowedOutputs: []string{"qq"},
	})
	if !limited.IsOutputAllowed("qq") {
		t.Fatal("★ 白名单内的通道被拒")
	}
	if limited.IsOutputAllowed("webui") {
		t.Fatal("★ 白名单外的通道被放行")
	}
}

// TestDuplicateInputWindow：同源同内容短窗口内第二次判重；异源/异内容不判重。
func TestDuplicateInputWindow(t *testing.T) {
	a := newAgent(core.AgentConfig{})
	if a.IsDuplicateInput("webui", "再发一次") {
		t.Fatal("★ 第一次输入被判成重复")
	}
	if !a.IsDuplicateInput("webui", "再发一次") {
		t.Fatal("★ 同源同内容第二次未判重")
	}
	if a.IsDuplicateInput("qq", "再发一次") {
		t.Fatal("★ 不同来源被误判重复")
	}
	if a.IsDuplicateInput("webui", "另一句") {
		t.Fatal("★ 不同内容被误判重复")
	}
}

// TestRealtimeFlag：IsRealtime 是构造期的可观察输出；nil receiver 安全。
func TestRealtimeFlag(t *testing.T) {
	if newAgent(core.AgentConfig{}).IsRealtime() {
		t.Fatal("★ 普通 agent 报告自己是 realtime")
	}
	var nilAgent *core.Agent
	if nilAgent.IsRealtime() {
		t.Fatal("★ nil receiver 报告 realtime=true")
	}
}

// TestEmptyCollectionsOnFreshAgent：新 agent 的驻留子 / rt 子 / 队列视图为空。
func TestEmptyCollectionsOnFreshAgent(t *testing.T) {
	a := newAgent(core.AgentConfig{IO: io.NewIOManager()})
	if got := a.Residents(); len(got) != 0 {
		t.Fatalf("★ Residents() = %v，期望空", got)
	}
	if got := a.RTChildren(); len(got) != 0 {
		t.Fatalf("★ RTChildren() = %v，期望空", got)
	}
	snap := a.DumpScheduler()
	if snap.Running != nil || len(snap.Queue) != 0 {
		t.Fatalf("★ 新 agent 的调度快照非空：running=%v queue=%d",
			snap.Running, len(snap.Queue))
	}
}

// TestResolveOutputTargetWithoutRegistry：未装配通道注册表时 ok=false，不 panic。
func TestResolveOutputTargetWithoutRegistry(t *testing.T) {
	a := newAgent(core.AgentConfig{})
	if _, ok := a.ResolveOutputTarget("anything"); ok {
		t.Fatal("★ 无注册表却报告解析成功")
	}
}

// TestGetKernelStatusNeverNil：状态查询必须返回可用对象。
func TestGetKernelStatusNeverNil(t *testing.T) {
	a := newAgent(core.AgentConfig{IO: io.NewIOManager()})
	if st := a.GetKernelStatus(); st == nil {
		t.Fatal("★ GetKernelStatus() 返回 nil")
	}
}
