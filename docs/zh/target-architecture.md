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

## 1. 三个层次与唯一判据

### 1.1 三层

| 层 | 物理位置 | 持有 | 零持有 |
|---|---|---|---|
| **内核** | `internal/kernelloop` + `internal/kstate` + `internal/kernelapi` + `internal/kernelboot` + `internal/kernelmod/*` | 模块产物（唯一一份）、事件投递、输入路由**推动**、定时器刻度、心跳、工具执行设施、agents 实例表 | **任何 agent 私有状态**；任何 agent 类型 |
| **基座** | `internal/agent/agent` | 构造面 + **4 个槽位** + 生命周期 + **请求/结果搬运** | 记忆模型、类型语义、模块句柄副本、`context`/`sched`/`sub` 实现、agentloop 风格、**任何渲染** |
| **类型** | `internal/agent/{mainagent,resident,rtchild,worker}` | agentloop、context 实现、记忆策略、overflow 处置、中断处置、定时任务、持久化/恢复 | 内核设施内部细节 |

### 1.2 判归属的唯一判据

> **「能不能只用 SDK 完整实现？」**
>
> - 能 ⇒ **内部插件**（计算也归它，不许让内核代算）
> - 不能 ⇒ **内核设施** / **基座**

这条判据的直接后果（已裁定）：

| 东西 | 裁决 | 理由 |
|---|---|---|
| `knowledge`（含 `kbtree`） | **内部插件** | SDK 有文件/HTTP/配置，够用 |
| `coldcontext` | **主 agent 类型** | 只有主 agent 用，SDK 够用 |
| 图记忆（`*GraphDB` / `*LightMemory`） | **类型内部实现** | 不是基座槽位；三种类型三种形态 |
| 工具执行 | **内核设施** | 需要跨 agent 的通道/设备授权与统一清洗，SDK 做不到 |

### 1.3 组合，不是继承

```
主 agent 模块                         驻留子模块
  ┌──────────────────┐                ┌──────────────────┐
  │ m.a *agent.Agent │ ← 句柄         │ r.a *agent.Agent │ ← 句柄
  │ mainState        │                │ residentState    │
  │ *GraphDB         │                │ *LightMemory     │
  └────────┬─────────┘                └────────┬─────────┘
           │ 组合（持有句柄）                    │
           └──────────► *agent.Agent ◄──────────┘
                         （基座，谁都不认识）
```

**禁止 `Main` 内嵌 `*Agent`**（洋葱，不是莲藕）。内嵌会让类型模块的方法自动提升成
基座的方法集，基座与类型在方法集上互相穿透 —— 那就是莲藕。

---

## 2. 装配骨架

### 2.1 `kernelboot`：BFS 分层并行

```go
// 边 a → b 表示 b 依赖 a。BFS 从入度 0 开始分层。
// 第 k 层任意节点的全部前驱都在 <k 层，且第 k 层内部两两无边
// ⇒ 第 <k 层就绪后，第 k 层任意子集可并发 Init。
```

性质（已被判据覆盖）：分层正确性、同层并行、有界并发、三类失败（缺依赖 / 循环 / Init 错）。

**它不是"跑起来的地方"，是"决定谁能先跑的地方"。** 运行期起点不在这里（见 §2.3）。

### 2.2 `kernelmod`：模块与设施名

20 个模块目录，40 处 `ks.Provide(设施名, 实现)`。

模块只做一件事：在 `Init` 里 `Provide`。**模块之间不互相 import 实现包。**

### 2.3 `kstate`：唯一挂载点

```go
type State struct {
    // ── 共享内核数据（有类型，但不是模块实现）──
    Cfg *types.Config
    CfgReg *config.ConfigRegistry
    DataDir, AgentWorkDir, CLISocket, HTTPAddr, BootMode string
    Ctx context.Context
    Stop func()
    Closers []func()
    Quiescers []Quiescer

    // ── 能力挂载点（零模块类型）──
    mu   sync.RWMutex
    deps map[string]any
}
```

`Provide / Require / Has / FacilityNames` 四个方法就是全部。

**纪律**：
- `Provide` 同名重复 **响亮报错**（装配计划有错，不许静默覆盖）
- `Require` 返回 `(nil, false)` 不 panic（热路径 + 独立工具如 `memgc` 需要零成本跳过）
- **消费方必须显式判 `ok` 并响亮报错** —— 静默降级是本仓反复栽过的形态

---

## 3. `kernelapi`：接口契约

叶子包，646 行，**依赖只有 stdlib + `kernelapi/model` + `agent/api` + `agent/io`**。

### 3.1 依赖方向（编译器强制）

```
kernelapi  →  stdlib + model + agent/api + agent/io
实现包     →  kernelapi              （产出者）
消费方     →  kernelapi + kstate     （接口 + 挂载点）
```

消费方**绝不** import `internal/memory` / `internal/security` / `internal/plugin` / `internal/knowledge`。

### 3.2 设施名与接口（26 个）

| 设施名 | 接口 | 提供方 | 消费方 |
|---|---|---|---|
| `config` | — | `kernelmod/config` | 装配侧 |
| `kernel-io` (`FacIO`) | — | `kernelmod/iobus` | 内核驱动层 |
| `eventbus` (`FacEventBus`) | `EventBus` | **`kernelmod/security`（代理面）** | 所有模块 |
| `eventbus.raw` (`FacEventBusRaw`) | — | `kernelmod/iobus` | **只有 security** |
| `eventbus.dispatch` | `EventDispatchSink` | `kernelmod/security` | `kernelloop.InstallDispatcher` |
| `memory.opener` | `GraphMemoryOpener` | `kernelmod/memory` | **类型模块**（开子视图） |
| `memory` | `GraphMemory` | `kernelmod/memory` | 主 agent 类型 |
| `memory.indexer` | `MemoryIndexer` | `kernelmod/memory` | 主 agent 类型 |
| `textmem` | `TextMemory` | `kernelmod/textmem` | 主 agent 类型 |
| `social` | `SocialStore` | `kernelmod/memory` | 主 agent 类型 |
| `docstore` | `DocStore` | `kernelmod/docstore` | 主 agent 类型 |
| `knowledge` | `Knowledge` | `kernelmod/knowledge` | → **G 批移入内部插件** |
| `distiller` | `Distiller` | `kernelmod/llm` 等 | 主 agent 类型 |
| `llm` | `LLMClient`（稳定抽象，错误可分类） | `kernelmod/llm` | **基座（第 7 面，只转发）** |
| `agents` | `AgentRegistry` / `AgentIndex` | **Group 0（与 `Cfg`/`CfgReg` 同级）** | `kernelmod/agent`（写）、`security`（读判定）、`kernelloop`（路由） |
| `multimodal` | `MultimodalSpace` | `kernelmod/multimodal` | 主 agent 类型 |
| `security` | `SecurityGate` / `SecurityScope` / `SecurityRound` | `kernelmod/security` | **内核驱动层**（判定点），基座只被查询 |
| `secretstore` | `SecretStore` | `kernelmod/security` | 装配侧 |
| `pluginhost` (`FacPluginRegistry`) | `PluginHost` | `kernelmod/stagehost` | 装配侧 + 类型注册 |
| `overflowsink` | `OverflowSink`（按 agentID 登记/取用） | **基座**（`Start` 时登记） | `kernelmod/llm`（两处触发时按 id 调） |
| `stagehost` | `StageHost` | `kernelmod/stagehost` | 内核驱动层 |
| `tracker` | `Tracker` | `kernelmod/tracker` | 主 agent 类型 |
| `supervisor` | `Supervisor` | `kernelmod/supervisor` | 装配侧 |
| `lua` | `LuaVM` | `kernelmod/lua` | provider 层 |
| `syncbus` | `SyncBus` / `SyncState` | `kernelboot` | 模块间同步 |
| `prompt.sections` | `PromptSections` | `kernelmod/stagehost` | 类型渲染（基座只搬原始信息） |
| `migration.preflight` | — | `kernelmod/migration` | 启动早期 |

### 3.2.1 `LLMClient`：内核给基座的稳定抽象

重试、fallback、可用性标记、活动源选择、用量记账**永远在内核**。
基座只描述「这一轮我要一个补全」，并且必须收到**可分类的错误**。

```go
// 内核设施（FacLLM 提供）
type LLMClient interface {
    // Complete 一轮请求（内部：resolve → 重试 → fallback → 记账 → 活动源更新）
    Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)
    // Stream 同上，按块回
    Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error)

}
```

**对 agentloop 而言，`Complete` / `Stream` 是阻塞调用**（用户裁定 2026-11-19）：

> 「agentloop 自身就是普通的接受，处理，渲染，调用，哪有发几条何时取的问题？
>   它就是阻塞死的，llms 响应不回来它本身也无法工作」

```
agentloop = 接受 → 处理 → 渲染 → 调用 → 阻塞等待这一轮的响应 → 下一轮
```

| 谁 | 做什么 |
|---|---|
| 类型 agentloop | 接受输入、跑自己的状态机、渲染**完整上下文**、交基座、**阻塞等这一轮** |
| 基座 | 只做转发：把渲染好的请求递交内核，把结果交回；不排序并发、不重试、不改活动源 |
| 内核 eventloop | 并行发出、维护**逐条**的取消/重试/fallback/记账、多路复用等待结果，交回各自调用方 |

★ 终态**没有** `Batch` / `Wait`。它们是我按"类型有调度权"的错误思路加进去的：
"按索引取就绪结果"是把内核的并发机制暴露给类型，而类型根本不需要它 ——
它阻塞等自己的那一轮即可。

**请求的递交是"呈交缓冲区"，不是"调用并等待"**（用户原话 2026-11-19）：

```
类型 agentloop 渲染好完整上下文
   ↓ 呈交
基座：把请求放进该 agent 的**请求缓冲区**（不阻塞、不等待）
   ↓
内核 eventloop：按自己的节奏取出、并行发出、多路复用等待各条响应
   ↓ 逐条归位
类型 agentloop：自己那一轮响应回来了才继续
```

**并发与多路复用是内核 eventloop 的存在理由**：一个进程一个 eventloop，
服务 N 个各自阻塞等待的 agentloop。**N 份阻塞在调用方，1 份多路复用在驱动层**，
这正是它能服务 N 个 agent 的原因。

★ 类型的差异落在**中断与输入处置**（四级抢占 / 单槽覆盖 / 串行子任务），
**不落在 LLM 结果的取用次序**。rt 的"最新者胜"是输入侧单槽覆盖 + 取消当前轮
（`rtSlot` + cancel），不是"取第几条结果"。

**错误分类是契约的一部分**（类型要按类别做不同处置，不能靠字符串匹配）：

| 错误 | 语义 | 类型的典型处置 |
|---|---|---|
| `ErrContextFull` | 上游明确判定装不下 | 调 `OverflowHandler`（A-1 的感知链） |
| `ErrAuth` | 凭证/权限（401/403，含 model_not_allowed） | 不重试；整源或模型级分别标记 |
| `ErrTransient` | 5xx/429/网络（**已按策略重试过**仍失败） | 帧失败或走自己的降级 |
| `ErrCanceled` | 用户/抢占取消 | 终止还是重跑 = **类型决定** |
| `ErrAllFailed` | 全部源试过（携带每源原因清单） | 帧失败；原因必须能透到日志/状态面 |
| `ErrPartial` | `Batch` 中部分成功（`[]CompletionResult` 里带每条的 error） | 类型决定：整批重做 / 只补失败条目 |

**边界（这几条是「稳定抽象」的实质）**：

1. **重试与 fallback 基座不可见、不可干预** —— 基座不该知道「有两个源」这件事。
2. **取消是请求级上下文**：基座传自己的 `ctx`，内核据此中断在途请求（含重试等待）。
   取消句柄仍归基座（那是类型的决定），取消的实现归内核。
3. **源偏好作为请求参数传入**（`Provider` / `ProviderManager` / `PreferredProvider`
   是 `AgentConfig` **公有字段**）—— 基座不排序、不判可用性。
4. **活动源是内核的事实**，不由基座字段改写（删掉 `a.provider = fbProvider` 那种写法）。
5. 与第 4 面（tool）**同构**：请求边界在基座，执行在内核，结果/错误结构化。

### 3.2.2 `AgentRegistry` / `AgentIndex`：agent 登记统一化

**仓库里所有「按 agent ID 记账的表」合并成一张。** 现状（实测）：

| 表 | 位置 | 内容 | 清理点 |
|---|---|---|---|
| `outputsByAgent` | `security/outputs.go` | 输出通道白名单 | `Agent.Stop` → `ReleaseOutputs` |
| `scopesByAgent` | `security/agentscope.go` | `AgentScope`（session/longtime 档位） | `Agent.Stop` → `ReleaseSecurityScope` |
| `principals.byID` | `security/principal.go` | 子工具 allow/deny | `teardownResidentWith` / `rtStopDeps` → `RevokeChildPolicy` |
| `InstanceManager.recs` | `agent/instances.go` | 创建参数（软重启/持久化） | `m.forget(id)` |
| `child.Registry` | `agent/child/` | 存活实例登记 | `children.Remove` |

同一个事件（“某个 agent 实例不在了”）要触发**四处清理，散在三个包、三条路径**。
漏一处就是残留：同名重建继承上一代的 longtime 档（**提权**）、继承上一代的输出授权、表单调增长。
今天的正确性**靠人记得**。

**终态：一张记录表，事实只有一份。**

```go
// Group 0 提供（与 Cfg / CfgReg 同级 —— 它必须先于任何模块存在）
type AgentRecord struct {
    ID, Type, ParentID string        // 身份（层级事实从 ParentID 来，不再有 IsChild 槽位）
    Arg   map[string]any             // 创建参数（软重启/持久化共用）
    Facts model.AgentFacts           // 父声明的事实：AllowedOutputs / AllowedTools / DenyTools / LongtimeProfile
    Lifecycle string                 // running / contextfull / stopped
}

// 写：只有实例生命周期写（内核侧）
type AgentRegistry interface { Register(AgentRecord) error; Update(id string, f func(*AgentRecord)); Remove(id string) }

// 读：安全模块判定、基座渲染、kernelloop 路由（只读，叶子层）
type AgentIndex interface { Facts(id string) (model.AgentFacts, bool) }
```

**合并后消失的机制**（这是合并的真收益，不是“少一张表”）：

1. **`AgentScope` 的 session 存储 + TTL 兜底**：session 档位来自本轮 payload（`security_profile`），
   是**每轮重新解析**的，随 `Round` 句柄走完这一轮即可 —— **不存就没有“End 没跑到卡死”**。
   `sessionExpires` / `clearSession` / `scopeTTLDefault` 一起删。
2. **`ForAgent` / `ReleaseAgent` / `DeclareOutputs` / `ReleaseOutputs` / `DeclareChildPolicy` / `RevokeChildPolicy`**：
   不再有 per-agent 视图要建要放，也不再往第二张表塞声明。
3. **四处分散清理点合并成“记录消失”一件事**。

**依赖方向（合并能不能成立的关键）**：安全模块**不能** import `internal/agent/agent`
（那是把墙焊到基座上）。所以记录类型与索引接口放叶子层：

```
internal/kernelapi/model/agentfacts.go   AgentFacts（纯数据，与 Decision/RoundInput 同层）
internal/kernelapi/                      AgentIndex（只读接口）
内核（Group 0）                           AgentRegistry 实现，Provide(FacAgentIndex, …)
kernelmod/security                       Requires(FacAgentIndex)，Runtime 持只读句柄
基座                                    零 import security，渲染侧直接读 AgentConfig
```

**必须守住的三个契約**（否则改语义）：

- **fail-open 不变**：记录缺失（裸 agent / 单测）或 `AllowedOutputs` 为空 ⇒ 放行。
- **deny 优先不变**：全局 policy deny 先判，再判父声明的 allow，顺序逐字保留。
- **`profiles` 校验不变**：未声明的档位名照样**响亮报错**（只是错误从 `BeginRound` 返回）。

**顺带解决的四件旧账**：`Hooks.IsChild` 真正可删（`ParentID` 从记录来）；
`MayOffload` 进 `Descriptor.Meta`；状态面 `"security"` 段有了数据源（遍历 agents 表）；
“撤销仍有存活实例的类型 ⇒ 拒绝”天然成立（就是那张表在查）。

### 3.3 接口设计纪律

> **按「消费方实际用到的方法集」定义，不是生产方的导出面。**

每加一个方法都要有**真实消费方**；没有消费方的能力留在实现包里，不进 `kernelapi`。

这条纪律的实例：`EventBus` 接口只有 `Publish / Subscribe / Unsubscribe` 三个方法，
尽管实现方（security 代理）内部还有闸门、脱敏、路由表 —— 那些不构成契约。

---

## 4. `kernelloop`：内核驱动机制

558 行，1 文件。**不认识任何 agent 类型。**

### 4.1 它持有的四张表

```go
type Loop struct {
    events  chan dispatchItem      // 事件投递队列（有界 1024，满则背压）
    stop    chan struct{}
    done    sync.WaitGroup

    periodicMu sync.Mutex
    periodic   []PeriodicTask      // 定时任务登记

    sinksMu sync.RWMutex
    sinks   map[string]InputSink   // agentID → 输入投递口

    taskStops map[string]chan struct{}  // owner → 停止通道
}
```

### 4.2 五组 API

| 组 | API | 谁调用 |
|---|---|---|
| **① 事件投递** | `Dispatch(evt, handlers)` / `InstallDispatcher(sink)` | 安全代理（投递）+ 安全模块（装 sink） |
| **② 输入路由** | `RegisterInputSink` / `UnregisterInputSink` / `Sink` / `RouteInput(agentID, evt, isInterrupt)` | 类型模块在 `Start` 时登记自己的 sink |
| **③ 定时器** | `RegisterPeriodic` / `PeriodicTasks` / `UnregisterPeriodicByOwner` | **装配侧**（见 §4.4） |
| **④ 输出消费** | `StartOutputConsumer(name, src, handle)` | 装配侧（iobus 适配成 `OutputSource`） |
| **⑤ 心跳** | `StartHeartbeat(hb)` | 装配侧 |

### 4.3 三条纪律

> **① 有界队列 + 背压，不丢事件。**
> 无界队列会让"订阅者变慢"悄悄变成"内存涨到 OOM"。

> **② 归属到不存在/已销毁的 agent ⇒ 返回 `false`，不吞输入。**
> 让调用方兜底并留痕。

> **③ 停机路径上不阻塞。**
> 队列满且已 `Stop` 时必须能返回，否则 `CloseAll` 会卡在一个没人消费的队列上。

### 4.4 定时器为什么必须在装配侧接线

`kernelloop` 不认识 agent，所以**它不可能知道"给谁定时"**。

正确形态（`cmd/homed/runtime.go:114` 注释已写死）：

```go
// ★ 转换点在装配侧（本文件）而不是内核循环里：
//   循环不认识任何 agent 类型（否则"内核认识模块"就重新成立了）。

func periodicOf(a *agentCore.Agent) []kernelloop.PeriodicTask {
    return []kernelloop.PeriodicTask{{
        Owner:    a.Config().ID,
        Name:     "overflow-watch",
        Interval: 30 * time.Second,
        Run:      func() { /* 闭包捕获 a —— 循环完全不知道 a 是什么 */ },
    }}
}
```

**这是 overflow 触发、心跳、蒸馏、归档等一切"周期性动作"的唯一正确落点。**
不许让 `kernelloop` 直接调 agent 方法（那等于把内核与 agent 重新焊死）。

### 4.5 `InputSink` 的最小契约

```go
type InputSink interface {
    Deliver(evt any)   // ← 去掉了 isInterrupt bool（A2 批）
}
```

它只说一句话：**把这条输入交给这个 agent。**

至于那个 agent 之后怎么处理（排队 / 抢占 / 覆盖 / 丢现场），**全是该类型 agentloop 的事**。
L1–L4 的行为契约完全归类型 —— 基座/内核**只搬运标签**。

---

## 5. eventloop 与 agentloop：关系与区别

这是整个架构最容易混淆的一处。必须逐条钉死。

### 5.1 一句话区别

```
┌─────────────────────────────────────────────────────────────────┐
│ 内核 eventloop（驱动层）                                          │
│   一个进程一份。推动所有 agent、所有模块、所有定时器。              │
│   它认识"agents 表"里有多少个 id，但**不认识任何一个 agent 是什么**。│
└───────────────────────┬─────────────────────────────────────────┘
                        │ RouteInput(id, evt)  ← 只搬运
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ agentloop（每个实例一份，由类型注册）                              │
│   N 个 agent = N 个 agentloop。驱动**自己**的任务状态机。           │
│   它认识自己的 TaskFrame / context / scheduler，                    │
│   但**不认识别的 agent，也不认识内核设施**（只经 4 个槽位）。        │
└─────────────────────────────────────────────────────────────────┘
```

### 5.2 对照表（逐项，无重叠）

| 维度 | 内核 eventloop（`kernelloop`） | agentloop（类型注册的 `Loop`） |
|---|---|---|
| **实例数** | 1（进程级） | N（每个 agent 一个） |
| **载体** | `*kernelloop.Loop` | `Hooks.Loop func(a, f) Outcome` 的实现体 |
| **认识 agent 类型吗** | **不认识**（import 只有 kernelapi + model） | 认识自己的载体类型 |
| **持有排队/抢占/挂起吗** | **不持有**（全在类型的 scheduler） | 全部持有 |
| **持有 TaskFrame 吗** | **不知道 TaskFrame 存在** | 唯一持有者 |
| **goroutine** | 自己起：1 dispatcher + N periodic + N output consumer | 自己起：每实例 1 个 `schedulerLoop` |
| **输入** | `RouteInput` 把 evt 交给 sink | sink 内部进自己的队列 / 抢占槽 |
| **输出** | `StartOutputConsumer` 推动 iobus 的输出泵 | 产出 `response` 交回槽位 0 的契约 |
| **定时器** | 刻度（`runPeriodic`） | **不持有定时器**（定时动作由装配侧注册，闭包捕获 a） |
| **工具执行** | **提供执行设施**（`kernelapi.ToolExecutor`） | **只提交批请求，拿回同序结果** |
| **LLM 调用** | 并行发出 + 多路复用 + 逐条重试/fallback/记账 | **阻塞等自己那一轮**（接受→处理→渲染→调用→等） |
| **中断** | 只搬 `priority` 标签 | `priority` → 排队/抢占/覆盖，全自定 |
| **生命周期** | `Start`/`Stop` 一次 | `RuntimeHooks.Start/Stop` 每实例 |
| **停机顺序** | 先 `UnregisterInputSink`，再 `Stop` | 先 `ctx.Done` 退循环，再归还 sink |

### 5.3 为什么必须分开（三条硬理由）

**① 认识方向是单向的。**
如果 agentloop 由内核推动，内核就要持有 `*Agent` ⇒ `kernelloop` import agent 包
⇒ "内核认识模块"重新成立 ⇒ 整个解耦归零。

**② 数量级不同。**
内核 1 个，agent N 个（驻留子可增殖）。放进同一个循环 = 一个 agent 卡住就卡住全部。

**③ 崩溃隔离不同。**
`agentloop` 需要 `recover` 后重启自己（`schedulerLoop` 现有实现）；
内核循环崩溃 = 进程不可用，必须让它最少做危险的事。

### 5.4 「工具执行归内核」的精确含义

**不是**把 `runToolBatch` 塞进 `loop.go`（那需要 `*Agent`，做不到）。

是：

```
类型 agentloop               内核工具执行设施
    │                              │
    │  RunToolBatch(calls, opts)   │
    ├─────────────────────────────►│  kernelapi.ToolExecutor
    │                              │   ├─ toolroute.Dispatch   （路由）
    │                              │   ├─ security 授权闸      （判定）
    │                              │   ├─ 设备通道授权          （判定）
    │                              │   ├─ 并发判据 batchRunnable
    │                              │   └─ 返回值清洗
    │  []BatchResult（同序）        │
    │◄─────────────────────────────┤
    ▼                              ▼
  落消息（按索引）              完全不知道调用方是谁
```

基座的 `RunToolBatch` 是一条 **5 行转发**：

```go
func (a *Agent) RunToolBatch(calls, opts) []BatchResult {
    exec, ok := a.ks.Require(kernelapi.FacToolExec)
    if !ok { /* 响亮报错：装配缺设施 */ }
    return exec.(kernelapi.ToolExecutor).Execute(a.ctx, calls, opts)
}
```

**当前状态（未达成）**：`runToolBatch` 实体仍在 `internal/agent/agent/toolbatch.go:69`，
基座自己执行。这是 L 批的活。

### 5.5 「中断」的归属边界

```
内核      只做：搬运 evt，附 priority 标签（L1..L4 是四个字符串常量）
          ✗ 不排序、不合并、不夹取、不决定何时打断

基座      只做：把 evt 交给 sink（InputSink.Deliver，无布尔）
          ✗ 不判断"这是中断还是排队"

类型      全做：L4 → 抢占（cancel LLM + 现场入栈）
              L3 → 挂起当前、插入新任务
              L2 → 排队但优先
              L1 → 排队尾部
          ✓ 单 inputCh，分拣归类型投递口
```

**L4 → L3 的夹取在注入路径**（安全闸门），不在投递路径。
理由：L4 是"只有编译期内置插件可声明"的级别，外部插件拿到 L4 就会抢占全部执行
且父无从分辨来源。夹取发生在**注入那一刻**，是授权问题，不是调度问题。

---

## 6. 基座 `*Agent`：精确形态

现状（`4ed18fb4` 实测）：**25,086 行 / 69 文件 / 39 字段 / `Hooks` 13 槽 / 51 个 import / `surface.go` 34 访问器。**
目标：见下。

### 6.1 四个槽位（`Hooks` 13 → 4）

```go
type Hooks struct {
    // ① 上下文预算：拼时间线时能用多少 token
    ContextBudget func(a *Agent, b kit.TokenBudget) int

    // ② agentloop：驱动一帧到终态
    Loop func(a *Agent, f *TaskFrame) agenttypes.Outcome

    // ③ 超页处置：唯一的超页决策点
    OverflowHandler func(a *Agent, f *TaskFrame, req agenttypes.OverflowRequest) (handled bool, pruned int)

    // ④ 生命周期：单对象，不是两个状态字段
    Runtime RuntimeHooks   // {Initialize, Start, Stop, Restore, Persist}
}
```

**被删的 9 个**及去向：

| 槽位 | 去向 | 批次 |
|---|---|---|
| `IsChild` | `AgentConfig` 数据字段（层级事实，不是结论） | E |
| `IsRealtime` | **删**（rt 类型自己知道自己是 rt） | E |
| `ComeFrom` | `AgentConfig` 数据字段 | E |
| `MayOffload` | `registry.Descriptor.Meta` | E |
| `Prune` | `OverflowHandler` 内部（类型自己调 `PruneContext` 原语） | A2 |
| `OverBudget` | `OverflowHandler` 内部 | A2 |
| `EnforceOverflow` | 改名 `OverflowHandler` | A-1 |
| `RunStage` | `standard` 的实现（5 处调用点同包） | I |
| `GraphMemory` | **类型内部实现**（主 `*GraphDB` / 驻留·rt `*LightMemory` / worker nil） | E |
| `SecurityGate` | 内核驱动层判定；基座只被查询 `SecurityPosture` | E |

### 6.2 基座零持有的 13 项

| # | 零持有 | 归属 |
|---|---|---|
| 1 | `context` 实现（相关度裁剪） | 类型（主 `RelevanceContext`） |
| 2 | `sched` 实现（排队/抢占/挂起） | 类型（`standard` 提供实现，主与驻留各自持有实例） |
| 3 | `subStates` 分区 | 类型 |
| 4 | `mainState` / `residentState` / `rtState` | 类型 |
| 5 | 图记忆句柄 | 类型 |
| 6 | `IOManager` | 内核驱动层 |
| 7 | `publisher` / `agentEventBus` | 内核驱动层 |
| 8 | `allowedOutputs` | 内核驱动层（授权判定） |
| 9 | 工具执行链 | 内核设施 |
| 10 | 定时器 | 装配侧（闭包捕获 a） |
| 11 | 输出泵 | 内核驱动层 |
| 12 | 蒸馏 / 归档 / 合并 / 审阅 / 卸载 | 主 agent 类型 |
| 13 | 类型分支（`IsChild` / `parentID != ""` / `IsRealtime`） | **零**（全走注册） |

### 6.3 `surface.go` 34 → 5

保留：`Config()` / `Hooks()` / `SetHooks()` / `Ctx()` / `Cancel()`。

删：`HooksValue`（重复）、`Impl`/`SetImpl`、`ContextOf`/`Sched`/`Sub`、
`Provider`/`ProviderManager`/`PreferredProvider`（都是 `AgentConfig` 公有字段），其余。

### 6.4 基座的唯一假设

> **「划分了对应的 inputch 与 outputch 的 agent，会用合理的方式处理 input 的各种消息，
> 以及按照契约产生 output 以及可能产生的纯文本输出。」**

除此之外基座**不假设任何东西**：
- ✗ 不假设有 agent-loop 风格（没有默认状态机、没有静默 fallback）
- ✗ 不假设有记忆
- ✗ 不假设有安全
- ✗ 不假设有插件

**未注册槽位 ⇒ 响亮装配错误**，不是"退化为 no-op"。
唯一例外：`OverflowHandler == nil` 是**明确的策略**（rt 整窗、不处置），
必须在 `Descriptor` 里写明理由。

---

## 7. `registry`：唯一的动态面

`internal/agent/registry` 解决的是「**基座不预设清单**」这条原则的第四次落地。

### 7.1 一张表管五件事

```go
type Descriptor struct {
    Meta      // 类型元数据（含 MayOffload 等）
    Actions  []Action                    // ① 父 → 子的管理动作（admin_child 用）
    Reports  []Report                    // ② 子 → 父的上报类别（CallParent 用）
    Sections []StatusSection             // ③ 状态面分段
    Timers   []agenttypes.TimerSpec      // ④ 定时任务声明
    Construct  func(host Host, arg map[string]any) (string, error)  // ⑤ 实例化
    Initialize func(instance any) error
    Admin      func(host Host, action string, arg map[string]any) (string, error)
    Help       func(host Host) string
}
```

**`Actions` 与 `Reports` 严格对称**：

```
父 → 子   admin_child(type, action, meta, arg)     ← Actions
子 → 父   CallParent(level, kind, ...)             ← Reports
         两者都是同一张 registry 的两面
         底层都是「inputch + InjectInterruptTextOpts + Priority L1–L4」
```

★ **`admin_child` 的顶层参数只有四个：`type` / `action` / `meta` / `arg`**（用户裁定 2026-11-18）。
`id` / `text` / `residual` 以及某个动作要的任何键，**全在 `arg` 里**：

```
admin_child(type="resident", action="destroy", arg={"id":"kb17","residual":"keep"})
```

把通用动作的入参摊到顶层（曾实现成 `type/action/id/text/residual/arg/meta` 七个）
是错的：**动作需要什么由类型声明**（`Actions[].Args` / `Required`），
内核只按声明取值；顶层每多一个键，就是内核多认识一种动作语义。

★ **父子交互只有两条协议，没有第二条路径**：

| 方向 | 唯一入口 | 谁能用 |
|---|---|---|
| 父 → 子 | `admin_child` 族（`Action` / `admin_child_help` / `admin_child_list`） | 模型调用**与**类型代码内部发起，**同一条路** |
| 子 → 父 | `CallParent(level, kind, text, meta)`（`notify_parent` 工具） | 子类型的 agentloop |

`child.Registry` / `InstanceManager` 是**内核骨架的内部实现**（`Add`/`Get`/`StopOne`/`Cleanup`），
**不是任何 agent 类型的 API**。类型（含主 agent）不得直接 `children.Get(id)`、
不得调 `InstanceManager.DestroyChild`、不得调 `Agent.StopResidents` ——
那些都是绕过协议。凡是要动子，就构造一个 admin_child 请求走 `registry.Dispatch`。

**所以 `callparent_kind` 零消费者不是设计债务** ——
它零消费者是**实现落后于设计**：父侧认领表应当由 `Reports` 生成（与 `admin_child`
从 `Actions` 生成表严格同源），而这一步还没实现。

### 7.2 与工具表、状态面、定时器的同构

| 面 | 载体 | 谁注册 | 基座知道什么 |
|---|---|---|---|
| 工具表 | `registry.Tool` | 类型 / 内核 | **只按表渲染**，不含 if 分支 |
| 管理动作 | `Descriptor.Actions` | 类型 | 只按表分发到 `Admin` |
| 上报类别 | `Descriptor.Reports` | 类型 | 只按表找父侧处理者 |
| 状态段 | `Descriptor.Sections` + `s.RegisterSection` | 类型 / 插件 | 只按 key 收集 |
| 定时器 | `Descriptor.Timers` | 类型 | 只转成 `kernelloop.PeriodicTask` |

**五个面同一条原则：「基座不预设清单」。** 这是同一条判据的第五次落地。

### 7.3 类型注册只对内置插件开放

```
pluginhost（内置插件，线程级，同地址空间）
    │  EventBus.Publish("agent.type.register", payload)
    │  payload 携带**真实 Go func value**（不是 JSON）
    ▼
security.EventPolicy.Publishable 允许该事件
    │
    ▼
订阅侧：isBuiltinPlugin() 再检查一次
    │
    ▼
registry.Register(Descriptor)
```

**为什么必须是真 Go func**：跨进程插件结构上不可能传函数值 ⇒
这条限制自动把"跨进程插件不许注册类型"变成**编译期/运行期都成立的硬边界**，
不需要额外策略。

**注销时**：live instances > 0 ⇒ **拒绝**并列出实例 id。不许"先注销再让实例悬空"。

### 7.4 `InstanceManager`：创建/销毁/软重启归内核

```
InstanceManager 实现 registry.Host
    ListChildren / GetChild / DestroyChild / SendToChild / OwnerID / DataDir
    RecreateChild / RestartInstance / Restore / Records
```

**软重启**（`RestartInstance`）= 只重建一个主 agent 实例，**内核不重启**：

```
Stop 旧实例（RuntimeHooks.Stop → 归还 InputSink → 取消 ctx）
→ Construct（新的 a，同一 id）
→ Initialize（类型注册 4 槽 + Sections + Timers）
→ Restore（从 <data>/agents/instances.json 恢复子实例表）
→ Start
```

**持久化落点**：`<data>/agents/instances.json`（原子 tmp+rename；
表损坏 ⇒ **响亮报错**；单个实例失败不阻塞其余）。

---

## 8. 四个类型模块

### 8.1 对照表

| | 主 agent | 驻留子 | rt 子 | worker |
|---|---|---|---|---|
| 包 | `mainagent` | `resident` | `rtchild` | `worker` |
| 图记忆 | `*GraphDB` | `*LightMemory` | `*LightMemory` | nil |
| context 实现 | 相关度裁剪（剔页 / 归档 / 保护本帧现场） | 整窗，不裁 | **按组丢最旧**（一组=`assistant(tool_calls)`+紧随 `tool`，永不拆开；`history` 永不裁） | 无上下文面 |
| scheduler | ✅ 独立实例 | ✅ 独立实例 | ❌（`rtSlot` 单槽覆盖） | — |
| agentloop | 同步 standard | 同步 standard | **异步** | 串行 |
| toolcall | 同步提交 | 同步提交 | **异步提交** | 同步 |
| `OverflowHandler` | 剔页 → 裁剪 → 抢占 + 任务边界处置 → 恢复预算耗尽则终止 | `CallParent(L2, contextfull)` | **滑动窗口；滑无可滑（`drop == len(groups)` 仍超窗）⇒ `CallParent(L2, contextfull)`** | `nil`（唯一允许为空，无上下文面） |
| overflow 触发 | 内核两处（发出前 / 上游 `ErrContextFull` 后）**＋ 自主巡检**（拼消息后、工具回灌后、恢复重建后） | 内核两处 | 内核两处 | — |
| `Reports` | — | `contextfull` / `handover` | `contextfull`（滑无可滑时） | — |
| 生命周期 | `Restore`/`Persist` 子表 | 持久化三件 | 转录归档 | — |
| `Sections` | context / scheduler / memory / onnx / documents / text_memory / social / tracker | scheduler / resident | rt | — |
| 可 offload 拉子 | ✅（唯一） | ❌ | ❌ | ❌ |

**这张表的读法（三层架构的落点，不许读成「基座替类型做」）**：

```
基座（*Agent）  给 7 个面 + 4 槽 —— 它**不实现**上下文管理、不实现记忆
   ↑ 类型在基座之上自行实现，各自一份
类型（main/resident/rt/worker）  自己实现 context / memory / scheduler / overflow / 中断
   ↑ 内核推动它们，不实现它们
内核            推动（事件/输入/定时/LLM/工具执行）
```

表里每一格都是**该类型自己的实现**（住它自己的包），不是基座的能力差异。
基座在整张表里只贡献两样东西：**面（接口/数字/标签）** 与 **槽位（装配点）**。

### 8.2.1 rt 的滑动窗口（不是“不处置”）

`rt` 的上下文管理是**滑动窗口**，不是“无策略”：

- 视图（进 LLM 的 `msgs`）按组从**最旧**丢：一组 = `assistant(tool_calls)` + 紧随的 `tool` 消息
  （配对永不拆开），`system` 与本轮 `user` 永不丢；
- 存储（`rtChild.history`）**永不裁**（否则回收后上下文残缺）；靠“copy 一份再裁”保证；
- **滑无可滑**（`drop == len(groups)` 仍 `total > window`）⇒ 策略已失效，
  必须 `CallParent(L2, contextfull)` 让父决定（缩短本轮 / 丢弃 / 回收这通电话）。

因此 rt 的 `OverflowHandler` **不是 nil**：它就是“滑窗 + 失效则上报”。
今天 `rtTrimToWindow` 在滑无可滑时静默交出超窗请求、让上游返回 400 ——
这正是“策略失效却无人知晓”，与主 agent 早期“裁剪无解仍反复裁”同一个病。

### 8.2 为什么只有主 agent 能 offload

`MayOffload = true` 只给主 agent：让子再去拉孙子会形成**无界增殖**
（每层都能拉 MAX 个），而积压的源头是根那一条调度链。

这条从 `Hooks.MayOffload` 移到 `Descriptor.Meta` —— 它是**类型元数据**，不是行为槽位。

### 8.3 状态段的三条判据

不许把「实现归属」和「注册归属」混成一句：

1. **实现归属**：那份数据的计算在谁包里
2. **注册归属**：哪个实例持有这份数据
3. **纯内核事实**（不依赖任何类型）⇒ 基座注册

| 结构 | 实现归属 | 注册者 |
|---|---|---|
| `SchedulerStatus` | `standard` | **主 agent / 驻留子各自注册**（各自持有实例） |
| `ContextStatus` | 主 agent 自己的策略形态 | 主 agent |
| `MemoryStatus` | `memory` 模块的 `Snapshot()` | 主 agent（持有句柄） |
| `ONNXStatus` | 嵌入模块 | 主 agent |
| `ResidentStatus` | `resident` 包 | 驻留子 |
| `KnowledgeStatus` | **knowledge 插件** | **插件自己** |
| `RuntimeStatus` | 纯 Go runtime 事实 | **基座** |
| `LLMStatus` | provider 模块 | **基座**（`AgentConfig` 公有字段，每类型都有） |
| `SecurityStatus` | 安全模块（Guarded/Unguarded + active profile + 声明计数） | **基座或安全模块**（纯内核事实，不依赖类型） |

### 8.3.1 安全运行态显式化（`SecurityPosture`）

“安全模块未装配”**不是装配错误、不是拒绝，是一种运行态**。今天只能靠 nil 隐式推断，
而 nil 只出现于单测（生产永远装 Disabled Runtime）⇒ 运行态看不见。

```go
type SecurityPosture int
const (
    PostureGuarded    SecurityPosture = iota // 闸门已装配（含 Disabled ⇒ 判定零成本透传）
    PostureUnguarded                             // 未装配：恒放行（子 agent 可直接调）
)
```

必须三处显现：**启动日志一行**、**状态面 `"security"` 段**、**装配期返回值**。

与授权拆分的对应关系（两张表，不许混用）：

| 判据 | 用途 | 归属 | `PostureUnguarded` 时 |
|---|---|---|---|
| **范围**（父声明白名单） | **渲染**：进不进工具表 | **类型**（照 `Config().AllowedTools` 过滤） | **照旧过滤**（= 缺失渲染） |
| **闸门**（`CheckOutput`/`CheckTool`） | **调用**：过不过 | 安全模块 | **恒放行** |

`allowedTools` / `denyTools` 从 `residentChild` 提到 `AgentConfig`
（**类型的渲染层**要读），数据归属仍是 agents 表的 `AgentRecord.Facts`（§3.2.2）。

★ **渲染一律归类型**（用户裁定 2026-11-19）：
「各个提示词包括注入的 skill、注入的系统提示词等，都是基座提供原始信息，
agent 类型内部处理渲染，然后把渲染好的东西交给基座由基座去调内核走流程发 llm 调用」

落到代码：

| 现在住基座 | 归属 |
|---|---|
| `BuildSystemPrompt` / `renderSystemPrompt` / `kernelSections` / 插件段拼装 | 类型（主 agent） |
| `BuildMessages` / `renderL1Messages` | 类型（L1 渲染是主 agent 的上下文哲学） |
| `BuildToolDefs` / `BuildToolCatalog` / `cleanParams` | 类型（含白/黑名单过滤） |
| `BuildTaskMemoryContext` | 类型（记忆上下文是主 agent 的记忆装配） |
| `isOutputDeliveryTool`（"交付"标记语义） | 类型（**agentloop 自己解释工具标记**） |

基座留给「提示词/工具/标记」的只有**通道**：把类型交来的完整上下文
（`[]Message` + `[]ToolDef` + 选项）原样递交给内核。基座不拼段、不裁历史、
不认识人格、不认识插件段、不认识 `output_send__` 算不算交付。

---

## 9. 数据流全景（三条，不许串线）

### 9.1 输入流

```
插件 / 设备
   │ InjectInterruptTextOpts(channel, text, Priority)
   ▼
IOManager（内核驱动层）
   ├─ 安全闸门：输入进内核（`PostureGuarded` 时；`PostureUnguarded` 恒放行） |
   ▼
RouteInputByOwner：查通道登记表 → Owner 是谁
   ├─ 悬空（Orphaned && Owner==""）→ Stash，等父显式接回
   ├─ Owner == 自己 → 进自己的队列
   └─ Owner == 子 → kernelloop.RouteInput(childID, evt) → 子的 InputSink.Deliver
                                        │
                                        ▼
                              子的 agentloop（排队/抢占自定）
```

**关键**：路由发生在**进 agent 之前**。agent 不知道"这条输入是从哪条通道来的"以外的任何事。

### 9.2 事件流

```
任意模块
   │ bus.Publish(evt)       ← bus 是 FacEventBus 代理面（唯一可达）
   ▼
security proxy
   ├─ 策略闸门（Publishable）
   ├─ 脱敏
   └─ 交 EventDispatchSink
          │
          ▼
   kernelloop.Dispatch(evt, handlers)
          │
          ▼
   runDispatcher（单 worker，顺序）
          │
          ▼
   订阅者的 handler
```

**`FacEventBusRaw` 只有 security 能取。** 名字本身就是纪律。

### 9.3 工具流

```
类型 agentloop
   │ RunToolBatch(calls, opts)
   ▼
基座（5 行转发）
   │ ks.Require(FacToolExec)
   ▼
内核工具执行设施
   ├─ 安全闸门：工具执行前
   ├─ toolroute.Dispatch（按工具名找插件）
   ├─ 设备通道授权
   ├─ 并发判据 batchRunnable（>1 且全部 ParallelSafe 且不撞同通道）
   └─ 返回值清洗
   │ []BatchResult（**同序**）
   ▼
类型 agentloop（按索引落消息）
```

**同序是硬约束**：`assistant(tool_calls)` 与其后的 `tool` 结果必须相邻且一一对应，
按完成顺序落会被上游以 400 拒绝。

---

## 10. 不变量清单（可机械检验）

每条都是 `grep` / `go list -deps` 可判的。施工完成后逐条验收。

| # | 不变量 | 检验方式 |
|---|---|---|
| 1 | `kernelloop` 不认识任何 agent | `go list -deps ./internal/kernelloop` 只含 kernelapi + model + stdlib |
| 2 | 基座不认识类型语义 | `internal/agent/agent` 零 `IsChild`/`IsRealtime`/`parentID != ""` |
| 3 | 消费方不 import 实现包 | `internal/agent/**` 零 `internal/memory`/`security`/`plugin`/`knowledge` |
| 4 | 基座零持有 13 项 | 逐项 grep |
| 5 | `Hooks` 只有 4 槽 | 结构体字段数 = 4 |
| 6 | `surface.go` 只有 5 个访问器 | `grep -c "^func (a \*Agent)"` = 5 |
| 7 | 工具表表生成，零 if 分支 | `tooldefs.go` 无类型名出现 |
| 8 | 状态面表生成 | 基座 `collectKernelStatus` 无模块 import |
| 9 | 定时器全在装配侧 | `kernelloop` 零 `*Agent` |
| 10 | 类型注册只对内置插件 | `isBuiltinPlugin()` 是唯一检查点 |
| 11 | 注销时 live instances > 0 ⇒ 拒绝 | 判据覆盖 |
| 12 | 基座无默认实现 | `New()` 后未注册槽位 ⇒ 装配错误 |
| 13 | `OverflowHandler == nil` 只允许 **worker**（无上下文面） | 巡检：主/驻留/rt 三型必须有 handler |
| 14 | 重试/fallback/活动源切换只在 `kernelmod/llm` | 基座零 `maxAttempts` / `MarkUnavailable` / `a.provider =` |
| 15 | 按 agent ID 记账的表只有一张 | 零 `outputsByAgent` / `scopesByAgent` / `principals.byID` / `InstanceManager.recs` / `child.Registry` 并存 |
| 16 | 安全模块不 import `internal/agent/agent` | `go list -deps ./internal/security` 无该包 |
| 17 | 每批改动做完后，后续批次不需再动它 | **§0 第 13 条判据** |

### 第 14 条展开（这条最容易被违反）

```
A-1 改的每个位置，到 J 批完成时，是否还是现在这个形态？
    是 → 改对了（改的就是终态的一部分）
    否 → 改错了（改的是中间态，J 批要再改）
```

**中间态就是屎山的原材料** —— 不是因为它丑，是因为它要被改第二遍，
而第二遍改的人不知道该处本该是什么形态，只能就着现状再补一次。

---

## 11. 施工批次（前置关系）

| 批 | 内容 | 前置 |
|---|---|---|
| **A-1** | overflow 归类型（10 项，见 todo） | 无 |
| **A2** | 投递去布尔 + L4 夹取上移 + `Prune`/`OverBudget` 收敛 | A-1 |
| **E** | 授权渲染/调用分离 + `SecurityPosture` + 4 项数据事实下沉 | A2 |
| **G** | knowledge → 内部插件（kbtree 合并、自算 token/向量、HTTP 面） | E |
| **C+D** | agents 表归内核（**登记统一化**，§3.2.2）+ pluginhost 作发布方 + `Restore`/`Persist` + **LLM 设施归内核**（§3.2.1） | A-1 |
| **H** | `surface.go` 34 → 5 | E |
| **I** | 状态机搬入 `standard`（`task.go` 1,643 + `process.go` 809） | A-1/A2 |
| **J** | `context`/`sched` 接口 + 实现搬出（1,555 + 1,500） | **I** |
| **K** | 子 agent 包 4,614 行（rtchild/resident/spawn/rt_type/resident_type） | J |
| **L** | 清理（`interceptLoop` → 类型；import 收窄到零；黑盒判据 + 变异验证） | K |

### 11.1 落地进度（截至 2026-11-18 终）

| 批 | 状态 | 关键提交 |
|---|---|---|
| A-1 / A2 / E | ✅ 完成 | `8b580e4b`…`53a8cf88` |
| G | ✅ 完成 | `19f91d86`…`54c2540b`：knowledge 全归插件，`kernelmod/knowledge` / `plugins/kbtree` 删除 |
| C+D ① | ✅ 完成 | `d4f4f7d0`…`15a767de`：五表合一，四张旧表全删 |
| C+D ② | ✅ 完成 | `8e079475`：LLM 设施归内核（`kernelapi.LLMClient`） |
| C+D ③ | ✅ 完成 | `e4c47922`：`InstanceManager` 摘出 `*Agent`（`InstanceHost` 七方法） |
| H | ✅ 完成 | `3b3d3e21`：`surface.go` 34 → 2（构造面共 5） |
| L（import 归零） | ✅ 完成 | `42cf9fb4`：`internal/agent/**` 对 **8 个实现包全部归零** |
| L（架构判据） | ✅ 完成 | `0f43e062`：§10 的不变量变成可执行判据（含 8 条变异验证） |
| I / J / K | ⚠️ 未完成 | 见下 |

#### 不变量实测（§10，全部通过）

```
kernelloop 直接 import          : kernelapi + kernelapi/model only
internal/agent/** → memory       : 0
internal/agent/** → memory/*     : 0
internal/agent/** → nlp          : 0
internal/agent/** → security     : 0
internal/agent/** → plugin       : 0
internal/agent/** → knowledge    : 0
security → internal/agent/agent  : 0
Hooks 槽位                       : 4
surface.go 访问器                : 2（构造面共 5）
旧表 outputsByAgent / byID       : 定义已消失
```

#### I / J / K 为什么没做（结构性，不是时间）

`task.go`（1,529 行）与 `process.go`（809 行）的 step 函数引用
**38 种基座私有成员**（`a.sched` / `a.context` / `a.hooks` / `a.mainState` /
`a.usageLedger` …）。搬包意味着二选一：

1. 把这些全部导成方法 ⇒ 把 §6.3 刚删的 34 个访问器加回来
2. 把它们作为参数传进每一步 ⇒ 换一种形式的耦合（`step(a, f, sched, ctx, hooks…)`）

**真正的解法是 §6.2**：`context` / `sched` / `mainState` **本身**要先归类型
（J 批），状态机才有地方可搬。而 J 依赖 I、I 依赖 J —— 这不是"先后两刀"，
是**一次设计、一次落地**：

```
standard.Runtime{ frame, sched, context, hooks }   ← 单一载体
    ↓ 方法是它的方法（不再逐个传参）
stepPrepare / stepLLM / … → (*Runtime) 的方法
    ↓
类型只持有 Runtime，基座只剩 4 槽 + 阶段原语
```

强行拆一半会留下"半在基座半在 standard"的中间态 —— 那正是 §0 第 13 条
（每条改动先问终态）要消灭的东西。本轮**没有留下中间态**：
所有已完成的部分都是终态的一部分。

#### 为什么 I 只做了一部分（诚实记录）

`task.go`（1,529 行）与 `process.go`（809 行）里的状态机**仍在基座**。
要搬进 `internal/agent/standard`，需要先解决一个结构问题：

`stepLLM` / `stepPrepare` / `stepToolBatch` … 这些 step 函数里引用了
**38 种基座私有成员**（`a.sched` / `a.context` / `a.hooks` / `a.interruptInput` /
`a.usageLedger` / `a.MainState()` …）。搬包意味着要么把这些成员全部导成
方法（那就把 §6.3 刚删掉的访问器又加回来），要么把它们作为**参数**传进
每一步（那是另一种形式的耦合）。

真正的解法是 §6.2 那张表：`context` / `sched` / `mainState` **本身**
要先归类型（J 批），状态机才有地方可搬。而 J 批依赖 I、I 批依赖 J ——
这个循环说明**这两批必须一起设计、一起落地**，不是先后两刀。

本轮没有强行拆一半（那会留下"半在基座半在 standard"的中间态，
正是 §0 第 13 条要消灭的东西）。**下一步**：先设计 `standard` 的
`Runtime` 载体（承载 frame + sched + context 的单一结构），
让 step 函数成为它的方法，逐 step 迁出，一次到位。

**C+D 是本轮最大的「所有权交付」**，三件事必须同一批落地（否则会留下双份事实）：
① agents 表归内核并把安全三表并入（§3.2.2）；② LLM 重试/fallback 归内核、基座只持
`LLMClient` 句柄（§3.2.1）；③ `InstanceManager` 从 `*Agent` 摘下。
其中 ① 与 E 相邻：E 做「授权渲染/调用分离」，C+D 做「事实归一处」——
**两者之间不许出现「安全模块自己存一份、agents 表再存一份」的中间态**。

**I 必须在 J 之前**：`a.context.` 42 处 / `a.sched.` 54 处，74/65 个引用需要同包。

**A-1 与 I/J 零依赖**：唯一碰状态机的是 `task.go` 里三处**调用点改名**，
不需要同包（需要同包的是**删槽位**，不是改调用点）。
