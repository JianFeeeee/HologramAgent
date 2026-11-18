package registry

// childadmin.go —— **admin_child 三件套**的工具表生成（镜像 outputch 的设计）。
//
// ## 用户裁定（2026-11-18）
//
// > 「是内核提供一个全参数化的 adminchild（type，meta，action，arg）调用，
// >    各个子 agent 注册各自的管理行为，并和 outputch 设计一样有 help 调用
// >    与 skill 指导主 agent 如何使用」
//
// ## 与 outputch 的对应关系
//
//	output_send__{ch}        ↔  admin_child(type, action, meta, arg)
//	output_send__{ch}_help   ↔  admin_child_help(type)
//	output_list_channels     ↔  admin_child_list
//
// 三件的**来源**都是同一张表（outputch 那张是通道登记表，这里是类型注册表）。
// 类型自己声明 Meta/Actions/Help；内核只做"把它们渲染成工具 schema"。
//
// ★ 为什么工具名只有三个而不是每个类型一个：类型是**运行期可增删**的
//   （内置插件经 EventBus 注册/撤销），工具名固定才能让模型不必每次
//   重新学一套工具表；type 走参数，与 outputch 用通道名做工具名相反 ——
//   通道是装配期固定的，类型不是。

import (
	"fmt"
	"sort"
	"strings"
)

// ToolDefs 生成 admin_child 三件套的工具声明。
//
// 返回的 map 与 `toolDef(...)` 的形状一致（type/function/name/description/parameters），
// 因为基座的工具表就是那个形状 —— 本包不 import agent 包，只产出纯数据。
func ToolDefs() []map[string]interface{} {
	types := Types()
	if len(types) == 0 {
		return nil
	}

	var out []map[string]interface{}

	// ① admin_child：type + action + meta + arg
	actionEnum := []string{}
	typeEnum := []string{}
	var typeDocs []string
	for _, t := range types {
		d, ok := Lookup(t)
		if !ok {
			continue
		}
		typeEnum = append(typeEnum, t)
		desc := d.Description
		if desc == "" {
			desc = t
		}
		typeDocs = append(typeDocs, fmt.Sprintf("%s — %s", t, desc))
	}
	for _, a := range GenericActions() {
		actionEnum = append(actionEnum, a.Name)
	}
	// 类型独有动作：并集（不同类型可以有同名动作）。
	for _, t := range types {
		d, _ := Lookup(t)
		for _, a := range d.Actions {
			if !containsStr(actionEnum, a.Name) {
				actionEnum = append(actionEnum, a.Name)
			}
		}
	}
	sort.Strings(actionEnum)

	out = append(out, map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name": "admin_child",
			"description": "管理子 agent（全参数化入口）。type 选择子 agent 类型，action 选择动作，" +
				"meta/arg 是该动作的参数。\n" +
				"可用类型：" + strings.Join(typeDocs, "；") + "\n" +
				"通用动作：list（列出实例）/ create（按该类型的参数创建）/ inspect（查看单个）/" +
				"send（向实例注入输入，会打断它当前处理）/ destroy（销毁单个，可用 residual 决定残余任务）/" +
				"destroy_all（销毁该类型全部）/ restart（重启单个，用于强行停止跑飞的实例）。\n" +
				"类型独有动作见 admin_child_help(type)。",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"type": map[string]interface{}{
						"type":        "string",
						"enum":        typeEnum,
						"description": "子 agent 类型（留空 = 全部类型，仅 list/destroy_all 可用）",
					},
					"action": map[string]interface{}{
						"type":        "string",
						"enum":        actionEnum,
						"description": "要执行的动作",
					},
					"id":   map[string]interface{}{"type": "string", "description": "实例 id（inspect/send/destroy/restart 必填）"},
					"text": map[string]interface{}{"type": "string", "description": "send：要注入的内容"},
					"residual": map[string]interface{}{
						"type": "string", "enum": []string{"keep", "drop"},
						"description": "destroy：残余任务处置（keep=转回你的队列，默认 / drop=明确丢弃）",
					},
					"arg": map[string]interface{}{
						"type":        "object",
						"description": "动作参数（create 与类型独有动作用；键见 admin_child_help）",
					},
					"meta": map[string]interface{}{
						"type":        "object",
						"description": "附加元数据（透传给该类型的 Admin，内核不解释）",
					},
				},
				"required": []string{"action"},
			},
		},
	})

	// ② admin_child_help：技术细节收敛在这里（不进主工具描述）
	helpTypeEnum := append([]string(nil), typeEnum...)
	helpTypeEnum = append(helpTypeEnum, "")
	out = append(out, map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name": "admin_child_help",
			"description": "查看某个子 agent 类型的完整用法：创建参数、类型独有动作、" +
				"以及该类型的注意事项。留空 type 列出全部类型的一行摘要。",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"type": map[string]interface{}{
						"type":        "string",
						"enum":        helpTypeEnum,
						"description": "要查看的类型（留空 = 全部摘要）",
					},
				},
			},
		},
	})

	// ③ admin_child_list：列出类型与实例
	out = append(out, map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "admin_child_list",
			"description": "列出已注册的子 agent 类型及其存活实例（等价于 admin_child(action=list)）。",
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
	})

	return out
}

// Help 渲染 admin_child_help 的正文。
func Help(host Host, typeName string) string {
	typeName = strings.TrimSpace(typeName)
	if typeName == "" {
		types := Types()
		if len(types) == 0 {
			return "没有已注册的子 agent 类型。"
		}
		var b strings.Builder
		b.WriteString("已注册子 agent 类型:\n")
		for _, t := range types {
			d, _ := Lookup(t)
			desc := d.Description
			if desc == "" {
				desc = "（无说明）"
			}
			fmt.Fprintf(&b, "  - %s — %s\n", t, desc)
		}
		b.WriteString("\n在某类型上调用 admin_child_help(type=\"<名字>\") 查看它的创建参数与动作。")
		return b.String()
	}

	d, ok := Lookup(typeName)
	if !ok {
		return fmt.Sprintf("类型 %q 未注册（可用: %v）", typeName, Types())
	}

	var b strings.Builder
	fmt.Fprintf(&b, "类型 %s\n", d.Type)
	if d.Description != "" {
		fmt.Fprintf(&b, "说明: %s\n", d.Description)
	}
	b.WriteString("\n【通用动作（内核实现）】\n")
	b.WriteString("  list / create / inspect / send / destroy / destroy_all / restart\n")

	if len(d.Actions) > 0 {
		b.WriteString("\n【类型独有动作】\n")
		for _, a := range d.Actions {
			fmt.Fprintf(&b, "  %s — %s\n", a.Name, a.Description)
			if len(a.Args) > 0 {
				keys := make([]string, 0, len(a.Args))
				for k := range a.Args {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if m, ok := a.Args[k].(map[string]interface{}); ok {
						desc, _ := m["description"].(string)
						fmt.Fprintf(&b, "      %s — %s\n", k, desc)
					}
				}
			}
			if len(a.Required) > 0 {
				fmt.Fprintf(&b, "      必填: %v\n", a.Required)
			}
		}
	}

	if d.Help != nil && host != nil {
		if extra := strings.TrimSpace(d.Help(host)); extra != "" {
			b.WriteString("\n【该类型的注意事项】\n")
			b.WriteString(extra)
			if !strings.HasSuffix(extra, "\n") {
				b.WriteString("\n")
			}
		}
	}

	briefs := host.ListChildren(typeName)
	fmt.Fprintf(&b, "\n【当前实例】%d 个\n", len(briefs))
	for _, x := range briefs {
		fmt.Fprintf(&b, "  - %s [%s] inputch=%v\n", x.ID, x.State, x.InputChs)
	}
	return b.String()
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
