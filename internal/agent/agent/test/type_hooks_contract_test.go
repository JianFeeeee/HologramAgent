package test

// 三类 agent 的**上下文槽位**必须各注册各的（用户 2026-11-18 指正）。
//
// ## 用户原话
//
//	「有些不是主 agent 专属的逻辑吗？你怎么放在 agent 底座里了」
//
// 上下文预算/裁剪/超页这几档里，「用整窗还是策略裁剪、撞窗口了上报还是自己裁」
// 是纯粹的类型语义。判据必须钉住：
//
//	① 主 agent：策略裁剪型（ContextBudget = 动态预算，Prune/Overflow 都注册）
//	② 驻留子：整窗型（ContextBudget = MaxContext，Prune/Overflow 明确为 nil）
//	③ rt：整窗型（同上），且不注册同步 loop（它跑 rtLoop）
//	④ 三者都能被创建出来且**不 panic**（Initialize 真的装了东西）
//
// 全部走导出面：Instantiate / SpawnResident / SpawnRTChild / HooksValue。

import (
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// hooksHarness 造一个有图记忆 + IO 的父，用来创建两类子。
type hooksHarness struct {
	agent *agent.Agent
	iom   *io.IOManager
	graph *memory.GraphDB
	dir   string
}

func newHooksHarness(t *testing.T) *hooksHarness {
	t.Helper()
	dir := t.TempDir()
	g, err := memory.NewGraphDB(filepath.Join(dir, "g.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	iom := io.NewIOManager()
	iom.RegisterInputChannel("in", io.ChannelDef{})
	iom.RegisterInputChannel("rt-in", io.ChannelDef{Realtime: true})
	iom.RegisterInputChannel("rt-out", io.ChannelDef{Realtime: true})

	a, instErr := agent.Instantiate(agent.TypeNameMain, agent.AgentConfig{
		Security: security.NewDisabledRuntime().AsGate(),
		ID:       types.AgentID("hooks-parent"),
		IO:       iom,
		Memory:   g,
		DataDir:  dir,
	})
	if instErr != nil {
		t.Fatalf("Instantiate(main): %v", instErr)
	}
	t.Cleanup(a.Stop)
	return &hooksHarness{agent: a, iom: iom, graph: g, dir: dir}
}

// budget 是一个用于比较的预算样本。
func budget() kit.TokenBudget {
	return kit.TokenBudget{MaxContext: 100000, ContextTokens: 1234}
}

// TestMainAgentRegistersStrategicHooks：主 agent 是策略裁剪型。
func TestMainAgentRegistersStrategicHooks(t *testing.T) {
	h := newHooksHarness(t)
	hk := h.agent.HooksValue()

	if hk.ContextBudget == nil {
		t.Fatal("★ 主 agent 没注册 ContextBudget")
	}
	got := hk.ContextBudget(h.agent, budget())
	if got != 1234 {
		t.Fatalf("★ 主 agent 的 ContextBudget = %d，期望策略裁剪型的 1234（ContextTokens）", got)
	}
	if hk.Prune == nil {
		t.Fatal("★ 主 agent 没注册 Prune —— 策略裁剪型必须能裁")
	}
	if hk.EnforceOverflow == nil {
		t.Fatal("★ 主 agent 没注册 EnforceOverflow —— 它必须自己处置超页")
	}
	if hk.Loop == nil {
		t.Fatal("★ 主 agent 没注册 Loop")
	}
}

// TestResidentRegistersWholeWindowHooks：驻留子是整窗型 + 明确的 nil 策略。
func TestResidentRegistersWholeWindowHooks(t *testing.T) {
	h := newHooksHarness(t)
	if _, err := h.agent.SpawnResident(agent.ResidentOptions{
		ID:       "kid",
		InputChs: []string{"in"},
		TempPath: filepath.Join(h.dir, "kid", "graph.db"),
	}); err != nil {
		t.Fatalf("SpawnResident: %v", err)
	}
	rc := h.agent.ResidentOf("kid")
	if rc == nil {
		t.Fatal("★ 创建成功后 ResidentOf 仍返回 nil")
	}
	hk := rc.AgentHooks()

	if hk.ContextBudget == nil {
		t.Fatal("★ 驻留子没注册 ContextBudget")
	}
	if got := hk.ContextBudget(rc.AgentForTest(), budget()); got != 100000 {
		t.Fatalf("★ 驻留子的 ContextBudget = %d，期望整窗型的 MaxContext=100000", got)
	}
	// ★ 这两个 nil 是**明确的策略**（整窗型不做相关性裁剪与本地超页处置），
	// 不是漏接线 —— 与"必填槽位缺失"是两件事。
	if hk.Prune != nil {
		t.Fatal("★ 驻留子注册了 Prune —— 整窗型不该按相关度裁")
	}
	if hk.EnforceOverflow != nil {
		t.Fatal("★ 驻留子注册了 EnforceOverflow —— 超页处置权在父")
	}
	if hk.Loop == nil {
		t.Fatal("★ 驻留子没注册 Loop（它与主 agent 同一套同步状态机）")
	}
}

// TestRTRegistersWholeWindowHooksOnly：rt 只注册上下文预算，不注册同步 loop。
func TestRTRegistersWholeWindowHooksOnly(t *testing.T) {
	h := newHooksHarness(t)
	if _, err := h.agent.SpawnRTChild(agent.RTChildOptions{
		ID: "rt1", Input: "rt-in", Output: "rt-out",
	}); err != nil {
		t.Fatalf("SpawnRTChild: %v", err)
	}
	rc := h.agent.RTChildOf("rt1")
	if rc == nil {
		t.Fatal("★ 创建成功后 RTChildOf 仍返回 nil")
	}
	hk := rc.AgentHooks()

	if hk.ContextBudget == nil {
		t.Fatal("★ rt 没注册 ContextBudget —— 通话历史会被按相关度裁")
	}
	if got := hk.ContextBudget(rc.AgentForTest(), budget()); got != 100000 {
		t.Fatalf("★ rt 的 ContextBudget = %d，期望整窗型 100000", got)
	}
	if hk.Prune != nil || hk.EnforceOverflow != nil {
		t.Fatal("★ rt 注册了 Prune/EnforceOverflow —— 它是整窗型，处置权在父")
	}
	// rt 的驱动是 rtLoop，不注册基座的标准 loop。
	if hk.Loop != nil {
		t.Fatal("★ rt 注册了 Loop —— 它跑 rtLoop，不经 RunTaskSteps")
	}
}
