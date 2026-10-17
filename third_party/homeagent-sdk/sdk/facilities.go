// Package sdk —— 内核**设施名**（插件声明启动时机时用的名字）。
//
// ## 用途
//
// 插件在 `plg.json` / `plugin.json` 里声明 `requires_facilities`，
// 内核据此决定**何时拉起它**（设计：kernel-modularization-design.md §2.6）：
//
//	不声明              ⇒ 等内核**完全**初始化（默认；存量插件零改动）
//	声明 ["config"]     ⇒ config 就绪即拉起（**尽早启动**）
//	声明 ["config","llm"]⇒ 两者都就绪才拉起
//
// ## ★ 为什么这些常量必须在**公开 SDK** 里
//
// 它们原本只定义在内核的 `internal/kernelmod`（Go 的 internal 规则禁止
// 外部包 import）⇒ 插件作者**无从得知**能写哪些名字，只能照文档抄，
// 而抄错（如 "llmservice" vs "llm"）的后果是：
//
//	插件**永远等不到就绪** ⇒ **永不启动**，且无报错。
//
// 这正是本仓反复吃亏的"静默降级"形态。把名字放进公开 SDK 后：
//
//	· 插件作者 import 常量 ⇒ **拼错是编译错误**（而不是运行期静默）
//	· 且这是内核与插件之间的**契约**，理应公开
//
// ## 内核侧的一致性
//
// 内核在 `internal/kernelmod/facilities.go` 定义同名常量，
// 判据 `TestFacilityNamesMatchPublicSDK` 保证两边**逐字一致**
// （防止改一处忘另一处 —— 那会让插件声明的名字悄悄失效）。
package sdk

// 内核设施名（模块 Provides 的名字）。
//
// ★ 这些是**字符串值**，与内核 internal/kernelmod/facilities.go 必须逐字一致。
const (
	// FacilityConfig 是配置快照（cfg）就绪。
	FacilityConfig = "config"
	// FacilityKernelIO 是 IOManager + EventBus 就绪。
	FacilityKernelIO = "kernel-io"
	// FacilityMemory 是图记忆（含索引/社交图/蒸馏）就绪。
	FacilityMemory = "memory"
	// FacilityLua 是 Lua 协议适配 VM 就绪。
	FacilityLua = "lua"
	// FacilitySupervisor 是代理生命周期守护就绪。
	FacilitySupervisor = "supervisor"
	// FacilityTracker 是变更追踪（overlayfs）就绪。
	FacilityTracker = "tracker"
	// FacilityTextMemory 是文本记忆就绪。
	FacilityTextMemory = "textmem"
	// FacilityLLM 是多源 Provider 管理就绪。
	FacilityLLM = "llm"
	// FacilityDocStore 是文档记忆就绪。
	FacilityDocStore = "docmem"
	// FacilityKnowledge 是知识库就绪。
	FacilityKnowledge = "knowledge"
	// FacilityMultimodal 是多模态向量空间就绪。
	FacilityMultimodal = "multimodal"
	// FacilityONNX 是依存句法分析器就绪。
	FacilityONNX = "onnx"
	// FacilityStageHost 是阶段管道 + 插件注册表就绪。
	FacilityStageHost = "stagehost"
	// FacilityAgent 是主 Agent 就绪（**最晚**的一个，通常在插件加载前）。
	FacilityAgent = "agent"
)

// KnownFacilities 返回本 SDK 已知的全部设施名（供工具链校验插件声明）。
//
// ★ 它让**打包期**就能发现"插件声明了不存在的设施名" —— 比运行期发现早得多
// （运行期发现意味着用户已经装上了那个永不启动的插件）。
func KnownFacilities() []string {
	return []string{
		FacilityConfig, FacilityKernelIO, FacilityMemory, FacilityLua,
		FacilitySupervisor, FacilityTracker, FacilityTextMemory, FacilityLLM,
		FacilityDocStore, FacilityKnowledge, FacilityMultimodal, FacilityONNX,
		FacilityStageHost, FacilityAgent,
	}
}
