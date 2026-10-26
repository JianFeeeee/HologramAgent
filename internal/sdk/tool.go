package sdk

// ToolSource 描述内核工具注册表/执行器的只读视图（由 StageHost 实现）。
// 用接口而非具体类型，避免 sdk 依赖内核包（内核包反向依赖 sdk）。
type ToolSource interface {
	GetToolDefs() []ToolDef
	ToolDef(name string) *ToolDef
	ExecuteTool(name string, args map[string]interface{}) (interface{}, error)
}

// ToolAPI exposes the kernel tool registry and executor
// (StageHost for plugin tools + IOManager for device/channel tools).
type ToolAPI interface {
	// GetToolDefs returns all tools registered on the stage host (plugin tools).
	GetToolDefs() []ToolDef
	// GetAllTools returns all tools exposed by IO devices/channels.
	GetAllTools() []ToolDef
	// CanUse 报告「执行该工具是否被授权」。
	//
	// 存在的理由（D4）：设备类工具的授权闸原本只存在于 core 的
	// executeToolCallInner，即**「agent 收到模型 tool_call」那条路径**。
	// 而 ExecuteTool 是**另一条**独立入口，不经那道闸 —— 于是凡是走
	// ToolAPI 的调用都能绕过 AllowedOutputs。实测范围不止序列：
	// cli 插件的 /terminal 就直接经 ToolAPI 调 agentcli 的终端工具
	// （cli/plugin.go:1038 自陈"ToolAPI 已允许跨插件调用工具"）。
	//
	// 语义与内核那道闸**必须一致**（core 的 TestCanUseAgreesWithInnerPath
	// 钉住这一点），否则两条路径判定不同同样是漏洞。
	//
	// 零值实现返回 true：未实现者行为不变（存量插件与测试替身不受影响）。
	CanUse(toolName string, args map[string]interface{}) bool
	// ToolDefByName 按名字查任一来源（StageHost 插件工具 / IO 设备工具 /
	// 内核内置工具）的声明。
	// 插件需要它来在**运行前**判断目标是否存在、是否并发安全 ——
	// 而工具是动态注册的，"不存在"是常态（见 seq 包的 missing 策略）。
	// 查不到返回 nil（调用方按"不存在"处理，不得 panic）。
	ToolDefByName(name string) *ToolDef
	// ExecuteTool executes a tool by name, resolving across the stage host first.
	ExecuteTool(name string, args map[string]interface{}) (interface{}, error)
}

// ---------------------------------------------------------------------------
// 内核内置工具的注册面（方案 B）
// ---------------------------------------------------------------------------
//
// 背景：`memory_*` / `knowledge_*` / `doc_*` / `person_*` 这 20+ 个工具是
// **内核内置**的，在 core.executeToolCallInner 里按**前缀分派**，
// 从不进 StageHost / IOManager ⇒ 插件经 ToolAPI 既查不到也调不了。
// 真机实跑实证：seq_run 报「工具 knowledge_list 不存在或未注册」，
// 而同一轮模型直接调 knowledge_list 是成功的。
//
// 为什么用**晚绑定注入**而不是让 toolImpl 直接依赖 core：
// toolImpl 在 internal/sdk，而内置工具的执行依赖 core 的 *Agent
// （它持有 memory/knowledge/store 等状态，且这些状态**逐 agent 不同**
// —— 驻留子是轻量内核，memory 为 nil）。sdk 不能依赖 core（方向反了），
// 且 ToolAPI 是**全局单例**，无法天然携带 per-agent 状态。
//
// 注入方（core/bootstrap）负责提供两个函数；未注入时行为与今天完全一致
// （内置工具在 ToolAPI 上不可见），故存量插件与测试替身不受影响。

// BuiltinToolDef 是内核内置工具的声明。
type BuiltinToolDef struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
	// ParallelSafe 与 ToolDef.ParallelSafe 同义：声明此工具可被并发执行。
	// 零值 false = 默认串行（保守）。
	//
	// ⚠️ 内核曾经没有这个字段，于是内置工具的并发声明无处安放 ——
	// 先后试过"内核里一张硬编码 map"和"往 required 变参塞字符串"两种错做法。
	// 声明项必须落在**工具自己的结构体**上，与 NoMemory 同构。
	ParallelSafe bool
	// Serial 声明此工具必须串行，优先于 ParallelSafe。
	Serial bool
	// Timeout 声明本工具单次执行允许的最长时间（如 "3m"）。
	//
	// 与 ToolDef.Timeout 同义（对外插件 / 内置插件两条路都要能声明）。
	// 留空 = 内核默认 60s。
	Timeout string
	// NoMemory 声明本工具的**输出**不参与记忆计算（向量化/关键词/蒸馏），
	// 但原文仍留在上下文的 ToolResults 里可读。
	//
	// 与 ToolDef.NoMemory 同义（对外插件经 proc 桥、内置工具经这里，
	// 两条路必须能声明同一件事 —— 否则内置工具的输出无法抑制）。
	//
	// The coldcontext_query result includes recalled event text, so it must be marked no-memory.
	//   那些正文**已经在记忆体系里**（来自 L2/L3），若工具的返回文本再
	//   参与一次向量化与蒸馏，就等于把同一内容重新注入记忆 ——
	//   与「回填事件不再归档」是同一个自激回路的两个入口。
	NoMemory bool
}

// ToSchema 转成下发给模型的 function schema。
func (d BuiltinToolDef) ToSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        d.Name,
			"description": d.Description,
			"parameters":  d.Parameters,
		},
	}
}

// ConcurrencySafe 报告该工具是否可并发执行。
// Serial 优先：显式声明"必须串行"不允许被 ParallelSafe 或默认值覆盖。
func (d BuiltinToolDef) ConcurrencySafe() bool {
	return d.ParallelSafe && !d.Serial
}

// BuiltinProvider 由内核注入（导出：core 需实现它）。
type BuiltinProvider interface {
	// Defs 返回当前 agent 可见的内置工具声明（**已按运行期状态门控**）。
	Defs() []BuiltinToolDef
	// Exec 执行一个内置工具，返回给模型看的文本。
	Exec(name string, args map[string]interface{}) (string, error)
}

var builtinProv BuiltinProvider

// SetBuiltinProvider 注入内核内置工具的查询/执行面。
//
// 传 nil 表示撤销注入（测试隔离用）。注入后，内置工具对插件可见可调。
func SetBuiltinProvider(p BuiltinProvider) { builtinProv = p }

// builtinDefs 返回注入方提供的内置工具声明（未注入时为空）。
func builtinDefs() []BuiltinToolDef {
	if builtinProv == nil {
		return nil
	}
	return builtinProv.Defs()
}
