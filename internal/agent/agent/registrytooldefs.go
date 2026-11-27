package agent

// registrytooldefs.go —— 把 registry 里登记的工具**渲染成 schema**。
//
// ★ 为什么在基座：工具表是**内核统一入口**（谁注册谁拥有，基座按侧别取表渲染）。
//   类型只调用它，不重复实现 —— 否则"模型看得到什么"会有第二份事实来源。

import (
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// RegistryToolDefs 渲染注册表里的工具（按**本 agent 的侧别**过滤）。
func (a *Agent) RegistryToolDefs() []interface{} {
	side := registry.SideParent
	if a != nil && a.IsChildAgent() {
		side = registry.SideChild
	}
	tools := registry.ToolsFor(side)
	out := make([]interface{}, 0, len(tools)+3)
	// ★ `admin_child` 三件套的声明是**每轮现算**的（type/action 枚举来自类型表，
	//   而类型可被插件在轮与轮之间注册/撤销）。表里只放执行体，schema 在这里取。
	freshAdmin := map[string]registry.ToolDef{}
	if side == registry.SideParent {
		for _, d := range ChildAdminToolDefs() {
			freshAdmin[d.Name] = d
		}
	}
	// ★ 内核族工具的**运行期门控**（原先在 `KernelToolDefs` / `SecretToolDefs`
	//   两个独立渲染函数里）。现在声明只在登记表这一处，门控也跟着搬来 ——
	//   否则会出现"两个渲染源都出同一个工具名"⇒ 上游 400
	//   （`Tool names must be unique`，实测真内核 112 条里 7 个重复）。
	//
	// 门控是**能力是否已装配**的事实：
	//
	//	llm_*            需要 LLM 句柄（裸实例/未装配 ⇒ 不该出现）
	//	plgreload/get_*  需要插件登记表
	//	secret_*         需要变量库
	debFace := a
	gate := func(name string) bool {
		if debFace == nil {
			return true
		}
		switch name {
		case "llm_list_sources", "llm_set_source":
			return debFace.llm != nil
		case "plgreload", "get_plugin_tools":
			return debFace.pluginRegForStatus() != nil
		case "secret_submit", "secret_list", "secret_forget":
			return debFace.SecretStore() != nil
		}
		return true
	}
	for _, t := range tools {
		if !gate(t.Name) {
			continue
		}
		if fd, ok := freshAdmin[t.Name]; ok {
			t.Description, t.Properties, t.Required = fd.Description, fd.Properties, fd.Required
		}
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": t.Properties,
					"required":   t.Required,
				},
			},
		})
	}
	_ = agentAPI.ToolCall{}
	return out
}

// outputSendPrefix 是动态生成的输出门工具名前缀。
//
// ★ 它**只在一处**（本函数下方）：`output_send__<通道>` 是按通道动态生成的，
//
//	没有登记点可以打标记，所以这是唯一必须靠前缀判定的地方 —— 也是唯一允许有的。
const outputSendPrefix = "output_send__"

// IsOutputGateTool 是「这个工具属于主动输出门吗」的**唯一判据**。
//
// rtchild / worker 需要剥掉它们（输出走别的管线），主 agent 需要保留它们。
// 判据 = 注册表标记（`registry.Tool.OutputGate`）+ 动态生成的那一族。
//
// ★ 为什么集中在这里：原先 rtchild 自己写 `strings.HasPrefix(name, "output_send__")
//
//	|| name == "output_list_channels"`，worker 写另一份禁令表 —— 两个类型
//	两份前缀判定，改名时各漏各的。
func IsOutputGateTool(name string) bool {
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, outputSendPrefix) {
		return true
	}
	for _, t := range registry.ToolsFor(registry.SideAny) {
		if t.Name == name && t.OutputGate {
			return true
		}
	}
	for _, t := range registry.ToolsFor(registry.SideParent) {
		if t.Name == name && t.OutputGate {
			return true
		}
	}
	for _, t := range registry.ToolsFor(registry.SideChild) {
		if t.Name == name && t.OutputGate {
			return true
		}
	}
	return false
}
