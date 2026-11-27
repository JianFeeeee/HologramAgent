package agent

// cfgsurface.go —— agent 读写配置的**唯一入口**。
//
// ## 为什么配置不能落在 agent 状态里
//
// 改造前的形态：装配期把 `core.agent.*` 读成快照塞进 `AgentConfig`，
// 基座再拷进 `mainState`。于是：
//
//	① 配置改了要重启才生效
//	② 同一份值有两个落点（真值 + 副本），实测漂移过一次
//	③ 这些"值"住在 `mainState` 里 ⇒ `mainState` 搬不走
//
// 现在：配置管理器由内核注入（`AgentConfig.Cfg`），**用到时才读**。
//
// 	配置 → 内核持有的事实（ConfigRegistry）
// 	基座 → 只持句柄（本文件）
// 	agent → `a.Cfg().GetXxx("core.agent.…")`
//
// ## 常量集中在这里
//
// 键名散写在各处会让"这条键到底谁在读"变成 grep 谜题（实测
// `core.agent.max_context_size` 一度注册了却零读取）。所有键在这里有一行。

import (
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// Cfg 返回配置管理器（未注入 ⇒ nil，调用方须用 CfgOr 系列）。
func (a *Agent) Cfg() kernelapi.ConfigStore {
	if a == nil {
		return nil
	}
	return a.cfgStore
}

// 配置键（集中声明，避免散写）。
const (
	CfgKeyMaxContextSize  = "core.agent.max_context_size"
	CfgKeyDistillInterval = "core.agent.distill_interval"
	CfgKeyArchiveInterval = "core.agent.archive_interval"
	CfgKeyReviewInterval  = "core.agent.review_interval"
	CfgKeyMergeInterval   = "core.agent.merge_interval"
	CfgKeyMaxToolTurns    = "core.agent.max_tool_turns"
	CfgKeyOffloadEnabled  = "core.agent.offload_enabled"
	CfgKeyOffloadBusy     = "core.agent.offload_busy_after"
	CfgKeyOffloadMinPend  = "core.agent.offload_min_pending"
	CfgKeyOffloadMaxRes   = "core.agent.offload_max_residents"
	CfgKeyCtxUtilization  = "core.agent.context.utilization_percent"
	CfgKeyCtxMaxTarget    = "core.agent.context.max_target_tokens"
	CfgKeyCtxMemoryRatio  = "core.agent.context.memory_ratio_percent"
	CfgKeyCtxProtected    = "core.agent.context.protected_count"
	CfgKeyMainProvider    = "core.agent.llm.main_provider"
	CfgKeySystemPrompt    = "core.agent.system_prompt"
	CfgKeyThinkingEnabled = "core.llm.thinking_enabled"
)

// CfgInt 读整型配置（未注入 ⇒ def）。
func (a *Agent) CfgInt(key string, def int) int {
	if c := a.Cfg(); c != nil {
		return c.GetInt(key, def)
	}
	return def
}

// CfgString 读字符串配置（未注入 ⇒ def）。
func (a *Agent) CfgString(key, def string) string {
	if c := a.Cfg(); c != nil {
		return c.GetString(key, def)
	}
	return def
}

// CfgBool 读布尔配置（未注入 ⇒ def）。
func (a *Agent) CfgBool(key string, def bool) bool {
	if c := a.Cfg(); c != nil {
		return c.GetBool(key, def)
	}
	return def
}

// CfgDuration 读时长配置（未注入 ⇒ def）。
func (a *Agent) CfgDuration(key string, def time.Duration) time.Duration {
	if c := a.Cfg(); c != nil {
		return c.GetDuration(key, def)
	}
	return def
}

// SetCfgValue 写一条配置（agent 改自己的运行参数时用）。
func (a *Agent) SetCfgValue(key string, value interface{}) error {
	if c := a.Cfg(); c != nil {
		return c.Set(key, value)
	}
	return nil
}

// ───────────────────────── 配置快照：**现读**，不存字段 ─────────────────────────
//
// 这些值原先在 `AgentConfig` 里被装配点快照一次（`DistillInterval` /
// `MaxContextSize` / `CtxTuning` / `ThinkingEnabled` …），于是改配置必须重启，
// 而且"同一个键两个落点"（ConfigDef 的 Default 与装配点字面量）容易漂移。
//
// ★ 现在一律现读（docs §2 结论：AgentConfig 只装配置项透传）。
// 每读一次都是 `CfgStore.GetXxx`，它内部有缓存 —— 不会真的每次都打配置库。

// CtxTuning 返回上下文预算的可调参数（`core.agent.context.*`）。
//
// 零值语义：未注入配置管理器时返回零值 —— 那是"独立工具/单测"的明确情形，
// 调用方（kit.ComputeTokenBudget）对零值已有历史默认。
func (a *Agent) CtxTuning() kit.ContextTuning {
	if a == nil || a.cfgStore == nil {
		return kit.ContextTuning{}
	}
	return kit.ContextTuning{
		UtilizationPercent: a.CfgInt(CfgKeyCtxUtilization, 0),
		MaxTargetTokens:    a.CfgInt(CfgKeyCtxMaxTarget, 0),
		MemoryRatioPercent: a.CfgInt(CfgKeyCtxMemoryRatio, 0),
		ProtectedCount:     a.CfgInt(CfgKeyCtxProtected, 0),
	}
}

// DistillInterval 返回上下文蒸馏间隔（`core.agent.distill_interval`）。
func (a *Agent) DistillInterval() time.Duration {
	return a.CfgDuration(CfgKeyDistillInterval, 30*time.Minute)
}

// ArchiveInterval 返回冷文档归档间隔（默认继承蒸馏间隔）。
func (a *Agent) ArchiveInterval() time.Duration {
	d := a.CfgDuration(CfgKeyArchiveInterval, 0)
	if d <= 0 {
		return a.DistillInterval()
	}
	return d
}

// ReviewInterval 返回关系复审间隔（默认继承蒸馏间隔）。
func (a *Agent) ReviewInterval() time.Duration {
	d := a.CfgDuration(CfgKeyReviewInterval, 0)
	if d <= 0 {
		return a.DistillInterval()
	}
	return d
}

// MergeInterval 返回实体合并间隔（默认继承蒸馏间隔）。
func (a *Agent) MergeInterval() time.Duration {
	d := a.CfgDuration(CfgKeyMergeInterval, 0)
	if d <= 0 {
		return a.DistillInterval()
	}
	return d
}

// MaxContextSize 返回活跃上下文条数上限（`core.agent.max_context_size`）。
func (a *Agent) MaxContextSize() int {
	n := a.CfgInt(CfgKeyMaxContextSize, 0)
	if n <= 0 {
		return 30
	}
	return n
}

// ThinkingEnabled 报告是否启用模型思考模式（`core.llm.thinking_enabled`）。
func (a *Agent) ThinkingEnabled() bool {
	return a.CfgBool(CfgKeyThinkingEnabled, false)
}

// ───────────────── 父声明类：装配期写入，运行期只读 ─────────────────
//
// ★ 为什么不进 AgentConfig（docs §2 的 12 字段里没有它们）：
// 它们的值是**父在创建这一刻声明的**，不是"配置中心里那条键"。
// 但也不能纯现读 —— 子 agent 的优先源是本 agent 的父声明（可能被
// admin_child 的 arg 覆盖），与"总默认源"不是同一个东西。
//
// 形状与 MountDevice / MountSemantic 一致：装配期写一次，之后只读。

// SetPreferredProvider 设置本 agent 的优先模型源（空 = 继承/默认）。
func (a *Agent) SetPreferredProvider(name string) {
	if a == nil {
		return
	}
	a.semanticMu.Lock()
	a.preferredProvider = name
	a.semanticMu.Unlock()
}

// SetSystemPrompt 设置本 agent 的系统提示词素材（父传给子的那份）。
//
// ★ 是**素材**不是渲染结果：渲染归类型（docs §0.3）。
func (a *Agent) SetSystemPrompt(p string) {
	if a == nil {
		return
	}
	a.semanticMu.Lock()
	a.systemPrompt = p
	a.semanticMu.Unlock()
}
