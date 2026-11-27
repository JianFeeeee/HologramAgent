# 目标架构（施工依据）

> 本文是 `todo.md` §10 六之二的**结构化转写**，不是新裁定。
> 所有数字均为 2026-11-18 实测（`4ed18fb4`）。
> 与本文冲突的旧文档（`kernel-modularization-design.md` 等）以本文为准。

---

## 0. 一个术语必须先把口径钉死

**「eventloop」（用户语境）≠ `internal/kernelloop/loop.go` 这 558 行。**

用户原话：

> 「eventloop 也是协调各个模块以及处理各个工具调用的核心推动者」

这里的 eventloop 指**内核驱动层整体**：

```
内核驱动层 = kernelloop（推动机制）+ 它推动的内核模块（iobus / stagehost /
             toolroute / security / pluginhost）
           ≠ kernelloop/loop.go 一个文件
```

`loop.go` 是**纯推动机制**，它的 import 只有：

```
stdlib（fmt/log/os/runtime/sync/time）
+ internal/kernelapi
+ internal/kernelapi/model
```

**它不认识任何 agent 类型，也不能认识。** 这条是编译期可检验的硬边界。

于是所有"某个能力归 eventloop"的表述，落到代码上都是二选一：

| 表述 | 落点 |
|---|---|
| 「循环**推动**它」 | `kernelloop` 里加一个 `RegisterXxx` / `StartXxxConsumer`（循环不知道它是什么） |
| 「循环**执行**它」 | 循环**不认识**它 ⇒ 实际是内核模块提供能力，循环只推动调用 |

「工具执行在 eventloop」属于后者。见 §5.4。

---

## 0.1 分层的物理判据：内核空间 / syscall / 用户空间

> 用户裁定 2026-11-20。这条是本文其余部分的**第一判据**，与它冲突的表述一律以它为准。

用程序内存空间作类比：

```
┌──────────────────────────────────────────────┐
│ 内核（kernelmod 各模块）                        │
│   持有：类型表 · 实例表 · 事件总线 · 刻度         │
│        工具执行设施 · 通用扩展槽                 │
│   执行：实例管理（create/destroy/restart/restore）│
│        批量持久化时机 · 批量 stop/restore        │
│   接收：eventbus 上的全部事件                    │
│   按 core.agent.main_type 选定主 agent          │
│──────────────────────────────────────────────│
│ 基座（*Agent，每实例一份）                       │ ← 内核空间最上层 + syscall
│   按具名设施取内核设施，不往上传                  │
│   给本实例装配 IO 面 / 工具集面                  │
│   对类型只暴露 syscall 形态的方法                 │
│──────────────────────────────────────────────│ ← 唯一边界（编译期可检验）
│ 类型（main / resident / rt / worker / 插件类型）  │ ← 用户空间
│   自己的私有实现自己 new、自己持有、自己回收       │
│   实现自己的 Persist / Restore                   │
│   不碰实例表、不订阅总线、不 import kstate         │
│   不发事件                                        │
└──────────────────────────────────────────────┘
```

### 由此直接推出的硬结论

| # | 结论 | 可检验形式 |
|---|---|---|
| 1 | **基座是内核模块的一部分**，不是 agent 的一部分 | 基座包可 import `kstate`；类型包不许 |
| 2 | **agent 实例完全不在内核里** | 四个类型包 `kstate` grep = 0 |
| 3 | **eventbus 是内核模块之间的通信通道**，与 agent 无关 | 类型包 `EventBus().Subscribe` grep = 0 |
| 4 | `AgentConfig` 是**配置项透传**，不是依赖容器 | 其中不许出现设施句柄（`LLM`/`StageHost`/`ToolExec`/`Memory`/`Indexer`/`DocStore`…） |
| 5 | 设施由**基座**按具名设施取，不向上传给类型 | 类型里 grep `kernelapi.Fac*` = 0 |
| 6 | 实例表 / 类型表 / 刻度**全在内核空间** | 类型不持、不查、不改 |
| 7 | 类型注册事件**由内核模块自己订阅并处理** | 订阅点只在内核模块里 |
| 8 | 中断由**内核驱动层 → 基座 → 实例**投递 | 类型不订阅总线、不轮询 |
| 9 | `admin_child` 是**内核派发给选定主 agent 的 syscall** | schema / 归属 / 执行在基座 + 内核 |
| 10 | 实例管理（create/destroy/restart/restore/stop-all）是**内核执行** | 类型只发一次 syscall，拿结果 |
| 11 | 批量持久化的**时机**归内核 | 类型只实现"存我自己的时候做什么" |
| 12 | **基座不把内核对象交给类型** | `a.StageHost()` 这类面不面向类型 |

### 判断一处代码归属的三问

1. 它**持有什么**？（设施句柄 / 内核表 → 内核空间；自己的数据 → 用户空间）
2. 它**被谁调用**？（只有内核调 → 内核；类型能调 → 必须是 syscall）
3. 它**知道谁存在**？（知道 `kstate` / 其他模块 → 内核；只知道基座 → 用户空间）

---

## 0.2 判别一切归属的唯一三问

不管遇到什么代码，问三个问题：

```
1. 它持有什么？
   设施句柄 / 内核表 / 内核事件总线  → 内核空间（基座或 kernelmod）
   它自己的数据（帧、队列、图库、上下文）→ 用户空间（类型）

2. 它被谁调用？
   只被内核调            → 内核
   类型也要调            → 必须是 syscall（基座的一个方法）

3. 它知道谁存在？
   知道 kstate / 其他内核模块 → 内核
   只知道基座这一个对象      → 用户空间
```

答不出、或答案在两个格子里来回跳，说明归属还没想清楚，**不许动手**。

---

## 0.3 隔离铁律：agent 间任何情况不允许共享

> 用户裁定原文：「agent 间是隔离实例，任何情况不允许共享」。

这是**边界本身**，不是权衡。推论：

```
每实例一份状态   sched / frame / ctx / 中断栈 / 记忆 / 记账 各实例独立
每类型一份实现   main / resident / rt / worker 各写自己的
不许共用实现     不因为"现在行为一样"就让两个类型指向同一个包
```

**关键区分**：

| 说法 | 是否成立 |
|---|---|
| 「两份代码今天长得一样」 | ✅ 事实 |
| 「所以可以共用一份」 | ❌ **禁止** |
| 「重复 1,600 行 > 共享一个实现」 | ❌ 这不是判据。依据是隔离；重复只是它的必然表现 |
| 「改主 agent 的调度器，子 agent 被牵连」 | ❌ 这是要被消灭的现象 |

**终态检验**：修改 `mainagent/` 里任何实现，`resident` 的编译与行为不变；反之亦然。

**因此必须消失的公共包**：

| 包 | 现在 | 为什么错 |
|---|---|---|
| `internal/agent/standard/` | 1,581 行（调度器 + 中断栈 + 状态面） | 全类型共用一台调度器 |
| `internal/agent/agenttypes/` | 733 行（Frame/Step/Outcome/ResidentOptions…） | 公共值类型层；已被父子交互原语取代 |
| `internal/agent/kernelflow/` | ~120 行（Marks/LevelOf/Driver） | 公共契约层（本轮误加） |
| `internal/agent/agentloop/` | 已删 | 公共实现点（本轮误加） |

---

## 1. 两个世界：内核模块 vs agent 类型

### 1.1 物理区分

```
┌── 内核模块（internal/kernelmod/*）───────────────────────────────┐
│  package 里有 init() { kernelmod.Register(Module{}) }            │
│  声明 Name / Requires / Provides                                 │
│  进依赖图，按层并行装配，Init(ks) 里取/放具名设施                  │
│                                                                  │
│  其中 **internal/agent/agent（基座）就是一个内核模块**            │
└──────────────────────────────────────────────────────────────────┘

┌── agent 类型（internal/agent/{mainagent,resident,rtchild,worker}）─┐
│  **不是内核模块**：不 Register、不进依赖图、没有 Requires/Provides │
│  只经 init() → registry.Register(Descriptor) 登记进类型表          │
│  Init 由内核在构造实例时经 Construct 调用                          │
└──────────────────────────────────────────────────────────────────┘
```

### 1.2 硬结论（每条都可 grep）

| # | 结论 | 检验 |
|---|---|---|
| 1 | 类型包不碰内核 | 四类型 `internal/kstate` grep = 0 |
| 2 | 类型包不碰内核模块 | 四类型 `internal/kernelmod` grep = 0 |
| 3 | 类型包不碰设施名 | 四类型 `kernelapi.Fac` grep = 0 |
| 4 | 类型只认识基座 | 类型 import 只允许 `internal/agent/{agent,io,kit,registry,api,…}` 这类**非内核**包 |
| 5 | 基座可以取设施 | 它自己是模块，`Init(ks)` 里 `ks.Require(FacXxx)` |
| 6 | 基座不把内核对象交给类型 | 类型可见面只有 syscall（见 §3） |

### 1.3 为什么基座是「内核空间最上层」

```
kernelmod/*            内核空间（模块、设施、总线、刻度）
        ↓
internal/agent/agent   基座 = 内核空间最上层 + syscall 提供者
        ↓ 唯一边界（编译期可检验）
internal/agent/{main,resident,rt,worker}   用户空间
```

基座干两件事：

1. **替本实例装配面**（IO 面、工具集面、能力句柄），字段在 `*Agent` 上
2. **对类型只暴露 syscall**（§3 那张清单）

---

## 2. `AgentConfig`：只有一条标准

> **只装核心 config 模块内容的透传。**

```go
type AgentConfig struct {
    // ── 身份（内核分配 / 父声明）──
    ID            types.AgentID
    TypeName      string
    ParentID      string
    KernelSource  string
    WorkDir       string
    DataDir       string

    // ── 父声明的策略（创建时定死，运行期不改）──
    TaskPrompt      string
    AllowedOutputs  []string
    AllowedTools    []string
    DenyTools       []string
    AgentCap        io.OutputCapability

    // ── 核心 config 模块的透传读面（唯一入口）──
    Cfg kernelapi.ConfigStore
}
```

**12 个字段。多一个都不许。**

判据：这个字段的值，是「谁是谁」的身份/父声明，还是「配置中心里那条键」？
都不是 → 它不属于这里。

**必须删掉的（现在还在里面）：**

| 类 | 字段 | 去处 |
|---|---|---|
| LLM 设施 | `Provider` `ProviderManager` `LLM` | `Agent` 字段（`llm`） |
| 工具设施 | `ToolExecutor`（零赋值）`ToolExec` | `Agent.RunToolBatch` 已存在，删字段 |
| IO / 阶段 / 总线 / 安全 / 记录表 / 插件 | `IO` `StageHost` `EventBus` `Security` `Agents` `PluginReg` `PluginDir` | `Agent` 字段 |
| 记忆子系统 | `Memory` `LightMemory` `Indexer` `Tracker` `DocStore` `SocialStore` `TextMemory` | 主 agent 私有实现；`LightMemory` 归子类型自己的图记忆面 |
| 语义计算 | `Extractor` `Norm` `MemoryOps` `DepParser` `Embedder` `MultimodalSpace` `EmbeddingProvider` `EmbeddingError` `EmbeddingModelPath` | `Agent` 字段 + **纯计算 syscall**（§3.2，批量；见 §3.4） |
| 主 agent 私有值 | `Personality` `PersonaStore` `FusionCfg` `ToolDefs` `ContextStats` | 主 agent 自己的包 |
| 配置快照 | `SystemPrompt` `PreferredProvider` `ThinkingEnabled` `MaxToolTurns` `MaxContextSize` `DistillInterval` `ArchiveInterval` `ReviewInterval` `MergeInterval` `CtxTuning` `InputProcessing` `Offload` | `a.CfgString/Int/Bool/Duration("core.…")` 现读 |

### 2.1 施工分期（43 个字段怎么分三批走）

一次性把 47 个字段砍到 12 个会让每一次编译红掉一大片、且中途行为不可验证。
按**来源**分成三批，每批都是"一整类归属搬完"的终态：

| 批 | 迁的是什么 | 去哪 | 批终态判据 |
|---|---|---|---|
| **B1 语义计算 9 个** | `Extractor` `Norm` `MemoryOps` `DepParser` `Embedder` `MultimodalSpace` `EmbeddingProvider` `EmbeddingError` `EmbeddingModelPath` | 基座字段（`MountSemantic` / `MountSemanticOps`）+ syscall（§3.4） | 已做：字段=0，syscall=11，`FacEmbedder` 由 `multimodal` 提供 |
| **B2 面 8 个** | `IO` `StageHost` `Security` `Agents` `PluginReg` `PluginDir` `Memory` `Indexer` `Tracker` `DocStore` `SocialStore` `TextMemory` | `Agent` 字段（装配点直接挂，与 `MountDevice` 同形） | 类型仍经 `a.IO()` / `a.StageHost()` / `a.Gate()` / `a.AgentsRegistry()` 读 |
| **B3 配置快照 11 个** | `SystemPrompt` `PreferredProvider` `ThinkingEnabled` `MaxToolTurns` `MaxContextSize` `DistillInterval` `ArchiveInterval` `ReviewInterval` `MergeInterval` `CtxTuning` `InputProcessing` | `a.CfgString/Int/Bool/Duration("core.…")` **现读** | 字段=0；`core.agent.*` 每个键在 `registry.go` 已注册 |
| **B4 主私值 5 个** | `Personality` `PersonaStore` `FusionCfg` `ToolDefs` `ContextStats` | 主 agent 自己的包（`st.*` / `Wrap(a)`） | 只剩主 agent 能接触，基座 grep = 0 |
| **B5 死字段 2 个** | `ToolExecutor`（零赋值零读）`EventBus`（1 写 0 读） | 删 | grep = 0 |

**B1 已落地**（2026-11-20）。剩下的 B2–B5 顺序不许换：
B3 依赖 B2（`Cfg` 是 B2 里唯一保留的那个透传读面），B4 依赖 B3（主 agent 的
配置快照要先能现读）。


---

## 3. 基座 `*Agent` 的精确形态

### 3.1 结构体字段（面 + 能力句柄）

```go
type Agent struct {
    // ── 身份 ──
    id, typeName, parentID, kernelSource, workDir, dataDir, logIdentity
    cfg AgentConfig          // 只读副本（12 字段那些）
    hooks Hooks              // 槽位
    children child.Registry  // 子实例表（内核骨架）
    instances *InstanceManager

    // ── 面（装配点填）──
    io        *agentIO.IOManager
    stageHost *kit.StageHostType
    cfgStore  kernelapi.ConfigStore
    gate      kernelapi.SecurityGate
    agents    kernelapi.AgentRegistry

    // ── 能力（装配点填，零缓存，直接调用）──
    llm        kernelapi.LLMClient
    extractor  artifacts.Extractor
    norm       artifacts.Norm
    memOps     kernelapi.MemoryOps
    embedder   Embedder                 // 稀疏词向量（fastText），零状态
    multimodal kernelapi.MultimodalSpace // 稠密多模态空间（可选载入）

    // ── 槽（见 §6）──
    extSlots map[string]ExtSlot

    // ── 运行期 ──
    ctx, cancel
    statusSections
    periodic []PeriodicTask
    device   agentAPI.DeviceSurface
    builtinTools agentAPI.BuiltinToolHandlers
}
```

**这里没有**：`sched` / `frame` / `publisher` / `graph` / `graphOpener` / `indicator`。

### 3.2 类型可见的方法（完整清单）

> ★ 本节**从代码导出**（`go doc -all ./internal/agent/agent Agent`，2026-11-25）。
> 上一版是手写的，列了已删方法、漏了一半新方法 —— 这是本仓"文档当事实"的
> 典型代价。改这一节时请重新跑那条命令，不要凭记忆补。

`*Agent` 当前有 **181** 个导出方法。按**调用方**分四类，只有第一类是"类型可见"。

#### ① 类型可见（syscall 面 —— 用户空间唯一入口）

```go
// 身份与日志
ID() · TypeName() · Logf(format, args...) · LogIdentityTag()
TaskPrompt() · SystemPrompt() · PreferredProvider() · IsChildAgent() · KernelSource()

// 面（只读）
IO() *agentIO.IOManager          StageHost() *kit.StageHostType
ToolRegistry() *kit.ToolRegistry Cfg() kernelapi.ConfigStore
Gate() kernelapi.SecurityGate    AgentsRegistry() kernelapi.AgentRegistry
Children() *child.Registry        Instances() *InstanceManager
AgentCapabilities() io.OutputCapability  PromptSections()
PluginHost() · PluginDirPath() · SecretStore() · BuiltinTools() · Device()

// 配置现读（不许缓存快照 —— docs §2 B3）
CfgString · CfgInt · CfgBool · CfgDuration · SetCfgValue
SystemPrompt · ThinkingEnabled · MaxToolTurns · MaxContextSize
PreferredProvider · DistillInterval · ArchiveInterval · ReviewInterval · MergeInterval
CtxTuning() · DataDir() · InstanceDir() · EnsureWorkDir()

// syscall：父 ↔ 子（两条协议，都走 map[string]interface{}）
ExecuteAdminChild(tc) · ExecuteAdminChildHelp(tc) · ExecuteAdminChildList()
CallParent(level, kind, text, meta) · EmitReply · EmitSkippedReply
InstallCallParent · DeclareChildPolicy · RevokeChildPolicy
RegisterInboundChannel · RouteInputByOwner · RouteInputTo
InjectSelf · InjectSelfChannel · SelfInputChan · IsDuplicateInput
ResolveOutputTarget · IsOutputAllowed · IsKernelLevelSource

// syscall：LLM（**类型唯一的模型入口**，不许碰 Provider / ProviderManager）
CompleteLLM(ctx, req) · CompleteLLMNoRetry(ctx, req) · StreamLLM(ctx, req)
LLMSources() · LLMSourceSupports(kind) · ActiveLLMSource() · LLMMaxContextTokens()

// syscall：工具
RunToolBatch(calls, opts) · PublicToolDefs() · RegistryToolDefs()
ToolDefinitions() · ToolAllowedForRender(name) · ToolParallelSafe(name)
FilterToolDefsForRender(tools) · RegisterExtSlot(tools)

// syscall：语义计算（**纯计算**：句进→向量出、向量对→相似度出；批量）
EmbedTexts(texts) · EmbedTextsDense(texts) · EmbedTextClean(text)
Similarity(a,b) · DenseSimilarity(a,b) · ImageDense(img, mime)
DenseFingerprint() · DenseDim() · MultimodalStatus() · EmbedderLoaded()
CleanText(s) · ExtractKeywords(text, n) · DepParser()

// syscall：rt 输出通道（docs §6 旁支：`outputch(rt)` 归 device 抽象）
SetRTOutputChannel(name) · RTOutputChannel() · EmitRTText(text) · CloseRTOutput()

// syscall：安全闸
SecurityDecisionFor(tool) · CheckPath · CheckNetwork · CheckOutputGate
ReviewToolResultContent · ArchiveBeforeWrite · ScrubForLog · BeginSecurityRound
ReleaseSecurityScope · SecurityPosture() · SetProviderSecretStore

// syscall：状态段
RegisterStatusSection · StatusSections() · CollectStatusSections()
RegisterSection · RegisterPeriodic · StartPeriodic · PeriodicTasks() · PeriodicForLoop()
InterruptPending() · RoundsExecuted() · ContextLen() · ContextAccumulated()
ContextWindow() · ContextTokenBudget() · UsageLedger() · GetKernelStatus()

// syscall：阶段
RunStage(stage, ctx) · OnStage(stage, fn) · DriverOfStage · LayerOfStage

// 生命周期（**只被内核调**）
Start() · Stop() · Validate() · RestoreInstance(dir)

// 运行期身份（类型自己的状态面用）
OwnerID() · ParentSource() · ToolSeq() · ToolHost()
```

#### ② 装配点（内核在构造之后挂面 —— 类型不调）

```go
MountSurface(io, stageHost, gate, agents, pluginReg, pluginDir, llm)
MountToolRegistry(tr) · MountProviders(provider, pm)
MountSemantic(embedder, multimodal) · MountSemanticOps(extractor, norm, memOps)
MountTypeFaces(toolDefs, stats, depParser) · MountMainValues(fusion, persona, personaStore)
MountDevice · InstallInstanceManager · InstallCallParent
SetHooks · SetProvider · SetSecurity · SetAgentsRegistry · SetCtx · SetBuiltinTools
SetToolExec · SetSystemPrompt · SetPreferredProvider · SetCfgValue
EnsureInstanceManager · NewSituation() · SituationKeys() · MediaBlockID()
```

★ 纪律：**凡是有 `Mount*` 的方法，装配点必须有一行对应的调用**。
漏掉不报错、只是"少了东西" —— 本仓已踩三次（`StartOutputDispatch`、
`MountToolRegistry`、`FacToolReg` 未声明）。见 rplan P5。

#### ③ 内核执行体（注册表分派进来，类型不调）

```go
ExecuteTool · ExecuteOutputSendTool · ExecuteOutputSendHelp · ExecuteOutputListChannels
ExecuteInputChannels · ExecuteLLMTool · ExecuteSecretTool · ExecuteGetPluginTools
ExecutePluginReload · ExecuteAdminChild*(三件套)
```

#### ④ 判据探针（**非生产接口**，grep 一眼可见）

```go
ProbeSecurityRuntime · ProbeSecurityDecision · ProbeSecurityRound
ProbeDeclareChildPolicy · ProbeRevokeChildPolicy · LogPostureIfUnguarded
```

★ 这一组刻意带 `Probe` 前缀：它们在生产代码里出现一次就是错的。

#### 不许出现的方法

`Scheduler()` / `SetScheduler()` / `Publish()` / `EventBus()` / `Notify()` /
`GraphDB()` / `GraphMem()` / `GraphOpener()` / `LightMemory()` /
`RaiseKernelInterruptAt()` / `RaiseVector()`

★ 判据 `TestSurfaceHasFiveAccessors` 用 `reflect` 钉这个清单。
★ `Provider()` / `ProviderManager()` **存在**（装配点要用），约束是
「类型不许调用它们」（§9.6），那是**调用侧**的约束，不是方法集的约束 ——
把它们列进方法集禁名会得到一个必须开白名单的假判据（实测踩过）。
★ `InterruptPending()` / `RoundsExecuted()` 同样**存在**：它们是
`RuntimeHooks` 的槽位（类型自己的队列状态与轮次），基座上的同名方法是
给状态面用的转发。


### 3.4 语义计算：基座只提供纯计算，不做任何记忆语义

> 用户裁定 2026-11-20：「基座提供的是计算接口，即输入一个句子输出一个向量，
> 以及输入两个向量返回相似度。支持一 call 多发（一次输入若干个句子/句子对，返回）。
> 收到结果后如何使用如何持久化是类型的事。」

**边界：**

| 归基座（纯计算） | 归类型（语义与持久化） |
|---|---|
| 句 → 稀疏向量（fastText 模型加载、jieba 分词、词向量合成） | 何时给一条事件算向量 |
| 句 → 稠密向量（多模态空间文本编码器） | 向量存在哪（`context.json` / `ColdDoc` / `memory_blocks.vector`） |
| 图 → 稠密向量（多模态空间图像编码器） | 向量是否落盘、何时失效重算 |
| 两个稀疏向量 → 余弦 | 空间隔离判据（`DenseFP == queryFP`） |
| 两个稠密向量 → 余弦 | 阈值（`sceneRelevanceFloor` 0.20、`>0.01` 收不收） |
| 空间身份（provider / 失败原因） | 文本⊕块向量的融合（`FuseVectors`）、跨模态融合权重 |

**批量是形状而不是优化选项**：一次 call 传 N 个句子、返回 N 个向量，索引一一对应。
单发场景写 `EmbedTexts([]string{x})[0]`，语义与批量里的那一项完全相同。

**因此没有这些方法**：`Embed`（单发）、`EmbedClean`（清洗归 `MemoryOps.CleanText`）、
`SemanticScore`（"够不够相似"是类型判定，阈值是类型的数）。

**为什么现在能大胆并行**：内核侧退化成**无状态 call-reply** 之后，
N 个句子的向量化是 N 个独立计算，可以并发下发、也可以由 provider 内部批处理；
类型侧拿到结果后如何写入自己的存储仍是串行的那一件事（它持锁）。
状态（模型、分词器、ONNX 会话）只在**内核设施**里有一份，不再有"每个类型各建一个模型"。

---

### 3.3 槽位（`Hooks`）

```go
type Hooks struct {
    Runtime RuntimeHooks     // 见下
    ContextBudget func(a *Agent, b kit.TokenBudget) int
    OverflowHandler func(a *Agent, frame any, req OverflowRequest) (handled bool, pruned int)
}

type RuntimeHooks struct {
    Initialize      func(a *Agent) error          // 装配期一次
    Restore         func(a *Agent, dir string) error  // 内核驱动恢复，目录由内核给
    Start           func(a *Agent)                // 起自己的循环
    Stop            func(a *Agent)                // 停自己
    DrainTerminal   func(a *Agent, reason string) // 给从未运行的待办任务补终态（停机时）
    InterruptPending func(a *Agent) bool          // 类型自己的队列还有中断吗
    RoundsExecuted  func(a *Agent) int            // 状态面要的轮次数
}
```

★ `Hooks` 是 **3 个**槽位、`RuntimeHooks` 是 **7 个** —— 判据用 `reflect`
钉死字段集（`TestHooksHasExactlyFourSlots` / `TestRuntimeHooksSlotSet`）。
上一版文档只写 4 个（缺 `DrainTerminal`/`InterruptPending`/`RoundsExecuted`），
过期三个月无人发现，因为当时没有判据盯结构体。

**没有 `Loop` 槽位**。基座不认识帧，就不可能有「驱动一帧」的槽。
类型在 `Runtime.Start` 里 `go m.loop()`。

**为什么 `DrainTerminal` 是槽位而不是基座代码**：队列归类型（§0.3 隔离铁律），
基座不认识队列，也就不能枚举里面的任务。类型交出的这个钩子做这件事 ——
带 `ResponseCh` 的同步注入方（cli / a2a 无超时）否则会永久挂起。

---

## 4. 类型侧：各自持有什么

### 4.1 对照表

| | `mainagent` | `resident` | `rtchild` | `worker` |
|---|---|---|---|---|
| `frame.go` | `TaskFrame` | `ResidentFrame` | **无**（流式） | **无** |
| `sched/` | 自己的 `Scheduler`/`Level`/`Task`/`interruptState` | **再复制一份**，归它 | **无**（`rtSlot` 单槽） | **无** |
| 中断标记解释 | L4 抢占 / L3 挂起 / L2 优先 / L1 队尾 | 高优入队，**不挂起** | **无级别**：任何输入覆盖当前轮；`stop` 清槽 | — |
| 帧推进 | 自己 `task.go` `process.go` | 自己 `task.go` `process.go` | 自己 `rtLoop` | 自己串行 |
| 上下文值类型 | `ctx.ContextEvent` 等（自己定义） | 自己定义一份 | 自己滑窗 | 无 |
| 图记忆 | **私有实现** + 注册 `graph_*` 槽内工具 | 经槽调工具 | 经槽调工具 | 无 |
| 文本记忆 / 冷文档 | **私有实现** | — | — | — |
| 渲染（提示词/工具清单/消息） | 自己 | 自己 | 自己 | 自己 |
| 转投（offload） | **自己 agentloop 内、经 `admin_child`** | — | — | — |
| 停机补终态 | 自己 `Runtime.Stop` 里排空队列 | 自己同一份 | 无队列 | 无 |
| 能力面 | `CapText` | `CapText` | `CapText\|CapRT` | `CapText` |

**`mainagent/sched/` 与 `resident/sched/` 今天逐字相同** —— 注释只能写：
「当前行为相同，各自持有；改一方不影响另一方」。**不许**写「复用」。

### 4.2 每个类型包的文件布局（**从磁盘导出**，2026-11-25）

> ★ 上一版是手写的：`mainagent/` 实际 59 个 .go 文件，文档只列了 15 项 ——
> 而它是全仓最大的类型包。漏列的后果不是"文档难看"，是**读文档的人以为
> 某个能力不存在**（该节曾漏掉 `toolresult_budget.go` / `airinterrupt.go` /
> `overflow_policy.go`，正好都是"超限处置"那条线）。
> 改这一节请跑 `ls internal/agent/<type>/*.go`，不要凭记忆列。

```
mainagent/  (59 个 .go + 6 个子包：coldcontext ctx memory sched services textmem)
  airinterrupt.go argvalidate.go builtin_handlers.go builtin_toolapi.go
  contextstats.go crossmodal.go dense_adapter.go distill.go
  effective.go embedder_face.go eventloop.go extslot_graph.go
  frame.go graph_access.go graphmedia.go install.go
  install_base.go interrupt.go loop.go mainagent.go
  maintenance.go marks.go medialoop.go mediaref.go
  memorypass.go modalfallback.go module.go offload.go
  overflow.go overflow_policy.go overflowstat.go persona.go
  plugins.go primitives_effective.go primitives_tokenbudget.go process.go
  promptsections.go runtime_main.go scheduler_agent.go stage.go
  stages_exported.go state.go statussections.go statussections_memory.go
  surface_now.go task.go ticker.go tokenbudget.go
  toolbatch_primitive.go toolcall.go tooldefs.go toolresult_budget.go
  toolresult_face.go toolroute.go tools_kernel_family.go tools_memory_defs.go
  tools_private.go tools_register.go values.go
```

```
resident/   (19 个 .go + 2 个子包：ctx sched)
  ctxmgr.go driver.go embedder_face.go frame.go
  interrupt.go manager.go marks.go persist.go
  primitives.go process.go ratio.go resident.go
  runtime.go schedstatus.go state.go task.go
  tools.go type.go values.go
```

```
rtchild/    (6 个 .go，无子包)
  intercept.go manager.go rtchild.go runtime.go
  state.go type.go
```

```
worker/     (8 个 .go，无子包)
  args.go entry.go instance.go state.go
  tools.go type.go worker.go worker_type.go
```

**基座自己**：`internal/agent/agent/` 56 个 .go（面 + syscall + 注册表接线）；
`internal/agent/registry/` 5 个（类型表 + 工具表 + 持久化）。

**不再存在的包**（重构删掉的，列在这里防人往回找）：
`standard/`（全类型共用调度器）、`agenttypes/`（公共值类型层）、
`kernelflow/`（公共契约层）、`agentloop/`（公共实现点）、
`mainagent/docstore/`（改名 `coldcontext/`）。

---

## 5. 父子交互：只有两条协议

```
父 → 子   admin_child(type, action, meta, arg)
          顶层**只有四个参数**。业务键全在 arg（map[string]interface{}）

子 → 父   CallParent(level, kind, text, meta)
          meta 也是 map[string]interface{}
```

**两条都走 map ⇒ 不需要共享 Go 类型。**

于是这些**不是跨类型值**，归各自类型：

| 类型 | 归谁 |
|---|---|
| `ResidentOptions` / `ResidentInfo` / `InputchRecord` / `ResidualPolicy` / `ChannelDisposition` | `resident/opts.go` |
| `OffloadOptions` | `mainagent/offload.go` |
| `ChildTaskState` | `worker/state.go` |

父侧只做两件事：传 `arg` 键、读返回文本。**不许**为"传结构体"再造一层转换。

---

## 6. 通用扩展槽：主 agent 对外暴露能力的唯一路径

### 6.1 它是什么

```
内核持有【槽】—— 通用，不是某个能力专用
      ↑
被选中为主 agent 的类型，在 Initialize 里注册：
   · 一批**工具**（名字 / schema / 标记）
   · 它们的**处理钩子**（注册时逻辑即交出）
      ↓
这些工具进工具面，**任何 agent 都能调用**
      ↓
内核一视同仁处理：同路由 / 同安全闸 / 同并发判据 / 同超时 / 同脱敏
      ↓
调那个已交出的钩子 → 返回 **map**（与外部插件注册的 tool 完全同形）
```

### 6.2 三条硬规则

| # | 规则 |
|---|---|
| 1 | **只有被选中为主 agent 的那个类型才注册** —— 其他类型根本不注册 |
| 2 | **处理逻辑在注册时交出**（钩子交出去，不回主 agent 的 agentloop） |
| 3 | 主 agent 在 `admin_child` 创建子 agent 时，用参数**划定暴露哪些槽内工具** |

### 6.3 与类型私有工具的区别

| | 类型私有工具 | 槽内工具 |
|---|---|---|
| 例 | `stablemem_*` / `coldcontext_*` / `social_*` / `persona_set` | 主 agent 注册的那批 |
| 数据 | 在类型自己手里 | 注册时钩子持有（已交出） |
| 执行 | 类型自己的 agentloop | **内核执行设施**（`toolexec`） |
| 进槽吗 | **否** | 是 |
| 谁能用 | 只有类型自己 | 任何 agent（受创建参数划定） |

**「读谁的私有状态，就归谁执行」仍然成立** —— 槽内工具能被内核执行，
是因为钩子在注册时已经交出去了。

### 6.4 图记忆就是这样暴露的

主 agent 在 `Initialize` 里注册**两个普通工具**（就是 tool，跟插件注册的完全同类）：

```
graph_recall  {keywords, depth}  → map     只读
graph_commit  {triples}          → map     写主库，按调用方打归属标记
```

**权限与只读隔离在闭包里实现**，不是开个口子透出去：

```go
// 主 agent 交出的闭包自己取调用方身份（执行链传下来的真实宿主，调用方改不了）
func callerID(c registry.ToolCtx) string {
    a, _ := c.Host.(*agent.Agent)
    if a == nil { return "" }
    return string(a.ID())
}

// 写：不知道是谁写的就拒写（而不是默认算成主 agent）
who := callerID(c)
if who == "" { return "graph_commit 无法确定调用方身份，已拒绝写入" }
m.sv.Memory.Commit(triples, "agent:"+who, 0)
```

**没有** `graph_open` / `graph_close` / `graph_export`，**没有** `handle` 参数：

| | 为什么 |
|---|---|
| 生命周期 | 数据与库全在主 agent 这边，暴露「开/关」毫无意义 |
| handle | 子 agent 只调工具，不需要（也不可能）拿到库句柄 |
| export | 没有"子的 temp 库"这回事了，没有可导出之物 |

子 agent（resident / rt / 插件类型）侧：

```
模型自己调 graph_recall / graph_commit
   ↓ RunToolBatch（工具调用的唯一入口）
内核 toolexec → DispatchTool → 主 agent 交出的闭包
   ↓ 返回 map
进 tool message
```

它们**不认识** `internal/memory`、`GraphDB`、`LightMemory`、`Path()`、
`OpenReadOnly`、`OpenLight`、`LightCommon`、`LightTemp`、`ExportTriples` ——
那整套逻辑是**被删除的**，不是改成经工具调一次。

### 6.5 判据

| # | 判据 |
|---|---|
| 1 | `GraphMemoryOpener` / `OpenReadOnly` / `OpenLight` / `LightCommon` / `LightTemp` / `ExportTriples` / `LightMemory` / `CloseGraph` 在非注释代码里 grep = 0 |
| 2 | `AgentConfig` 里没有 `LightMemory` / `GraphOpener`；基座没有 `GraphOpener()` |
| 3 | 槽内工具只有 `graph_recall` / `graph_commit`，都没有 `handle` 参数 |
| 4 | `resident` / `rtchild` / `kernelmod/agenttype` 的 `memoryspec` 不再自己开库 |
| 5 | 基座只有 `RegisterExtSlot(tools []registry.Tool) error`（无别名、无 `ExtSlotCall`、无包级句柄表） |

---

## 7. 中断标记：透传归内核，解释归类型

### 7.1 两层分清

```
契约层（唯一）：internal/agent/io + SDK
  键名：priority / stop / interrupt / 未来任意标记
  取值：priority ∈ {"L1","L2","L3","L4",""}
  防提权：reservedPayloadKeys（插件不许覆盖内核语义键）
        ↓ 原样透传（一个字节不改）
解释层（各类型）：各自的包
  mainagent    L4 抢占 / L3 挂起 / L2 优先 / L1 队尾
  resident     高优先级入队，不挂起
  rtchild      没有级别：任何输入覆盖当前轮；stop 清槽
  插件类型     经 AgentFrame.Marks() 拿原样 map，自己的 Loop 解释
```

### 7.2 禁止的形态

```go
// ❌ 任何共享包或基座里出现这些
func InterruptLevel(evt *InputEvent) Level          // "标记 → 级别"的解析
func ParseInterruptLevel(s string) (Level, bool)
evt.Payload["priority"]                              // 键名字面量
```

判据：共享包/基座里 `InterruptLevel|ParseInterruptLevel|Payload["priority"]` grep = 0。

---

## 8. 能力面：通道能不能划进来

```
通道侧  InputChannel.Capabilities : io.OutputCapability   ← 已存在
agent 侧 Meta.AgentCap            : io.OutputCapability   ← 新增
        划入 = 两侧都 Supports(所需能力位)
```

### 8.1 具名能力

| 位 | 含义 | 谁声明 |
|---|---|---|
| `CapText` | 文本对话（所有类型） | 全部 |
| `CapRT` | 实时流（新值作废旧值） | 仅 `rt` |
| （可加）`CapAudio` / `CapImage` / `CapStructured` | | 声明者自己 |

### 8.2 唯一转换点

```go
// io 包：能力名 ↔ 位的唯一映射表
var capabilityByName = map[string]OutputCapability{
    "text": CapText, "rt": CapRT, "audio": CapAudio,
    "image": CapImage, "file": CapFile, "structured": CapStructured,
}
```

插件侧经中立字符串（`sdk.AgentTypeSpec.Capabilities []string`），
`kernelmod/agenttype` 单点转换；拼错**响亮拒绝**。

### 8.3 判据

| # | 判据 |
|---|---|
| 1 | 基座包里 `CapRT` / `CapAudio` 等具体能力名 grep = 0 |
| 2 | 划入校验只有 `AssignWithCapability`（两侧位掩码） |
| 3 | 加第三种能力：基座与既有三型 diff = 0 |

★ **能力面 ≠ 权限面**：能力面答"能不能承接这种通道"（结构事实），
权限面答"这个 agent 被授权用哪些通道"（`AllowedOutputs` / 安全闸）。两者都要过。

---

## 9. agent 侧没有事件总线：只有 stage 钩子与输出通道

### 9.1 结论

```
agent 类型  ──RunStage(stage, ctx)──▶  StageHost ──▶ 插件 stage handler
agent 类型  ──EmitOutputTo(ch, type, args)──▶ 输出通道 device ──▶ 插件
插件        ──InjectText / InjectInterrupt / …──▶ 输入通道 ──▶ agent 类型
```

**就这三条。** 没有 `Publish`、没有 `Subscribe`（对 agent 而言）、没有 `EventAgentOutput` / `EventStage` / `EventToolCall` / `EventContentDelta`。

### 9.2 为什么不需要事件

| 原来靠事件传递的 | 真正的既有载体 |
|---|---|
| 输入到达（`raw_input`） | `on_input` 阶段钩子（`RawMessage` / `UserID` / `GroupID` / `Extra`） |
| LLM 轮末聚合（`agent_llm_chain` / `reasoning`） | `post_action` 阶段钩子（`LLMText` / `ReasoningContent` / `TokenUsage` / `ToolCalls`） |
| 工具调用（`tool_call`） | `before_toolcall` / `after_toolcall` 阶段钩子（`ToolCalls` / `ToolResults`） |
| 最终输出（`agent_output`） | `before_output` / `after_output` 阶段钩子（`FinalText`）+ `EmitOutputTo` 本身 |
| **流式增量**（`content_delta` / `reasoning_delta`） | `on_llm_call` 阶段钩子（基座在 LLM 流上逐 chunk 推送，带 agent 标识） |

**第 5 行原先写错了**（2026-11-27 更正）：曾写成「输出通道调用本身」
（`EmitOutputTo` 逐 chunk + `payload{"delta": true}`）。那是**臆造**——输出通道的
SDK 契约只有三个参数键：

	handler(args)  args = {payload, type, meta}     ← 无 delta / 无 reset / 无 kind

`RegisterOutputChannel` 的原话是「registers an output channel that the
**`output_send` tool** can route to」——它是**工具的执行后端**（内核侧就是一个
`channelDevice`），不是流式载体。把增量塞进它的 payload 会同时坏两件事：

	① 输出通道的语义被污染（工具后端要理解"增量/清屏"）
	② 流式依赖一个"恰好逐条调工具后端"的实现细节

**正确的载体是 `on_llm_call`**：LLM 调用归基座（`StreamLLM` / `CompleteLLM`
是唯二入口），所以"模型逐 token 产出了什么"只有那条链在第一现场掌握。
基座在该阶段上逐 chunk 推送，携带 agent 标识，由插件做流式消费。

```go
// 主 agent 流式循环
for chunk := range stream {
    m.a.IO().EmitOutputTo("cli", ch, "text", map[string]interface{}{
        "content": chunk, "delta": true,
    })
}
```

```go
// 插件的输出通道 handler（本来就运行在插件 goroutine 里，直接持有 conn）
s.RegisterOutputChannel("cli", 1, "CLI 终端", sdk.ChannelDef{}, func(args) (any, error) {
    text, _ := args["payload"].(string)
    delta, _ := args["delta"].(bool)
    if delta { fmt.Print(text) } else { fmt.Println(text) }
    return map[string]any{"status": "ok"}, nil
})
```

### 9.3 现在错在哪（实测）

| 错误 | 位置 |
|---|---|
| 输出通道 handler 内部自己 `Publish(EventAgentOutput)` 回发"我输出了" | `plugins/cli/plugin.go:84`、`plugins/webui/plugin.go:208,221` |
| cli / webui **订阅** `content_delta` 才拿到流式文本 | `cli/plugin.go:245`、`webui/handler_chat.go:803`、`webui/handler_openai.go:263` |
| 插件自己 `Publish(EventStage)` 推进度 | `webui/plugin.go:272,280,284` |
| 基座存在 `publisher` / `EventBus()` / `Publish()` | `agent/agent_eventbus.go`（**已删**） |
| 基座存在 `Notify(Progress)` 翻译层 | `agent/agent_notify*.go`（**已删**） |
| `publishStageEvent` 由基座发 | `agent/stagekernel.go`（**已删**） |

### 9.4 内核模块之间的事件照旧

`agent.type.register` / `update` 这类是**内核模块之间**的通信，与 agent 无关：

```
安全模块持有总线（唯一）
kernelmod/* 经代理面发布/订阅
agent 类型：零接触
```

### 9.5 判据

| # | 判据 |
|---|---|
| 1 | 基座包 `publisher` / `EventBus()` / `Publish(` / `Notify(` / `Progress` grep = 0 |
| 2 | 四类型包 `Publish(` / `EventBus()` / `Notify(` grep = 0 |
| 3 | `events` 包里 `EventStage` / `EventAgentOutput` / `EventRawInput` / `EventToolCall` / `EventContentDelta` / `EventReasoningDelta` 全部不存在 |
| 4 | 插件侧 `s.Publish(&sdk.Event{Type: EventStage/EventAgentOutput…})` grep = 0 |
| 5 | 插件侧 `s.Subscribe(sdk.EventContentDelta|EventReasoningDelta|EventStage…)` grep = 0 |
| 6 | 四类型包 `Config().Provider` / `ProviderManager` / `.Provider().Chat` / `Provider().ChatStream` grep = 0（注释除外） |
| 7 | `AgentConfig.Provider` / `ProviderManager` 只由装配侧填（内核 `mod.go`）；四类型包构造子实例时只传 `LLM` |

### 9.6 LLM 通道：类型只经基座，基座只转发

```
类型  ──CompleteLLM / StreamLLM──▶  基座（*Agent）
                                        │  只做一件事：把这句"我要一次补全"
                                        │  翻译成设施的一次调用（带 AgentID + 优先源）
                                        ▼
                                  LLMClient（内核设施 kernelmod/llm）
                                        │  源解析（含 Preferred / Request.Source 点名）
                                        │  每源用自己的 model
                                        │  瞬时错误重试（NoRetry 时跳过）
                                        │  跨源回退 + 可用性标记/复位 + 活动源
                                        │  错误**类别化**
                                        ▼
                                  Provider.Chat / ChatStream（裸上游）
```

**两条硬边界：**

1. **类型不许出现 `Config().Provider` / `ProviderManager` / `.Provider().Chat`。**
   绕过去就同时丢掉候选链、跨源回退、可用性标记、错误分类 —— 而且不会报错，
   只是"配置里配了优先源却没生效"。判据见 §13 第 20 条。
2. **子实例只继承 `LLM` 设施句柄，不继承 `Provider` / `ProviderManager`。**
   设施在内核里是**一份**，各 agent 靠自己的 `ID` 分开记账与选源；
   类型不该看见"有几条源"这件事。

**"按能力选源"怎么走**：类型调 `LLMSources()` 拿到快照（名字 / vision / audio / 可用性），
挑一个声明了该能力的，然后把名字放进 `CompletionRequest.Source`。
点名的效果**只是把该源排到候选链最前，不关闭回退** —— 源抖动时仍会走到下一条。

**流式是必须的，不是优化**：整段生成的时长不再受整体超时限制，
只要 token 在流动就不会被判定空闲超时。累积出的 `CompletionResponse`
与一次非流式补全等价，后续阶段/工具循环逻辑完全不变。
增量经**输出通道**出去（`EmitOutputTo(ch, ch, "text", {"delta": true})`），
不走事件（见 §9.2 第 5 行）。

---

## 10. 生命周期：全部内核执行

```
实例管理（create / destroy / restart / restore / stop-all / 批量持久化时机）
   = 内核执行（InstanceManager），类型只发一次 syscall、拿结果

恢复
   基座提供挂点 RuntimeHooks.Restore(a, dir)
   类型自己挂实现；**内核驱动调用**；目录由内核传入（类型只决定文件名）

停机补终态
   = **agentloop 内部语义**：类型在自己的 RuntimeHooks.Stop 里
     排空自己的队列 + 给带 ResponseCh 的任务补终态
   基座**不枚举队列**（它不认识队列）

软重启
   内核：Stop → Construct（同 id）→ Initialize → Restore → Start
```

---

## 11. 内核模块清单（终态）

### 11.1 保留的 18 个

| 模块 | Provides |
|---|---|
| `iobus`（kernel-api） | `FacKernelIO` `FacEventBusRaw` |
| `config` | `FacConfig` |
| `agentreg` | `FacAgentRegistry` `FacAgentIndex` |
| `update` | `FacUpdate` |
| `security` | `FacSecurity` `FacEventBus` `FacEventDispatch` |
| `lua` | `FacLua` |
| `supervisor` | `FacSupervisor` |
| `tracker` | `FacTracker`（内核运维设施） |
| `migration` | `migration` |
| `onnx` | `FacONNX`（语义计算能力） |
| `multimodal` | `FacMultimodal` `FacMultimodalStatus` `FacEmbedder`（语义计算能力：稠密空间 + 稀疏词嵌入） |
| `llm` | `FacLLM` `FacLLMClient` `FacLLMDefault` `FacBaseAPIKey` |
| `stagehost` | `FacStageHost` `FacPromptSections`（**不持有记忆**） |
| `pluginhost` | `FacPluginRegistry` |
| `agent` | `FacAgent`（**基座**） |
| `agenttype` | `FacAgentTypeHost` |
| `device` | `FacDevice` |
| `toolexec` | `FacToolExec` |

### 11.2 删除的 3 个

```
kernelmod/memory     → mainagent/memory/
kernelmod/textmem    → mainagent/textmem/
kernelmod/docstore   → mainagent/coldcontext/
```

### 11.3 消失的 7 个设施名

```
FacMemory  FacMemoryIndexer  FacMemorySocial  FacMemoryDistiller
FacGraphOpener  FacTextMem  FacDocStore
```

★ `FacEmbedder` **不在此列**（2026-11-20 修正）：词嵌入是**语义计算能力**
（§3.4），它在 §3.1 是基座字段、在 §3.2 是 syscall，必须有设施来源。
它由 `multimodal` 模块一并提供 —— 与 `FacMultimodal` 同住一个模块
（`docs/zh/multimodal-space.md:262`：provider 为空时退回纯 fastText 文本路径，
两条档位本来就是同一个模块的两侧）。**名字归内核模块，计算面归基座，
语义归类型。**

### 11.4 断掉的 6 条 Require 边

```
stagehost   去掉 FacMemory FacTextMem FacDocStore
pluginhost  去掉 FacMemory FacTextMem FacDocStore
agent       去掉 FacMemory FacDocStore FacTextMem
```

### 11.5 配置键改名

```
core.memory.multimodal_space.*   →   core.multimodal.*
```

理由：内核里不再有 memory 这个概念；`multimodal` 留内核是**语义计算能力**，
与记忆无关。

★ `multimodal` 是 **describe 系**（`describe_image` / `transcribe_audio` /
`ocr_image` 的向量空间），不是 `seemedia` 那套"看"的工具。两件事不同：
`seemedia` 是插件（提供原生视觉输入），`multimodal` 是内核语义计算设施。

---

## 12. 插件侧：不许依赖内核记忆面

### 12.1 删除

| 删 | 说明 |
|---|---|
| `sdk.MemoryAPI` / `TextMemoryAPI` / `DocMemoryAPI` | 插件不再有记忆面 |
| `plugin.Registry.SetMemory` / `SetTextMemory` / `SetDocStore` / `SetIndexer` | 全部删 |
| `stagehost` 里 4 行 `pluginReg.SetMemory(...)` | 全删 |
| cli `/memory query|graph|text|context|tools` | **插件自己按规则打开 sqlite 读** |
| webui 记忆面板 | 同上 |
| healthcheck 记忆检查 | 同上 |

### 12.2 数据库路径规则（插件自己拼）

```
<core.daemon.data_dir>/memory/graph.db        主图库
<core.daemon.data_dir>/memory/text/           文本记忆
<core.daemon.data_dir>/memory/documents/      冷文档
```

### 12.3 `knowledge` 不动

它早就是**内部插件**（`internal/kernelmod/knowledge` 不存在，`FacKnowledge` 已删），
自己管自己的存储与 HTTP 面。

---

## 13. 施工判据汇总（全部可机械检验）

```bash
# ⚠️ 全部判据必须**排除注释行** —— 基座的海量注释在解释"为什么不要 X"，
#    那些提及是合法的（本仓文化就要求把理由留在代码旁）。
#    `grep -c` 直接数会把注释算进去，得出来的"非 0"全是假报警。
#    统一的辅助写法（排除注释行 **与测试文件**）：
#
#    ★ 测试文件必须排除：判据自己会造 `memory.GraphDB` 来装配 harness、
#      会写 `agentIO.CapRT` 来构造用例。它们不是"基座认识类型语义"，
#      而是"判据在验证基座不认识它" —— 把它们算进去会让判据永远红，
#      然后人就开始改判据，而不是改代码。
#
#    ★ 别用 `xargs grep -h` 或 `grep -h`：加了 `-h` 就丢掉文件名，
#      `grep -v "_test.go:"` 那一步随之失效，测试文件的命中全回来（实测过）。
chk() { grep -rnE "$1" --include='*.go' "$2" \
          | grep -v "_test.go:" \
          | grep -vE ":[0-9]+:[[:space:]]*//" \
          | wc -l ; }

# 基座不认识类型语义
chk "TaskFrame|Scheduler|Suspend|TaskKind|Outcome|ContextEvent" internal/agent/agent/   # = 0（类型语义）
# 基座不认识事件总线
chk "publisher|EventBus\(|Publish\(|Notify\(" internal/agent/agent/   # = 0（事件总线）
# 基座不认识图记忆
chk "GraphDB|LightMemory|GraphOpener|Indexer|DocStore|TextMemory" internal/agent/agent/   # = 0（图记忆）
# 基座不认识标记解释
chk "InterruptLevel|ParseInterruptLevel" internal/agent/agent/   # = 0（标记解释）
# 基座不认识具体能力名
chk "CapRT|CapAudio|CapImage" internal/agent/agent/   # = 0（具体能力名）
# 基座不认识"是否 rt"
chk "IsRealtime" internal/agent/agent/   # = 0（是否 rt）

# 类型互不依赖
go list -deps ./internal/agent/mainagent | grep -cE "resident|rtchild|worker" = 0
go list -deps ./internal/agent/resident  | grep -cE "mainagent|rtchild|worker" = 0
go list -deps ./internal/agent/rtchild   | grep -cE "mainagent|resident|worker" = 0
go list -deps ./internal/agent/worker    | grep -cE "mainagent|resident|rtchild" = 0

# 类型不碰内核
grep -rE "internal/(kstate|kernelmod)|kernelapi\.Fac" internal/agent/{mainagent,resident,rtchild,worker} = 0

# 公共包消失
test ! -d internal/agent/standard && test ! -d internal/agent/agenttypes && test ! -d internal/agent/kernelflow

# 内核不再有记忆
test ! -d internal/kernelmod/memory && test ! -d internal/kernelmod/textmem && test ! -d internal/kernelmod/docstore
grep -E "FacMemory|FacTextMem|FacDocStore|FacGraphOpener" internal/ = 0
# FacEmbedder 保留（§11.3 修正），但它只许出现在 multimodal 模块与基座

# AgentConfig 字段数 = 12
#
# ★ 不能用 `awk '/^type AgentConfig struct/,/^}/'` —— 结构体里的注释块
#   含顶格 `}`（写 Markdown 式的块引用时会出现），awk 会提前结束。
#   按行号范围查更稳（结构体起止行见 `grep -n "^type AgentConfig struct"`）：
awk 'NR>=365 && NR<=525 && /^\t[A-Z][A-Za-z]+[ \t]/{c++} END{print c}' internal/agent/agent/agent.go   # = 12
# AgentConfig 里不许出现设施句柄（§2 结论 4）
grep -E "kernelapi\.|agentAPI\.|artifacts\.|\*kit\.StageHostType|ToolExecutor" <AgentConfig 段> = 0

# 语义计算只在基座（§3.4）—— 类型只经 syscall，不调任何计算函数
grep -rE "VectorizeMap|VectorizeCleanMap|Vectorize\(" internal/agent/{mainagent,resident,rtchild,worker} = 0
grep -rE "CosineSimilarity|DenseCosine" internal/agent/{mainagent,resident,rtchild,worker} = 0

# 槽内工具进渲染面 **且不重复渲染**（2026-11-25 纠正本条）
#
# ★ 上一版写的是 `grep "ToolsFor(registry.SideAny)" internal/agent/mainagent/ = 1`
#   —— 那条判据本身是**错的**：它要求存在一行代码，而那行代码正是重名的来源
#   （槽工具登记在 registry 表里，`RegistryToolDefs()` 已经渲染过一遍，
#     再按 `ToolsFor(SideAny)` 拼一次 ⇒ 同一名字出现两次 ⇒ 上游硬拒整轮请求：
#     `Tool names must be unique`。实测：新内核连「你好」都答不出来）。
#   正确的判据是"渲染结果里没有重名"，观测**出口**而不是源码：
grep -r "ToolsFor(registry.SideAny)" internal/agent/mainagent/   # = 0（不许再拼一次）
go test ./internal/architecture/test/ -run TestExtSlotToolsObeyChildAllowDeny
go test ./internal/architecture/test/ -run TestMountEntriesAreReachable

# 公有面只有一个来源（四类工具的合并点）
grep -rc "PublicToolDefs()" internal/agent/{mainagent,resident,rtchild,worker} | awk -F: '{s+=$2} END{print s}'   # 各类型只调它
grep -E "m\.a\.IO\(\)\.GetAllTools|stageHostNow\(\)\.GetToolDefs|KernelToolDefs\(m\.a\)" internal/agent/mainagent/tooldefs.go = 0

# 槽工具只给子侧（注册者自己不渲染）
grep -A2 'Name:        "graph_recall"' internal/agent/mainagent/extslot_graph.go | grep 'Side:        registry.SideChild'

# tool 与 stagehost 已解耦（§11.1）
grep -E "func \(h \*StageHostType\) (RegisterTool|GetToolDefs|ExecuteTool|ToolDef|ToolCount)" internal/agent/kit/stagehost.go = 0
test -f internal/kernelmod/tool/mod.go

# 输出通道必须有消费者（否则 EmitOutputTo 原地蒸发）
grep -r "StartOutputDispatch" cmd/ internal/ | grep -v "func "   # ≥ 1

# 基座不持任何记忆/知识状态类型
chk "MemoryStatus|TextMemoryStatus|KnowledgeStatus|DocumentStatus|SocialStatus" internal/agent/agent/   # = 0（记忆状态类型）
# 插件无记忆面（查**接口/函数**，不查值类型别名）
chk "MemoryAPI|TextMemoryAPI" internal/sdk/   # = 0（`DocMemoryAPI` 不匹配这两个词）
chk "interface .*MemoryAPI|func .*MemoryAPI" internal/sdk/   # = 0
```

---

## 14. 名词表（给不了解本仓的读者）

| 词 | 含义 |
|---|---|
| **基座** | `internal/agent/agent`。内核模块之一，给 agent 实例装配面、对类型只暴露 syscall |
| **类型** | `mainagent` / `resident` / `rtchild` / `worker`。不是内核模块，只登记类型表 |
| **帧** | 一帧任务的完整现场（输入、消息、工具批、游标）。各类型自己定义 |
| **调度器** | 队列 + 抢占 + 挂起栈。各类型自己持有、自己驱动 |
| **agentloop** | 类型自己的「接受→处理→渲染→调用→等这一轮→下一轮」流水线 |
| **中断标记** | 输入 payload 里的 `priority` / `stop` / `interrupt`。内核只透传，类型自己解释 |
| **能力面** | agent 声明"我能承接哪种通道"的位掩码，与通道侧能力位对称比较 |
| **扩展槽** | 主 agent 对外注册工具的唯一路径；钩子注册即交出，内核一视同仁执行 |
| **槽位** | `Hooks` 里的装配点（Initialize / Restore / Start / Stop / ContextBudget / OverflowHandler） |
| **syscall** | 基座暴露给类型的方法（RunToolBatch / admin_child / CallParent / Embed…） |
