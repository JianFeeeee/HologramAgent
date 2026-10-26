package kernelmod

// 设施名常量（依赖图上被引用的名字）。
//
// ★ 为何用常量而不是裸字符串：拼错依赖名时**编译不过**，
// 而不是"永远等不到就绪"（运行期表现是静默挂起 / 静默拿到 nil，极难排查）。
//
// ★ 为何放在 core.go 之外：用户要求"根目录只有 core.go"，
// 而 core.go 专注注册表职责。设施名是**模块间的协议**，与注册表分开更清晰。
const (
	// ── 插件可依赖的设施（与公开 SDK 的 sdk.Facility* **逐字一致**）──
	//
	// ★ 判据 TestFacilityNamesMatchPublicSDK 守这个一致性。
	// 这些名字会出现在 plugin.json 的 requires_facilities 里。
	FacConfig     = "config"
	FacKernelIO   = "kernel-io"
	FacMemory     = "memory"
	FacLua        = "lua"
	FacSupervisor = "supervisor"
	FacTracker    = "tracker"
	FacTextMem    = "textmem"
	FacLLM        = "llm"
	FacDocStore   = "docmem"
	FacKnowledge  = "knowledge"
	FacMultimodal = "multimodal"
	FacONNX       = "onnx"
	FacStageHost  = "stagehost"
	FacAgent      = "agent"

	// ── 内核**内部**顺序标记（**不是**给插件的设施）──
	//
	// ★ 为什么不与上面的混在一起：这两类东西的**消费者不同**。
	//   · 上面的：插件按它声明启动时机（对外契约，必须进公开 SDK）
	//   · 这里的：只用于**内核模块之间**表达顺序（如"迁移必须先于扫盘"）
	//
	// 混在一起会让"内核内部标记"被误当成"插件可选设施"
	// ⇒ 插件可能声明它（然后永远等不到 —— 因为它的就绪时机是装配中途，
	// 而插件提交发生在装配**完成之后**……实际会立刻满足，但语义是错的：
	// 它不该成为插件的契约）。
	//
	// 判据 TestFacilityNamesMatchPublicSDK 只比对上面那组 ⇒ 天然拦住
	// "把内部标记泄漏成对外契约"。

	// FacMigrated 标记"知识库目录迁移已完成"（用于让 store 排在迁移之后）。
	FacMigrated = "knowledge-migrated"

	// FacVersion 标记"内核版本自检与升级回滚判定已完成"（update 模块提供）。
	//
	// 语义：读到它时，ks.Update 里的版本已经是**回滚后的最终版本**。
	// 需要按版本行事的模块（healthcheck 上报版本、pluginhost 判插件兼容性）
	// 应 Requires 它，否则可能读到升级前那个已被回滚的版本。
	//
	// 为什么在内部标记段：它是内核自用的启动时序事实，不是插件可选的
	// 外部契约（插件声明它只会立刻满足，没有意义）。
	FacVersion = "kernel.version"

	// FacSecurity 是**安全防火墙**设施（security 模块提供）。
	//
	// ★ 它不是"某个能力"，而是横跨 Core 与全部外部模块的**数据面闸门**：
	//   Core → 外部、外部 → Core 的每一条数据流都要经过它（显式代理）。
	//   需要被安全审查才能访问外部模块的模块声明依赖它。
	FacSecurity = "security"

	// FacDevice 是**设备/工具数据面**设施（device 模块提供）。
	//
	// 统一拥有：工具注册与分发、inputch、outputch、设备登记。
	// 它不是"新的一层存储"，而是模型可见能力的唯一入口。
	FacDevice = "device"
)
