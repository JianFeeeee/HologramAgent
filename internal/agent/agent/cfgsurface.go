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
