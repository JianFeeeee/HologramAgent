package test

// internal/agent/agent 的黑盒判据：只用 New(AgentConfig) + 公开方法，
// 输入 → 可观察输出。不构造 TaskFrame、不调 stepLLM/runStage、
// 不碰 a.sched、不读源码。

import (
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/resident"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

func newAgent(t *testing.T, cfg agent.AgentConfig) *agent.Agent {
	t.Helper()
	// ★ 唯一造实例入口是 `agent.New`（类型差异由类型自己的 Install 挂）。
	if cfg.TypeName == "" {
		cfg.TypeName = agent.TypeNameMain
	}
	return agent.New(cfg)
}

// TestAgentID：构造时给的 ID 必须能从 ID() 读回。
func TestAgentID(t *testing.T) {
	a := faces{Security: security.NewDisabledRuntime().AsGate()}.mount(newAgent(t, agent.AgentConfig{ID: types.AgentID("contract-agent")}))
	if got := a.ID(); got != types.AgentID("contract-agent") {
		t.Fatalf("★ ID() = %q，期望 contract-agent", got)
	}
}

// TestOutputAllowlist：AllowedOutputs 为空 = 完整授权；非空 = 白名单。
func TestOutputAllowlist(t *testing.T) {
	all := faces{Security: security.NewDisabledRuntime().AsGate()}.mount(newAgent(t, agent.AgentConfig{}))
	if !all.IsOutputAllowed("qq") || !all.IsOutputAllowed("webui") {
		t.Fatal("★ 空白名单应完整授权")
	}
	limited := (faces{Security: security.NewDisabledRuntime().AsGate()}).mount(agent.New(agent.AgentConfig{AllowedOutputs: []string{"qq"}}))
	if !limited.IsOutputAllowed("qq") {
		t.Fatal("★ 白名单内的通道被拒")
	}
	if limited.IsOutputAllowed("webui") {
		t.Fatal("★ 白名单外的通道被放行")
	}
}

// TestDuplicateInputWindow：同源同内容短窗口内第二次判重；异源/异内容不判重。
func TestDuplicateInputWindow(t *testing.T) {
	a := faces{Security: security.NewDisabledRuntime().AsGate()}.mount(newAgent(t, agent.AgentConfig{}))
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

// TestAgentCapabilities：能力面是构造期的可观察输出；nil receiver 安全。
func TestAgentCapabilities(t *testing.T) {
	// 未声明能力面 ⇒ 零值（没有任何能力位）。
	if got := (faces{Security: security.NewDisabledRuntime().AsGate()}).mount(newAgent(t, agent.AgentConfig{})).AgentCapabilities(); got != 0 {
		t.Fatalf("★ 未声明能力面的 agent 报告了 %v", got)
	}
	// 声明了就读得到（能力面是纯数据透传）。
	a := faces{Security: security.NewDisabledRuntime().AsGate()}.mount(newAgent(t, agent.AgentConfig{AgentCap: agentIO.CapText | agentIO.CapRT}))
	if !a.AgentCapabilities().Supports(agentIO.CapRT) {
		t.Fatal("★ 声明了 CapRT 但读面拿不到")
	}
	var nilAgent *agent.Agent
	if nilAgent.AgentCapabilities() != 0 {
		t.Fatal("★ nil receiver 报告了能力位")
	}
}

// TestEmptyCollectionsOnFreshAgent：新 agent 的驻留子 / rt 子 / 队列视图为空。
func TestEmptyCollectionsOnFreshAgent(t *testing.T) {
	a := faces{Security: security.NewDisabledRuntime().AsGate(), IO: io.NewIOManager()}.mount(newAgent(t, agent.AgentConfig{}))
	// ★ 子 agent 的视图走**唯一协议**（`ListChildren` 只读简报），
	//   不再有 `Residents()` / `RTChildren()` 这种按类型具名的访问器 ——
	//   那种访问器每加一种类型就要加一个（正是要消灭的形态）。
	if got := a.ListChildren(""); len(got) != 0 {
		t.Fatalf("★ ListChildren() = %v，期望空", got)
	}
	// ★ 调度快照**归类型**（docs §0.3：队列/帧/中断栈是类型内部实现）。
	//   基座没有 Snapshot();"新实例没有积压"改由 ListChildren 的空结果表达。
}

// TestResolveOutputTargetWithoutRegistry：未装配通道注册表时 ok=false，不 panic。
func TestResolveOutputTargetWithoutRegistry(t *testing.T) {
	a := faces{Security: security.NewDisabledRuntime().AsGate()}.mount(newAgent(t, agent.AgentConfig{}))
	if _, ok := a.ResolveOutputTarget("anything"); ok {
		t.Fatal("★ 无注册表却报告解析成功")
	}
}

// TestGetKernelStatusNeverNil：状态查询必须返回可用对象。
func TestGetKernelStatusNeverNil(t *testing.T) {
	a := faces{Security: security.NewDisabledRuntime().AsGate(), IO: io.NewIOManager()}.mount(newAgent(t, agent.AgentConfig{}))
	if st := a.GetKernelStatus(); st == nil {
		t.Fatal("★ GetKernelStatus() 返回 nil")
	}
}
