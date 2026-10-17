# 内核模块化与并行装配设计

> **状态**：设计已定（2026-10-16），实现分阶段推进。
> 本文是**唯一的装配架构权威**。任何新增内核模块或插件都必须遵循本文的依赖声明契约。
>
> 关联文档：`kernel-secret-firewall-design.md`（安全模块的装配点约束是本文 §2.3 的实例）、
> `resident-subagent-design.md`（inputch 共享总线是 §5 的既有先例）。

## 1. 问题陈述

### 1.1 现状（实测数据）

| 指标 | 实测值 |
|---|---|
| `main()` 行数 | **277 行** |
| 内核模块（`init*` 调用） | **12 个** |
| `main()` 里"必须早于 / 必须在之前"注释 | **11 处** |
| 外部插件 | **42 个** |
| 内置插件 | **21 个** |
| 生产启动耗时 | **13 秒**（12:50:21 → 12:50:34） |

### 1.2 两个具体的病

**病 A：装配顺序只写在注释里，错了不报错——静默降级**

现场故障（2026-10-16）：安全模块装配晚于 `cfgReg.SetLLMSnapshotFile()`，
而后者启动即读 `core.llm.*`（需主密钥解密）。后果链：

```
密钥未装配 → Dump 解密失败（只打一行 warning）→ key 变成空串
→ initLLMProviders 拿到空 key → 所有 LLM 调用 401 invalid api key
```

症状极具误导性：401 看起来像"key 配错了"，实际是**纯粹的启动顺序问题**。
查 key 值、改 key、重配 provider **全部无效**。

> 这个缺陷**没有任何机器校验会拦它**——11 处"必须早于"都是人写的注释。

**病 B：串行等待被放大**

现场实测（同一次启动）：

```
12:50:24  clawhubadapter 启动
12:50:24→29  bilibili pysidecar ping timeout (5s)   ← 阻塞
12:50:29→34  gaode    pysidecar ping timeout (5s)   ← 阻塞
12:50:34     剩下 20+ 个插件才开始加载
```

- 两个 5 秒**完全串行**
- 而且卡住了**后面 20+ 个插件的加载**（`registry.go:415` 的加载循环是串行的）

⇒ 13 秒启动里 **10 秒（77%）是串行等待**。

### 1.3 为什么现在必须解决

后续内核模块**必然膨胀**。当前形态下每加一个模块都要：
1. 想清楚它是第几个装配（11 处注释不够，因为依赖是隐式的）
2. 改 `main()` 的调用位置
3. 如果顺序错了 → **静默出错**，症状出现在很远的地方

**这是"添加新模块的成本"，而它随模块数增长而非线性增长。**

## 2. 核心设计：声明式依赖 + 拓扑排序 + 分阶段并行

### 2.1 两类模块，两套策略（★ 必须区分）

| | **内核模块** | **外部插件** |
|---|---|---|
| 数量 | 12 个 | 42 个 |
| 形态 | 编译进 homed，同进程 | `plugin.bin` 独立进程 / `.so` |
| 依赖形态 | **真实数据流**（要拿前面的返回值） | **相互几乎无依赖** |
| 主要病 | 顺序错 → 静默降级（病 A） | 串行等待被放大（病 B） |
| 策略 | **拓扑排序 + 启动断言**（保持同步） | **有界并发**（Worker Pool） |
| 并行收益 | 小（实测大部分 <100ms） | **大（实测 10s / 13s）** |

> **★ 不得混淆**：给内核模块加并行是"用异步换不确定"，收益小而风险大；
> 给插件加并行是"把独立的等待重叠"，收益大且语义清晰。

### 2.2 内核模块：声明式依赖

```go
// Module 是一个内核模块的装配契约。
type Module interface {
    // Name 是模块的唯一标识（用于依赖图节点）。
    Name() string
    // Requires 声明本模块依赖的模块名（**不是"我要在谁之后"，
    // 而是"我需要谁已就绪"**——前者是顺序，后者是依赖）。
    Requires() []string
    // Provides 声明本模块提供的设施名（供他人依赖）。
    Provides() []string
    // Init 执行装配。ctx 提供按需取用已就绪模块的设施。
    Init(ctx *BootCtx) error
}
```

**关键约束**：

1. **`Requires` 里出现的名字必须已被某个模块 `Provides`**，否则**拒绝启动**
   （不是静默跳过——那正是病 A 的根源）。
2. **循环依赖必须拒绝启动**（拓扑排序会检测到）。
3. **`Init` 内不得调用未声明的依赖**（`BootCtx` 只暴露已就绪的设施，
   取未声明的会返回错误而不是 nil）。

#### 为什么不用总线 / 事件驱动

已评估过 `callbus + call/reply 缓存 + syncbus 状态位` 的方案，结论是**不采用**：

| 反对理由 | 依据 |
|---|---|
| **数据依赖无法靠事件解决** | `initLLMProviders(cfg, luaVM, key)` 要的是**返回值**（一个 `*VM`、一份 `cfg`）。事件到了还是得持有那个对象 ⇒ 顺序问题原地不动 |
| **同步返回值退化成异步** | 每个调用点都要变状态机，或加等待（= 又回到顺序问题） |
| **丢失可追溯性** | 现在 `panic` 有完整 Go 栈；缓存式调用只有"某条 call 没被处理" |
| **把故障变隐蔽** | 病 A 的根因是"顺序错不报错"。总线把它从"调用顺序"换成"订阅时序"，**更难查**（现有 `events.Bus` 同步、**无回放** ⇒ 订阅前发布 = 永久丢失） |
| **安全模块本身是反例** | 它用"全局单例 + `Global()` 惰性 nil"而非总线，**照样踩了顺序问题**（`ToConfig()` 在它之前读密文时 `Global()` 返回 nil → 静默空 key） |

> **结论**：总线适合**运行期的松耦合通知**（如 `events.Bus`、inputch 共享总线，
> 二者已有且有效），但**不适合启动期的确定性装配**。启动期要的是拓扑保证，不是异步通知。

### 2.3 装配调度：依赖图 + BFS 分层并行

#### 2.3.1 核心洞察（用户指出）

> 依赖图是一个由依赖关系生成的图，通过 BFS 遍历：**当前遍历到的节点初始化后，
> 下一层 BFS 就可以安全地并行初始化** —— 这是依赖图的性质。

这句话是整个调度器的**正确性来源**，值得展开说明为何成立：

设依赖图 `G`，边 `a → b` 表示 **b 依赖 a**（a 必须先就绪）。
BFS 从**入度为 0** 的节点（无依赖者）开始，逐层推进：

```
第 0 层 = { v | v 无未满足的前驱 }
第 k 层的每个节点 v，其全部前驱都在第 <k 层
  ⇒ 只要第 <k 层全部**已就绪**，第 k 层的任意子集都可以并发 Init
```

**关键性质**：第 k 层内部**两两之间无边**（否则其中一个会在第 ≥k+1 层）。
所以你不需要为"同层能否并行"做任何额外判断 —— 分层本身已经保证了这一点。

这就是为什么用 BFS 而不是"一次拓扑排序 + 逐个执行"：
拓扑序给的是**一个**合法顺序（全串行），而 BFS 分层给的是**最大化并行的顺序集合**。

#### 2.3.2 算法（伪代码）

```go
// 已被声明的设施名（Provides 的并集，随就绪增长）
ready := map[string]bool{}
// 未就绪节点：名字 -> 模块
pending := allModules

for len(pending) > 0 {
    // ① 取当前所有"依赖已全部就绪"的节点
    layer := []Module{}
    for name, m := range pending {
        if allSatisfied(m.Requires(), ready) {
            layer = append(layer, m)
        }
    }

    // ② 一个都取不到 ⇒ 有环（或依赖了不存在的设施）⇒ **拒绝启动**
    if len(layer) == 0 {
        return UnresolvableError{remaining: pending, declared: ready}
    }

    // ③ 同层并行 Init（层内无边 ⇒ 安全）
    for _, m := range layer {          // ← §2.3.4 的有界并发：不是无限制并行
        go func(m Module) {
            err := m.Init(ctx)          // 失败必须收集，不能丢
            onDone(m, err)
        }(m)
    }
    waitAll(layer)

    // ④ 层完成 ⇒ 提交它们提供的设施，进入下一层
    for _, m := range layer { ready[m.Name()] = true }
    for _, m := range layer { delete(pending, m.Name()) }
}
```

#### 2.3.3 三类失败必须**响亮拒绝启动**

| 失败 | 何时被发现 | 为何不能容忍 |
|---|---|---|
| **依赖了不存在的设施** | 建图时 | 拼错名字或忘声明 `Provides` —— 静默跳过就是病 A |
| **循环依赖** | ② 取不到节点时 | 无解；必须报出**环上的成员**（否则无法修） |
| **`Init` 返回错误** | ③ 层执行中 | 模块起不来；带着半初始化状态继续 = 后面的静默错误 |

**错误信息必须含足够定位信息**：

- 缺依赖：谁、缺谁、**已声明了哪些**（便于发现拼写差异）
- 循环：**环上的节点列表**（不只是"有环"）
- Init 失败：模块名 + 原始错误（不包装掉）

#### 2.3.4 层内并行必须**有界**

同层无边保证**安全**，但不保证**无代价**：

- 42 个插件同时 spawn 会造成内存/CPU 尖峰
  （每个 ~11MB `plugin.bin` 加载 + 握手 + `Start()` 里的重型初始化）
- 所以层内用 **Worker Pool**，默认并发上限 4（待实测内存尖峰后定）

**★ 注意**：有界并发**不削弱正确性** —— 同层节点两两无边，
无论以什么顺序完成，下一层的就绪条件都相同。并发度只影响**速度**，不影响**顺序**。
这是本设计相对"总线异步"的根本优势：**并行是可调参数，而顺序由依赖图保证**。

#### 2.3.5 与"Phase"的关系

§2.3.1 的算法是**通用调度器**；下面三个阶段是它的**输入分组**
（即：哪些模块进同一张依赖图）：

```
第 0 组（前置，**单独一张图，串行**）
  ├─ 配置库打开 + 种子
  ├─ ★ 安全模块装配（必须早于任何读 core.llm.* 的操作）
  └─ cfg 快照（ToConfig）+ LLM 快照文件
  ↑ 这三步的顺序由**副作用**决定，不是由依赖声明决定
    ⇒ **不进入通用调度器**，保持显式串行（见下）

第 1 组（内核模块，通用调度器）
  └─ 12 个模块 → 依赖图 → BFS 分层 → 同层有界并行

第 2 组（外部插件，通用调度器）
  └─ 42 个插件 → 依赖图（当前几乎无边 ⇒ 单层全并行）

第 3 组（就绪断言）
  └─ 关键依赖缺失 → 拒绝启动
```

**为何第 0 组不进入调度器**（重要，且我实测踩过）：

它的顺序由**副作用**决定（"密钥必须先装上，否则读不了密文"），
而不是由"我需要谁的返回值"决定。这类约束**用依赖图表达不了**：
`SetLLMSnapshotFile` 依赖的不是"安全模块提供的某个设施"，
而是"**读密文这个动作**发生前密钥已就绪"。

> 实测教训（2026-10-16）：这一组顺序错了，症状是 LLM 全 401，
> 而看起来像"key 配错了"。修法只能是**显式串行 + 结构性断言**
> （`cmd/homed/bootstrap_order_test.go`），不能靠依赖图。

**所以本设计不追求"一切都进调度器"** —— 强行把副作用顺序塞进依赖声明，
只会得到一个"看起来统一、实际靠隐式约定"的系统（= 病 A 换个形态）。
边界要划清：**数据依赖进图，副作用顺序显式串行 + 断言**。

### 2.4 外部插件：有界并发 + 自行声明超时

#### 2.4.1 有界并发

`registry.go:415` 当前是串行 `loadOne`。改为 Worker Pool：

- **默认并发上限 4**（可配置）：42 个 proc 插件同时 spawn 会造成内存/CPU 尖峰
  （每个 ~11MB `plugin.bin` 加载 + 握手 + `Start()` 里可能的重型初始化）
- **分池**：按 manifest 声明的"重/轻"分池，重插件单独串行或低并发
- **失败隔离**：单个插件失败不得影响同批其他插件（现在串行也有这个性质，并行要显式保证）

#### 2.4.2 ★ 插件自行声明超时（用户要求）

**动机**：超时值现在由**内核猜**（`defaultHandshakeTimeout = 10s`）。实测后果：

- 轻插件用不到 10s（浪费）
- 重插件 10s 可能不够 ⇒ 它"失败"，且失败得很晚（白等 10 秒才报错）
- 现场两个 pysidecar 各 5 秒硬编码超时（`clawhubadapter/sidecar.go:215`）

**只有插件自己知道"我大概要多久"**，所以这个值应由它声明。

**契约（外部插件，写进 `plugin.json`）**：

```jsonc
{
  "name": "my-plugin",
  "entry": "plugin.bin",
  // 本插件自行声明的建链（握手）超时，单位秒。
  // 省略或 <=0 ⇒ 用内核默认值。
  "handshake_timeout_sec": 30
}
```

**★ 两层超时必须分清**（这是本设计的关键区分）：

| 层级 | 位置 | 谁来声明 | 说明 |
|---|---|---|---|
| **L1：内核 ↔ 插件建链** | `proc/process.go` `HandshakeTimeout` | **外部插件** `plugin.json` | 内核能干预的边界 |
| **L2：插件内部的子资源等待** | 插件自己的代码（如 `clawhubadapter/sidecar.go:215`） | **插件自己**（内置插件走自己的 `ConfigDef`） | 不属于 SDK 契约，是插件实现细节 |

> **为什么 L2 不放进 SDK**：内核不知道也不该知道插件内部在等什么
> （Python 子进程？JVM？大模型加载？）。把它塞进 SDK 会让内核承担
> 它无法验证的语义。内置插件（编译进 homed）**本来就能读自己的 `ConfigDef`**，
> 走那条路即可；外部插件在自己的 `Start()` 里处理。

### 2.5 依赖图的制品化（编译期产出 + 二进制 + hash）

#### 2.5.1 为何要制品化（不只是"省一次计算"）

依赖图可以在运行期从声明重建（零制品）。制品化的真正价值是**可审计性**：

| 收益 | 说明 |
|---|---|
| **不运行就能看启动计划** | 运维可在部署前预检"这套模块会按什么顺序起来" |
| **★ 启动顺序变化在评审时可见** | 图文件的 diff 进 PR —— 这是当前**最大的盲区**（现在顺序变化只在代码的调用位置里） |
| **部署前可校验** | 不必"起一次才知道顺序对不对" |

#### 2.5.2 ★ 必须补强的一点：hash 要绑定二进制，不只是自校验

**只做"图文件自校验"会漏掉最危险的一种情况**：

| 情况 | 图完整性 | 会被发现吗 |
|---|---|---|
| 图文件损坏（磁盘错/传输截断） | 坏 | ✅ 自校验能拦 |
| **图与二进制不匹配**（改了 `Requires` 但图没重生成） | **完好** | ❌ **漏掉** |

第二种更危险：图文件自身完好、hash 也对，但它描述的是**旧代码** ⇒
按旧图调度 ⇒ 顺序错 ⇒ **正是本文要消灭的病 A**。

**修法**：把图的 hash **绑定到二进制**（构建期用 `-ldflags` 注入）：

```
构建时：
  ① 产出 depgraph.bin + 计算其 hash H
  ② go build -ldflags "-X ...kernelboot.embeddedGraphHash=H"

启动时：
  ③ 读磁盘上的 depgraph.bin，计算 hash H'
  ④ H' != embeddedGraphHash  ⇒ **拒绝启动**（不猜哪边对）
```

这样两类不一致都被拦住：图坏了（H' 变了）、图与代码不匹配（H ≠ 内嵌值）。

> **★ 为何"不猜哪边对"**：图的作者是构建流程，二进制的作者也是它。
> 两者不一致意味着**制品装配出了问题**，此时任何一边都不可信。
> 猜一个是拿启动正确性赌运气。

#### 2.5.3 二进制格式（精密二进制，不用 JSON）

格式要求与 `internal/plugin/proc/shmcodec.go`（共享段）同一套理由：
**紧凑、无歧义、解析失败可检测**。

```
┌─────────────────────────────────────────────┐
│ magic    8B   "HGDEPS01"                    │  ← 版本随格式破坏性变更而变
├─────────────────────────────────────────────┤
│ version  4B   uint32 格式版本             │
├─────────────────────────────────────────────┤
│ modCount 4B   uint32 模块数                 │
├─────────────────────────────────────────────┤
│ 模块表（每项）：                           │
│   nameLen   2B  uint16                      │
│   name      NB  UTF-8                       │
│   provides  NB  设施名（逗号分隔，已排序）  │  ← 排序保证 hash 稳定
│   requires  NB  同上                        │
├─────────────────────────────────────────────┤
│ hash     32B  SHA-256（前面所有字节）      │  ← 自校验：防截断/位翻转
└─────────────────────────────────────────────┘
```

**为何字段要排序**：同一份声明必须产出**逐字节相同**的文件，
否则 hash 在无害改动（代码顺序调换）后也会变，导致"每次都说不一致"而失去意义。

**为何自校验 hash 还要加内嵌 hash**：自校验只证明"文件没坏"，
内嵌 hash 才证明"文件与这份二进制配套"（§2.5.2）。

#### 2.5.4 文件位置与缺失时的行为

- **位置**：与内核二进制**同目录**（满足"随核心分发"）：`<homed 所在目录>/depgraph.bin`
- **缺失时**：
  - **开发/单测环境**（`go run` / `go test`）：缺失 ⇒ **运行期从声明重建**
    （否则每次改代码都要跑生成器，且测试无法独立运行）
  - **生产环境**：缺失 ⇒ **拒绝启动**（不静默回退到运行期重建）

**如何区分两者**：用内嵌 hash 是否为空作为判据 ——
构建脚本注入了 hash ⇒ 生产（缺失即错）；未注入 ⇒ 开发（允许重建）。

> 为何生产不许回退：运行期重建会让"图与代码不匹配"这个风险**变成静默容忍**，
> 而那正是本文 §2.5.2 要拦的东西。

#### 2.5.5 工具与构建接线（已实现）

**生成器**：`cmd/homed-depgraph`

| 参数 | 作用 |
|---|---|
| `-from <json>` | 从声明清单读取（过渡期形态；模块接口接入后改为 import 登记包） |
| `-out <path>` | 产出二进制制品（默认 `<exe目录>/depgraph.bin`） |
| `-print` | 只打印**分层计划**（运维/评审用，不落盘） |
| `-digest` | 只输出 digest（供 `-ldflags` 注入） |

**★ 拒绝产出空图**：没有 `-from` 时**报错退出**，而不是产出空图。
理由：空图会让 homed 认为"没有依赖" ⇒ 按一个**错误的计划**启动 ——
那比直接报错危险得多。

**★ 产出前先 `Build()` 一次**：能在编译期发现的依赖错误（缺依赖 / 环 / 重名）
不留到运行期。这是制品化的额外收益。

**Makefile 接线**（`make build`）：

```make
GRAPH_HASH=""; \
if [ -f "$(GRAPH_DECL)" ]; then \
    构建生成器 && 取 digest && 打印计划 && 落盘图 && 回显 digest ; \
fi ; \
CGO_ENABLED=1 go build ... \
    -ldflags '$(LDFLAGS) -X ...kernelboot.EmbeddedGraphHash=$$GRAPH_HASH' \
    -o build/homed ./cmd/homed/
```

⚠ **必须写成一条 shell 命令**（`\` 续行）：Make 的每行 recipe 是**独立 shell**，
普通变量跨行不保留（第一版就踩了这个，踩法是"digest 算出来了但注入是空"）。

#### 2.5.6 过渡期说明（诚实记录）

现有 12 个内核模块还是 `init*` 函数形态（**不是** `kernelboot.Module` 接口），
所以生成器目前靠 `deploy/depgraph-decls.json` 这份**手工维护的声明清单**工作。

**这本身就是一个过渡态风险**：清单是手写的 ⇒ 可能与真实代码不符。
缓解：

1. 清单文件进 PR ⇒ 与代码一起被评审
2. §2.5.2 的内嵌 hash 仍然生效（图与二进制的配套被校验）
3. **Step 4 后续增量**：把 12 个 `init*` 迁成 `Module`，声明清单即废止，
   生成器改为 import 模块登记包（那时"清单与代码不符"**结构上不可能**）

> **★ 当前已产出的分层结果（实测）**：
>
> ```
> 第 0 层（1 个）: config
> 第 1 层（7 个，可并行）: docmem, lua, memory, multimodal, security, textmem, tracker
> 第 2 层（3 个，可并行）: knowledge, llm, stagehost
> 第 3 层（1 个）: agent
> ```
>
> 对比现在硬编码的 **12 步串行** —— 这是本设计给出的**并行度提升的直接证据**。

## 2.6 syncbus：插件与内核的状态同步（插件调度的唯一驱动）

> 本节是用户设计意图的完整落实记录。**先有本节，后有实现**
> （用户要求："先完整落实计划与文档按计划推进"）。

### 2.6.1 要解决的问题

现状是**一刀切**（实测 `cmd/homed/main.go:291`）：

```
12 个 init* 全部做完 ──────────────► loadPlugins() 一次性加载全部插件
```

后果：**不需要内核全部就绪的插件，被无谓地卡在最慢的一环之后**。
一个只读本地文件的插件，也要等多模态空间（含 ONNX 加载）、知识库建索引
全部完成才能起来。

而内核那 12 步本身有**明确的先后关系**（§2.3 已算出 4 层），
所以"阶段"这个概念是天然存在的 —— 只是现在没有对外发布。

### 2.6.2 syncbus 是什么

**一份内核就绪状态的可读快照，只有一个写者。**

```go
// 内核侧（唯一写者）
type SyncBus struct {
    mu        sync.RWMutex
    ready     map[string]bool   // 设施名 -> 是否就绪
    completed []string          // 按就绪顺序记录（可审计/可调试）
}

func (b *SyncBus) MarkReady(facility string)     // ★ 只有内核能调
func (b *SyncBus) IsReady(facility string) bool  // 读取
func (b *SyncBus) Snapshot() map[string]bool     // 读取（只读副本）
```

**★ 写入点就是 BFS 的层边界**（这是本设计最自然的一处）：

`Run` 已经知道"第 k 层完成了哪些模块"（`OnLayerDone` 回调），
而每个模块的 `Provides` 就是它贡献的设施名。所以：

```
第 0 层完成（config）        ⇒ syncbus: config 就绪
第 1 层完成（memory,lua,...） ⇒ syncbus: memory,lua,keystore,... 就绪
第 2 层完成（llm,...）        ⇒ syncbus: llm 就绪
```

**同步点是层边界，而不是每个模块完成时** —— 因为层内是**并发**执行的，
若在层内逐个标记就绪，就绪顺序会**不确定**，而判据要求可复现。

### 2.6.3 权限约束：插件管理器对 syncbus **只读**

**这不是习惯，是权限设计。** 理由：

> 若插件管理器能写 syncbus，它就能**谎报内核状态**，
> 从而让插件在自己依赖的东西还不存在时启动 —— 那正是病 A（静默顺序错）。

所以类型层面就要分离：

```go
// 内核持有完整能力
type SyncBus struct { /* 含 MarkReady */ }

// 插件管理器只拿到只读接口（编译期就不可能写）
type SyncState interface {
    IsReady(facility string) bool
    Snapshot() map[string]bool
    // 注意：**没有** MarkReady/Reset —— 想写也写不了
}
```

**★ 用接口隔离而非"约定不要写"**：约定会被后人无意破坏，
而"接口上没有这个方法"是编译期保证。这与本仓
`Get[T]` 泛型取用（类型错=编译不过）是同一个思路。

### 2.6.4 插件调度：声明时序 ⇒ 依赖齐了就拉起

**插件在自己 manifest 里声明它需要哪些设施**（与 `handshake_timeout_sec` 同一套路：
插件自己声明，内核不猜）：

```json
{
  "name": "myplugin",
  "requires_facilities": ["config", "llm"]
}
```

插件管理器的调度逻辑（**被初始化后**执行）：

```
1. 遍历所有已发现插件
2. 对每个插件 P：
     若 P 声明了 requires_facilities 且**全部就绪** ⇒ 拉起 P
     若 P 声明了 requires_facilities 但**未全就绪** ⇒ 挂起，等 syncbus 更新
     若 P **未声明** ⇒ 等内核**完全**初始化完成后拉起  ← ★ 默认路径
3. 内核每完成一层 ⇒ 回调 ⇒ 重新检查挂起队列
```

### 2.6.5 ★★ 兼容性：不声明 ⇒ 现行为，**存量插件零改动**

| 插件声明 | 启动时机 | 与现状对比 |
|---|---|---|
| **不声明**（默认） | 内核**完全**初始化完成后 | **完全一致** ← 存量 42 个插件走这条 |
| 声明 `requires_facilities` | 依赖的设施全就绪时 | 新能力（尽早启动） |

**这是本设计的硬约束**（与 §3 其他硬约束同级）：

> 未声明 ⇒ 行为逐字不变。存量插件**一个都不用改**。

为什么这条不能妥协：42 个存量插件里任何一个若因本设计而改变启动时机，
就可能在不该动的时候动（例如某插件假设"我 Start 时 llm 一定已就绪"）。
**静默改变启动时机 = 静默引入竞态**。

### 2.6.6 「伴随启动」与「不伴随启动」

用户要求的两分类：

| 类别 | 语义 | 实现 |
|---|---|---|
| **伴随启动** | 依赖齐了**自动**拉起（跟随内核时序） | 声明 `requires_facilities` |
| **不伴随启动** | **不**自动拉起，等显式指令 | 声明 `auto_start: false` |

**不伴随 = 被管理而非被启动**：它的生命周期由 agent 经
`plugin_enable` / `plugin_disable` 工具控制（见 §2.6.7）。

这与现有的 `disabled_plugins` 配置（`registry.go:1046 DisablePlugin`）
是**互补**关系：
- `disabled_plugins` = **运维层面**的持久禁用（配置里记住）
- `auto_start: false` = **时序层面**的不自动拉起（仍可被显式启用）

### 2.6.7 插件生命周期模型：运行态与持久态**必须分离**

> ★ 本节修正了本设计早期的一处**误判**。第一版写的是
> "内核能力齐全，只缺两个 agent 工具" —— 实测证明那是错的。

#### 2.6.7.1 误判及其纠正

早期判断：`Registry.DisablePlugin` / `EnablePlugin` 已实现且是真停真起，
所以只要把这两个 API 包成 agent 工具即可。

**实测发现的问题（用户指出："只有 reload"）**：

```go
EnablePlugin(name) → Enable(name) → cfgReg.RemoveDisabledPlugin(name)  // ★ 改了持久状态
```

也就是说：现有 API 里**没有任何一个能"只改变运行态"**。它们全都是
"改运行态 **顺带**改持久态"。这不是"缺两个工具"，而是**语义缺失**。

#### 2.6.7.2 正确的模型：插件 = agent 的应用程序

用户给的心智模型是关键：

> 插件就是 agent 的应用程序。这些应用程序有**开机自启**，
> 有**开机不自启**。agent 应能主动**启动**、**关闭**它们。

按"应用程序"模型，**四个正交概念**：

| 概念 | 语义 | 是否持久 | 类比 |
|---|---|---|---|
| **自启**（auto_start） | 开机时是否自动拉起 | ✓ 写 manifest | 开机启动项 |
| **启用/禁用** | 是否**允许**加载 | ✓ 写配置 | 程序是否被系统禁止运行 |
| **启动**（start） | **现在**拉起 | ✗ 运行态 | 双击打开 |
| **关闭**（stop） | **现在**停掉 | ✗ 运行态 | 关闭窗口（≠ 卸载、≠ 禁止自启） |

**现状缺失的正是后两个**（运行态）。后果很具体：

- agent 想"临时关掉某插件看现象" ⇒ 只能调 `DisablePlugin` ⇒
  **顺带写了持久禁用** ⇒ 下次开机它也不自启了 —— 用户会发现
  "我只是关了一下，怎么以后都不启动了"
- agent 想"把刚才关的插件开回来" ⇒ `EnablePlugin` ⇒ 清了禁用标记 ⇒
  **下次开机会自启** —— 但如果它本来是"不自启"的，这个改动把用户设置改了

⇒ 用户的抱怨"只有 reload"精确指向这里：`plugin_restart` 是唯一
**不改持久状态**的操作，但它是"停+用新代码起"，不是"启动/关闭"。

#### 2.6.7.3 运行态门（runtime gate）与持久态门（disabled_plugins）

实现上要两个**独立**的门，而不是复用同一个：

```
loadOne(name):
    ① isDisabled(name)  ⇒ 持久禁用（disabled_plugins 配置）→ 拒绝
    ② runtimeStopped(name) ⇒ 运行态被显式关闭 → 拒绝
```

**★ 为什么不能复用同一个门**：两者的**清除条件不同**。
- 持久禁用由"启用"清除（用户级决定，要跨重启保留）
- 运行态关闭由"启动"清除（会话级决定，重启后自然复位）

若复用，就会出现 §2.6.7.2 那两个具体问题。

**★ 重启后的语义（重要且必须明确）**：
`runtimeStopped` **不持久** ⇒ 重启后所有插件按
`auto_start` + `disabled_plugins` 正常决定。
理由：运行态关闭是"这一次不想让它跑"，不是"以后都不跑"。
若它持久，agent 关掉一个插件后再也没法让它随内核起来 —— 那正是要修的毛病。

#### 2.6.7.4 agent 工具（本次要补的）

| 工具 | 转调 | 改持久态？ |
|---|---|---|
| `plugin_start` | 新 API `StartPlugin(name)` | ✗ |
| `plugin_stop` | 新 API `StopPlugin(name)` | ✗ |
| `plugin_enable` | 已有 `EnablePlugin` | ✓ |
| `plugin_disable` | 已有 `DisablePlugin` | ✓ |
| `plugin_restart` | 已有（`ReloadOne`） | ✗ |

**高危约束**（两个都要）：
1. `plugin_stop` / `plugin_disable` 必须**拒绝作用于自身**（`pluginmgr`）
   —— 把它停掉后 agent 就失去了插件管理能力，且**无工具能再启动它**
   （鸡生蛋：要启动它得先用它）。这个自锁必须在内核层面拦，不能靠 agent 自觉。
2. 同样拒绝 `webui`：停掉它 agent 与人的界面就断了。
   （这条是"约定"而非"硬约束" —— webui 可被外部手段恢复，但仍是坏体验。）

#### 2.6.7.5 SDK 兼容性：一个必须记住的**例外**

本轮给公开 SDK 的 `PluginMgrAPI` 新增 3 个方法。按 SDK 的版本纪律
（`third_party/homeagent-sdk/docs/versions.md`），"插件调用、内核实现"的
新增属**纯追加** ⇒ 存量插件不需要改、也不需要重编。

**但这次有一个例外，必须记住**：

| 对象 | 是否需要动 |
|---|---|
| 已编译的存量插件（`.bin`） | ❌ 不需要 |
| **存量插件的源码要重编** | ⚠️ **需要同步 hmapdev** |
| 新插件 | ✅ 自动 |

**原因**：本轮不只改了 `sdk/`（插件**调用**的一侧），还改了
`tools/hmapdev/templates/proc_main.go.tmpl` —— 那是**生成插件代码**的模板。
`plugin.bin` 里的 `procPluginMgr` 是**编译期**由模板生成的具体类型，
必须满足 `PluginMgrAPI` 的**全部**方法（Go 接口精确匹配）。

模板不同步 ⇒ 生成物不满足接口 ⇒ **编译失败**（不是运行期降级）。

**★ 这个不对称最容易被漏的地方**：
- 同进程插件（`.so` / 内置工厂）**直接调 SDK** ⇒ 不受影响
- 子进程插件走 **proc 桥** ⇒ 需要 ① 桥白名单 ② hmapdev 模板 **两处**同步

我当时以为"仅新增，低风险"，实测发现漏了两处（proc 桥 4 个位置 + 模板）。

**判据兜底**：`internal/plugin/proc/bridge_sync_test.go` 3 条，直接从
**SDK 源码**提取方法集，然后断言：
1. 每个方法在 proc 能力表里有登记
2. 每个方法在 Handle 分派 + case 体里都有
3. hmapdev 模板的 `procPluginMgr` 实现了全部方法

> **为何判据必须从 SDK 源码提取**（而非手写清单）：手写清单在"SDK 加了
> 方法但忘了桥接"时**也不会包含新方法** ⇒ 判据照样绿，守卫形同虚设。
> 本仓已有先例（`TestCapability_AllMethodsClassified` 初版就是这个毛病）。

#### 2.6.7.6 原 §2.6.7 的实测清单（保留作对照）



实测现状（`internal/plugins/pluginmgr/plugin.go`）：

| 能力 | 内核 API | agent 工具 |
|---|---|---|
| 安装 | `plugin_install` | ✅ |
| 列清单 | `plugin_list` | ✅ |
| 状态 | `plugin_status` | ✅ |
| 重启 | `plugin_restart` | ✅ |
| 删除 | `plugin_remove` | ✅ |
| 详情 | `plugin_info` | ✅ |
| **禁用** | `Registry.DisablePlugin(name,by)` ✅ 已实现 | ❌ **缺工具** |
| **启用** | `Registry.EnablePlugin(name)` ✅ 已实现 | ❌ **缺工具** |

**缺口性质**：内核能力齐全且 `DisablePlugin` 是**真停**
（调 `Stop()`、从注册表删除、走 `detachPlugin`、写 `disabled_plugins` 配置），
`EnablePlugin` 同样已实现 —— 只是没暴露成 agent 工具。

⇒ 新增 `plugin_enable` / `plugin_disable` 两个工具，直接转调已有 API。

**★ 权限注意**：这两个工具让 agent 能改变系统能力边界，属**高危**。
须：
1. 复用现成的 plugin 工具门禁（与 `plugin_remove` 同级）
2. `plugin_disable` 必须拒绝禁用自身同类（**不能把 pluginmgr 自己禁掉** ——
   那会让 agent 失去插件管理能力，且无工具能再启用它）

### 2.6.8 与现有机制的关系（不重复造轮子）

| 现有机制 | 关系 |
|---|---|
| `EventBus`（`initKernelAPI` 产物） | **不同物**：EventBus 是运行期**事件流**（多播、有订阅者），syncbus 是启动期**状态快照**（一次性、无历史）。不要合并 |
| `Plugin.Registry.Load`（`registry.go:415`） | syncbus 决定"**何时**调 Load"，不替代 Load 本身 |
| `handshake_timeout_sec`（§2.4） | 同一套路：插件自己声明，内核不猜 |
| `disabled_plugins` 配置 | **互补**（见 §2.6.6） |
| BFS 层边界（§2.3） | **syncbus 的写入点**（见 §2.6.2） |

### 2.6.9 判据要求（§4 的具体化）

| 判据 | 必须断言 |
|---|---|
| 默认路径不变 | **未声明**的插件在内核**完全**完成后才起（存量兼容的硬证据） |
| 只读隔离 | 插件管理器**拿不到**写接口（编译期：接口上没有该方法） |
| 按声明启动 | 声明 `["config"]` 的插件在**第 0 层完成后**就起，**早于** llm |
| 层边界可复现 | 同一份声明 ⇒ 同一就绪顺序（层内并发不得导致就绪顺序抖动） |
| 不伴随不自动起 | `auto_start:false` 的插件即使依赖齐了也**不**自动拉起 |
| 不谎报状态 | 只有内核能写；插件管理器写的路径**不存在** |

**变异要求**：每条判据都要能被"删掉对应机制"咬住（§4.2 通用要求）。

### 2.7 声明层已落地（Step 4b 第一步，2026-10-16）

`internal/kernelboot/modules/declarations.go` 把 **main() 里隐含在语句顺序中的
依赖关系**变成显式声明。实测分层结果：

```
6 层 / 17 模块，最宽层 9 个可并行（原来是 17 步严格串行）
```

#### 2.7.1 ★ cfg 的归属：修掉了"无主模块"（用户指出的问题）

**实测现状**（`main.go:186`）：

```go
cfg := cfgReg.ToConfig()    // 之后 cfg 是个游离的局部变量
```

然后被**手工传给 11 个 `init*`** —— 谁需要它，就要靠人肉排到这一行之后。
这正是"加模块必须调顺序"的病根之一。

**修法**：`cfg` 归 `config` 模块持有，图上提供 `FacConfig` 设施。
其它模块声明 `Requires: ["config"]` 即可取用。

**★ 一个必须注意的细节**：`config` 在图上**无依赖**（第 0 层），
但它有**阶段前提**——必须在 Group 0（含安全块）之后才能跑。

为什么不把这写成图边（`Requires: ["crypto"]`）：
1. Group 0 的产物（密钥/变量库）**不由任何调度模块提供** ⇒ 图会报
   "依赖了不存在的设施"（我第一版就是这么写的，判据当场抓到）
2. 更本质：这是**时序**要求（"读密文前先装好密钥"），不是**数据依赖**
   （安全块并不产出 cfg 所需的数据）。§2.3.5 已说明这类不能进依赖图

⇒ 正确表达是**阶段**：Group 0 串行跑完 ⇒ 才启动调度器。
判据 `TestConfigModuleRunsAfterGroup0` 守这个前提（变异 3/3 相关全咬住）。

#### 2.7.2 Group 0 的显式清单（可审计）

副作用顺序项现在有**显式名字**，而不是散落在 main 的注释里：

```
platform-check → heartbeat-early → logging → data-dirs
  → config-registry → security
```

**★ 为何列出来**：新增 Group 0 项必须同时补结构性判据
（`cmd/homed/bootstrap_order_test.go` 断言真实相对行号）。
判据 `TestGroup0StaysOutOfScheduler` 保证 Group 0 与可调度模块**不重叠**。

#### 2.7.3 判据 + 变异（全部咬住）

| 判据 | 守什么 |
|---|---|
| `TestDeclaredLayeringMatchesRealTopology` | 9 条关键顺序（每条都来自**真实签名**，非抄写法顺序） |
| `TestLayeringYieldsRealParallelism` | 分层确实带来并行（≥2 模块同层） |
| `TestKernelAPIIsInFirstLayer` | `initKernelAPI()` 真无依赖 |
| `TestGroup0StaysOutOfScheduler` | 副作用顺序不被误放进图 |
| `TestConfigModuleRunsAfterGroup0` | cfg 的**阶段前提**（安全块在 Group 0 里、config 不在） |
| `TestDeclarationsTraceToRealCallSites` | **声明与真实签名不漂移**（改了签名要改声明） |
| `TestNoOrphanFacilities` | 设施名无重复提供者 |

**变异 5/5 咬住**：漏 lua→llm、漏 migration→store、config 挪回 Group 0、
去掉 security、签名漂移。其中**变异 3 正是"无主模块"的复现**。

#### 2.7.4 未做（Step 4b 的剩余）

声明层已完成且可验证，但 `main.go` **仍走原来的串行代码**。
下一步是把 `Run(plan)` 真接进 main（届时 17 步变 6 层），
并让 `loadPluginsScheduled` 取代 `loadPlugins`（syncbus 驱动插件）。

这两步都要改生产启动路径 ⇒ 需要更谨慎的验证（含真机启动对比）。

### 2.8 完整启动流程与真实依赖（逐行核对，2026-10-16）

> ★ 本节是用户要求的"**先完整捋一遍再声明依赖**"的产物。
>
> **为什么必须做这一步**：我第一版依赖是从**函数签名**推的
> （`initLuaVM(cfg)` ⇒ 依赖 config）。签名只是**代理证据**，它说不了
> 下面四类隐藏依赖 —— 而隐藏依赖恰恰是"加模块就出错"的主因。

#### 2.8.1 第 0 阶段：core 自己先立起来（**不依赖任何模块**）

| # | 行 | 步骤 | 依赖 | 性质 |
|---|---|---|---|---|
| 0.1 | 41 | `requireSupportedPlatform()` | 无 | 平台门（比 flag 还早） |
| 0.2 | 43 | `parseFlags()` | 无 | |
| 0.3 | 46 | guard 模式分流 | 无 | 早退 |
| 0.4 | 71 | `startHeartbeat(dataDir)` | dataDir | ★ **时序**：必须在任何重型初始化前（否则 guard 判 stale 而 SIGKILL，实测 6 连杀） |
| 0.5 | 78 | `setupLogging(dataDir)` | dataDir | |
| 0.6 | 82 | `ensureDataDirs(dataDir)` | dataDir | 产出 agentWorkDir |
| 0.7 | **89** | `NewConfigRegistry(config.db)` | dataDir | ★ **core 的根** |
| 0.8 | **91** | `SeedDefaults` | cfgReg | |
| 0.9 | 115 | `HardenDataDir` | dataDir | 权限收紧（内嵌密钥的必要缓解） |
| 0.10 | 119 | `LoadOrCreateKey` | dataDir | ★ 必须在**任何读密文**前 |
| 0.11 | 126 | `LoadKeyring` | key | E-1 轮换 |
| 0.12 | 137 | `AdditionalDecryptKeys` | dataDir, key | 内嵌钥迁移 |
| 0.13 | 143 | `security.InitGlobal` | key | 变量库 |
| 0.14 | 153 | `InitConfigCodecWithOldKeys` | key | ★ 必须早于 0.15 |
| 0.15 | 157 | `EncryptExistingSecrets` | codec | 幂等迁移 |
| 0.16 | 173 | `AuditSecrets` | key | 诊断（E-2） |
| 0.17 | 184 | `SetLLMSnapshotFile` | **security** | 读 `core.llm.*` |
| 0.18 | 186 | `cfg := cfgReg.ToConfig()` | **security** | 读 `core.llm.*` |

**★ 0.17/0.18 是"阶段依赖"，不是"数据依赖"**：
它们并不消费安全模块的产物，而是"必须在密钥装好之后读，否则解出空串"。
实测后果（我引入过）：全 provider 401，症状像"key 配错了"。
⇒ 归 **Group 0**（显式串行 + 结构性判据），不进依赖图。

#### 2.8.2 ★★ 四类**签名看不到**的隐藏依赖（本次逐项排查结果）

用户指出"必须先完整捋一遍"，以下是排查出的四类。**其中 1 项是真缺陷隐患**。

**(A) 包级可变全局单例 —— 实测只有 1 处**

全仓扫描 `internal/` 的包级 `var`（排除常量/嵌入资源/注册表）后，
**唯一**的可变全局单例是：

```go
// internal/nlp/parser.go:15
var defaultParser Parser            // SetDefaultParser 写，GetParser/NewExtractor 读
```

**★ 它构成一个真实的运行时依赖**：

- 写者：`initONNXParser`（main:289）
- 读者：`nlp.NewExtractor(nil)` —— 在**构造时**快照 `defaultParser`
  （`parser.go:60-62`），由 `extractKeyTriples`（`pipeline.go:503`）在
  **蒸馏循环里**调用
- 读取者的启动点：`startMemoryCandidateConsumer`（main:226）→ `distiller.Start()`

**顺序上：蒸馏在 226 行启动，解析器在 289 行才装上。**
按签名看这是"缺依赖"，但我核实后确认**当前是安全的**，原因是：

```go
func (d *Distiller) distillLoop() {
    ticker := time.NewTicker(d.cfg.Interval)   // ★ 首次执行要等一个 Interval
    for { select { case <-ticker.C: d.distillOnce() ...
```

`distillLoop` **不是立即执行**，首个 tick 要等 `Interval`（配置项，远大于
启动耗时）⇒ 那一刻 `defaultParser` 早已装好。

**★ 但这个"安全"是巧合，不是保证**：它依赖 `Interval > 启动耗时`。
若将来有人把 Interval 调成 0/极小，或把 `initONNXParser` 挪得更后，
就会**静默降级**（`pipeline.go:60` 的注释已记录后果：
`defaultParser` 为 nil ⇒ POS 模板抽不出 ⇒ 图记忆里 188 个实体全是整句复合值
⇒ 跨维度检索全失效，基线 0/5）。

⇒ **处置**：声明式下把它变成**显式依赖**（`onnx-parser` 必须在
`text-memory` 之前）—— 这正是模块化的价值：**把"碰巧安全"变成"结构保证"**。

**(B) 共享文件/SQLite 的读写顺序**

| 句柄 | 写/建 | 读/依赖 |
|---|---|---|
| `config.db` | `NewConfigRegistry`(89) | 此后所有 `cfgReg.*` |
| `memory/graph.db` | `initMemoryStack`(190) | `newStageAndRegistry`(271)、`newMainAgent`(276) |
| `memory/text/` | `initTextMemory`(220) | 同上 |
| `knowledge/` | `initKnowledgeStore`(263) | 同上；且 `initKnowledgeMigration`(262) 必须**早于**它 |
| `adapters/`（lua） | `initLuaVM`(200) | `initLLMProviders`(243) |

⇒ 这些是**真实数据依赖**，已在声明里表达。

**(C) `defer` 的逆序语义**

原 main 用 `defer closeMem()` 等 5 处，释放顺序 = **注册顺序的逆序**（LIFO）。
模块化后改为 `Put("<facility>.close", fn)` + `CloseAll()` 按**发布顺序逆序**执行
⇒ 语义**逐字等价**（判据 `TestBootCtxCloseAllIsReverseOrder` 守）。

**(D) 包级 `init()` 注册（BuiltinFactory）**

`newStageAndRegistry` 依赖**各插件包 `init()` 自注册的工厂**
（`plugin.RegisterFactory`），而 merge 顺序由 **import 顺序**决定。
这**不是**图能表达的顺序（Go 规范保证 `init()` 在任何 `main` 语句之前跑完）
⇒ 视为"启动前已就绪"，不进依赖图。但它意味着：
**删除一个插件包的 import 会静默让它消失**（有空导入清单把关）。

#### 2.8.3 依赖表的两个来源（**必须一致**）

| 来源 | 强项 | 弱项 |
|---|---|---|
| **签名**（参数从哪来） | 明确、可直接扫 | 看不到全局单例/文件/init 注册 |
| **运行期读取**（谁在何时读） | 抓到 (A)~(D) | 要逐个函数追 |

**⇒ 判据必须同时钉住两者**：
- `TestDeclarationsTraceToRealCallSites`：声明与**签名**不漂移
- §2.8.2(A) 的 onnx→textmem 顺序：声明与**运行期读取**不漂移

## 3. 兼容性约束（★ 硬约束）

1. **存量插件的 `plugin.json` 不变必须照常工作**
   - `handshake_timeout_sec` 省略 ⇒ 用默认值（与改动前逐字一致）
   - 本仓无 `DisallowUnknownFields`，故"旧内核读新插件"与"新内核读旧插件"都无害
2. **存量内核模块的行为不得改变**——拓扑排序后，原来的顺序必须仍被满足
   （若拓扑排序得出的顺序与手写顺序冲突，说明手写顺序里有**未声明的隐式依赖**，
   必须显式声明，而不是让排序器猜）
3. **装配失败必须响亮**：缺依赖 / 循环依赖 / `Init` 返回错误 ⇒ 拒绝启动
   （不得静默跳过——那正是病 A）

## 4. 判据要求

每条实现阶段必须配套判据，且判据**必须测真实入口**（本仓反复教训）。

### 4.1 调度器（BFS 分层）的判据

调度器是**纯逻辑**（给定依赖图 → 给定层次与并行分组），所以判据可以很硬：

| 要守的性质 | 判据方式 | 变异验证 |
|---|---|---|
| **分层正确** | 给一个已知图，断言层次划分（同层两两无边、每个节点的前驱都在更早层） | 把 bfs 退化成"全串行"必须仍能看出**层次**丢失 |
| **同层确实并行** | 层内两个模块各 sleep，总耗时 ≈ 单个（而非相加）；或用并发计数器观测"同时在跑" | 改成逐个串行必须报红 |
| **有界并发生效** | 层内放 8 个模块、上限设 2 ⇒ 观测峰值并发 ≤ 2 | 忽略上限必须报红 |
| **缺依赖拒绝启动** | 声明依赖一个不存在的设施 ⇒ 必须错误，且错误里含**缺的名字**与**已声明列表** | 静默跳过必须报红 |
| **循环依赖拒绝启动** | A→B→A ⇒ 必须错误，且错误里含**环上的节点** | 报"有环"但不说成员也算不合格 |
| **Init 失败传播** | 同层一个模块 Init 报错 ⇒ 整体失败（不被其他成功掩盖） | 吞掉错误必须报红 |
| **无依赖 ⇒ 单层全并行** | 42 个无边节点 ⇒ 一层（仅受并发上限约束） | —— |

**★ 调度的判据不能只看"最终都 Init 了"**：那是串行也能满足的弱断言。
必须断言**层次结构与并行性本身**（本会话反复吃过的"判据太弱"）。

### 4.2 各阶段判据

| 阶段 | 判据 | 变异验证要求 |
|---|---|---|
| 第 0 组 | 安全模块装配点早于任何读 `core.llm.*` 的操作（结构性断言） | 移回去必须报红 |
| 第 1 组 | 12 个内核模块走调度器後，**结果与手写顺序等价**（不是"换个顺序也能跑"） | 打乱声明仍得出同一层次 |
| 第 2 组 | 外部插件并发加载：确实并行；并发上限生效；单插件失败不连累他者 | 去掉并发必须报红 |
| Step 2 | 声明的超时**真的穿过 NewWithTimeout → Start → Spawn**；未声明时用默认 | 断接线必须报红 |
| 第 3 组 | 关键依赖缺失 → 拒绝启动（不是静默降级） | 静默跳过必须报红 |

**★ 启动顺序类判据的注意**（本会话踩过两次）：

1. 只断言"某个调用出现在某行"不够——必须断言**真实的相对位置**，
   且要覆盖**所有**读取点（第一版只盯了 `ToConfig()`，漏了 `SetLLMSnapshotFile`，
   于是判据变绿而线上照旧报错）。
2. **只测辅助函数不够**——必须覆盖**完整接线**。
   实测（Step 2）：`TestSpawnHonorsDeclaredHandshakeTimeout` 直接把
   `Options.HandshakeTimeout` 喂给 `Spawn`，删掉 `Start()` 里传值那一行它**依然全绿**。
   补一条走 `NewWithTimeout → Start → Spawn` 的判据后才咬住。

## 5. 与既有机制的关系

| 既有机制 | 关系 |
|---|---|
| `events.Bus` | **保留**，用于运行期事件通知（同步、无回放，不适合启动装配） |
| `inputch` `ChannelRegistry` | **保留**，父与驻留子共享的通道登记表（运行期共享总线的既有先例） |
| 插件 SDK | **保留并扩展**：`plugin.json` 加 `handshake_timeout_sec`（Step 2 已完成） |
| `security.Global()` | **保留**，但其"惰性 nil 容忍"**仅限真正可选的模块**；关键依赖必须显式检查 |
| `pluginReg` 的 9 个 `SetXxx` | **保留**，作为"窄接口 + 显式注入"的既有范例 |

## 6. 实施顺序（每步可独立验证、可回滚）

```
Step 1  ✅ 本设计文档（本文）
Step 2  ✅ L1 超时声明：plugin.json → manifest → proc.NewWithTimeout → Start → Spawn
          判据 8 条（含真实消费者 + 完整接线）+ 变异 2/2 咬住
Step 4a ✅ 依赖图制品化：kernelboot 包（BFS 分层 + 二进制编解码 + 双重 hash）
          + cmd/homed-depgraph 生成器 + Makefile 接线
          判据 20 条 + 变异 8/8 咬住
Step 4b ⏳ 把 12 个 init* 迁成 kernelboot.Module（声明清单即废止）
Step 3  ⏳ 外部插件有界并发加载（收益点：实测 10s → 预计 ~2s）
Step 5  ⏳ 第 3 组就绪断言（把 §1.2 的坑变成"启动即报错"）
```

**每步都必须**：加判据 + 变异验证 + 真机验证（启动耗时可测，是天然度量）。

### 6.1 Step 2 的实测结果（已完成）

| 项 | 结果 |
|---|---|
| 新增判据 | 8 条（解析 / 存量兼容 / 换算 / 上限 / 未知键 / Spawn 行为 / 完整接线 / 默认值） |
| 变异验证 | 断接线、忽略参数 ⇒ **2/2 咬住** |
| 真实消费者 | 用新建的 `slowshakeplugin`（故意延迟握手）证明不同超时值产生**不同可观测结果** |
| 关键教训 | 初版判据直接给 `Spawn` 传参，**绕过**了 `Start()` 接线 ⇒ 删接线仍全绿（第 10 次同类） |

## 7. 待定问题

- [ ] 内核模块的 `Provides`/`Requires` 命名空间是否需要前缀（避免撞名）？
- [ ] 第 2 组（插件）的默认并发上限取值？（需实测内存尖峰后定，暂定 4）
- [ ] 是否允许插件声明"依赖其他插件"（插件间依赖）？当前认为不需要——
      插件间通信走 `events.Bus` / inputch，不该有启动期依赖。
- [ ] 调度器是否需要持久化"上次成功的层次"以便对比？（用于发现"层次变了"这种隐式顺序变更）

---

## 8. Step 4c：插件宿主归位为内核模块（方案 A，2026-10-16）

### 8.1 决策：为什么调度逻辑必须在**内核模块**里

用户选定方案 A。自举问题说清了原因：

- `pluginmgr` 是**插件** ⇒ 它自己要被拉起 ⇒ 若调度逻辑在它内部，
  就得**先拉起自己才能拉起自己**（循环）。
- 调度器要订阅内核的 syncbus（内核设施），依附内核模块更自然。

⇒ **调度放内核模块**（`internal/kernelmod/pluginhost`），
`pluginmgr` 插件只保留**人机界面**职责（列表/启停/装卸的工具与 HTTP）。

### 8.2 ★★ 核心收益：插件加载不再等 agent 的 35 秒

原先 `wirePluginSDK` 写在 **agent 模块**里（第 4 层），于是插件加载被迫排在
agent 之后 —— 而 agent 层实测要 **35 秒**（context dense backfill 750 events
+ 重建索引）。实测时间线（修复前）：

```
21:33:51  early_probe 被拉起   ← 装配期（plugin-host 自己）
21:34:27  第 4 层完成: agent, plugin-host
```

⇒ **早拉起 36 秒**。修复后同一机器稳定复现。

**拆法的依据（先核实再动手）**：

| 插件加载的硬前置 | 来源 |
|---|---|
| `plugin.Registry` | stage-host 模块（第 3 层） |
| `StageHost`（注册工具） | stage-host 模块 |
| `Cfg` / 插件目录 | config 模块（第 0 层） |
| LuaVM / Supervisor / Tracker / Indexer | 第 1~2 层 |

而 `StatusProvider` / `TerminalAPI`（来自 agent）是**软前置**：

- SDK 的 `Plugin` 接口只有 `Name/Start/Stop` —— **没有 `Configure`**。
  （agent 模块里那句"插件 Configure 需要 StatusProvider"的注释**已过时**。）
- 唯一在 `Start` 阶段引 `s.Status()` 的插件是 `healthcheck`，
  且它是 **nil-guarded**（`if s.Status() != nil { … }`）。
- `cli` 插件的 `s.Status()` 全在 HTTP handler 里（请求时才调）。

⇒ 拆成"底层 7 项在 plugin-host 注入、agent 相关的 2 项由 agent 模块
**后补注入**"。所有 setter 都是**简单赋值**（`r.status = sp`），
可在插件加载后再调用；插件对它们**懒读** ⇒ 安全。

### 8.3 分层结果（编译期算出）

```
第 0 层（2）: config, kernel-api
第 1 层（8）: doc-store, knowledge-migration, lua, memory, multimodal, onnx-parser, supervisor, tracker
第 2 层（3）: knowledge-store, llm, text-memory
第 3 层（1）: stage-host
第 4 层（2）: agent, plugin-host        ← ★ 并行（原先 agent 独占第 4 层）
```

`plugin-host.Requires` = `config, kernel-io, memory, lua, supervisor, tracker,
text-memory, doc-store, knowledge, stage-host`（**刻意不含 agent**）。

### 8.4 本模块做什么（用户的说法）

> 插件管理器不应该在被拉起后就去读插件列表获取元数据并写入缓存准备拉起吗？

对。`plugin-host.Init` 五步：

1. **注入**插件 SDK 的底层 7 项依赖（不含 agent 的两项）
2. **扫一次**目录 → **元数据缓存** `PluginCatalog`（存进 `ks.PluginCatalog`）
3. 按**三形态**分流（§2.6.6）
4. 拉起"声明设施已就绪"的；"等更晚设施"的挂到 syncbus（`waiter.go`）
5. **加载内置插件** —— `LoadSkipping(dir, skip)`

**元数据缓存**同时供三方用：调度器（判时机）、`pluginmgr`（列列表/状态）、
显式启用路径（校验名字存在）。原先这两处各读各的 `plugin.json`。

### 8.5 ★★ 实测踩到的两个真缺陷（都必须写下来）

#### 缺陷 1：内置插件全部消失（19 个）

第一版只处理"有 `plugin.json` 的**外部插件**" ⇒ 结果：

| | 插件数 | 工具数 |
|---|---|---|
| 缺陷版 | 2 | **10** |
| 修复版 | 20 | **86** |

丢的是 `webui`（人机界面）、`pluginmgr`（插件管理）、`cli`、`files`、
`cmd`、`timer`、`mcp`… —— 全是**编译期工厂注册**的内置插件，
目录里**没有 `plugin.json`**。

**为什么危险**：日志看起来正常！只有一条容易被当噪音略过的提示
（我初版还把它写成"跳过 19 个非插件目录" —— 措辞本身就是误导）。
若无真机 e2e，生产会从 **337 工具掉到 10 个**。

**修法**：走 `LoadSkipping(dir, skip)` —— 那是**唯一**同时加载
外部插件（扫目录）与内置插件（遍历工厂表）的入口。
并修正日志措辞（"N 个目录无 plugin.json，**内置插件属正常**"）。

#### 缺陷 2：三形态的 skip 集合漏项

`skip` 集合若不含 `manual`（`auto_start=false`）⇒ 该插件会被
`LoadSkipping` **立即全量加载** ⇒ 与"启用但不自动启动"**完全相反**。

**修法**：抽 `buildSkipSet(g)`，把三形态 + 内置插件的边界写在一处：

| 类别 | 在 skip 里？ | 理由 |
|---|---|---|
| ② `auto_start=false` | ✅ | 否则被全量加载（违背语义） |
| ③ `disabled` | ✅ | 显式跳过（registry 另有禁用门兜底） |
| ① 声明了依赖的 | ✅ | plugin-host 自己按时机拉起（防重复） |
| **内置插件** | ❌ | **必须**走 `LoadSkipping` 的工厂分支 |

### 8.6 ★★ 判据教训：源码字符串检查**连续骗过我三次**

| 版本 | 判据写法 | 变异 | 结果 |
|---|---|---|---|
| 一版 | `Contains(code, "LoadSkipping(")` | 删掉调用 | **仍绿**（注释里有该字样） |
| 二版 | `Contains(code, "LoadSkipping(dir, skip)")` | 改成 `(dir, nil)` | **仍绿**（注释第 116 行正好写着这个串） |
| 三版 | **AST**：找 `CallExpr` 且第二实参非 `nil` | 同上 | ✅ 咬住 |
| 三版 | AST：`buildSkipSet` 有真实调用点 | 改成不调用 | ✅ 咬住 |

**根因**：本仓规范要求"每个关键决定都在注释里讲清楚" ⇒ 注释里**几乎必然**
出现关键符号名 ⇒ `strings.Contains` 型判据**永远绿**。

**结论（写进规范）**：

> 源码字符串检查**只能**用于断言"某个符号定义是否存在"
> （如 `func buildSkipSet(`），
> **绝不能**用于断言接线/调用顺序。
> 断言接线必须用：① **AST**（`CallExpr` + 实参检查），
> 或 ② **行为测试**（真的调那个函数）。
> 顺序类不变量用**行号/字节位置比较**（本文件 §4 的既有做法）。

### 8.7 syncbus 的落点修正（实测撞到）

模块的 `Init(ks)` **只能看到 `KernelState`**，而 `SyncBus` 原先只是
`RunKernelModules` 的局部变量 ⇒ 模块**拿不到它**，无法订阅状态变化
（`plugin-host` 要挂"等 agent 就绪再拉起某些插件"的回调）。

**修法**：`SyncBus` 放进 `KernelState`（`ks.SyncBus`，由 `Run` 填入）。
它本来就是**内核级共享状态** —— 用户"内核用一个结构体收集全部共享变量"
的设计正好覆盖它。

### 8.8 迁移引入的**新时序要求**（必须复核的清单）

把插件拉起从"装配后"挪到"装配期"，引入了两处顺序要求：

| 事项 | 必须在 | 理由 |
|---|---|---|
| failback 插件白名单 | **装配前** | 装配期已拉起 ⇒ 晚设则受限启动失效 |
| 白名单的 `SetLoadAllowlist` | stage-host 就绪后、插件拉起前 | 需要 `plugin.Registry`（stage-host 产物） |

实现：`resolveFailbackAllowlist`（装配前解析）+
`OnLayerDone` 钩子在 registry 就绪时 `SetLoadAllowlist`。

### 8.9 本步判据

`internal/kernelmod/pluginhost/catalog_test.go`（6 条，全绿）：

1. `TestClassifyThreeForms` —— 三形态分流 + **互斥性**
2. `TestCatalogCachesMetadata` —— 每个字段都缓存对
3. `TestScanReportsNoManifestDirs` —— 无 manifest 的目录被**如实报告**
4. `TestBuiltinsAreNotSkipped` —— **内置插件不在 skip 里**（行为判据）
5. `TestSkipSetHasRealCallSite` —— **AST** 判定 `buildSkipSet` 被真实调用
   且 `LoadSkipping` 的第二实参非 `nil`
6. `TestCatalogConcurrentRead` —— 并发读安全

**变异验证 5/5 咬住**：去掉 `LoadSkipping` / `buildSkipSet` 跳内置插件 /
`manual` 不进 skip / `(dir, skip)→(dir, nil)` / `buildSkipSet` 不被调用。

**连带修复的 3 个既有判据**（因锚点随模块化失效，全部**换锚而非放宽**）：
`TestMainInitsSecurityBeforeProviders`、`TestSecurityModuleInitializesBeforeConfigSnapshot`
（并额外加固："provider 注册确实在 llm 模块里"+ "llm 模块确实被 import"）、
`webui/handler_plugin_test.go` 的 mock（补 SDK 新增的 3 个方法）。

### 8.10 补充：复用内核 Scheduler（删掉我手写的重复实现）

第一版我在 `pluginhost/waiter.go` 手写了一套"等设施就绪再拉起"的调度
（124 行）。**那是重复造轮子**：`kernelboot.Scheduler`（309 行）做的
正是这件事，而且已有完整判据（拼错校验、同批去重、`OnUpdate` 订阅、
`LaunchedFlag`、`SetLaunchErrorHandler`…），我的手写版只覆盖了一小半。

**修法**：删掉 `waiter.go`，改用 `kernelboot.NewScheduler(ks.SyncBus, launch)`。

★ 且这里**不需要两段式**（原调度器在 main 里、装配前拿不到插件注册表，
所以要 `attach`/`SetLaunch` 分两段）：`plugin-host` 在**第 4 层**，
执行 `Init` 时 `stage-host`（第 3 层）已就绪 ⇒ 注册表**已存在**
⇒ 拉起动作可一次性接上。**分层位置变好，顺带消掉了两段式的复杂度。**

**教训**：迁移功能时先搜"内核里是不是已经有这个能力"。重复实现不只是
浪费，更会让"两份实现改一处漏一处"成为必然。

### 8.11 补充：typed-nil 陷阱（实测 panic）

`ks.SyncBus` 语法上是 `*SyncBus`。若直接传给 `NewScheduler(bus SyncState, …)`：

```go
var nb *SyncBus
var i SyncState = nb   // ★ i != nil！经典的 typed-nil 陷阱
```

接口值**非 nil**（它带类型信息），于是 `bus.IsReady(...)` 解引用空指针 ⇒ **panic**。

**实测复现**：`TestNilTypedBusSafety` 确认 panic 真实发生
（`invalid memory address or nil pointer dereference`）。
而 panic 在**插件拉起路径**上 ⇒ 整个内核启动失败。

**修法**：显式判 `ks.SyncBus != nil` 后再赋给接口变量；
为 nil 时用一个**已标记内核完成**的空 bus 退化（安全退化：依赖未就绪的
插件在 `Start` 时自行处理，healthcheck 之类本就 nil-guarded）。

**判据** `TestNilSyncBusDoesNotPanic` 两段：
① 确认陷阱**真实存在**（typed-nil 确实 panic —— 否则判据是空的）；
② `mod.go` 必须有显式 nil 判断，且**不得**直接传 `ks.SyncBus`。
（★ 这条用 `strings.Contains` 是**可取**的：它断言的是"是否存在某个
防御性判断"，属于"符号定义是否存在"那一类，不是接线断言。）

### 8.12 ★★ 违反 §2.6.5 硬约束（我引入的第三个缺陷 —— 有实测后果）

#### 缺陷本身

把插件拉起搬进 `plugin-host.Init`（第 4 层）后，我按"本模块依赖是否覆盖
插件声明"把插件分成 `launchNow` / `waitLater`，并**在 Init 里直接加载 launchNow**。

问题：**未声明依赖**的插件 `RequiresFacilities` 是**空** ⇒ "本模块依赖覆盖空集"
**恒真** ⇒ 它们被归入 `launchNow` ⇒ **第 4 层就被加载**。

而 §2.6.5 的硬约束是：

> 未声明 ⇒ 启动时机**逐字不变**（内核**完全**初始化完成后）
> ……**静默改变启动时机 = 静默引入竞态**。

**实测证据**（planA4.log）：

| | 时刻 | 阶段 |
|---|---|---|
| `loaded: healthcheck` | **21:54:37** | 第 4 层（agent 并行中） |
| 内核装配完成 | 21:55:12 | — |

⇒ 早了 **35 秒**。

#### ★ 后果不是理论上的 —— 精确地丢了一个工具

`internal/plugins/healthcheck/plugin.go:249`：

```go
if s.Status() != nil {
    p.selfToolNames["healthcheck_kernel"] = true
    s.RegisterTool("healthcheck_kernel", …)
}
```

第 4 层时 agent 还没建好（它与 plugin-host **并行**）⇒ `s.Status()` 为 **nil**
⇒ **静默少注册一个工具**。实测工具数 **86**（应为 **87**），**无任何报错**。

这正是 §2.6.5 那句警告的**逐字实现**：nil 保护把"时序错了"降级成
"少个功能"，是最难发现的一类缺陷。修复后（planA5）87 工具、`healthcheck_kernel` 回归。

#### 修法

1. **不再自己判时机**：全部自动启动的插件都交给 `kernelboot.Scheduler`
   ——它已正确实现 §2.6.5（空声明 ⇒ 等 `KernelComplete()`）与 §2.6.6
   （`!AutoStart` ⇒ 不自动拉起），且有完整判据。
2. **未声明的 + 内置的**：挂到 syncbus 的 `KernelComplete` 上再 `LoadSkipping`
   （内核**推**状态，不轮询）。

### 8.13 ★★ 双加载（第 4 个缺陷，按构造修复）

`buildSkipSet` 我第一版只跳过 `declared ∪ manual ∪ disabled`
⇒ **未声明的外部插件不在 skip 里** ⇒ 它同时被两条路加载：

- 调度器（它管未声明的，等 `KernelComplete` 后拉起）
- `LoadSkipping`（不在 skip 里 ⇒ 也加载）

**实测（变异 G'，忠实复现第一版）**：`undeclared_probe` 被 loaded **2 次**，
工具数 **92 → 97**（+5 = 它重复注册的那批）。

planA5 侥幸未触发，只因测试目录里那 3 个外部插件恰好都声明了依赖或不自启
—— **不能依赖这种巧合**。

**修法（按构造消除）**：

> **catalog 里登记过的插件，一律不交给 `LoadSkipping`。**

这样两条路**按定义不相交**：Scheduler 管 catalog 内全部（三形态之一），
`LoadSkipping` 只管 catalog **之外**的 —— 也就是**没有 `plugin.json` 的内置插件**
（编译期工厂，只有它能加载）。

**真机终验**（有未声明外部插件 `undeclared_probe`）：

| 检查 | 结果 |
|---|---|
| 未声明插件等内核完全初始化 | ✅ `22:16:53`（装配完成时刻） |
| 声明的插件早启动 | ✅ `22:16:25`（早 **28 秒**） |
| 未声明插件只加载一次 | ✅ 1 次 |
| `healthcheck_kernel` 回归 | ✅ 92 工具 |

### 8.14 本步判据（终版，9 条）

新增两条**守 §2.6.5 与双加载**：

8. `TestUndeclaredPluginsWaitForKernelComplete` —— 用**真的 Scheduler + 真的
   SyncBus** 跑生产路径：未声明的在内核完成前**不得**被拉起，完成后**必须**被拉起；
   声明的在依赖就绪时**应当**被拉起（保住本步核心收益）。
9. `TestUndeclaredNotDoubleLoaded` —— catalog 内的插件**必须在** skip 里
   （否则双加载）；内置插件（不在 catalog）**不得**在 skip 里。

**变异验证（4 轮）**：

| 变异 | 结果 |
|---|---|
| 未声明的也直接拉起（复现 §2.6.5 违反） | 单元判据咬住 |
| 把 `LoadSkipping` 挪回 Init（复现原 bug） | 真机：healthcheck **loaded 2 次**，工具 87→**161** |
| `skip` 只含 declared/manual/disabled（第一版写法） | 单元 + **真机双咬**：`undeclared_probe` 2 次、工具 92→**97** |

★ **变异 G 的教训**：第一次变异（只删 `c.Names()` 循环）是**空操作** ——
因为 `c.Names()` 与三形态循环**重叠**（三形态之和 = catalog 全部）。
**变异必须忠实于"原缺陷的写法"，否则变异本身无效**（假绿）。改成
"skip = declared+manual+disabled"（第一版的真实写法）后才咬住。
