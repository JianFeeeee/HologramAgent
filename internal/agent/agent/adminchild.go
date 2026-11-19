package agent

// adminchild.go —— `admin_child` 三件套的**执行侧**（内核统一入口）。
//
// ## 用户裁定（2026-11-18）
//
// > 「是内核提供一个全参数化的 adminchild（type，meta，action，arg）调用，
// >    各个子 agent 注册各自的管理行为，并和 outputch 设计一样有 help 调用
// >    与 skill 指导主 agent 如何使用」
//
// > 「创建子 agent 执行在主 agent？不应该在内核吗？」
//
// ## 分工
//
//	工具表    registry.ToolDefs()      —— 从注册表生成（类型增删 ⇒ 工具表自动变）
//	分发      registry.Dispatch()      —— 通用动作内核实现，类型独有动作转发
//	执行      InstanceManager          —— 实例的创建/销毁/注入/重启
//	本文件    这三者的**薄接线**：把 ToolCall 的参数拆出来交给 registry
//
// 本文件刻意不做业务判断：任何"某类型该怎么做"的知识都在类型自己的
// Descriptor（Construct / Admin / Help）里。

import (
	"fmt"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// executeAdminChild 执行一次 admin_child 调用。
func (a *Agent) executeAdminChild(tc agentAPI.ToolCall) string {
	m := a.EnsureInstanceManager()
	if m == nil {
		return "实例管理器未就绪（内核装配错误）"
	}
	typeName := strArg(tc, "type")
	action := strArg(tc, "action")

	// arg 是一个对象：它的键与 type/meta 合并后交给 Dispatch。
	// 这样"create 的参数"与"通用动作的 id/text"共用一个入口，
	// 与 output_send__ 的 payload/type/meta 三参形态对齐。
	arg := map[string]interface{}{}
	if raw, ok := tc.Arguments["arg"].(map[string]interface{}); ok {
		for k, v := range raw {
			arg[k] = v
		}
	}
	if raw, ok := tc.Arguments["meta"].(map[string]interface{}); ok {
		arg["meta"] = raw
	}
	// 直接给在顶层的那几个通用参数也一并收进来（模型常常直接写 id/text）。
	for _, k := range []string{"id", "text", "residual"} {
		if v, ok := tc.Arguments[k]; ok {
			if s, isStr := v.(string); !isStr || strings.TrimSpace(s) != "" {
				arg[k] = v
			}
		}
	}

	out, err := registry.Dispatch(m, typeName, action, arg)
	if err != nil {
		return fmt.Sprintf("admin_child 失败：%v", err)
	}
	return out
}

// executeAdminChildHelp 渲染 admin_child_help 的正文。
func (a *Agent) executeAdminChildHelp(tc agentAPI.ToolCall) string {
	m := a.EnsureInstanceManager()
	return registry.Help(m, strArg(tc, "type"))
}

// ExecuteAdminChildList 列出全部类型与实例（= admin_child(action=list) 无 type）。
func (a *Agent) ExecuteAdminChildList() string {
	m := a.EnsureInstanceManager()
	out, err := registry.Dispatch(m, "", registry.ActionList, nil)
	if err != nil {
		return fmt.Sprintf("admin_child_list 失败：%v", err)
	}
	return out
}

// strArg 取字符串参数（去空白）。
//
// ★ 本文件的本地小工具：`admin_child` 的参数本来就是平铺进 map 的，
//
//	与任何类型的 `strArg` 无关（那些在类型包里，各管自己的工具参数）。
func strArg(tc agentAPI.ToolCall, key string) string {
	if tc.Arguments == nil {
		return ""
	}
	s, _ := tc.Arguments[key].(string)
	return strings.TrimSpace(s)
}
