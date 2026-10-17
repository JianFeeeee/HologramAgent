package proc

import (
	"encoding/json"
	"strings"
)

// RPC 协议定义：控制面（§3.2 method id 平移为 method 名）。
//
// 帧格式：**换行分隔的 JSON**（NDJSON），双向复用同一对 stdio 管道。
//   内核 → 插件 stdin ：请求 / 响应
//   插件 → 内核 stdout：请求 / 响应
//
// 为什么不用 length-prefixed 二进制帧：工具调用结果中位数仅 93B（§2.5），
// JSON 序列化 3-8 µs 对比 LLM 单轮 2-8 秒占 0.0001%，可读性与可调试性更值。
// 大 payload（多媒体二进制）走共享内存 arena，不进 RPC 帧（§3.3 实验 10：18-22x）。

// 协议兼容性判据：**大版本号唯一**。
//
// 不自定协议号。插件的兼容性由双方各自的版本号表态：内核侧 meta.Version、
// 插件侧 sdk_version（SDK 版本跟随内核中版本，patch 恒为 .0）。
// 版本号唯一事实源就是这两个 meta.Version（见 docs/zh/ci-cd-runbook.md）。
//
// 判据是**主版本相同**：
//   - 1.x ↔ 1.y：兼容。1.0.0 之后全是纯追加，不调新面就不受影响。
//   - 跨主版本（如 1.x → 2.x）：拒绝建链，提示用配套 hmapdev 重编。
//     主版本跃迁正是**破坏性变更**的定义（1.0.0 从 C ABI 改子进程即属此列），
//     所以它天然是正确的分界，不需要另一个号来复述这件事。
//
// 布局一致性另由 stageFieldCount（描述符个数）把关：它是结构的派生事实，
// 也不是版本号。两者错配都在握手上显式失败，不半兼容运行。

// MajorVersion 取版本号的主版本段（"1.4.0" → "1"，"" → ""）。
func MajorVersion(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '.'); i >= 0 {
		return v[:i]
	}
	return v
}

// Direction 无需显式字段：靠 Method 是否为空区分请求与响应
// （与 clawhubadapter/sidecar 的成熟做法一致）。

// Request 是一次 RPC 调用。
//
// ID 语义：
//   - ID > 0  ：需要响应，调用方在 pending 表等待
//   - ID == 0 ：通知（fire-and-forget），被调方不得回响应
//
// 通知用于事件投递等不关心结果的路径（§2.4 约束 B：内核发通知绝不等待消费者）。
type Request struct {
	ID     uint64          `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response 是对 Request 的应答。Error 非空表示失败。
type Response struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// ---- kernel → plugin（内核调用插件；工具执行、Cleaner、stage、输出等）----
const (
	// MethodPluginInit 传插件名与配置，插件构造实例但不启动。
	MethodPluginInit = "plugin.init"
	// MethodPluginStart 插件注册工具/阶段/通道（其间会反向发起大量 core.* 调用）。
	MethodPluginStart = "plugin.start"
	// MethodPluginStop 优雅停止：插件侧先跑 RunStopHandlers 再 Stop()。
	MethodPluginStop = "plugin.stop"
	// MethodToolInvoke 执行插件工具。
	MethodToolInvoke = "tool.invoke"
	// MethodCleanerInvoke 在插件进程内执行工具或通道声明的 Cleaner。
	// Cleaner 是函数，不能随注册请求 JSON 序列化；内核保留 RPC 回调闭包，
	// 需要参与向量化/蒸馏时把原文送回插件执行真正的 Cleaner。
	MethodCleanerInvoke = "cleaner.invoke"
	// MethodStageInvoke 执行阶段处理器。数据经共享段传递，参数只带阶段名与段世代号。
	MethodStageInvoke = "stage.invoke"
	// MethodOutputInvoke 经插件输出通道发送。
	MethodOutputInvoke = "output.invoke"
	// MethodArenaAlloc 向内核申请一块共享内存（返回偏移与大小）。
	// MethodArenaFree 通知内核回收先前申请的共享内存。
	//
	// 这两个是**内部传输层接口**，不由插件开发者直接使用：共享内存是
	// 内核的内部实现，公开 SDK 仍是普通字符串/Map，模板运行时按
	// payload 大小自动选择内联 JSON 还是共享槽。
	MethodArenaAlloc = "arena.alloc"
	MethodArenaFree  = "arena.free"
	// MethodHandshake 建链首帧：交换协议版本、SDK 版本、共享段规格。
	MethodHandshake = "handshake"
)

// ---- plugin → kernel（51 个 method id 平移，§3.2）----
//
// 编号本身扔掉：不再维护"下一个可用 id 是 52"，加能力不用改两边常量表，
// 也不再出现 47 夹在 7 和 8 之间的历史痕迹。
const (
	// 注册面（原 case 1/2/3/4/46）
	MethodToolRegister   = "tool.register"   // 1  CORE_REGISTER_TOOL
	MethodStageRegister  = "stage.register"  // 2  CORE_REGISTER_STAGE
	MethodOutputRegister = "output.register" // 3  CORE_REGISTER_OUTPUT_CH
	MethodAPIRegister    = "api.register"    // 4  CORE_REGISTER_PLUGIN_API
	MethodInputRegister  = "input.register"  // 46 CORE_REGISTER_INPUT_CH

	// IO 注入（原 case 5/6/7/47）
	MethodIOInjectText      = "io.injectText"      // 5  CORE_INJECT_TEXT
	MethodIOInjectInterrupt = "io.injectInterrupt" // 6  CORE_INJECT_INTERRUPT_TEXT
	MethodIOInjectTextNoMem = "io.injectTextNoMem" // 7  CORE_INJECT_TEXT_NO_MEMORY
	MethodIOInjectSync      = "io.injectInputSync" // 47 CORE_INJECT_INPUT_SYNC
	// 带媒体的注入：blocks 随参数 JSON 一并过来，内核侧转成
	// payload["media_blocks"]，由 resolveInput 归一进统一输入主干。
	// 与 SetToolBlocks 的区别：这三个是「主动发起一轮带图的对话」，
	// 后者是「工具返回值里带图」，只能在工具调用内部用。
	MethodIOInjectMedia          = "io.injectMedia"
	MethodIOInjectMediaSync      = "io.injectMediaSync"
	MethodIOInjectInterruptMedia = "io.injectInterruptMedia"
	// MethodIOSetToolBlocks 多模态注入——今日 C ABI 侧是空实现（§1.4），
	// 子进程下二进制落 arena、描述符回传，首次真正可用。
	MethodIOSetToolBlocks = "io.setToolBlocks"

	// 生命周期（原 case 8）
	MethodLifecycleAutoRestart = "lifecycle.autoRestart" // 8 CORE_SET_AUTO_RESTART

	// 图记忆（原 case 9/10/11/12/13）
	MethodMemoryRecall     = "memory.recall"     // 9
	MethodMemoryCommit     = "memory.commit"     // 10
	MethodMemoryIntrospect = "memory.introspect" // 11
	MethodMemoryMerge      = "memory.merge"      // 12
	MethodMemoryPurge      = "memory.purge"      // 13

	// 文档记忆（原 case 14/32/33/34）
	MethodDocQuery  = "doc.query"  // 14
	MethodDocInsert = "doc.insert" // 32
	MethodDocRemove = "doc.remove" // 33
	MethodDocStats  = "doc.stats"  // 34
	// MethodDocInsertMedia 写入文档并关联媒体（附件带 data 则落盘去重，
	// 只带 digest 则引用已有内容）。
	MethodDocInsertMedia = "doc.insertWithMedia"

	// 知识库（原 case 15/35/36）
	MethodKnowledgeSearch = "knowledge.search" // 15
	MethodKnowledgeAdd    = "knowledge.add"    // 35
	MethodKnowledgeList   = "knowledge.list"   // 36
	// MethodKnowledgeAddMedia 写入知识并关联媒体（与 doc.insertWithMedia
	// 同形）。媒体成为一等节点参与跨模态召回；未接入多模态空间时与
	// knowledge.add 等价。
	MethodKnowledgeAddMedia = "knowledge.addWithMedia"

	// 文本记忆（原 case 41）
	MethodTextMemoryAppend = "textmemory.append" // 41

	// 设置（原 case 16/17/18/26/27/28/29/30/31/42/43/44/45/51）
	MethodSettingsGet         = "settings.get"         // 16
	MethodSettingsSet         = "settings.set"         // 17
	MethodSettingsRegisterDef = "settings.registerDef" // 18
	MethodSettingsGetCore     = "settings.getCore"     // 26
	MethodSettingsSetCore     = "settings.setCore"     // 27
	MethodSettingsListCore    = "settings.listCore"    // 28
	MethodSettingsGetPlugin   = "settings.getPlugin"   // 29
	MethodSettingsSetPlugin   = "settings.setPlugin"   // 30
	MethodSettingsListPlugin  = "settings.listPlugin"  // 31
	MethodSettingsList        = "settings.list"        // 42
	MethodSettingsDefs        = "settings.defs"        // 43
	MethodSettingsDump        = "settings.dump"        // 44
	MethodSettingsPlugins     = "settings.plugins"     // 45
	MethodSettingsDataDir     = "settings.dataDir"     // 51
	// 密文读写（强制加密，不看键名）—— 与 settings.set/get 的区别是
	// 这两个**必然**走加密路径，用于插件用中性键名存凭据的场景。
	MethodSettingsSetSecret = "settings.setSecret" // 52
	MethodSettingsGetSecret = "settings.getSecret" // 53

	// 变量库（全局共享，非 per-plugin；见 internal/security 与 corehandler_secrets.go）。
	//
	// 与 settings 的分工是本质的：settings 是**每个插件自己的**配置，
	// secrets 是**全进程共享**的变量库单例（主/子/驻留子 agent 与全部插件共用）。
	MethodSecretsAvailable = "secrets.available" // 32
	MethodSecretsStore     = "secrets.store"     // 33
	MethodSecretsList      = "secrets.list"      // 34
	MethodSecretsForget    = "secrets.forget"    // 35
	MethodSecretsFill      = "secrets.fill"      // 36
	MethodSecretsEncrypt   = "secrets.encrypt"   // 46
	MethodSecretsDecrypt   = "secrets.decrypt"   // 47

	// LLM 源（原 case 19/20/37）
	MethodLLMListSources   = "llm.listSources"   // 19
	MethodLLMSetSource     = "llm.setSource"     // 20
	MethodLLMCurrentSource = "llm.currentSource" // 37

	// 社交图（只读，原 case 21/22/38/39/40）
	MethodSocialGetPerson   = "social.getPerson"    // 21
	MethodSocialGetNetwork  = "social.getNetwork"   // 22
	MethodSocialGetTrait    = "social.getTrait"     // 38
	MethodSocialGetRelation = "social.getRelations" // 39
	MethodSocialListPersons = "social.listPersons"  // 40

	// 事件（原 case 23/24 —— 今日均为空实现「给不了」，
	// 子进程下经事件环 + eventfd 首次真正可用，见 §3.6/§3.8）
	MethodEventsSubscribe   = "events.subscribe"   // 23
	MethodEventsUnsubscribe = "events.unsubscribe" // 24

	// 插件管理（原 case 48/49/50）
	MethodPluginReloadOne  = "plugin.reloadOne"  // 48
	MethodPluginListLoaded = "plugin.listLoaded" // 49
	MethodPluginIsDisabled = "plugin.isDisabled" // 50
	// ---- 运行态启停（设计 §2.6.7）：与 isDisabled（持久态）分开 ----
	//
	// ★ 为何必须桥接：子进程插件走本桥，未在此登记的方法会被
	// corehandler 直接拒（"未知 method"）⇒ SDK 上加了方法但插件**调不通**。
	// 同进程插件（.so）直接调 SDK，不受影响 —— 这个不对称容易漏。
	MethodPluginMgrStop             = "plugin.mgrStop"             // 51
	MethodPluginMgrStart            = "plugin.mgrStart"            // 52
	MethodPluginMgrIsRuntimeStopped = "plugin.mgrIsRuntimeStopped" // 53

	// 共享段锁仲裁（新增，无对应 method id —— C ABI 下不存在跨进程锁概念）
	MethodStageLock   = "stage.lock"
	MethodStageUnlock = "stage.unlock"
)

// 原 case 25（CORE_FREE_STRING）无对应 RPC method：
// C ABI 下需要显式释放跨边界字符串，进程模型下由各自 GC 管理，概念消失。

// HandshakeParams 是内核 → 插件的建链首帧：告知内核侧规格。
type HandshakeParams struct {
	CoreVersion string `json:"core_version"` // 内核版本（插件据此判主版本兼容）
	PluginName  string `json:"plugin_name"`  // 内核分配的插件名
	// FieldCount 是 StageContext 描述符个数。插件据此与自己的编译期
	// 字段数比对：不匹配意味着标志位基址会错位，必须重编（而非静默错读）。
	FieldCount uint32 `json:"field_count"`
	ShmSize    int    `json:"shm_size"`
	// EvtRingSize 是事件环段大小（0 表示不支持事件环）。插件据此 mmap fd 4。
	EvtRingSize int `json:"evt_ring_size,omitempty"`
}

// HandshakeResult 是插件 → 内核的建链应答：上报自身信息。
type HandshakeResult struct {
	// SDKVersion 是插件编译时链接的公开 SDK 版本。内核据此判主版本兼容。
	SDKVersion string `json:"sdk_version"`
	PluginName string `json:"plugin_name"`
	PID        int    `json:"pid"`
}

// StageInvokeParams 是 stage.invoke 的参数。
//
// **注意：不含 StageContext 数据本身**——数据在共享段，此处只带定位信息。
// 这是共享内存数据面的意义：并发改写同一份状态，而非各持副本
// （副本模型实测 35.8~36.8% lost update，§8.4）。
type StageInvokeParams struct {
	Stage string `json:"stage"`
	// Seq 是内核写入共享段后的世代号，插件读到的 seq 应 >= 此值。
	Seq uint64 `json:"seq"`
}

// StageInvokeResult 是插件执行 stage 后的应答。
type StageInvokeResult struct {
	// DirtyFields 是插件实际写回共享段的字段数，0 表示只读插件。
	// 内核据此判断是否需要重读共享段，也用于诊断"谁改了什么"。
	DirtyFields int `json:"dirty_fields"`
	// Seq 是插件写回后的世代号。
	Seq uint64 `json:"seq"`
}

// ToolInvokeParams：工具调用（原 go_invoke_tool）。
//
// 按 **funccall 模型**，内核（caller）为每次调用标定一块内存帧交给插件
// （callee），插件在这块内存里工作：
//
//	[0, ArgsLen)               参数 JSON
//	[ArgsLen, Frame.Length)    结果区（内核预留的预算）
//
// 结果放得下就写在帧内；**不够用时插件才向内核申请扩容**（arena.alloc），
// 并在返回引用上打 sharedRefFlagExpand，内核据此单独归还扩容块。
//
// 参数永远在共享内存里，不存在“小 payload 走内联”的按大小分支。
//
// Args 仅剩给**直连 RPC 的调用方**（process/bench 测试不建 Host，拿不到
// 共享内存）；内核的 invokeTool 始终走 Frame。
type ToolInvokeParams struct {
	Name    string                 `json:"name"`
	Frame   SharedRef              `json:"frame,omitempty"`
	ArgsLen uint32                 `json:"args_len,omitempty"`
	Args    map[string]interface{} `json:"args,omitempty"` // 仅直连 RPC 调用方使用
}

type ToolInvokeResult struct {
	ResultRef SharedRef   `json:"result_ref,omitempty"`
	Result    interface{} `json:"result,omitempty"` // 仅直连 RPC 调用方使用
}

// CleanerInvokeParams / CleanerInvokeResult：跨进程计算层清洗。
// Scope 取 tool / input / output，Name 是工具名或通道名。
//
// 与工具调用用**同一个 funccall 帧模型**：
//
//	[0, InputLen)               输入文本
//	[InputLen, Frame.Length)    结果区（内核预留的预算）
//
// 结果放不下时插件申请扩容块，并在 TextRef 上打 sharedRefFlagExpand。
type CleanerInvokeParams struct {
	Scope    string    `json:"scope"`
	Name     string    `json:"name"`
	Frame    SharedRef `json:"frame,omitempty"`
	InputLen uint32    `json:"input_len,omitempty"`
}

type CleanerInvokeResult struct {
	TextRef SharedRef `json:"text_ref,omitempty"`
}

// 调用帧的结果预算。
//
// 内核按“参数长度 + 预算”标定帧；结果超出预算不是失败，插件会申请扩容块。
// 预算取 64KB：覆盖绝大多数工具结果，使常态调用完全免于第二次分配。
const (
	toolResultBudget    = 64 * 1024
	cleanerResultBudget = 64 * 1024
)

const (
	CleanerScopeTool   = "tool"
	CleanerScopeInput  = "input"
	CleanerScopeOutput = "output"
)

// OutputInvokeParams：输出通道发送（原 go_invoke_output）。
//
// 与 C ABI 路径的关键差异：**可同步等待真实结果**。
// C ABI 下因 cgo 不可嵌套，只能异步 fire-and-forget，导致 output_send
// 永远返回成功（§9.4，现网 2 次消息发不出而模型以为成功）。
// 进程模型下 RPC 天然可等应答，该缺陷从根上消失。
type OutputInvokeParams struct {
	Channel string `json:"channel"`
	// Frame 是调用帧：[0, ArgsLen) 是参数 JSON；[ArgsLen, Frame.Length) 是结果区。
	// 与 ToolInvokeParams 同一 funccall 帧模型（§13.3）。没有帧时（直连 RPC
	// 测试）回退到 Args。大 payload 不再爆 stdin/stdout 管道——这正是同步
	// output_send 卡住的根因之一。
	Frame   SharedRef              `json:"frame,omitempty"`
	ArgsLen uint32                 `json:"args_len,omitempty"`
	Args    map[string]interface{} `json:"args,omitempty"` // 仅直连 RPC 调用方使用
}

// OutputInvokeResult 与 ToolInvokeResult 同形：结果优先写进帧结果区，
// 放不下才申扩容块并打 sharedRefFlagExpand。标量响应（如 "ok"）直接在
// Result 字段返回，不走共享内存。
type OutputInvokeResult struct {
	ResultRef SharedRef   `json:"result_ref,omitempty"`
	Result    interface{} `json:"result,omitempty"` // 仅直连 RPC 调用方使用
}

// ArenaAllocParams / ArenaAllocResult：插件向内核申请共享内存。
//
// 内核返回的 Ref.Length 是槽容量（可写上限），插件写入后自行把 Length
// 改成实际 payload 长度再随业务 RPC 回传。
type ArenaAllocParams struct {
	Size uint32 `json:"size"`
}

type ArenaAllocResult struct {
	Ref SharedRef `json:"ref"`
}

// ArenaFreeParams：插件通知内核回收共享内存。只带描述符，槽号在 Flags 里。
type ArenaFreeParams struct {
	Ref SharedRef `json:"ref"`
}

// PluginInitParams：插件构造参数（原 case init_plugin）。
type PluginInitParams struct {
	Name   string                 `json:"name"`
	Config map[string]interface{} `json:"config,omitempty"`
}
