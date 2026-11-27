package agent

// publictooldefs.go —— 基座给类型提供**公有工具清单**（已过滤）。
//
// ## 三类工具，归属各不同
//
//	公有  基座直接按差异呈现 ← 本文件
//	槽    基座直接按差异呈现（父为子划定的范围）
//	私有  类型自己执行、自己追加（基座不认识）
//
// 类型渲染本轮工具表 = `a.PublicToolDefs()` + 自己的私有表。
//
// ## 为什么必须由基座给
//
// 公有面的来源不止一处：
//
//	registry 表      内核族 / admin_child / output_* / llm_* / input_channels
//	IO 设备工具      通道与设备能力
//	插件工具          插件经 ToolRegistry 注册的那些
//
// 此前**每个类型自己拼**这几处，结果各不相同：
//
//	mainagent  四处全取（789 行 buildToolDefs）
//	resident   只取 registry（连 IO 设备工具都看不到）
//	worker     取 registry 后自己再筛一遍
//	插件类型   一处都不取
//
// 四个类型看得到的公有面因此互不相同 —— 而它们**本该同构**（docs §0.3：
// 类型之间的差异来自声明，不来自各自拼装）。
//
// ## 过滤在这里，不在类型的出口
//
// 本函数返回的就是**已按 Side + allow/deny 过滤过**的清单（用户裁定
// 2026-11-25：「基座给类型提供的是直接过滤后的清单」）。类型拿到之后
// 只对自己的私有工具再判一次 —— 公有面不再经第二遍过滤。

import (
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// PublicToolDefs 返回**本 agent 可见的公有工具清单**（已过滤）。
//
// 顺序固定：内核族声明 → IO 设备工具 → 插件工具 → registry 表。
// 顺序稳定的意义：工具表每轮重建，顺序乱跳会让上游的 prefix cache 失效。
func (a *Agent) PublicToolDefs() []interface{} {
	if a == nil {
		return nil
	}
	var tools []interface{}

	// ★ 内核族工具的声明**不在这里**：它们在 `registry.Tool` 登记项里
	//   （名字 + 声明 + 归属 + 执行体同一处），由 ④ 的 RegistryToolDefs 渲染。
	//   此前这里另有一份 `KernelToolDefs(a)` ⇒ 同一工具名出现两次 ⇒
	//   上游 400（`Tool names must be unique`）。真内核实测 112 条里 7 个重复。

	// ② IO 设备工具（通道 / 设备能力）。
	//
	// ★ 必须走 `ListChannels()` 而**不是** `GetAllTools()`：
	//
	//	GetAllTools  只遍历 **自己的** m.devices，**无父回退**
	//	ListChannels 先取自己的，再并入 parent.ListChannels()（按通道名去重）
	//	ExecuteToolCtx 自己有就自己执行，没有则向 parent 回退
	//
	// 而驻留子的 io 是「空 IOManager + 共享 ChannelRegistry + SetParentIO(父)」
	// —— 设备全在父那边。于是 GetAllTools() 对子恒为空（连它能执行的
	// 设备工具都列不出来），而 ListChannels() 能列出父的通道与它们的工具。
	//
	// 子"能执行父的设备工具"与"看得见父的设备工具"必须是同一件事：
	// 只给执行不给渲染 = 模型永远想不到调。
	if iom := a.IO(); iom != nil {
		seen := map[string]bool{}
		for _, ch := range iom.ListChannels() {
			for _, td := range ch.Tools {
				if seen[td.Name] {
					continue // 同一工具可能被多个通道声明（去重按工具名）
				}
				seen[td.Name] = true
				tools = append(tools, llmToolSchema(td.Name, td.Description, cleanParamsOf(td.Parameters)))
			}
		}
	}

	// ③ 插件工具：住在 `ToolRegistry`（插件注册进来的工具，与阶段管道无关）。
	if tr := a.ToolRegistry(); tr != nil {
		for _, td := range tr.GetToolDefs() {
			tools = append(tools, llmToolSchema(td.Name, td.Description, cleanParamsOf(td.Parameters)))
		}
	}

	// ④ registry 表（按本 agent 的侧别渲染）。
	tools = append(tools, a.RegistryToolDefs()...)

	// ⑤ 主动输出通道工具（`output_send__<通道>`）。
	//
	// ★ 它们读的是**基座**字段（`a.IO()` 的通道表 + `a.IsOutputAllowed()` 的
	//   授权事实），与"是哪个类型"无关 —— 所以声明归基座，类型不声明。
	//
	// `output_send__<通道>` 是按通道**动态生成**的（通道由插件登记，没有静态
	// 名字），因此它是唯一必须按前缀判定的形态；`_help` 跟随同一条规则。
	//
	// 未授权（父收窄了子的输出能力）就不生成 —— 模型看不到它，自然不会调。
	for _, ch := range a.outputSendChannels() {
		capStr := a.IO().GetChannelCapabilities(ch.Name).String()
		desc := ch.Description
		if desc == "" {
			desc = ch.Name + " 输出通道"
		}
		tools = append(tools, toolSchemaOf("output_send__"+ch.Name,
			desc+"。能力: "+capStr+"。\n"+
				"payload 是消息载荷，可以是**一条字符串**，也可以是**数组**（一次发多条）。\n"+
				"type 是载荷类型，meta 是发送元数据 —— 两者的格式与取值由该通道定义，"+
				"用 output_send__"+ch.Name+"_help 或对应 skill 查看。\n"+
				"type/meta 只给一个值时，会应用到 payload 的每一条。",
			map[string]interface{}{
				"payload": map[string]interface{}{
					"description": "消息载荷。字符串=发一条；数组=一次发多条（按数组顺序送达）",
					"oneOf": []interface{}{
						map[string]interface{}{"type": "string"},
						map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
				},
				"type": map[string]interface{}{
					"description": "载荷类型。字符串=全部同类型；数组=与 payload 一一对应（数量必须相等）。缺省按通道默认",
					"oneOf": []interface{}{
						map[string]interface{}{"type": "string"},
						map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
				},
				"meta": map[string]interface{}{
					"description": "发送元数据（JSON 字符串）。只给一个值时广播给所有条；数组则一一对应。格式见 _help 或对应 skill",
					"oneOf": []interface{}{
						map[string]interface{}{"type": "string"},
						map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
				},
			}))
		tools = append(tools, toolSchemaOf("output_send__"+ch.Name+"_help",
			"查看 "+ch.Name+" 输出通道的 meta 格式说明和 type 枚举", map[string]interface{}{}))
	}

	// ★ 过滤在这里做**一次**：类型拿到的就是最终可见的公有面。
	return a.FilterToolDefsForRender(tools)
}

// llmToolSchema 拼一条 OpenAI 风格的工具声明。
func llmToolSchema(name, desc string, params map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        name,
			"description": desc,
			"parameters":  params,
		},
	}
}

// toolSchema 拼一条声明，并把 properties 包成合法 JSON Schema。
//
// ★ OpenAI 要求 `parameters` 顶层**必须有 `type: object`**：
//
//	{"type":"object","properties":{...}}
//
// 直接把"properties 那一层"当成 parameters 交上去，会得到
//
//	{"payload":{...},"type":{...},"meta":{...}}
//
// —— 顶层没有 type 也没有 properties，上游报
// `Invalid schema for function 'output_send__cli'` 并**拒绝整轮请求**。
// 这不是推测：新旧内核对照时新内核当场撞上（旧内核正常），
// 因为搬运 output_send__* 时把 toolDef 的包装层丢了。
func toolSchemaOf(name, desc string, properties map[string]interface{}) map[string]interface{} {
	params := map[string]interface{}{
		"type":       "object",
		"properties": properties,
	}
	return llmToolSchema(name, desc, params)
}

// cleanParamsOf 归一化一份参数表（互斥形态补 type=object）。
//
// ★ 与 mainagent 的 `cleanParams` 同一语义：空表要写成
// `{"type":"object"}`，否则部分上游拒收。
func cleanParamsOf(p map[string]interface{}) map[string]interface{} {
	if p == nil {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	if _, ok := p["type"]; !ok {
		if _, hasProps := p["properties"]; hasProps {
			p["type"] = "object"
		}
	}
	return p
}

// 供类型判断"是不是我自己的私有工具"时对照 IO 设备来源用。
var _ = agentIO.ToolDef{}

// outputSendChannels 返回**本 agent 被授权**的主动输出通道。
//
// 两个条件缺一不可：
//
//	通道类型是输出（DeviceOutput / DeviceIO）
//	父为本 agent 授权了该通道（IsOutputAllowed）
//
// 未授权就不生成工具 —— 模型看不到它，自然不会调（设计与 §4.4/R2 一致）。
func (a *Agent) outputSendChannels() []agentIO.ChannelInfo {
	if a == nil || a.IO() == nil {
		return nil
	}
	var out []agentIO.ChannelInfo
	for _, ch := range a.IO().ListChannels() {
		if ch.Type != agentIO.DeviceOutput && ch.Type != agentIO.DeviceIO {
			continue
		}
		if !a.IsOutputAllowed(ch.Name) {
			continue
		}
		out = append(out, ch)
	}
	return out
}
