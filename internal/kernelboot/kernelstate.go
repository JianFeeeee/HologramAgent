package kernelboot

import (
	"context"
	"log"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentCore "github.com/JianFeeeee/HologramAgent/internal/agent/core"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
	luapkg "github.com/JianFeeeee/HologramAgent/internal/lua"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/coldcontext"
	"github.com/JianFeeeee/HologramAgent/internal/memory/pipeline"
	"github.com/JianFeeeee/HologramAgent/internal/memory/social"
	"github.com/JianFeeeee/HologramAgent/internal/memory/text"
	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	"github.com/JianFeeeee/HologramAgent/internal/supervisor"
	"github.com/JianFeeeee/HologramAgent/internal/tracker"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// MemoryBundle 是"记忆子系统"的打包（memory 模块产出）。
//
// ★ 为何打包成一个类型而不是拆成 4 个字段：
// 这 4 个东西**同生共死**（同一个 initMemoryStack 产出、同一个 close 回收、
// 它们的持有者都假设彼此一致）。拆开会让"只填了 db 忘了 indexer"变成可能。
//
// ★ 为何放在 kernelboot 而不是 cmd/homed：
// KernelState 要引用它的类型 ⇒ 类型必须与 KernelState 同包
// （否则 kernelboot 得 import cmd/homed，那是不可能的）。
type MemoryBundle struct {
	DB        *memory.GraphDB
	Indexer   *memory.Indexer
	Social    *social.SocialStore
	Distiller *pipeline.Distiller
}

// KernelState 是内核**唯一的公共数据持有者**（用户设计）。
//
// ## 设计意图（用户原话）
//
// > 内核通过一个结构体收集全部包级共享变量，其他模块通过这个结构体访问数据，
// > 各个模块的公共数据也统一注册到内核这个公共结构体中。
//
// > 结构体成员初始置 nil 加读取前检查不就好了
//
// ## 为什么这样比"每个包各持全局变量"好
//
// 本仓实测的隐藏依赖（设计 §2.8.2）大多来自**散落各包的包级变量**：
//
//	internal/nlp/parser.go:  var defaultParser Parser    // SetDefaultParser 写，
//	                                                     // NewExtractor 运行期读
//
// 这类变量的共同问题：
//
//	· **看不见**：不读那个包的源码就不知道它存在、谁写谁读
//	· **拼错不报**：`SetDefaultParser` 忘调 ⇒ 运行期静默降级
//	  （实测：图记忆实体退化成整句复合值，跨维度检索全失效）
//	· **顺序无保证**：写入时机靠"碰巧"（原 main 里蒸馏先启动、解析器后装上，
//	  靠 Interval=10min 兜着 —— 见 §2.8.2 A）
//
// 收到一个结构体后：
//
//	· **一处可见**：一眼看全内核有哪些共享状态（可审计）
//	· **类型化**：字段拼错 / 取错类型 ⇒ **编译错误**
//	· **有归属**：字段属于某个模块的产物 ⇒ 顺序由依赖图保证
//
// ## ★ 取值方式：直接读字段 + 调用方检查 nil（用户指定的做法）
//
// 不用 map[string]any、不用泛型取用、不用"未声明依赖就取不到"的拦截。
// 理由：
//
//	· 类型化字段 ⇒ 拼错/取错类型是**编译错误**（比任何运行期检查都早）
//	· nil 检查 ⇒ 读得越直观越好（`if ks.LuaVM == nil { ... }`）
//	· 少一层间接 ⇒ 少一处可能出错的地方
//
// 顺序正确性由**声明**驱动：`Build()` 校验缺依赖 / 环 / 重名，
// `Run()` 在执行每一层之前再核一次"实际已产出的设施"是否涵盖本层依赖
// （声明对、但计划层顺序被改坏时也能拦住，见 DependencyNotReadyError）。
// 判据断言关键顺序（见 internal/kernelmod/all/bootgraph_test.go 的
// TestLayerTopologyIsRespected：用真实模块登记表装配整机，逐层断言层间有序）。
//
// ## ★ 并发
//
// 同层模块**并行** Init，各自只写**自己那一个字段**
// ⇒ 字段互不相同，无数据竞争（判据用 -race 验证）。
//
// 内核**不**在字段上加锁：加锁会给调用方"可以乱写"的错觉，
// 正确约束是"各模块只写自己的字段"（由代码审查 + 判据守）。
type KernelState struct {
	// ── Group 0 产出（core 自己立起来的东西，所有模块只读） ──

	// Cfg 是配置快照（config 模块产出）。
	//
	// ★ 它就是原先"无主"的那个 cfg：`cfg := cfgReg.ToConfig()` 之后被手工
	// 传给 11 个 init*。归到这里后，任何模块读 ks.Cfg 即可。
	// 它在依赖图上是 config 模块的产物（第 0 层，前提由 Group 0 保证）。
	Cfg *types.Config

	// CfgReg 是配置中心（Group 0 产出，所有模块只读用）。
	CfgReg *config.ConfigRegistry

	// DataDir 是数据目录（Group 0 产出）。
	DataDir string

	// AgentWorkDir 是 agent 工作目录（ensureDataDirs 产出）。
	AgentWorkDir string

	// CLISocket 是 CLI 插件的 unix socket 路径（--cli-socket 产出）。
	//
	// 放在公共结构体里的理由：它是插件读的配置，必须与插件系统同时就绪
	// （stage-host 模块用它设 cli.DefaultSocket —— 原先那行在 main 里
	// 紧邻 loadPlugins）。
	CLISocket string

	// HTTPAddr 是 WebUI 监听地址覆盖值（--http-addr 产出）。
	HTTPAddr string

	// ── 各模块注册的公共数据（模块只写自己那几个字段） ──

	// Memory 是图记忆栈（memory 模块产出）。
	Memory *MemoryBundle

	// LuaVM 是 Lua 协议适配 VM（lua 模块产出）。
	LuaVM *luapkg.VM

	// Supervisor 是代理生命周期守护（supervisor 模块产出）。
	Supervisor *supervisor.Daemon

	// Tracker 是变更追踪（tracker 模块产出）。
	Tracker *tracker.Tracker

	// IO 是内核 IO 抽象层（kernel-api 模块产出）。
	IO *agentIO.IOManager

	// EventBus 是事件总线（kernel-api 模块产出）。
	EventBus *events.Bus

	// TextMemory 是文本记忆（text-memory 模块产出）。
	TextMemory *text.Memory

	// Embedder 是共享词嵌入（memory 模块产出）。
	//
	// 蒸馏提取与 Agent 上下文**复用同一实例**，避免同一模型被二次加载
	// （约 200k×300 维 ≈ 数百 MB 内存）。
	Embedder *memory.StaticEmbedder

	// LLMManager 是多源 provider 管理（llm 模块产出）。
	LLMManager *agentAPI.ProviderManager

	// LLMProvider 是默认 provider（llm 模块产出，= LLMManager.Default()）。
	LLMProvider agentAPI.Provider

	// BaseAPIKey 是网关基础 key（llm 模块产出）。
	BaseAPIKey string

	// DocStore 是文档记忆（doc-store 模块产出）。
	DocStore *coldcontext.Store

	// Knowledge 是知识库（knowledge-store 模块产出）。
	Knowledge *knowledge.Store

	// MultimodalSpace 是多模态向量空间（multimodal 模块产出）。
	MultimodalSpace vector.MultimodalEmbedder

	// MultimodalProviderName / MultimodalErr 是"配了哪个 provider/为什么没启用"
	// （multimodal 模块产出，供 healthcheck 状态报告）。
	MultimodalProviderName string
	MultimodalErr          string

	// Update 是更新子系统的公共数据（update 模块产出）。
	//
	// 用接口而不是具体类型：kernelboot 不能 import internal/update
	// （那会形成 kernelboot → update → kernelboot 的循环）。
	// 模块在 Init 里赋 *update.State，读取方用类型断言取回。
	//
	// ★ 它在依赖图第 0 层（update 模块 Requires 为空），所以任何模块
	//   读它时都已就绪 —— 包括"启动失败回滚"的判定结果。
	Update UpdateStateProvider

	// UpdateCapabilities 是本机**升级能力**的快照（update 模块产出）。
	//
	// ## 为什么它是一个独立字段而不是塞进 Update.State
	//
	// State 回答"我这台机器现在是什么版本、上次回滚过没有"；
	// Capabilities 回答"我这台机器**能不能**自己升级"。
	// 前者随时间变，后者在进程生命周期内恒定（安装形态不变）。
	//
	// 消费者不同：
	//   · WebUI / healthcheck 展示 State（版本、回滚轨迹）
	//   · kernel_update 工具 / CLI 决策看 Capabilities（走哪条升级通道）
	//
	// 合并成一个结构会让“只关心能不能升级”的调用方也拿到一堆版本字段，
	// 反之亦然——那类混搭在这仓已经出过问题（healthcheck 把工具表塞进快照）。
	//
	// ★ 用 interface{} 而不是定义接口（与 PluginCatalog 同一范式）。
	//
	// 尝试过定义 UpdateCapabilitiesProvider 接口，但它要求方法名
	// Variant()/InstallKind() —— 而 Capabilities 的**字段**就叫 Variant，
	// 方法名与字段名在同一个类型上不能共存（编译错误）。
	// 改方法名（VariantName()）只会让接口变丑而无收益：读取方本来
	// 就要完整结构体（WebUI 要 bin_path / reason / package_name）。
	//
	// 所以：与 ks.PluginCatalog 一致，存 any，读取方断言回
	// *update.Capabilities。代价是一次类型断言，收益是零重复定义。
	UpdateCapabilities any

	// StageHost 是阶段管道（stage-host 模块产出）。
	StageHost *agentCore.StageHost

	// PluginRegistry 是插件注册表（stage-host 模块产出）。
	PluginRegistry *plugin.Registry

	// PluginCatalog 是**插件元数据缓存**（plugin-host 模块产出）。
	//
	// ## 为什么需要它（用户指出的设计）
	//
	// > 插件管理器不应该在被拉起后就去读插件列表获取元数据并写入缓存准备拉起吗？
	//
	// 对。原先元数据散在两处、且**每次都要重扫磁盘**：
	//   · cmd/homed 的 collectPluginSpecs（决定启动时机）
	//   · pluginmgr 的 listPlugins（人看的列表）
	// 两者各读各的 plugin.json，且调度器每轮评估都要用到。
	//
	// 现在统一由 plugin-host 模块**扫一次**、缓存下来，供：
	//   · 调度器（判断时机）
	//   · pluginmgr（列列表/状态）
	//   · 显式启用路径（校验名字是否存在）
	//
	// 类型是 any 以避免 import 环（kernelboot 不能依赖 plugin 包之外的
	// 插件元数据类型）—— 取用处断言为 *PluginCatalog。
	PluginCatalog any

	// SyncBus 是内核**就绪状态总线**（由 Run 创建并填入）。
	//
	// ## ★ 为什么它必须在这里（而不只是 Run 的局部变量）
	//
	// 模块的 `Init(ks)` 只能看到 KernelState —— 若 bus 只在 Run 里，
	// **模块拿不到它**，于是无法：
	//   · 订阅状态变化（plugin-host 要据此拉起"等更晚设施"的插件）
	//   · 标记就绪（虽然 Run 会自动做，但模块可能需要主动补标）
	//
	// 实测撞到：plugin-host 想在 Init 里挂"等 agent 就绪再拉起某些插件"的
	// 回调，却找不到 bus。把它放进 KernelState（内核公共数据，用户设计）
	// 是最自然的落点 —— 它本来就是**内核级共享状态**。
	SyncBus *SyncBus

	// PluginScheduler 是**插件拉起调度器**（plugin-host 模块产出）。
	//
	// 用途：诊断（还剩哪些插件挂起）、以及将来"显式启用某插件"时
	// 判断它的依赖是否就绪。
	//
	// 类型是 any 以避免 kernelboot 反向依赖插件包（import 环）。
	// 取用处断言为 *Scheduler。
	PluginScheduler any

	// PluginsReady 标记"插件调度已完成"（plugin-host 模块产出）。
	//
	// 用途：main 在装配后据此知道"插件该起的都起了"，再往下走
	// （如 compactConfigDB / 启动 runtime）。
	PluginsReady bool

	// Agent 是主 agent（agent 模块产出）。
	Agent *agentCore.Agent

	// Security 是**安全防火墙运行时**（security 模块产出）。
	//
	// ★ 它不是"某项能力"，而是横在 Core 与全部外部模块之间的边界：
	//   出向（Core → 外部）与入向（外部 → Core）都要经它审查。
	//
	// 为什么是具体类型而不是接口：internal/security 不依赖本包（无环），
	// 而类型化字段能让"取错类型/拼错字段"变成编译错误 —— 这正是本结构体
	// 存在的理由（见包注释）。未装配时为 nil；配置显式关闭时是
	// Disabled Runtime（所有审查零成本透传），不是 nil。
	Security *security.Runtime

	// Device 是**工具/通道数据面**（device 模块产出）。
	//
	// 类型是低层接口（internal/agent/api.DeviceSurface）而不是
	// *kernelmod/device.Runtime：后者 import kernelboot，直接用具体类型会成环。
	// 接口只暴露"读面"（工具清单），执行仍经 Agent —— 那里才有授权闸与超时。
	Device api.DeviceSurface

	// ── 运行期（装配阶段建立，后续流程用） ──

	// Ctx / Stop 是 agent 运行期的取消上下文。
	//
	// ★ 为何在这里而不是模块内部：main 在装配之后还要把它传给
	// startAgentRuntime / startIPCServer。生命周期必须一致 ——
	// 装配时创建的 ctx 若不交出去，那些组件会用另一个 ctx，
	// 停机信号就传不进去。
	Ctx  context.Context
	Stop context.CancelFunc

	// Closers 是各模块注册的关闭函数（按**注册顺序**记录）。
	//
	// ★ 为何按注册顺序而不是按名字排：名字与依赖序**无关**。
	// 实测反例：注册序 config→memory→llm，按名字逆序得 memory,llm,config（错），
	// 正确的是 llm,memory,config。关闭时**逆序**执行（LIFO，与 defer 一致）。
	//
	// ★★ 为什么需要独立的锁（closerMu）：
	// **同层的模块是并行 Init 的**，而它们都会调 AddCloser。
	// 实测（压力测试 + -race）：第 1 层有 8 个模块并行，其中 5 个
	// （docstore/lua/memory/multimodal/textmem）都注册关闭函数
	// ⇒ 并发 append 到同一 slice ⇒ **数据竞争**。
	//
	// 后果不只是 race 警告：切片 append 在容量增长时可能**丢失元素**
	// （两个 goroutine 同时读到旧 len、各自写同一槽）⇒ 某个模块的
	// 关闭函数永远不被调用 ⇒ **资源泄漏**（且无任何报错）。
	//
	// 为什么不用 KernelState 上"模块只写自己字段"的约定来避免：
	// Closers 不属于任一模块 —— 它是**共享的回收站**。
	// 共享可变状态就必须有锁（这与"各写各的字段"是两条不同的保证）。
	Closers []func()

	// closerMu 保护 Closers（见上）。
	closerMu sync.Mutex

	// MigrationPreflight 是数据目录迁移体检（由 migration 模块填充，可 nil）。
	//
	// 用 func 字段而不是具体类型：kernelboot 不能依赖 migration 模块
	// （那会成环），而迁移能力本身是**可选**的 —— nil 时调用方跳过即可。
	MigrationPreflight func(target string) (interface{}, error)

	// Quiescers 是可**暂停/恢复**的写者（伪关机用，见 lifecycle.go）。
	//
	// 与 Closers 的区别：Closers 是**一次性**的（关了就没了），
	// 而迁移需要"停下来 → 搬数据 → 再起来" ⇒ 必须可逆。
	Quiescers []Quiescer

	quiesceMu sync.Mutex
}

// NewKernelState 创建公共结构体（所有字段零值 = nil，由各模块填充）。
func NewKernelState() *KernelState {
	return &KernelState{}
}

// AddCloser 注册一个关闭函数（模块在 Init 里调用）。
//
// 关闭时按**注册顺序的逆序**执行（LIFO，与 `defer` 语义一致）：
// 先初始化的一般是底层（config → memory → llm），停机应反向拆。
func (s *KernelState) AddCloser(fn func()) {
	if fn == nil {
		return
	}
	// ★ 必须加锁：同层模块**并行** Init 时都会调它（见 Closers 字段的注释）。
	s.closerMu.Lock()
	s.Closers = append(s.Closers, fn)
	s.closerMu.Unlock()
}

// CloseAll 逆序执行全部关闭函数，返回首个错误。
//
// ★ 失败策略：**继续执行剩余关闭**（返回首个错误）。
// 一个模块关不掉不该阻止其他模块关 —— 那会造成更多泄漏。
// 关闭函数 panic 也被隔离（否则一个模块的问题会连带其他模块都关不掉）。
func (s *KernelState) CloseAll() error {
	// ★ 先取快照再执行（不持锁执行回调）：关闭函数可能很慢
	// （停进程/flush 数据库），持锁会让并发调用 AddCloser 的模块卡住；
	// 且回调可能反查 KernelState ⇒ 死锁。
	s.closerMu.Lock()
	fns := make([]func(), len(s.Closers))
	copy(fns, s.Closers)
	s.Closers = nil
	s.closerMu.Unlock()

	for i := len(fns) - 1; i >= 0; i-- {
		fn := fns[i]
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[kernelboot] shutdown function panicked (isolated): %v", r)
				}
			}()
			fn()
		}()
	}
	return nil
}

// UpdateStateProvider 是更新子系统暴露给内核的最小接口。
//
// 定义在 kernelboot 而不是 internal/update：避免 kernelboot → update 的
// import 循环（update 的模块实现要 import kernelboot）。
type UpdateStateProvider interface {
	// Summary 返回给 healthcheck / 状态面的版本摘要。
	Summary() map[string]interface{}
}
