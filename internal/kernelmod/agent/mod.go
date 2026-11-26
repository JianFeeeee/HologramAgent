// Package agent 是 agent 模块：主 Agent（需在插件加载前创建，
// 因为插件 Configure 需要 StatusProvider）。
//
// ## 依赖
//
// buildAgent 收 provider/providerMgr/iom/mem.db/mem.indexer/trk/coldcontextStore/
// knowledge/mem.social/textMem/personality/pluginReg/embedder/
// multimodalSpace/stageHost/evBus ⇒ 依赖除 supervisor 外的几乎全部下层模块。
//
// 人格设定（loadPersonality）在本模块内完成 —— 它只读配置文件，
// 没有独立产物的价值，不必单列模块。
package agent

import (
	"fmt"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	agentProvider "github.com/JianFeeeee/HologramAgent/internal/agent/provider"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod/toolexec"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	"log"
	"path/filepath"
	"strings"
	"sync"

	agentCore "github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	agentRegistry "github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	"github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/pkg/types"

	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent"
	mainmem "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/memory"
	"github.com/JianFeeeee/HologramAgent/internal/artifacts"
	"github.com/JianFeeeee/HologramAgent/internal/nlp"
	// ★ 空白导入**主 agent 类型模块**：它的 init 把工厂登记进类型表，
	//   内核启动时 `Instantiate("main", …)` 才有实例（用户裁定：
	//   「没有被调用就没有这个 agent 的实例」）。装配点不需要知道它怎么装。
	// ## 类型模块的**装配注册**（加新类型的唯一编辑点）
	//
	// import 即注册：每种类型在自己的 `init()` 里 `registry.Register(Descriptor)`。
	// 加一种新类型 = ①新建 `internal/agent/<type>/` 包 ②**在这里加一行**。
	// 基座 / 执行设施 / 既有类型一行都不用改。
	//
	// ★ 漏加一行的后果是**静默的**：`admin_child(type="<type>")` 会报
	//   "类型未注册"，而编译期什么都不报（Go 不会因为"这个包没被 import"报错）。
	//   实测已经漏过一次（resident / rtchild 搬成独立包后忘了加），
	//   所以有判据 `TestBuiltinAgentTypesAreRegistered` 钉住。
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent"
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/resident"
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/rtchild"
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/worker"
)

// Module 提供主 Agent。
type Module struct{}

func (Module) Name() string { return "agent" }
func (Module) Requires() []string {
	return []string{kernelmod.FacLLM, kernelmod.FacTracker,
		kernelmod.FacStageHost,
		// ★ FacSecurity（2026-10-30 补）：这是**真实存在**的依赖，
		// 之前只是没在图上声明。
		//
		// 为什么它必须在这里而不是“反正 security 在第 1 层、agent 在第 4 层”：
		//
		//  1. 图是**唯一**保证顺序的地方。不声明 => `buildAgent` 读 `ks.Security`
		//     就是读一个“恰好已被别人填好”的字段，那是隐式顺序依赖，
		//     正是本模块化设计要消灭的东西（见 kernelboot.Module 的 Requires 注释）。
		//  2. 如果将来 security 因为新依赖被推到第 5 层，本模块的
		//     `ks.Security == nil` 检查会**在运行期**报错 —— 而声明了依赖，
		//     图会直接把它排后，运行期不会发生。
		//  3. 判据 TestAgentDeclaresSecurityDependency 钉住它（删掉必红）。
		//
		// 是否应该反过来把工具执行完全迁进 device，让 agent 不持有 security？
		// 那是 device 第二阶段的事（见 device/mod.go 包注释）。当前阶段 toolcall
		// 仍在 core，所以依赖是真实的，就该如实声明。
		kernelmod.FacSecurity,
		// ★ FacToolReg（2026-11-25 补）：插件工具登记簿。
		//
		// 这是同一个教训的第三次（前两次：FacSecurity 漏声明、Mount* 漏接线）：
		// **不声明依赖 ⇒ `MustValue` 返零值 ⇒ 挂了个 nil ⇒ 整块能力静默消失**。
		// 实测：工具表从 104 掉到 44（少了全部插件工具），而没有任何报错，
		// 所有单元判据都是绿的。
		//
		// 缺它时无法在运行期"响亮失败" —— `MountToolRegistry(nil)` 是个
		// 合法调用（未挂载 = nil 是明确定义的状态）。所以只能靠图声明保证。
		kernelmod.FacToolReg,
	}
}
func (Module) Provides() []string { return []string{kernelmod.FacAgent} }

func (Module) Init(k *kstate.State) error {
	if !k.Has(kernelapi.FacLLM) {
		return errMissing("agent", kernelmod.FacLLM)
	}
	if !k.Has(kernelapi.FacPluginRegistry) {
		return errMissing("agent", kernelmod.FacPluginRegistry)
	}
	// ★ FacSecurity 已声明在 Requires 里，到这里它必须非 nil。
	//
	// 为何在这里“响亮失败”而不让 nil 传给 Agent：nil 会让
	// `securityDecisionFor` 恒放行，而策略表本身完全正确 —— 结果是
	// “配置里写了黑名单、日志里一条拒绝都没有”，看起来像没人违规。
	// 这种静默降级比启动失败难查得多。
	//
	// 注意区分：**配置显式关闭**时 security 模块装的是 Disabled Runtime
	// （非 nil，零成本透传），所以这里报错只可能是装配顺序错了。
	if !k.Has(kernelapi.FacSecurity) {
		return errMissing("agent", kernelmod.FacSecurity)
	}
	// ★ 主 agent 的唯一实例化点（用户裁定：「内核启动后自动实例化一个主 agent」）。
	//
	// 走类型注册表而不是直接 `agent.New(...)`：这样主 agent 与驻留子 / rtchild
	// 走**同一张表、同一个调用**，而"某类 agent 有没有实例"完全由
	// "有没有那条 Instantiate 调用"决定。
	mainAgent, err := buildAgent(k)
	if err != nil {
		return err
	}
	k.Provide(kernelapi.FacAgent, mainAgent)

	// 插件 SDK 接线（必须在 loadPlugins 之前）。
	wirePluginSDK(k)
	return nil
}

func init() { kernelmod.Register(Module{}) }

const defaultSystemPrompt = `You are HologramAgent, a continuously running personal assistant.

The system injects the available tools and installed skills on every turn:
- Each tool's description states when to use it, what its parameters mean, and known pitfalls. Read it before calling; never guess usage from the tool name.
- The skill index is only an index. Use skill_info to read a skill in full, and follow its documented steps for multi-step procedures.

Answer only from what the tools actually return. If something could not be retrieved, say so plainly. Never fill gaps by completing, guessing, or inventing.

Reply in natural language, like a person talking: do not enumerate which tools you called, their parameters, or timings unless the user explicitly asks. For how to address the user and what tone to use, see [Persona].`

func buildAgent(k *kstate.State) (*agentCore.Agent, error) {
	cfg := k.Cfg
	cfgReg := k.CfgReg
	// ★ 能力一律从挂载点按**具名设施**取，再断言到具体/接口类型。
	//   「谁提供什么名字」的知识只在 kernelapi（常量）与本文件（断言）里，
	//   kstate 不再持有任何模块类型（用户裁定 2026-11-17）。
	providerMgr := kernelmod.MustValue[*agentProvider.ProviderManager](k, kernelapi.FacLLM)
	provider, _ := providerMgr.Default(), true
	iom := kernelmod.MustValue[*agentIO.IOManager](k, kernelapi.FacIO)
	pluginReg := kernelmod.MustValue[*plugin.Registry](k, kernelapi.FacPluginRegistry)
	// StageHost 在挂载点里存 any（kstate 不持有模块类型），取用处断言回具体类型。
	stageHost, _ := kernelmod.MustValueAny(k, kernelapi.FacStageHost).(*kit.StageHostType)
	var evBus kernelapi.EventBus
	if impl, ok := k.Require(kernelapi.FacEventBus); ok {
		evBus, _ = impl.(kernelapi.EventBus)
	}
	secRT := kernelmod.MustValue[*security.Runtime](k, kernelapi.FacSecurity)

	sysPrompt := cfgReg.GetString("core.agent.system_prompt", defaultSystemPrompt)
	if sysPrompt == "" {
		sysPrompt = defaultSystemPrompt
	}

	// D4：把"设备是否已授权"的判据注入插件侧（toolImpl.CanUse 用）。
	// ⚠️ 必须放在 agent 构造**之后**——判据要用 agent 的 allowedOutputs，
	// 而 registry 早于 agent 构造（故这里传的是晚绑定闭包）。
	// 未注入时 ToolAPI 路径对设备放行：那是"授权可被绕过"的既成缺口。
	// 注入后，cli 的 /terminal、seq 序列等一切走 ToolAPI 的调用都受同一道闸。
	// ★ AgentConfig 只剩**身份与父声明 + 配置读面**（docs §2 的 12 字段）。
	//   设施句柄一律在构造之后挂载（MountSurface / MountProviders /
	//   MountSemantic / SetToolExec）—— 那是 §2 结论 4 的落地形态。
	rootCfg := agentCore.AgentConfig{
		ID:      "main",
		Cfg:     cfgReg,
		DataDir: cfg.Daemon.DataDir,
		// ★ 工作目录由**内核**派生（docs §10：实例目录内核分配，类型只消费）。
		//
		// 缺它的后果（实测）：`InstanceDir()` 返回空 ⇒ 上下文 savePath 为空
		// ⇒ 超长工具结果落盘报「未配置持久化目录」⇒ 340k token 的召回正文
		// 原样塞进上下文（正是§13 那条 single-event 超预算）。
		WorkDir: filepath.Join(cfg.Daemon.DataDir, "agents", string(agentCore.TypeNameMain), "main"),
	}

	// ★ 造主 agent：走**唯一那张类型表的 Construct**（所有 agent 对内核一致）。
	//
	// 内核把配置项放进 arg（`arg["cfg"]`），类型的 Construct 自己造实例并交回。
	// 于是"谁来填这些配置项"的答案只有一个（内核），换主 agent 类型不改装配点。
	rootHost := agentCore.NewRootHost(rootCfg)
	mainType := cfgReg.GetString("core.agent.main_type", agentCore.TypeNameMain)
	if mainType == "" {
		mainType = agentCore.TypeNameMain
	}
	d, okType := agentRegistry.Lookup(mainType)
	if !okType {
		return nil, fmt.Errorf("agent: 主 agent 类型 %q 未注册（core.agent.main_type 配错了？已注册: %v）",
			mainType, agentRegistry.Types())
	}
	// ★ 主 agent 私有值随**装配料**一起进 Construct（`arg` 就是装配料的
	//   正当通道，不进 AgentConfig —— §2 结论 4）。
	//
	//   为什么必须在这一步给：`installState` 跑在 Initialize 期内，而
	//   `MountMainValues` 若是"造完之后再挂"，人格在 installState 里读到的
	//   还是 nil（2026-11-26 新旧对照实测 `personality=false`，旧内核 true）。
	rootArg := map[string]interface{}{
		"cfg":          rootCfg,
		"id":           "main",
		"fusionCfg":    nil,
		"personality":  loadPersonality(cfg, cfgReg),
		"personaStore": config.RegistryPersonaStore{Reg: cfgReg},
	}
	if _, _, cerr := agentRegistry.Instantiate(d, rootHost, rootArg); cerr != nil {
		return nil, fmt.Errorf("agent: 造主 agent（类型 %q）失败: %w", mainType, cerr)
	}
	agentAny := rootHost.Root()
	agent, _ := agentAny.(*agentCore.Agent)
	if agent == nil {
		return nil, fmt.Errorf("agent: 类型 %q 没有交回根实例（SetRoot 未调用）", mainType)
	}

	// ★ 语义计算设施挂载（docs §3.1 / §3.4）：装配点把它们交给**基座字段**，
	//   不进 AgentConfig（§2 结论 4：AgentConfig 是配置项透传，不是依赖容器）。
	//
	//   两类东西来源不同，所以分两次挂：
	//     MountSemantic     句→向量的计算（FacEmbedder / FacMultimodal）
	//     MountSemanticOps  名字能否落库 / 文本→三元组（artifacts 契约的两个实现）
	// ★ 面：IO / 阶段管道 / 安全闸 / 记录表 / 插件表 / LLM 设施。
	agent.MountSurface(iom, stageHost, secRT.AsGate(), agentregOf(k), pluginReg, cfg.Plugin.Dir, llmClientOf(k))
	// ★ **插件工具登记簿**（独立设施，2026-11-25 拆分）。
	//
	// 缺这一句的后果（本轮实测）：登记簿本身是好的（60 个插件工具全都
	// 经 SetToolRegistrar 注册成功），但 `a.toolReg` 恒为 nil ⇒
	// `PublicToolDefs()` 少掉整个插件来源 ⇒ 工具表从 104 掉到 44，
	// 而所有判据都是绿的（判据自己 mount 了句柄）。
	//
	// 这是"新增挂载入口但没接进装配点"的第二次（第一次是
	// `IOManager.StartOutputDispatch`）。共用一句教训：
	// **凡是有 Mount* 的方法，装配点必须有一行对应的调用。**
	agent.MountToolRegistry(kernelmod.MustValue[*kit.ToolRegistry](k, kernelapi.FacToolReg))
	// ★ 模型源：只有根/主 agent 持源表（docs §9.6 硬边界 2；子只继承 LLM 句柄）。
	agent.MountProviders(provider, providerMgr)

	// ★ 主 agent 私有值已在上面随 `arg` 进 Construct（那里挂得比 installState 早）。
	// ★ 工具执行设施（内核唯一执行链）：基座只转发，宿主每次随 BatchOpts 传入。
	agent.SetToolExec(toolexec.New())

	mountSemanticFacilities(k, agent)
	// ★ **装配期只造实例，不启动**（2026-11-26 实测修复）。
	//
	// 此前这里有一行 `agentRegistry.StartInstance(agent)` ——
	// 而运行期起点 `startRuntime` 又调一次 `agent.Start`（`main.go:322`）。
	// 后果（真内核日志实测，冷启动就复现）：
	//
	//	[main:main] main started          ← 装配层，此时 kernelloop 还没建
	//	[kernelloop] started
	//	[main:main] main started          ← 运行期，这次才登记输入投递口
	//	[agent] status section "security" re-registered ×3
	//
	// 三处真实损害：
	//	① 类型运行期跑两遍 —— `StartMainRuntime` 里 `go m.SchedulerLoop()`
	//	   没有幂等守卫 ⇒ **同一主 agent 两个调度循环并发**（压测里
	//	   `max_concurrent=2`、响应时长成对出现，根因就是它）。
	//	② 第一次的 `RegisterInputSink` 静默失效（`kernelloop.Current()` 还是 nil）。
	//	③ 三个内核状态段被注册两遍（覆盖式注册的告警就是它报的）。
	//
	// 运行期起点只有一处：`cmd/homed/runtime.go` 的 `agentStarter()`。

	// ★ **恢复子 agent 挂到"插件就绪"之后**（2026-11-25 实测修复）。
	//
	// 缺陷：恢复在 `StartMainRuntime` 里做，而那是 `StartInstance` 触发的
	// （本模块 Init 期）；插件要到 `KernelComplete` 之后的 `loadRest` 才加载。
	// 于是恢复时 `cli` 的 inputch 还不存在 ⇒ 6 个子实例全跳过
	// （`划入 inputch cli: inputch 未注册`），日志却报 `restored 6`。
	//
	// 恢复要的是"通道已注册"，那是插件的产物 ⇒ 必须等 `PluginsReady`。
	// 与 pluginhost 的 `loadRest` 用**同一个**就绪信号。
	mountRestoreOnReady(k, agent)

	// ★ 图记忆读写钩子 → 事件总线：**已搬走**（2026-11-20）。
	//   图库归主 agent（`mainagent/memory`），事件总线归内核模块。
	//   接线在主 agent 自己的装配里（`mainagent/services` → SetAccessHook），
	//   内核不再认识图库类型。
	_ = evBus

	// D4：把「设备是否已授权」的判据注入插件侧（toolImpl.CanUse 消费它）。
	//
	// 为什么必须在这里：判据要用 agent 自己的 allowedOutputs，而
	// pluginReg 早于 agent 构造（newStageAndRegistry 在 main() 里先跑），
	// 所以 registry 存的是**晚绑定**闭包，注入点必须在 agent 建好之后。
	//
	// 不注入的后果（已实测的真实缺口）：设备授权闸只存在于
	// core.executeToolCallInner，即「agent 收到模型 tool_call」那条路径；
	// 而 ToolAPI.ExecuteTool 是**另一条**独立入口，不经那道闸 ⇒
	// 凡是走 ToolAPI 的调用都能绕过 AllowedOutputs。实测范围不止序列：
	// cli 的 /terminal 就直接经 ToolAPI 调 agentcli 的终端工具。
	pluginReg.SetDeviceAuthQuery(func(deviceID string) bool {
		// ★ 走**安全模块**的同一道闸（用户裁定：设备授权归安全模块）。
		//   agent 只回答"我是谁"，判定由 gate 做 ⇒ 与工具链同源同语义。
		return agent.IsOutputAllowed("device/" + deviceID)
	})

	// 方案 B：把 agent 的**内置工具面**注入 ToolAPI。
	//
	// 缺口背景：`memory_*` / `knowledge_*` / `doc_*` / `person_*` 这 20+ 个
	// 在 executeToolCallInner 里按前缀分派，从不进 ToolAPI ⇒ 插件经 ToolAPI
	// 既查不到也调不了。真机实跑实证：seq_run 报「工具 knowledge_list
	// 不存在或未注册」，而同一轮模型直接调它是成功的。
	//
	// 注入必须在此处（agent 构造之后）：内置工具的可见性由运行期状态门控
	// （memory/knowledge 是否就绪），而 provider 持有的是 agent。
	// ★ 内置工具族的 provider 现在由**类型自己**注入（mainagent 的 init 里），
	//   装配点不再替所有类型装一份。

	// ★ 主 agent 的标准实现由**它自己的类型工厂**挂齐（`agent/type_main.go`，
	//   经 `Instantiate(main)` 一步到位）。装配点**不许**再 `SetHooks` ——
	//   那会把工厂装满的槽位整表覆盖掉（实测：安全闸/上下文策略/stage 瞬间归零）。
	//
	//   用户裁定：「基座不假设任何 agentloop 样式」⇒ 用哪个 loop、注册哪些
	//   定时任务，是**该类型**的决定，不是装配点的追加，也不是基座的默认值。

	// ★ 主 agent 的**后台维护任务**在这里**声明**（装配期只登记，不启动）。
	//
	// 用户裁定 2026-11-17：「agent 基类应该暴露一个定时器回调接口用于触发
	// 定时任务，而不是现在这样硬编码」+「eventloop 维持定时器」。
	// ⇒ 类型模块声明"我要哪些任务"，**刻度**由内核事件循环承担
	//   （见 cmd/homed/runtime.go 的 startRuntime）。
	//
	// 为什么声明在装配点：蒸馏/归档/合并/复审/转投是**主 agent 的职责**，
	// 驻留子与 rtchild 不该跟着起这些任务。基座只提供声明面。

	return agent, nil
}

// wirePluginSDK 注入**只有 agent 才知道**的那两项（KERNEL-REGISTRY-SETLOCK 去重后）。
//
// ## 为什么这里不再注入那 7 项底层依赖（2026-10-24 去重）
//
// 原先本函数与 pluginhost.injectBaseDeps 各自把 LuaVM/BaseAPIKey/Supervisor/
// Tracker/Config/StageHost/Indexer 写一遍，而两个模块同处启动图第 4 层、
// **并行执行** ⇒ 同一批字段被两个 goroutine 无锁写（实测 4% 概率报 DATA RACE）。
//
// 判掉哪一侧：留 pluginhost，砍这边。理由是**必要性不对称**：
//
//	· pluginhost 的注入是**必需**的 —— 它自己紧接着就 LoadSkipping 拉插件，
//	  而它刻意 `Requires()` 不含 agent（核心收益：插件不必等 agent 那 35 秒）。
//	  砍掉它 ⇒ 第 4 层加载的插件拿到全 nil 依赖。
//	· agent 这边注入的这批值是**同一批 ks.* 字段的同一份值**，写第二次
//	  没有新增信息；砍掉它 ⇒ 语义不变（pluginhost 侧的 nil 守卫更严，
//	  Supervisor 只因 FacSupervisor 已在 Requires 里而恒非 nil）。
//
// ## 注意：去重**不能**替代加锁
//
// 去重后本函数仍有 SetStatusProvider / SetDeviceAuthQuery 两项要写，而
// pluginhost 同时可能正在加载插件（buildSDK 读 status）⇒ 写读依然并发。
// 所以 registry 侧 Set*/读侧的锁是**真正的修复**，去重只是消除了一处
// 无意义的重复写。两件事都做，缺一不可。
func wirePluginSDK(k *kstate.State) {
	// Agent 是本包自己的类型，构造期已知；不走挂载点断言。
	a, _ := kernelmod.MustValueAny(k, kernelapi.FacAgent).(*agentCore.Agent)
	if a == nil {
		return
	}
	// ★ 状态面 provider **必须**注入（2026-11-19 实测回归）。
	//
	// 重构时这里曾改成 `_ = a`（理由是"状态面由类型注册 RegisterStatusSection
	// 就够了"）—— 错在混淆了两件事：
	//
	//	RegisterStatusSection  类型往**段表**里挂自己那段快照
	//	SetStatusProvider      注册表能**取到整个状态**（GetKernelStatus）
	//
	// 插件的 `s.Status().GetKernelStatus()` 走第二条。不注入 ⇒ cli 的
	// `/status`、`/kernel`、`/runtime` 三个命令全部返回
	// "status provider not available"（现场实测）。
	//
	// ★ 1:1 对照旧内核发现的真实功能回归（旧内核 mod.go:317 有这一行）。
	reg, _ := kernelmod.MustValueAny(k, kernelapi.FacPluginRegistry).(*plugin.Registry)
	if reg != nil {
		reg.SetStatusProvider(a)
	}
}

// loadPersonality 载入人格（原 cmd/homed 的整块平移）。
func loadPersonality(cfg *types.Config, cfgReg *config.ConfigRegistry) *agentPkg.Personality {
	personalPath := filepath.Join(cfg.Daemon.DataDir, "personal", "personal.md")
	personality, err := agentPkg.LoadPersonality(personalPath)
	if err != nil {
		log.Printf("[homed] warning: load personality: %v", err)
	}
	if personality != nil && personality.Content != "" {
		log.Printf("[homed] persona source=file %s (takes precedence over config), %d bytes", personalPath, len(personality.Content))
		if hints := agentPkg.PersonaStaleHints(personality.Content); len(hints) > 0 {
			log.Printf("[homed] warning: persona file contains content that will rot %v -- migrate to config entry core.agent.personal_prompt "+
				"(the default template has no version number; when asked for the version, use the runtime snapshot)", hints)
		}
	} else if pv := cfgReg.GetString("core.agent.personal_prompt", config.DefaultPersonaPrompt); strings.TrimSpace(pv) != "" {
		personality = &agentPkg.Personality{Content: pv, Path: "(core.agent.personal_prompt)"}
		log.Printf("[homed] persona source=config entry core.agent.personal_prompt, %d bytes", len(pv))
	} else {
		log.Printf("[homed] persona source=none (config entry empty and no persona file)")
	}
	return personality
}

// errMissing 报告"依赖的模块产物为 nil"。
//
// ★ 为什么立刻报错而不是让 nil 传下去：nil 读出零值后 continue 会在**很远
// 的地方**变成模糊症状（如"模型没有配置"）。这里点出**是哪个模块的哪个
// 产物缺失**，把排查成本从"追几小时"降到"看一行"。
//
// 措辞刻意点出"依赖图本应保证它非 nil" —— 若真出现，说明
// ① 模块忘了声明依赖，或 ② Requires/Provides 设施名不一致（拼写漂移）。
func errMissing(moduleName, facility string) error {
	return fmt.Errorf("模块 %s 依赖的设施 %q 对应的数据为 nil —— "+
		"依赖图本应保证它已就绪。两种可能：① 该模块没在 Requires 里声明这个依赖；"+
		"② Provides/Requires 的设施名写得不一致（拼写漂移）", moduleName, facility)
}

// agentregOf 取 agent 记录表的写面（未装配 ⇒ nil：基座会用私有表，行为一致）。
func agentregOf(k *kstate.State) kernelapi.AgentRegistry {
	if impl, ok := k.Require(kernelapi.FacAgentRegistry); ok {
		if reg, _ := impl.(kernelapi.AgentRegistry); reg != nil {
			return reg
		}
	}
	return nil
}

// llmClientOf 取 LLM 设施（未装配 ⇒ nil：基座回落到单源直调）。
func llmClientOf(k *kstate.State) kernelapi.LLMClient {
	if impl, ok := k.Require(kernelapi.FacLLMClient); ok {
		if c, _ := impl.(kernelapi.LLMClient); c != nil {
			return c
		}
	}
	return nil
}

// mountSemanticFacilities 把内核侧的语义计算设施挂到基座字段上。
//
// ★ 为什么在这里（而不是 AgentConfig）：AgentConfig 只装配置项透传，
//
//	设施句柄由装配点直接交给基座（docs §2 结论 4 / §0.1 结论 5）。
//
// ★ 为什么每一项都允许缺失：这是"装配事实"而不是错误 ——
//
//	缺 FacEmbedder ⇒ 词嵌入退 TF-IDF 词频；缺 FacMultimodal ⇒ 稠密检索禁用；
//	缺提取器 ⇒ 归档只写元数据三元组。三者都是**明确的降级**，
//	日志里各自说清原因（不静默）。
func mountSemanticFacilities(k *kstate.State, a *agentCore.Agent) {
	if k == nil || a == nil {
		return
	}

	// ① 句 → 向量：稀疏词嵌入（永远有，输出可能是 TF-IDF 词频）+ 稠密空间（可选）。
	var emb agentCore.Embedder
	if impl, ok := k.Require(kernelapi.FacEmbedder); ok {
		if e, _ := impl.(agentCore.Embedder); e != nil {
			emb = e
		}
	}
	var mm kernelapi.MultimodalSpace
	if impl, ok := k.Require(kernelapi.FacMultimodal); ok {
		if m, _ := impl.(kernelapi.MultimodalSpace); m != nil && m.Loaded() {
			mm = m
		}
	}
	if emb == nil {
		log.Printf("[homed] warning: no FacEmbedder provided — sparse similarity falls back to raw tokens " +
			"(memory recall, L1 pruning and entity dedup will be coarse)")
	}
	a.MountSemantic(emb, mm)

	// ② 判定面：归一化（名字能否落库）+ 三元组提取器 + 纯函数记忆算子。
	//
	// 三者的实现分别在 memory / nlp 包，基座只认识契约（artifacts / kernelapi）。
	// 缺任何一个都是"这个部署不做那一档"，不是故障。
	//
	// ★ 三个实现包都由**装配点**直接 import（本文件在允许列表里）：
	//   nlp     → 三元组提取器（带 ONNX 依存句法）
	//   memory  → 名字归一化判定 + 纯函数记忆算子（本包已 import mainagent）
	// ★ 提取器需要词嵌入（TransE 语义验证），所以装配顺序是先 MountSemantic
	//   再构造提取器 —— 这里用同一个 emb 句柄。
	var extractor artifacts.Extractor
	if emb != nil {
		extractor = nlp.NewArtifactsExtractor(emb)
	} else {
		extractor = nlp.NewArtifactsExtractor(nil)
	}
	a.MountSemanticOps(extractor, mainmem.ArtifactsNorm(), mainmem.NewMemoryOps())
}

// mountRestoreOnReady 把"恢复子 agent"接到**插件全部注册完毕**之后。
//
// ## 两次失败换来的结论
//
// ① 原来在 `StartMainRuntime` 里恢复（`agent` 模块 Init 期）——
//
//	插件还没加载，`cli` 的 inputch 不存在 ⇒ 6 个子实例全跳过，
//	而日志报 `restored 6`（计数说谎）。
//
// ② 改挂 `SyncBus.OnUpdate` + `KernelComplete()` —— **仍然太早**：
//
//	实测 `restored 0` 出现在 `[cli] unix socket listening` **之前**。
//	原因：`KernelComplete` 是"模块装配完成"，而插件加载是**之后**的
//	另一个阶段（pluginhost 的 `loadRest`）。
//
// ## 正确的信号是 `tools_ready`
//
// `pluginhost` 在 `loadRest` 的 `sync.Once` 里、内置插件加载完之后
// `publishToolsReady(ks)`（`EventToolsReady`）。那是"工具已全部注册完毕"
// 的唯一信号 —— 通道（`inputch`）也在此刻才全部就位。
//
// 订阅发生在**模块 Init 期**（早于任何插件加载），所以不存在漏收。
func mountRestoreOnReady(k *kstate.State, agent *agentCore.Agent) {
	bus, _ := kernelmod.MustValueAny(k, kernelapi.FacEventBus).(kernelapi.EventBus)
	if bus == nil {
		// 无事件总线（裁剪版内核 / 判据）：立即恢复 ——
		// 那时也没有插件，不存在"通道未注册"的约束。
		restoreChildren(agent)
		return
	}
	var once sync.Once
	bus.Subscribe(events.EventToolsReady, func(*events.Event) {
		once.Do(func() { restoreChildren(agent) })
	})
}

// restoreChildren 走实例表恢复全部子 agent（**基座能力，类型不参与决策**）。
//
// ★ 直接调 `EnsureInstanceManager().Restore()` 而不是经主 agent 的 `Wrap`：
//
//	恢复是内核的执行（docs §10），kernelmod 也不该 import 具体类型包
//	（那会让依赖方向反了）。
func restoreChildren(a *agentCore.Agent) {
	if a == nil {
		return
	}
	im := a.EnsureInstanceManager()
	if im == nil {
		return
	}
	n, errs := im.Restore()
	if n > 0 || len(errs) > 0 {
		a.Logf("restore: %d ok, %d skipped（等 tools_ready 之后执行）", n, len(errs))
	}
	for _, e := range errs {
		a.Logf("restore child skipped: %s", e)
	}
}
