package test

// typepath_helper_test.go —— 判据统一走**类型表的 Construct**（与生产同一条路径）。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	"github.com/JianFeeeee/HologramAgent/pkg/types"

	_ "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent"
)

// agentOfType 经类型表的 Construct 造实例（生产同一条路径）。
//
// 判据用它，而不是裸 `agent.New`：类型差异（loop / context / scheduler）
// 由 Construct 装上，裸基座跑不起任何 agentloop。
func agentOfType(t *testing.T, typeName string, cfg agent.AgentConfig) *agent.Agent {
	t.Helper()
	if cfg.ID == "" {
		cfg.ID = types.AgentID("contract-agent")
	}
	cfg.TypeName = typeName
	d, ok := registry.Lookup(typeName)
	if !ok {
		t.Fatalf("类型 %q 未注册", typeName)
	}
	host := agent.NewRootHost(cfg)
	// ★ 走 `registry.Instantiate`（**两阶段**：construct + Initialize）。
	// 判据必须走与生产**同一条**入口 —— 否则"直接调 Construct 跳过
	// Initialize"会让实例缺调度器/上下文，而判据还绿（实测踩过）。
	if _, _, err := registry.Instantiate(d, host, map[string]interface{}{"cfg": cfg, "id": string(cfg.ID)}); err != nil {
		t.Fatalf("Instantiate(%s): %v", typeName, err)
	}
	a, _ := host.Root().(*agent.Agent)
	if a == nil {
		t.Fatalf("类型 %q 没有交回根实例", typeName)
	}
	return a
}

// newMainAgent 造一个主 agent 实例（生产路径）。
func newMainAgent(t *testing.T, cfg agent.AgentConfig) *agent.Agent {
	t.Helper()
	return agentOfType(t, agent.TypeNameMain, cfg)
}
