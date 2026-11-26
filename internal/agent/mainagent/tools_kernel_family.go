package mainagent

// tools_kernel_family.go —— **内核设施类工具**的登记（2026-11-19 实测回归修复）。
//
// ## 事故：四个工具全废
//
// 真二进制实测（`/tmp/lt6`，2026-11-20）：
//
//	output_list_channels  → "不属于本 agent 的类型私有工具，也不在内核执行设施里"
//	llm_list_sources      → 同上
//	secret_list           → 同上
//	get_plugin_tools      → 同上
//
// ## 根因：两条断点
//
//  ① `llm_*` / `secret_*` / `plgreload` / `get_plugin_tools` **从没进过注册表**
//     ⇒ `OwnerOf(name)` 返回空 ⇒ 批路径判"不属于本类型" ⇒ 丢给内核设施
//     ⇒ 设施按序问 registry / toolroute / stageHost / io，四处都不命中
//     ⇒ 落到兜底那句"不存在或未注册"。
//
//  ② 即使进了注册表，`toolroute.Dispatch` 那条路也**永远是死的**：
//     `SetBuiltinTools` 全仓零调用 ⇒ `BuiltinTools()` 恒 nil。
//     而 `dispatch` 的 ② 步就是问它 —— 于是所有"族处理器"形态的工具
//     全部不可执行（`output_*` / `input_channels` 也在内）。
//
// ## 归位（用户裁定 2026-11-19 的判据）
//
// 「这个工具读的是不是本类型的私有状态？」
//
//	llm_*            读 AgentConfig.LLM / ProviderManager  → **基座**
//	secret_*         读 SecurityGate.Store()               → 基座
//	plgreload        读 PluginReg                          → 基座
//	get_plugin_tools 读 stageHost + io + 类型只读面        → 基座
//	output_*         读基座 io                             → 基座（已登记）
//	input_channels   读基座 io                             → 基座（已登记）
//
// 所以它们的 Owner 是**内核**（`registry.Tool.Owner` 空 = "core"），
// 由 `IsKernelFamilyTool` 判"交给内核设施执行"，本类型不自己跑。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// kernelFamilyTools 是**内核设施类**工具的名字（声明由基座渲染，
// `agent.KernelToolDefs` / `agent.SecretToolDefs`）。
//
// ★ 这张表只用于**归属判定**（谁执行），不用于渲染 —— 渲染仍归基座，
// 因为它们的 schema 读的是 `AgentConfig` 公有字段，与类型无关。
var kernelFamilyTools = []string{
	"llm_list_sources", "llm_set_source",
	"secret_submit", "secret_list", "secret_forget",
	"plgreload", "get_plugin_tools",
}

func init() {
	// 登记进注册表 ⇒ `OwnerOf` 有值（"core"），批路径据此判"不是本类型的"，
	// 交给内核设施；同时 `toolroute.Dispatch` 能查到它们。
	//
	// Handle 留空（DeclaredOnly）：执行体在基座
	// （`Agent.ExecuteLLMTool` / `ExecuteSecretTool` / `ExecutePluginReload` /
	//  `ExecuteGetPluginTools`），由 `toolroute` 按族转发。
	for _, name := range kernelFamilyTools {
		if err := registry.RegisterTool(registry.Tool{
			Name: name,
			// Description/Properties 留空：**声明由基座渲染**
			// （`agent.KernelToolDefs`），这里只登记"这个名字归内核"。
			Side:  registry.SideAny,
			Owner: "core",
			// 声明由**类型**渲染（`agent.KernelToolDefs` / `agent.SecretToolDefs`
			// —— 只有它知道 `core.llm.*` 与安全模块的运行期门控）。
			// 本表只让 `OwnerOf(name)` 查得到归属。
			OwnershipOnly: true,
			DeclaredOnly:  true,
		}); err != nil {
			panic("agent/tools: " + err.Error())
		}
	}
}

// IsKernelFamilyTool 报告某名字是否属于**内核设施类**（执行归内核）。
//
// ★ 判据是登记表里的 Owner (`core`)，不是名字前缀 —— 前缀判定在改名时
//
//	静默失效。本函数只是给"Owner == core"一个可读的名字。
func IsKernelFamilyTool(name string) bool {
	return registry.OwnerOf(name) == "core"
}
