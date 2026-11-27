package agent

// childadmin_register.go —— `admin_child` 三件套**登记进唯一那张工具表**。
//
// schema 由 `registry.ChildAdminTools()` 渲染（读的是类型表，每轮现算），
// 执行体是基座的方法（`ExecuteAdminChild*`）。
//
// ★ 它不归任何类型：内核派发给选定主 agent 的 syscall（docs §0.1）。
//   所以登记点在基座，`Owner: "core"`，`Side: SideParent`（只有父侧看得见）。

import (
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

func init() {
	registerChildAdminTools()
}

func registerChildAdminTools() {
	// admin_child / admin_child_help / admin_child_list 的声明来自类型表，
	// 而其内容（enum / 描述）是**每轮现算**的 —— 表里只放执行体与归属，
	// schema 由 `registrytooldefs.go` 在渲染时向 ChildAdminToolDefs 取。
	_ = registry.RegisterTool(registry.Tool{
		Name:   "admin_child",
		Side:   registry.SideParent,
		Owner:  "core",
		Handle: handleAdminChild,
	})
	_ = registry.RegisterTool(registry.Tool{
		Name:   "admin_child_help",
		Side:   registry.SideParent,
		Owner:  "core",
		Handle: handleAdminChildHelp,
	})
	_ = registry.RegisterTool(registry.Tool{
		Name:   "admin_child_list",
		Side:   registry.SideParent,
		Owner:  "core",
		Handle: handleAdminChildList,
	})
}

func handleAdminChild(c registry.ToolCtx, tc agentAPI.ToolCall) string {
	a, _ := c.Host.(*Agent)
	if a == nil {
		return "admin_child: 执行宿主不是 agent（装配错误）"
	}
	return a.ExecuteAdminChild(tc)
}

func handleAdminChildHelp(c registry.ToolCtx, tc agentAPI.ToolCall) string {
	a, _ := c.Host.(*Agent)
	if a == nil {
		return "admin_child_help: 执行宿主不是 agent（装配错误）"
	}
	return a.ExecuteAdminChildHelp(tc)
}

func handleAdminChildList(c registry.ToolCtx, tc agentAPI.ToolCall) string {
	a, _ := c.Host.(*Agent)
	if a == nil {
		return "admin_child_list: 执行宿主不是 agent（装配错误）"
	}
	return a.ExecuteAdminChildList()
}
