package main

// 按 syncbus 状态**分阶段**拉起插件（设计 §2.6）。
//
// ## 三条规则（按优先级）
//
//	未声明 requires_facilities          ⇒ 等内核**完全**初始化 ← **现行为，零改动**
//	声明了且依赖全就绪                   ⇒ 立即拉起（**尽早启动**）
//	声明了但依赖未齐                     ⇒ 挂起，等 syncbus 更新
//	声明 auto_start=false                ⇒ 不自动拉起，等 agent 显式启用
//
// ## ★ 兼容性硬约束（§2.6.5）
//
// 存量 42 个插件都不声明依赖 ⇒ 全部走第一条 ⇒ 启动时机**逐字不变**。
// 这不是"顺便兼容"，而是硬约束：任何一个存量插件若因本设计改变启动时机，
// 就可能在不该动的时候动（如假设"我 Start 时 llm 已就绪"）。
// **静默改变启动时机 = 静默引入竞态。**

import (
	"log"
	"strings"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	"github.com/JianFeeeee/HologramAgent/internal/recovery"
)

// resolveFailbackAllowlist 解析 failback 受限启动的插件白名单（装配**前**调用）。
//
// 返回 nil 表示非 failback 模式（普通启动不设白名单）。
func resolveFailbackAllowlist(cfgReg *internalConfig.ConfigRegistry,
	bootMode, dataDir string) []string {
	if bootMode != "failback" {
		return nil
	}
	list := cfgReg.GetString("core.agent.failback_plugins", "webui,pluginmgr,recoverydiag")
	// 优先使用 guard.yaml 经 recovery 任务下发的插件集（guard 是 failback 权威）
	if task, terr := recovery.LoadTask(recovery.TaskPath(dataDir)); terr == nil && len(task.Plugins()) > 0 {
		list = strings.Join(task.Plugins(), ",")
	}
	var names []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			names = append(names, s)
		}
	}
	log.Printf("[homed] failback boot: plugin allowlist = %v（装配前解析）", names)
	return names
}

// applyFailbackPluginAllowlist 在 **failback 受限启动**下设置插件白名单。
//
// ## ★ 必须在插件被拉起**之前**生效
//
// failback 是内核的受限恢复模式：只加载少量插件（webui/pluginmgr/recoverydiag）
// 以便直接产出恢复结论。白名单由 `guard.yaml` 的 recovery 任务或配置决定。
//
// 现在插件拉起由 plugin-host 模块负责，而它在装配期跑 ⇒ 本函数必须在
// **装配之前**被调用（main 里已如此安排）。
//
// 非 failback 模式：本函数什么都不做（普通启动不设白名单）。
func applyFailbackPluginAllowlist(cfgReg *internalConfig.ConfigRegistry,
	pluginReg *plugin.Registry, bootMode, dataDir string) {
	if bootMode != "failback" || pluginReg == nil {
		return
	}
	list := cfgReg.GetString("core.agent.failback_plugins", "webui,pluginmgr,recoverydiag")
	// 优先使用 guard.yaml 经 recovery 任务下发的插件集（guard 是 failback 权威）
	if task, terr := recovery.LoadTask(recovery.TaskPath(dataDir)); terr == nil && len(task.Plugins()) > 0 {
		list = strings.Join(task.Plugins(), ",")
	}
	var names []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			names = append(names, s)
		}
	}
	pluginReg.SetLoadAllowlist(names)
	log.Printf("[homed] failback boot: plugin allowlist = %v", names)
}
