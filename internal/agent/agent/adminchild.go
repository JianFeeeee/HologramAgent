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
	"sort"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// executeAdminChild 执行一次 admin_child 调用。
// ExecuteAdminChild 执行一次 admin_child 调用（类型内的工具也能用）。
func (a *Agent) ExecuteAdminChild(tc agentAPI.ToolCall) string {
	m := a.EnsureInstanceManager()
	if m == nil {
		return "实例管理器未就绪（内核装配错误）"
	}
	typeName := strArg(tc, "type")
	action := strArg(tc, "action")

	// ★ 顶层**只有四个参数**：type / action / meta / arg（文档 §7.1）。
	//
	// 曾经为了"模型常常直接写 id"而把顶层 id / text / residual 也收进来 ——
	// 那是把契约按模型的习惯改。后果是同一个动作有两种写法，而两种写法的
	// 参数合并顺序成了隐性约定（顶层覆盖 arg 还是反之？没人说得清）。
	//
	// 现在：arg 是**唯一**的动作参数载体。顶层多写的键**不生效**（不静默兼容）——
	// 那份执念交给 `admin_child_help` 去教模型。
	arg := map[string]interface{}{}
	if raw, ok := tc.Arguments["arg"].(map[string]interface{}); ok {
		for k, v := range raw {
			arg[k] = v
		}
	}
	if raw, ok := tc.Arguments["meta"].(map[string]interface{}); ok {
		arg["meta"] = raw
	}
	// 顶层多写的键：明确告知"不生效"，而不是静默忽略（静默忽略会让模型
	// 以为自己写对了，然后反复试同一个错）。
	if extra := topLevelExtraArgs(tc); len(extra) > 0 {
		return fmt.Sprintf("admin_child 参数错误：顶层只接受 type / action / meta / arg，"+
			"其余参数必须放进 arg 里。你多写了 %v。例如：\n"+
			`  admin_child(type="resident", action="create", arg={"id":"r1","input_chs":"qq"})`,
			extra)
	}

	out, err := registry.Dispatch(m, typeName, action, arg)
	if err != nil {
		return fmt.Sprintf("admin_child 失败：%v", err)
	}
	return out
}

// executeAdminChildHelp 渲染 admin_child_help 的正文。
// ExecuteAdminChildHelp 渲染 admin_child_help 正文。
func (a *Agent) ExecuteAdminChildHelp(tc agentAPI.ToolCall) string {
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

// topLevelExtraArgs 返回调用方写在顶层、但协议不接受的参数名。
//
// ★ 只报"明确不该在顶层"的键：模型偶尔会多写 description 之类的元信息，
//
//	那些也算违约（契约只有四个参数）。
func topLevelExtraArgs(tc agentAPI.ToolCall) []string {
	allowed := map[string]bool{"type": true, "action": true, "meta": true, "arg": true}
	var extra []string
	for k := range tc.Arguments {
		if !allowed[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra) // 稳定输出：判据与日志可比对
	return extra
}
