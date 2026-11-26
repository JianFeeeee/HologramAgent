package agent

// kernel_tooldefs.go —— **内核设施类工具的声明**（schema）。
//
// ## 归属判据（用户裁定 2026-11-19）
//
// 「这个工具读的是不是某个类型的私有状态？」
//
//	llm_*            读 kernelapi.LLMClient + ProviderManager     → 基座
//	plgreload        读插件注册表                                  → 基座
//	get_plugin_tools 同上                                          → 基座
//	output_*         读基座 io                                     → 基座（output.go 已执行）
//	input_channels   读基座 io                                     → 基座
//	secret_*         读 SecurityGate.Store()                       → 基座（secret_tooldefs.go）
//
// 它们与"是主 agent 还是驻留子/rt/worker"无关。留在 `mainagent` 的后果：
// rt / worker 拿不到（它们没有那些私有状态），只能各写一份重复实现 ——
// `worker/worker.go` 里就有一份 `ExecuteLLMTool`（第二份事实来源）。

import "fmt"

// KernelToolDefs 返回**内核设施类**工具的声明。
//
// 按运行期状态门控（与私有两族的做法一致）：
//
//	llm_*    LLM 设施已装配才有意义
//	plgreload / get_plugin_tools   插件注册表已装配
func KernelToolDefs(a *Agent) []interface{} {
	if a == nil {
		return nil
	}
	mk := func(name, desc string, props map[string]interface{}, required ...string) map[string]interface{} {
		if props == nil {
			props = map[string]interface{}{}
		}
		return map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": desc,
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": props,
					"required":   required,
				},
			},
		}
	}
	var out []interface{}
	if a.llm != nil {
		out = append(out,
			mk("llm_list_sources",
				"列出所有可用的 LLM 源（如 deepseek、openai、ollama），每个源有对应的 Lua 适配器和配置。如需切换 LLM 源，请使用 llm_set_source。",
				nil),
			mk("llm_set_source",
				"切换当前 LLM 源到指定名称。变更立即生效，后续对话将使用新的 LLM 源。源名称可通过 llm_list_sources 查看。",
				map[string]interface{}{
					"name": map[string]interface{}{"type": "string", "description": "LLM 源名称"},
				}, "name"),
		)
	}
	if a.pluginRegForStatus() != nil {
		out = append(out,
			mk("plgreload", "重载 plugins/ 目录的所有插件。扫描目录变更，原子化替换 IO 设备。", nil),
			mk("get_plugin_tools",
				"Get a plugin's full tool definitions (name / parameters / purpose). Pass the plugin name as plugin_name "+
					"(see the [Available tool capabilities] list in the system prompt). Omit it to get a summary of tools for every plugin.",
				map[string]interface{}{
					"plugin_name": map[string]interface{}{"type": "string", "description": "插件名（省略则列出全部插件）"},
				}),
		)
	}
	return out
}

var _ = fmt.Sprintf
