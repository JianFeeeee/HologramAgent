package agent

// kernel_family_register.go —— **内核族工具**在基座登记（声明 + 归属 + 执行体齐全）。
//
// # 判准（用户 2026-11-27）
//
//	「凡是非类型私有工具，都应该由基座呈现为公共工具表（差异化的）」
//
// 所以内核族的**三件事都归基座**，不该有任何一件挂在类型上：
//
//	声明     KernelToolDefs / SecretToolDefs / registry.Tool.Properties
//	归属     registry.Tool.Owner = "" ⇒ "core"
//	执行体   registry.Tool.Handle ⇒ 直接调本实例的方法
//
// # 此前错在哪（真实缺陷）
//
// 内核族的登记曾住在 `mainagent/tools_kernel_family.go`，并且用
// `OwnershipOnly + DeclaredOnly` 把一个**完整工具**拆成"只有归属"和
// "只有声明"两个残片：
//
//	OwnershipOnly  只登记归属，声明由类型渲染
//	DeclaredOnly   只登记声明，执行体在基座设施
//
// 两个标记都是表形态的漏洞 —— 工具表本该是"名字 + 声明 + 归属 + 执行体"
// 齐全的唯一一张表。拆开的后果是执行走了一条**平行的前缀路由**
// （`toolroute.Dispatch` + `BuiltinToolHandlers`），而那条路由的族处理器
// 只有一个类型注册过（`SetBuiltinTools` 全仓 1 个调用者）。
//
// 真内核对照实测（2026-11-27）：
//
//	[main:main]   tool llm_list_sources result: 可用 LLM 源:
//	[resident:r1] tool llm_list_sources result: Tool ... does not exist or is not registered
//
// 同一个内核工具，父能执行子不能 —— 而按"父子对内核唯一的区别是父被默认
// 拉起"，这不该发生。
//
// 收进基座一张表之后，`dispatch` 的第 ① 步（registry.DispatchTool）就命中，
// 不再需要第 ② 步那条前缀路由。

import (
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// kernelFamilyTool 是一个内核族工具的登记项（声明 + 执行体）。
//
// Handle 用**方法表达式**（`(*Agent).Method`）而不是闭包：登记发生在包
// init 里，那时没有实例；而 `registry.ToolCtx.Host` 在调用时就是本实例
// （见 `childadmin_register.go` 的同款形态）。
type kernelFamilyTool struct {
	name       string
	desc       string
	props      map[string]interface{}
	required   []string
	outputGate bool
	handle     func(c registry.ToolCtx, tc agentAPI.ToolCall) string
}

func init() {
	for _, t := range kernelFamilyTools() {
		if err := registry.RegisterTool(registry.Tool{
			Name:        t.name,
			Description: t.desc,
			Side:        registry.SideAny, // 内核工具：父子两侧一致
			Owner:       "",               // 空 ⇒ 注册表填 "core"
			Properties:  t.props,
			Required:    t.required,
			OutputGate:  t.outputGate,
			Handle:      t.handle,
		}); err != nil {
			// 装配期失败是计划错误（两个包声称同一工具）：panic 是刻意的。
			panic("agent/kernel-family: " + err.Error())
		}
	}
}

// agentOf 从 ToolCtx 取出本实例（与 childadmin 的取法一致）。
func agentOf(c registry.ToolCtx) *Agent {
	a, _ := c.Host.(*Agent)
	return a
}

// kernelFamilyTools 返回**全部内核族工具**的登记项。
//
// ★ 这是内核族名字的**唯一清单**：声明渲染（`KernelToolDefs` /
//
//	`SecretToolDefs`）也从这里取，两处不再手抄。
func kernelFamilyTools() []kernelFamilyTool {
	out := []kernelFamilyTool{
		{
			name: "llm_list_sources",
			desc: "列出所有可用的 LLM 源（如 deepseek、openai、ollama），每个源有对应的 Lua 适配器和配置。如需切换 LLM 源，请使用 llm_set_source。",
			handle: func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
				return agentOf(c).ExecuteLLMTool(tc)
			},
		},
		{
			name: "llm_set_source",
			desc: "切换当前 LLM 源到指定名称。变更立即生效，后续对话将使用新的 LLM 源。源名称可通过 llm_list_sources 查看。",
			props: map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "LLM 源名称"},
			},
			required: []string{"name"},
			handle: func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
				return agentOf(c).ExecuteLLMTool(tc)
			},
		},
		{
			name: "secret_submit",
			desc: "提交一个密钥/凭据到内核变量库（加密落盘）。后续用 secret_list 查看、secret_forget 删除。",
			props: map[string]interface{}{
				"alias": map[string]interface{}{"type": "string", "description": "变量别名（后续按它取用）"},
				"value": map[string]interface{}{"type": "string", "description": "变量值（明文只在此刻出现）"},
			},
			required: []string{"alias", "value"},
			handle: func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
				return agentOf(c).ExecuteSecretTool(tc)
			},
		},
		{
			name: "secret_list",
			desc: "列出变量库里已有的变量别名（**不返回值**）。",
			handle: func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
				return agentOf(c).ExecuteSecretTool(tc)
			},
		},
		{
			name: "secret_forget",
			desc: "从变量库删除一个变量。",
			props: map[string]interface{}{
				"alias": map[string]interface{}{"type": "string", "description": "要删除的变量别名"},
			},
			required: []string{"alias"},
			handle: func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
				return agentOf(c).ExecuteSecretTool(tc)
			},
		},
		{
			name: "plgreload",
			desc: "重新加载全部插件（改插件代码/配置后使用）。",
			handle: func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
				return agentOf(c).ExecutePluginReload()
			},
		},
		{
			name: "get_plugin_tools",
			desc: "列出插件提供的工具；传 plugin_name 只看该插件。",
			props: map[string]interface{}{
				"plugin_name": map[string]interface{}{"type": "string", "description": "插件名（留空=全部）"},
			},
			handle: func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
				name, _ := tc.Arguments["plugin_name"].(string)
				return agentOf(c).ExecuteGetPluginTools(name)
			},
		},
		{
			name: "output_list_channels",
			desc: "列出可用的输出通道（每个通道能发什么、需要什么权限）。",
			// OutputGate：它会让模型显式点名通道 ⇒ 属主动输出门。
			// 标记在登记的同一行给出，不靠名字前缀猜（改名字即静默失效）。
			outputGate: true,
			handle: func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
				return agentOf(c).ExecuteOutputListChannels()
			},
		},
		{
			name: "input_channels",
			desc: "查看输入通道（inputch）的注册与划拨情况。view: all=全部已注册（默认）| mine=划给本 agent 的 | unassigned=尚未划出的 | by_agent=按归属分组的划分总览 | detail=单个详情",
			props: map[string]interface{}{
				"view": map[string]interface{}{
					"type": "string",
					"description": "all=全部已注册（默认）| mine=划给本 agent 的 | " +
						"unassigned=尚未划出的 | by_agent=按归属分组的划分总览 | detail=单个详情",
					"enum": []string{"all", "mine", "unassigned", "by_agent", "detail"},
				},
				"name": map[string]interface{}{
					"type":        "string",
					"description": "view=detail 时必填：inputch 名",
				},
			},
			handle: func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
				return agentOf(c).ExecuteInputChannels(tc)
			},
		},
	}
	return out
}
