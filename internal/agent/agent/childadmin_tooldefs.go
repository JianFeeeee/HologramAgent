package agent

// childadmin_tooldefs.go —— `admin_child` 三件套的**渲染**（基座，2026-11-20）。
//
// 它读的是内核的类型表（`registry.Types()`），且只有**父侧**看得见。
// 类型不渲染它：`admin_child` 是内核派发给选定主 agent 的 syscall，
// 不是任何类型的私有工具。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// ChildAdminToolDefs 渲染 `admin_child` 三件套（每轮现算，不缓存）。
//
// ★ 缓存是错的：类型可在两轮之间被插件注册/撤销，缓存会让工具表停在
// 上一轮的类型清单上。
//
// 返回的每项是 `registry.ToolDef`（结构化），由调用方转成 LLM schema。
func ChildAdminToolDefs() []registry.ToolDef {
	if len(registry.Types()) == 0 {
		return nil
	}
	return registry.ChildAdminTools()
}
