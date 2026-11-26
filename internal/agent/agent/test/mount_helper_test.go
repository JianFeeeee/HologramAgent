package test

// mount_helper_test.go —— 判据用的**面挂载**辅助（不是生产路径）。
//
// ## 为什么判据需要它
//
// `AgentConfig` 已收敛到 12 个字段（docs §2：身份 + 父声明 + `Cfg`）。
// 面与设施句柄（IO / 阶段管道 / 安全闸 / 记录表 / 插件表 / LLM / 工具执行 /
// 渲染面 / 上下文统计）一律在构造**之后**挂载 ——
// 与 `MountDevice` / `SetBuiltinTools` 同形。
//
// 判据原先直接把这些塞进 cfg，那是被删的形态。本文件让判据只改**构造方式**，
// 不改它钉住的**语义**（断言一行不动）。
//
// ★ 生产路径**不用**它：装配点（`kernelmod/agent`）自己按需挂载。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/agent/provider"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"time"
)

// faces 是判据要挂到基座上的面与设施（零值字段 = 不挂）。
type faces struct {
	IO        *agentIO.IOManager
	StageHost *kit.StageHostType
	// ToolReg 是**插件工具登记簿**（与阶段管道是两个独立句柄，2026-11-25 拆分）。
	ToolReg      *kit.ToolRegistry
	Security     kernelapi.SecurityGate
	Agents       kernelapi.AgentRegistry
	PluginReg    kernelapi.PluginHost
	PluginDir    string
	LLM          kernelapi.LLMClient
	Provider     any
	ProviderMgr  *provider.ProviderManager
	ToolExec     agent.ToolExecutor
	ToolDefs     agent.ToolDefsFace
	ContextStats agent.ContextStats
	DepParser    func() (bool, string)
	FusionCfg    any
	Personality  any
	PersonaStore any
}

// mount 把 faces 挂到已构造的实例上。
func (f faces) mount(a *agent.Agent) *agent.Agent {
	if a == nil {
		return nil
	}
	a.MountSurface(f.IO, f.StageHost, f.Security, f.Agents, f.PluginReg, f.PluginDir, f.LLM)
	if f.ToolReg != nil {
		a.MountToolRegistry(f.ToolReg)
	}
	if f.ToolExec != nil {
		a.SetToolExec(f.ToolExec)
	}
	if f.ToolDefs != nil || f.ContextStats != nil || f.DepParser != nil {
		a.MountTypeFaces(f.ToolDefs, f.ContextStats, f.DepParser)
	}
	if f.FusionCfg != nil || f.Personality != nil || f.PersonaStore != nil {
		a.MountMainValues(f.FusionCfg, f.Personality, f.PersonaStore)
	}
	return a
}

// testCfg 是最小配置管理器：只提供判据关心的键，其余走默认值。
type testCfg struct{ distill time.Duration }

func (c *testCfg) GetString(key, def string) string        { return def }
func (c *testCfg) GetInt(key string, def int) int          { return def }
func (c *testCfg) GetBool(key string, def bool) bool       { return def }
func (c *testCfg) Set(key string, value interface{}) error { return nil }

func (c *testCfg) GetDuration(key string, def time.Duration) time.Duration {
	if key == agent.CfgKeyDistillInterval {
		return c.distill
	}
	return def
}
