# 内核安全模块（Secret Firewall）设计 · 计划追加

> **架构形态（2026-10-31 收敛后）**：security 是**外围内核到核心内核之间的防火墙**
> —— 它自己提供接口、判定全部内聚在自己内部。调用方只提供**事实**
> （谁在跑、有没有父、原始载荷），不提供**结论**（我是哪类主体、该走哪条判定链）。
>
> 落地形态：**每 agent 一个策略执行视图**（`AgentScope`），共享一个注册表
> （`profiles` / `principals` / `scopes`）。档位分两层：
> `sessionProfile`（轮次级，End 即清）/ `longtimeProfile`（agent 生命期）。
> 详见 `internal/security/agentscope.go` 与 `round.go` 的包注释。
>
> **状态：部分实现。** 凭据防火墙、配置加密、变量库统一与 C1–C8 判据已落地并**在生产线生效**；
> 但**三条能力机制完整却零生产调用方**（敏感词/注入预警、路径沙箱、网络闸）。
> 本文保留原始设计与逐条审计记录；未实现项统一登记在
> [`future-plan.md`](./future-plan.md) §一。
>
> ## 接线状态表（2026-10-31 实测，逐个查生产调用方）
>
> 本仓反复吃亏在「机制写了、判据绿了、生产里从不执行」。所以这里不写✅/❌定性，
> 只写**实测的生产调用点**：
>
> | 能力 | 机制 | 生产调用点 | 状态 |
> |---|---|---|---|
> | 出向凭据拦截 `PrepareOutbound` | 完整 | `eventguard.go` / 各界代理 | ✅ 生效 |
> | 入向凭据清洗 `ScrubInbound` | 完整 | `eventguard.go`、`events/bus.go`、`device/mod.go` | ✅ 生效 |
> | 执行前回填 `FillForExecution` | 完整 | stage 管道 | ✅ 生效 |
> | 工具黑白名单 `CheckTool` / `checkToolWithProfile` | 完整 | `security/agentscope.go` → `AgentScope.CheckTool` | ✅ **2026-10-31 接通** |
> | 按主体授权 `CheckToolFor` | 完整 | 同上（`checkToolForWithProfile` 同时叠主体策略） | ✅ **2026-10-30 接通** |
> | 插件命名档位 `DeclareProfile` | 完整 | `sdk/security_impl.go`、`proc/corehandler_security.go` | ✅ 声明侧通 |
> | 插件命名档位 `ActivateProfile` | 完整 | `security/agentscope.go` 的 `setSession` + `BeginRound` | ✅ **2026-10-31 接通**（每 agent 独立实例） |
> | 敏感词/注入 `ScanContent` | 完整（254 行） | **无** | 🔴 配置里的敏感词一条不生效，见 future-plan §1.2 |
> | 路径沙箱 `CheckPath` | 完整 | **无**（只被 Runtime 自己调 Policy） | 🔴 见 future-plan §1.3 |
> | 网络闸 `CheckNetwork` | 完整 | **无**（同上） | 🔴 见 future-plan §1.3 |
> | 事件总线授权 + 载荷过闸 | 完整 | `security/mod.go`（唯一装配点） | ✅ 生效 |
>
> ★ **历史的教训（两轮都错）**：
> 第一轮把「`Agent.security` 从未被赋值」诊断成「少一个注入点」而补了 `SetSecurity()`，
> 但生产装配链从不调它；第二轮才看清根因是 **`kernelmod/agent` 的 `Requires()` 没声明
> `FacSecurity`** —— 依赖图没理由把 security 的产物交给 agent。
> 所以「接线」必须在**依赖图层面**表达，不能靠事后注入。
>
> 记录一条由用户纠正后确立的**架构决定**，以及它带来的简化与下沉。
>
> 撰写时间：2026-10-14。触发：用户在讨论 commondev 加密 shell 时提出两点纠正。

---

## 0. 用户的两条纠正（原话，不可转述走样）

> 「『拦截后让 agent 用变量读取』意味着被拦截的明文要能再取回。
>   并不是明文取回，而是**后续使用仅可使用变量替代**。
>   我觉得这部分需要**下沉到内核内部作为安全模块**存在了。
>   （因为 llms 调用本身是不可信的）」

> 「这样插件设计就简单了，可以**直接将密钥明文投到消息内部**，
>   由**内核安全机制全权负责**」

**这两条纠正了一个错误方向**，本文先记录错在哪，再记录纠正后的设计。

---

## 1. 被纠正的错误方向（前稿的做法）

前稿（`commondev-agent-runtime-design.md` §2.7 之后那批讨论）里，我把
"敏感信息拦截"实现为 **commondev 插件内的 stage**：

```go
// 前稿：插件在 StageOnInput / StageAfterToolcall 上拦截
p.sdk.RegisterStage(sdk.StageOnInput, func(ctx *sdk.StageContext) error {
    res := p.icept.Intercept(ctx.RawMessage, "input")
    ctx.RawMessage = res.Text   // 换成 @{sec_1} 提示
    ...
})
```

**错在两处**：

| # | 错误 | 为什么错 |
|---|---|---|
| 1 | **把拦截器放在插件里** | 插件在**信任边界之外**。它是独立进程，别的插件（或它自己崩溃/被替换）都能绕过。而"LLM 调用不可信"意味着**每个** LLM 调用都要过这道闸——插件的注册是**自愿的**，不是强制的 |
| 2 | **把"可再取回"当成需求** | 前稿甚至设计了 `enc_show` 之外的取回路径。但用户要的是**只能替代引用，永远拿不到明文** —— 一旦有取回路径，LLM 就能把明文写回上下文，整层作废 |

### 1.1 还暴露了两个真实缺陷 ［已核 · 单测抓到］

即便只作为插件实现，前稿也有两个 bug（保留记录，因为它们说明"判据在起作用"）：

1. **`*SensitiveInterceptor` nil 时 panic** —— `Intercept` 里 `si.enabled` 直接解引用。
   已修（`si == nil ||`），这是真实健壮性问题：宿主可能在任何阶段调用。
2. **赋值形态丢失键名** —— `api_key = "xxx"` 被整体替成提示，人看不出发生了什么。
   `splitKeyValue` 对 `assigned_secret` 的命中形态判断有误。**修法未完成**（本文第 5 节列为待办）。

> 教训：这两个缺陷都是**单测**发现的，不是读码。印证本仓纪律——判据必须跑。

---

## 2. 纠正后的架构：下沉为内核安全模块

### 2.1 出向落点：provider 层（实测推翻初稿） ［已核 · 源码确认］

初稿写“唯一出网点是 `task.go:663`”。**实测扫全仓后这个结论是错的** —— 构造
`CompletionRequest` 的地方有 **7 处**（不含插件侧）：

| # | 位置 | 用途 |
|---|---|---|
| 1 | `internal/agent/core/task.go:663` | 主对话轮次（最主要） |
| 2 | `internal/agent/core/spawn.go:176` | `spawn_child` 的工作式轻量子 |
| 3 | `internal/agent/core/spawn.go:321` | LLM 相关工具（`llm_*`） |
| 4 | `internal/agent/core/modalfallback.go:347` | 多模态回退调用 |
| 5 | `internal/agent/api/provider.go:1140` | `ProviderManager.QuickChat` |
| 6 | `internal/sdk/llm_impl.go:56` | 插件调 LLM（`s.LLM().Chat`） |
| 7 | `internal/plugins/*`（multimodal / healthcheck / remotedevice） | 插件自己发起的 LLM 调用 |

⇒ **在 task 层拦一道会漏掉 4–7 四大类**。其中 #6 尤其要紧：
插件经 `s.LLM().Chat` 发起的调用**完全绕过**主对话路径。

#### 正确的落点：**provider 层**

所有路径最终都会进某个 `Provider.Chat(ctx, *CompletionRequest)`。所以安全模块
应放在**provider 调用的统一层**（装饰器/包装器），而不是 task 层：

```
任何来源 ──► CompletionRequest ──► ★ 安全包装器（必经）──► 真正的 Provider
```

**待确认**：是否存在不经 `Provider.Chat` 的出网路径（如某 provider 内部直发 HTTP）。
这一条必须先验（§5.1），否则安全模块仍有洞。

### 2.2 数据模型：明文可以自由流动，只在出口被替换

用户第二条纠正的直接后果：**插件可以把密钥明文投到消息内部**。

```
插件/工具/任何来源 ──(明文，随便用)──► f.Msgs
                                          │
                              ┌───────────▼───────────┐
                              │ 内核安全模块（必经）    │
                              │ 扫描 → 命中 → 换成别名  │
                              └───────────┬───────────┘
                                          ▼
                                   真正发往 LLM 的 req
```

好处（这是简化，不是妥协）：
- 插件不需要自己造加密库、别名表、stage 拦截（前稿那一整套）⇒ **插件变薄**
- 插件也不需要"安全地传输明文"的协议——**直接投，内核负责**
- 别名表归内核管理 ⇒ 全局唯一，且与 agent 实例解耦

**代价（必须写清）**：明文在**内核进程内存**里自由流动。
所以边界是"内核进程可信"，不是"内存里不存在明文"。

---

### 2.3 ★ 不是“拦截”，而是“拦截 + 暂存 + 别名 + 回填” ［用户 2026-10-14 纠正］

用户纠正了我“只做拦截”的单向理解，并给出**完整回路**：

> 「因为你只是简单的拦截的话，**绝大部分插件会失去原本的功能**。
>   安全模块是**拦截，加密暂存，提供调用别名，收到 toolcall 后自动将别名填充进去**。
>   因为内核 0 IO 设计，一切外部交互都是从插件走的，你只需要在 **toolcall 和
>   outputch 部分有注入**就行了」

⇒ **为何只拦截不行**（这是本节的关键）：

如果只在出向把明文换成 `@{sec_1}`，则插件再也**拿不到**密钥 ——
而插件恰恰是唯一能真正用密钥的地方（push / 调 API / 写文件）。
结果就是：**安全了，但活干不了了**。那等于把功能删掉，不是安全。

⇒ 必须是一个**闭环**：

```
① 拦截    LLM 出向前扫到明文  ──► 换成 @{别名}
② 加密暂存 把明文 AES-GCM **落盘**在内核（不落明文）
③ 别名    ★ 哈希键（由明文派生，见 §2.4）
④ 回填    插件调用（toolcall / outputch）前，把 @{别名} 换回明文
          —— ● **插件无感知**：它只看到已经填好的明文
```

### 2.4 ★ 变量索引：**哈希键** —— 于是自动获得“同变量合并” ［用户 2026-10-14 纠正］

用户纠正了两件事（我初稿用的是 `sec_1`、`sec_2` 顺序号，**那是错的**）：

> 「落盘暂存，各个变量通过**哈希键索引**，这样自然获得了**同变量合并能力**。
>   同时变量**不自动淘汰**，但是 agent 可主动调用安全模块提供的工具**提交/删除**」

#### 为何哈希键是对的（顺序号错在哪）

| | 顺序号 `sec_<n>`（错） | **哈希键**（对） |
|---|---|---|
| 同一 secret 出现两次 | 得到 `sec_1` / `sec_2` **两个别名** | 得到**同一个**别名 ⇒ ★ **自动合并** |
| 合并需要额外逻辑吗 | 需要（要扫描所有别名比对明文） | **不需要**（哈希本身就是键） |
| 时序依赖 | 有（序号与拦截顺序绑定） | 无（键由内容唯一确定） |
| 重启后一致吗 | 依赖持久化的计数器 | 重算即可（同一明文 ⇒ 同一键） |

> ⇒ **“同变量合并”不是额外功能，而是哈希索引的天然性质** —— 这正是用户要的。
> 这也消除了一个隐患：同一把密钥在对话里出现 5 次，不应产生 5 个别名，
> 否则日志/记忆里会积起一堆指向同一密钥的不同名字。

#### 键的构造

```
alias = "sec_" + hex( HMAC-SHA256(masterKey, plaintext)[:8] )
```

**两个细节不能省**：

1. **必须用 HMAC（带 masterKey），不能裸 SHA256**。
   裸哈希对短/低熵输入可被字典枚举 —— 而别名会出现在**上下文、日志、记忆**里，
   等于把“这个 secret 是什么”泄露出去。

   **实测演示**（本机跑过）：取明文 `hunter2`，裸 SHA256 取前 8 字节作别名，
   然后拿 5 个常见弱口令去试：

   ```
   别名（裸哈希）: f52fbd32b2b3b86f
   ★ 字典命中：'hunter2' → 别名直接暴露了 secret 内容
   ```

   换成 HMAC(masterKey) 后，攻击者**没有 masterKey 就无法枚举**。
2. **8 字节（16 hex）** 足够：碰撞概率对本场景可忽略，而短别名省上下文。
   实测别名形如 `sec_3718c87a11e9478b`（**20 字符**）。

> 为什么不加随机 salt：加了就**失去合并能力**（同一明文每次得到不同键）。
> 所以这里“确定性”是**特性**，不是缺陷；安全靠 HMAC 的密钥而不是 salt。
>
> **实测验证**（本机跑过）：
>
> | 断言 | 结果 |
> |---|---|
> | 同明文两次 ⇒ 同一别名（自动合并） | ✅ 两次都是 `sec_3718c87a11e9478b` |
> | 不同明文 ⇒ 不同别名 | ✅ |
> | 别名长度（要进上下文） | ✅ 20 字符 |
> | 换 masterKey ⇒ 别名变（键依赖密钥） | ✅ |

#### 生命周期：不自动淘汰，但可**主动**提交/删除

| 行为 | 谁决定 | 说明 |
|---|---|---|
| **自动淘汰** | ❌ **不做** | 用户明确：变量不自动淘汰 |
| **主动提交** | agent 调安全模块的工具 | 把某些明文显式登记为变量（即使尚未出现过） |
| **主动删除** | agent 调安全模块的工具 | 明文用完即弃（如轮换后）
| **持久化** | 内核 | **落盘暂存**（加密），重启不丢 |

**为何“不自动淘汰”是合理的**：

- 任务可能跨很多轮、跨多个子 agent（§2.6 全局共享），早先登记的变量随时要能用；
- 自动淘汰需要定策略（按时间？按引用计数？）——而策略错了会导致“变量静默失效”、
  任务在很后面才坏掉，且现象难归因；
- 反过来，**泄漏风险由“主动删除”控制**，而不是靠超时时间。

⚠ **待定（T9）**：不自动淘汰 ⇒ 落盘文件会只增不减（除非人主动删）。
需要定一个**可观测**的规模上限降级策略（如超 N 条时告警而非静默删）。

### 2.5 ★ 两个注入点就够了（内核 0 IO） ［已核 · 源码确认］

用户指出：“内核 0 IO 设计，一切外部交互都是从插件走的”。
⇒ 于是**只需在两根咽喉上做替换**：

| 方向 | 咽喉 | 位置 | 作用 |
|---|---|---|---|
| **入向**（→插件） | `StageHost.ExecuteTool(name, args)` | `internal/agent/core/stages.go:140` | toolcall 参数里的 `@{别名}` → 明文 |
| **入向**（→插件） | `OutputChannel` 的 device `Execute` | `internal/agent/core/output.go:76` | 输出载荷里的 `@{别名}` → 明文 |
| **出向**（→LLM） | provider 层的 `Chat(req)` | `internal/agent/api/`（装饰器） | `f.Msgs` 里的明文 → `@{别名}` |

#### 入向覆盖确认（调插件工具的入口）

| 路径 | 位置 | 覆盖 |
|---|---|---|
| agent 发的 tool_call（主路径） | `toolcall.go:133` → `stages.go:140` | ✅ |
| 插件经 ToolAPI 调另一个插件 | `sdk/tool_impl.go:47` | ✅ |
| 设备/通道工具 | `io/channel.go:714/730` | ✅（走同一 `Execute`） |
| outputch 出向到插件 | `output.go:76` → `dev.Execute` | ✅ |

> ⇒ **只需两处**（`StageHost.ExecuteTool` + `IOManager` 的设备 `Execute`）
> 就覆盖全部插件交互 —— 因为**内核自己不碰外部世界**（零 IO）。
> 这是“零 IO”设计的一个**意外红利**：安全边界只有两个点。

### 2.6 为何“插件无感知”很重要（不是洁癖）

| 理由 | 说明 |
|---|---|
| **插件无法绕过** | 插件根本不知道有替换这回事 ⇒ 无法写“我自己解密” 的代码去绕开 |
| **存量插件无需改** | 所有已有插件（qq / browser / …）自动获得该能力 |
| **接口不变** | 不需要给 SDK 加任何参数 ⇒ 不破兼容（本仓“只增不减”） |
| **职责单一** | 安全归内核；插件只管业务 |

#### ⚠ 与“插件明文中转”的关系（用户拍板的简化）

用户已明确：“插件可以直接将密钥明文投到消息内部，由内核安全机制全权负责”。

⇒ 两者不矛盾，而是**分工**：

| 环节 | 谁负责 |
|---|---|
| 插件要向 LLM 交代“我用的是哪条密钥” | **直接写明文**（内核出向会把它变成 @{别名}） |
| 插件要实际**用**密钥（如 push） | 写 `@{别名}`，内核**入向**换成明文后再交给插件 |
| 别名表 / 加密存储 / 扦描 | **全部在内核**（插件不参与、不知情） |

**待确认**：入向替换也会换掉 `cmd_run` / `terminal_write` 的**参数**（它们是工具调用）。
这带来一个新问题 —— **agent 可以把 `@{别名}` 写进任意命令参数**，等于获得了
“在任意命令里使用明文”的能力。这正是用户要的（否则没法用密钥），
但它把“能做什么”的边界交回给了 **命令本身**（即 enc_shell 的安全边界问题）。详见 §5 T8。

### 2.7 ★ 变量系统是**全局通用**的（父子 agent 同一套） ［用户 2026-10-14 追加］

用户明确：

> 「注意安全模块的变量系统是**全局通用**的，驻留式子 agent、子 agent、
>   主 agent 是**同一套变量系统**」

#### 为何必须全局（不能 per-agent） ［已核 · 源码确认］

驻留子共享父的对象（`resident.go:184-185`）：

```go
Provider:        a.provider,
ProviderManager: a.providerManager,   // ★ 共享同一个 ProviderManager
StageHost:       a.stageHost,
```

而 `ProviderManager` **全进程只建一份**（`cmd/homed/bootstrap.go:249` 建一次；
`resident.go:185` 把它传给每个子）。⇒ 安全模块（作为 provider 装饰器）
**天然就是全进程单例**。

#### 为何“全局”是正确设计（而不只是实现便利）

| 理由 | 说明 |
|---|---|
| **否则无法跨 agent 协作** | 父拦截得到 `@{sec_1}`，要能交给子去干活；子的 `@{sec_1}` 若不同则断链 |
| **别名不泄露就能共享** | `sec_<n>` 本身不含明文，共享它不降低安全性 |
| **与 L4 归属正交** | 驻留子的 `KernelSource`/权限是 per-agent（隔离）；变量系统是全局（共享）。**两件事，不要混** |

#### ⚠ 一个必须想清的后果：权限边界不因共享而放松

变量全局 ⇒ **任何 agent 都能写 `@{sec_1}`**。但回填后**能不能真的用**，取决于该
agent 的工具/通道授权：

| 层 | 作用域 | 管什么 |
|---|---|---|
| **变量系统** | ★ **全局** | 别名 ↔ 明文（不区分 agent） |
| `AllowedOutputs` / 工具授权 | **per-agent** | 该 agent 能不能调某个工具/通道 |
| `KernelSource`（L4） | **per-agent** | 该 agent 的上级是谁 |

⇒ 子 agent 拿到了明文，但**能否拿它去 push** 仍受它自己的授权限。
两者是**正交**的两道门（变量共享 ≠ 授权共享）。

#### 实现形态

⇒ 安全模块不能挂在 `*Agent` 上（那是 per-agent），而应：

```
internal/security/ 里的一个**进程级单例**（或挂在 ProviderManager 上）
    ├─ 别名表  alias → secret（全局）
    ├─ 出向扫描（provider 装饰器，天然单例）
    └─ 入向回填（StageHost.ExecuteTool / 设备 Execute；两者也是共享对象）
```

> **恰好舒服的地方**：两个注入点（`StageHost` / `IOManager`）与出网点
> （`ProviderManager`）**都已经是共享对象** ⇒ 安全模块挂上去就是全局的，
> **不需要额外做“跨 agent 同步”**。

### 2.8 与"两阶段拦截"的关系

前稿用 `StageOnInput` + `StageAfterToolcall`。那两个点**仍然有意义，但不是安全边界**：

| 位置 | 性质 | 说明 |
|---|---|---|
| `StageOnInput` / `StageAfterToolcall` | **提示性、可绕过** | 提前替换能让日志/记忆干净一些，但**不构成安全** |
| **两根咽喉**（入向 `ExecuteTool`/`Execute`，出向 `Chat`） | ★ **强制的安全边界** | 无论前面漏了什么，这里都是最后一道且必经 |

⇒ 结论：**安全责任放在咽喉上**；前面的 stage 拦截降级为“卫生措施”（可选、可关）。

## 3. 硬约束（安全模块的不可让步项）

| # | 约束 | 理由 |
|---|---|---|
| **S1** | **不提供"取回明文"的任何接口** | 用户明确：只能**替代引用**。一旦有取回路径，LLM 就能把明文写回上下文，整层作废 |
| **S2** | 替换必须落在**必经点**上 | 插件是自愿注册的 ⇒ 插件侧拦不算边界。
  已核两个必经点：出向 `Provider.Chat`、入向 `StageHost.ExecuteTool` + 设备 `Execute` |
| **S3** | 别名**全局唯一且不泄漏明文** | ★ **哈希键**：`sec_ + HMAC-SHA256(masterKey, plain)[:8]`。
  **必须用 HMAC 而非裸哈希**（裸哈希可被字典枚举；而别名会出现在上下文/日志/记忆里）；
  **不加 salt**（加了就失去合并能力，确定性是特性） |
| **S4** | **不可误伤** | 误报会把正常内容换掉、搞坏任务。模式库保守优先——**宁可漏，不可误伤** |
| **S5** | **不得静默** | 每次拦截要可观测（别名、来源位置、长度、时间），否则出问题无从追查 |
| **S11** | ★ **同变量自动合并** | 哈希键的天然性质：同一明文多次出现 ⇒ **同一个别名**（不是多别名）。
  不能退化成顺序号 —— 那会在日志/记忆里积起一堆指向同一密钥的名字 |
| **S12** | ★ **不自动淘汰**，但可**主动**提交/删除 | 用户明确：不自动淘汰（跨轮/跨子 agent 随时要用）；
  泄漏风险靠**主动删除**控制，不靠超时。自动淘汰错策略会导致“变量静默失效” |
| **S6** | 关闭开关是**显式特权** | 能关掉它的人必须知道自己关掉的是什么（与本仓 `offload_enabled` 同一条纪律） |
| **S7** | 替换后**仍要能干活** | 被替换掉的明文必须**可用别名引用**（否则任务直接坏掉）——这是 S1 与可用性的平衡点 |
| **S8** | ★ **回填必须成对**（只拦不填 = 删除功能） | 用户明确：只拦截会让“绝大部分插件失去原本功能” |
| **S9** | ★ **插件无感知** | 替换发生在路由到插件的**之前**；插件代码不改、不可分辨、无法绕过 |
| **S10** | ★ **变量系统全局唯一** | 主/子/驻留子 agent **同一套**别名表。理由：`ProviderManager` 全进程只建一份（`bootstrap.go:249`），且共享才能跨 agent 协作。且**与权限正交** —— 变量共享 ≠ 授权共享 |

> **S1 与 S7 不矛盾**：别名可**被引用**（用于命令填充），但**不可被读取**。
> 读接口与用接口是两件事 —— 这正是前稿没分清的地方。

---

## 4. 需要改哪些地方（下沉清单）

**总原则（用户 2026-10-14 拍板）**：安全模块 = **拦截 + 加密暂存 + 别名 + 回填**，
**插件完全无感知**。因为**内核零 IO**（一切外部交互都从插件走），
所以只需在**两根咽喉**上做替换即可覆盖全部。

| # | 改动 | 位置 | 性质 |
|---|---|---|---|
| **C1** | **出向拦截**（→LLM）：包装 `Provider.Chat`，在 `CompletionRequest` 出网前扫到明文就换成 `@{别名}` + 加密暂存 | `internal/agent/api/`（装饰器） | 新增（核心） |
| **C2** | ★ **入向回填**（→插件，toolcall）：`StageHost.ExecuteTool` 入口把 `args` 里的 `@{别名}` 换回明文 | `internal/agent/core/stages.go:140` | 新增（核心） |
| **C3** | ★ **入向回填**（→插件，outputch）：`IOManager` 的设备 `Execute` 前把载荷里的 `@{别名}` 换回明文 | `internal/agent/io/channel.go` / `output.go:76` | 新增（核心） |
| **C4** | **变量库**：★ **哈希键索引 + 落盘加密暫存**（`internal/security/`） | 新建 `internal/security/` | 新增 |
| **C8** | ★ **供 agent 调用的提交/删除工具**（主动登记、主动删除） | 内核内置工具（`builtinToolOwner` 注册） | ✅ **已做**（`secret_submit/list/forget`）|
| **C5** | **LLM 返回也要扫**：LLM 可能把明文编造/输出出来 ⇒ 返回内容同样过一遍 | ✅ **已做**：拦在 `a.context.Append` 咽喉处（非调用点） |
| **C6** | **记忆写入前扫一次**：防明文被写进长期记忆 | ✅ **已做**：`Append`/`InsertByTimestamp` + `emitMemoryCandidate` |
| **C7** | 插件的加密 shell / enc_* 工具族**大幅删减**：不再自带加密库与别名表 | commondev 插件 | ✅ **已做**（比原计划**更保守**，见 §5.4）|

> **C2 + C3 是“插件无感知”的全部机制保证**：
> 插件代码**一行不改**，也无法分辨自己拿到的是明文还是被替换过的。
> 无需给 SDK 加任何参数 ⇒ **不破接口兼容**（本仓“只增不减”）。
>
> **C1 是对用户“两个终端工具需联动”那条要求的直接回答**：
> 不必改 `terminal_*` / `cmd_run` 这两个内置插件 —— 它们的**输出**最终进 `f.Msgs`
> （被 C1 截），它们的**参数**走 `ExecuteTool`（被 C2 填），双向都覆盖。

---

## 5. 待办与未决

> ★ **2026-10-31 迁出说明**：本节里**仍未实现**的事项已全部迁到
> [`future-plan.md`](./future-plan.md) §一「安全」。下面保留原表作为**设计讨论的历史记录**
> （它们的推理过程本身有价值），但**不要再把这张表当待办清单用** ——
> 它历史上滞后过（T3/T4 已落地却仍挂在表里）。
> 每行的「现状」列是 2026-10-31 实测结果。

| # | 事项 | 现状（2026-10-31） |
|---|---|---|
| **T1** | ~~修 `splitKeyValue` 对 `assigned_secret` 的键名丢失~~ | ✅ **已修**（插件侧；迁内核时可一并搬） |
| **T2** | 把前稿的 `intercept.go` / `encstore.go` 从插件**迁到内核** | ✅ **已迁**（`internal/security/gate.go` + `store.go`；插件对安全机制无感知） |
| **T3** | 密钥从哪来 | ✅ **已定且已落地**：内嵌主密钥（构建期 `-ldflags`）+ 磁盘钥回退 + keyring（`internal/security/key.go`） |
| **T4** | 变量持久化与生命周期 | ✅ **已落地**：落盘加密持久化（`security/variables.json`），不自动淘汰 |
| **T9** | 不自动淘汰的规模上限 | ❌ **未做** → future-plan §1.5 |
| **T10** | 提交工具的泄露面（`${ENV}` / `@file:` 不明文路径） | ❌ **未做** → future-plan §1.5 |
| **T5** | 压缩对别名的影向 | ❌ **未做** → future-plan §1.5 |
| **T6** | 明文在内存中的擦除 | ❌ **未做**（Go 字符串不可变，需先定“努力擦除” vs “不承诺”）→ future-plan §1.5 |
| **T7** | 引用外部文件、内核只持路径 | ❌ **未做** → future-plan §1.5 |
| **T8** | 入向回填的安全边界（哪些工具允许触发回填） | ❌ **未做** → future-plan §1.5 |

### 5.1 ★ 配置落盘加密 ［已完成 · 真机验证］

**用户需求（2026-10-14）**：通用配置在数据库里全是明文；配置管理器应提供
加密选项，并为插件暴露加密存储能力（密钥由内核统一管理）。

#### 要解决的问题

`config.db` 是 **0644 全局可读**的（同目录还有 WAL/SHM），而里面装着
LLM API 密钥、WebUI 口令、插件凭据 —— 任何本机用户一条 sqlite3 查询
就能拿到全部明文。

#### 做法：在**存储边界**加密，读写接口不变

```
Set/Register/RegisterDef/seed → Seal → DB 里是 enc:v1:<base64>
Get/Dump/ToConfig/GetString → Open → 调用方永远只看到明文
```

★ **关键取舍：读接口保持「返回明文」**。

初稿曾想给 `Dump()` 返回掩码，但那会同时打断 `ToConfig()`
（它靠 Dump 重建 LLMSource，含 api_key）与所有内部读取方 —— 而它们
**本来就在可信边界内**。本需求防的是「静态文件泄漏」，不是「内部读取」。
已配判据 `TestToConfigStillGetsRealAPIKey` 钉死这一点。

#### 敏感判定（两种声明 + 键名兜底）

| 途径 | 说明 |
|---|---|
| `ConfigDef.Secret == true` | ★ **SDK 里早就有这个字段，但从未实现** |
| `ConfigDef.Type == "password"` | 旧写法，兼容保留 |
| 键名模式 | 无 def 的插件键靠它兜底 |

**键名匹配必须按「词段」而不是子串**：初版用 `strings.Contains(key,"token")`，
把 `core.llm.max_tokens`（输出上限）与 `core.agent.context.max_target_tokens`
（上下文预算）也加密了。真机跑出来才发现。现在改为切词后精确匹配，
`max_tokens` 的末词段是 `tokens`（复数）⇒ 不命中。

#### 密钥：域隔离派生

```
subKey = HMAC-SHA256(masterKey, "homeagent/config-at-rest/v1")
```

同一把主密钥服务多类用途（变量别名 HMAC、配置 AES、未来审计签名…）。
共用一把会在域之间产生攻击面（一处的输出变成另一处的合法输入），
所以每类用途派生独立子密钥。判据 `TestDomainSeparation` 钉住它。

密钥来源与安全模块一致：`HOMED_SECURITY_KEY` > `<dataDir>/security.key`（0600）。

#### 为插件暴露的加密存储能力

SDK 新增两个方法（`SettingsAPI`）：

| 方法 | 语义 |
|---|---|
| `SetSecret(key, value)` | **强制加密**，不看键名（插件可能用 `blob`/`conn` 这类中性键名） |
| `GetSecret(key)` | 读回（透明解密） |

★ **无密钥时 `SetSecret` 拒绝写入**，不静默存明文 —— 插件以为存了密文
而实际磁盘上是明文，是最危险的静默降级。

#### 落点清单

| # | 改动 | 位置 |
|---|---|---|
| **E1** | 密钥域派生 + 编解码器 | `internal/security/codec.go` |
| **E2** | 敏感判定 + 迁移 + 加密写入 | `internal/config/secret.go` |
| **E3** | 读路径透明解密（Get/Dump/GetString/listPrefix/PluginSettings） | `internal/config/registry.go` |
| **E4** | ★ `Secret` 标志透传（**三处转换点都漏了它**） | `internal/sdk/settings_impl.go` |
| **E5** | 显式加密 API（`SetSecret`/`GetSecret`） | registry + sdk + **SDK 接口** |
| **E6** | ★ proc 桥：协议常量 + 3 处注册（路由/分发/能力表）+ 模板实现 | `internal/plugin/proc/`、`templates/proc_main.go.tmpl` |
| **E7** | WebUI：密码框 + 显示/隐藏按钮 | `internal/plugins/webui/dashboard.js` |
| **E8** | 启动装配 + 存量迁移 | `cmd/homed/main.go` |

#### ☆ 踩到的两个真坑

1. **`Secret` 字段从未生效的根因**：SDK 一直有 `Secret bool`，但内核侧
   **三处**转换点（`RegisterDef`/`Defs`/`mapDefs`）全都漏了它。
   插件声明了意图，前端拿到的 meta 里却没有 —— 于是敏感项被渲染成
   普通文本框明文回显。已配判据 `TestSecretFlagReachesSettingsMeta`。

2. **新增 proc method 要改 4 个地方**：协议常量、外层路由、内层分发、
   能力表。我只改了分发，真机 e2e 立刻报 `未知 method: settings.setSecret`
   —— 这正是「真机端到端不可省」的实例。

3. **守卫判据本身有盲区**：`TestCapability_AllMethodsClassified` 号称
   「本文件最重要的测试」，但它用手工维护的方法清单 —— 新方法不加进清单
   就无人管（变异实验 V3 证实：删掉能力表条目，判据仍绿）。
   已改为**从 protocol.go 源码正则提取**全部 method 常量，覆盖面自动跟上。

#### 验证

- `internal/config/secret_test.go` 17 条 + `internal/security/codec` 判据
- `internal/plugin/proc/e2e_secret_test.go` —— **真实 hmapdev 模板编译的插件**
  跑完整链路：插件读回明文(27B)、磁盘存密文(83B)
- `internal/plugins/webui/secret_meta_test.go` 2 条（声明→meta、UI 明文/磁盘密文）
- 变异 6 条全部咬住（含 V3 暴露并修复的守卫盲区）
- 真机：3 条存量迁移、明文残留 0、`max_tokens` 未被误加密
- 全量回归 `./internal/... ./cmd/... ./pkg/...` 零失败

---

### 5.2 前提验证（安全模块本体） ［已完成 · 全部通过］

**结论：安全模块可以稳在 provider 层。** 两条前提均已实测：

#### P1：所有出网均经 `Provider.Chat` / `ChatStream` ［已核］

| 事实 | 依据 |
|---|---|
| `Provider` 是干净的两方法接口 | `internal/agent/api/provider.go:307`（`Chat` + `ChatStream` + `MaxContextTokens`） |
| 所有调用点都用接口方法 | `process.go:138/155/191`、`spawn.go:184/321`、`modalfallback.go:347`、`provider.go:1140` |
| 插件经 `s.LLM().Chat` 也回同一层 | `internal/sdk/llm_impl.go:48` → `l.mgr.Default()` |
| 唯一一处内部直发 HTTP（`provider.go:446`） | 在 `LuaAdaptedProvider` **实现内部**，不是绕过 |
| `lua/vm.go` 的 `CallTransformRequest` | 是适配器层的载荷改写（在 provider 内部调用），不是绕过 |

#### P2：`ProviderManager.Register` 是**唯一入口** ［已核 · CodeGraph 交叉验证］

装饰 `Register` 即覆盖全部取用路径（`Get` / `Default` / `OrderedProviders` / `ResolveForModel` 都从同一 map 取）。

`NewLuaAdaptedProvider` 的全部调用者（CodeGraph 查得 **6 个**）：

| 调用者 | 位置 | 是否经 `Register` |
|---|---|---|
| `initLLMProviders` | `cmd/homed/bootstrap.go:248` | ✅ 下一行就是 `providerMgr.Register` |
| `runFailbackRecovery` | `cmd/homed/failback.go:28` | ✅ `Register("rescue", …)` |
| `ReloadFromConfig` | `internal/sdk/llm_impl.go:115` | ✅ `l.mgr.Register` |
| 其余 3 个 | 测试文件 | — |

⇒ **无任何真实路径绕过 `Register`。**

#### 已落地的验证判据

`internal/agent/api/security_premise_test.go`（4 条，均配变异验证）：

| 判据 | 验证什么 |
|---|---|
| `TestPremise_RegisterDecorateCoversAllAccessPaths` | 4 条取用路径都拿到装饰后实例 |
| `TestPremise_DecoratorSeesEveryChat` | `Chat` / `ChatStream` 都经装饰器 |
| `TestPremise_DecoratorCanRewriteMessages` | 装饰器**能改写**消息（不只是读） |
| `TestPremise_StreamAlsoDecorated` | 流式也被覆盖 |

变异实验（两个都咬住）：不包裹 ⇒ “装饰器应看到 1 次 Chat，实际 0”；
只读不改 ⇒ “明文被原样透传，改写未生效”。

> **方法论教训**：这一步最初我用 `grep` 逐个查，漏了 `sdk/llm.go` 等入口，
> 也没发现 `lua/vm.go` 的变换链路。改用 **CodeGraph**（`codegraph_callers` / `codegraph_node`）
> 后，它一次列出 `Chat` 的 **9 个调用者**与 `NewLuaAdaptedProvider` 的 **6 个调用者**，
> 遗漏面直接归零。**查接口关系用 codegraph，不要用 grep 翻**。

---

### 5.3 ★ 持久化侧路的明文拦截（C5/C6） ［已完成 · 真机验证］

### 发现的真实漏洞：出向拦截挡不住持久化

`§4` 的 C5/C6 原本是「计划项」。实测把它们变成了**必须做** —— 因为发现了
一条完整的泄漏侧路：

出向拦截（provider 装饰器）只改 `f.Msgs` —— **要发出去的那一份**。
但内核把同样内容**另存副本**送往持久化，它们不经过 provider：

| 持久化字段 | 来源副本 | 是否被 provider 拦住 |
|---|---|---|
| `ContextEvent.Input` | `f.CleanInput`（用户输入原文） | ❌ |
| `ContextEvent.Response` | `f.Response` | ❌ |
| `ContextEvent.ToolResults` | `f.ToolResults` | ❌ |
| `memory_candidate.payload` | 同上（走 `EmitOutput`，**不经 context**） | ❌ |

后果：明文被 LLM 看到之前虽已换成别名，但**完整地写进了 `context.json`
与长期记忆** —— 而记忆跨会话保留、每轮被召回。泄漏面比重放一次大得多。

### 修法：拦在**咽喉**，不拦在调用点

写 context 的地方有 **5 处**（`task.go` ×3、`toolcall.go` ×1，加插入变体），
`memory_candidate` 另走一条。逐个补必漏 —— 事实上第一版正是只补了
`turnEvt`，而漏掉了 `f.CleanInput`（判据实测确认泄漏仍在）。

最终落点：

| 咽喉 | 覆盖 | 文件 |
|---|---|---|
| `RelevanceContext.Append` | 全部追加写入 | `context.go` |
| `RelevanceContext.InsertByTimestamp` | 按时间插入（另一入口） | `context.go` |
| `Agent.emitMemoryCandidate` | 记忆候选（不经 context） | `distill.go` |
| 共用实现 | `scrubEventSecrets` / `scrubMemoryCandidate` | `memory_scrub.go` |

清除字段：`Input` / `Response` / `ToolResults[].Output` /
`Blocks[].Text`（**仅文本块**；图像/视频/音频的 `Data` 是二进制负载，
正则扫它既无意义又会损坏数据）。

### ☆ 一处必须记录的脆弱性

实测发现：`Append` 的 scrub 会**连带改掉** `f.ToolResults` —— 因为
`turnEvt.ToolResults = f.ToolResults` 是值切片，共享底层数组。
于是「去掉 `scrubMemoryCandidate`」的变异**不会被端到端判据发现**。

这是脆弱设计（隐式依赖别名）。所以：

1. `scrubMemoryCandidate` **显式保留**，不指望上游顺手改了；
2. 增加判据 `TestEmitMemoryCandidateScrubsPayload`，走**真实入口**
   `a.emitMemoryCandidate` 而不是直接调底层函数 —— 初版直接调底层，
   于是「去掉入口处的调用」这个变异**不报红**（判据测错了层次）。

> 通用教训：**判据必须走真实入口**。测底层函数只能证明「函数对」，
> 证明不了「入口调了它」。本次变异实验连续暴露了三次同类错误
> （M2 判据漏测 `Alias()`、C6-M2 漏测 `memory_candidate`、M-B 测错层次）。

### 与 `f.Msgs` 原地改写的**刻意不对称**

| 路径 | 语义 | 为何 |
|---|---|---|
| `f.Msgs` | **原地改写** | 它就是要发出去的那份；改掉后明文永久离开上下文 |
| `f.ToolResults` | **拷贝后改写** | 同一轮里还被其它代码读（诊断、状态面），不能就地改 |

这个不对称是刻意的，判据 `TestOutboundScrubIsInPlaceByDesign` 与
`TestToolResultsScrubDoesNotMutateSource` 分别钉住，免得后来者「统一」成一种写法。

### 验证

- `internal/agent/core/persist_leak_test.go` 4 条（输入侧 / 回复侧 / 记忆块 / 记忆候选入口）
- `internal/agent/core/toolresult_scrub_test.go` 3 条（含**真实调度循环**的 e2e）
- `internal/agent/core/inplace_scrub_test.go` 1 条（原地改写语义）
- 变异 4 条（含 2 次修好的判据盲区）
- 全量回归零失败

---

### 5.4 ★ C7：插件加密层的删减（比原计划更保守） ［已完成］

#### 先验证前提，再动代码

C7 原计划是「插件不再自带加密库与别名表」。但「删掉插件那套，内核能顶上吗」
是个**必须先验的断言** —— 不成立的话就是削功能而非去冗余。验了三条：

| 判据 | 结论 |
|---|---|
| `TestKernelFillCoversBuiltinToolsToo` | 内核回填覆盖**所有**工具名（含内置 `cmd_run`） ⇒ 插件的**填充**功能冗余 |
| `TestKernelFillHappensBeforePluginHandler` | 内核在 handler **之前**回填 ⇒ 插件自己的别名表对内核别名是**死代码** |
| `TestKernelAndPluginAliasSyntaxDoNotCollide` | 内核只吃 `[A-Za-z0-9_]`，插件的 `@{gitea-local}`（含连字符）不受影响 |

#### 删了什么（691 行）

| 删除 | 理由 |
|---|---|
| `intercept.go` / `intercept_stage.go` / `intercept_test.go`（**691 行**） | 插件版拦截**覆盖面严格小于**内核版：它只能看 `RawMessage`/`ToolResults`，内核还能覆盖 `Input`/`Response`/`Blocks`/`memory_candidate` |
| `enc_intercepts` 工具 + `Ledger.Interceptions` | 拦截记录由内核拥有（内核日志 + `secret_list` 的命中计数） |
| `intercept_enabled` 配置项 | 全局开关是 `core.security.enabled`（在 WebUI 设置页）。插件留一个同名开关会让人以为它能控制内核行为 —— 那是误导 |

★ **两套并存的真实害处**：内核先回填，插件再扫一遍时明文已是别名 ⇒ 纯空转，
而且可能把别名**二次改写**。

#### ☆ 但有三样**不能删**（原计划漏判的）

| 保留 | 为什么内核顶不上 |
|---|---|
| `enc_shell` 的 **argv 保护**（脚本走 stdin） | 内核 `cmd_run` 是 `sh -c "<整条命令>"` ⇒ 回填后的明文**会进 argv**，本机任何进程 `/proc/<pid>/cmdline` 都能读到。实测确认 |
| `enc_shell` 的 **输出脱敏** | 防命令意外回显凭据（如 `git remote -v` 带出 token） |
| `enc_store` 的 **`${ENV}` / `@file:` 来源** | 这两条路径**明文从不进会话** —— 正是文档 T10 要求的更安全形态，而内核 `secret_submit` 目前只接受直传明文 |

⇒ **C7 的最终形态是「互补」而非「替代」**：
插件保留**执行侧**保护（argv / 脱敏）与**安全来源**（`${ENV}`/`@file:`），
内核负责**拦截 / 存储 / 回填 / 记忆侧路**。

#### 给内核的后续项（进 §6）

`secret_submit` 应支持 `${ENV_NAME}` 与 `@file:/path` 两种来源 ——
把「明文不进会话」的能力从插件上移到内核（T10）。

---

### 5.5 ★ 变量库统一（插件不再自建加密存储） ［已完成］

**用户指出的问题（2026-10-14）**：

> 不对，你写了两遍变量，为什么不直接让安全模块暴露相关接口让插件使用？

**确认属实。** 插件自带了一整套与内核**完全重复**的加密存储：

| | 内核 `internal/security.Store` | 插件 `unifiedSecretFile` |
|---|---|---|
| 落盘 | `secrets.json` | `secrets.json` |
| 加密 | AES-256-GCM | AES-256-GCM |
| 别名 | HMAC 派生（自动合并）| 用户指定 |
| 引用 | `@{sec_xxx}` | `@{name}` |

★ 重复的真正代价不是代码量，而是**两套库互不可见**：
内核拦截时登记的变量插件看不到、插件登记的拦截又不认识，
同一凭据可能在两处各存一份而使用者不知哪个权威。

#### 做法：SDK 新增 `SecretsAPI`

```
Store(value, note) → alias      值交给内核，返回别名
List()                          元信息（**不含明文**）
Forget(alias)                   删除
Fill(text) → out, n, aliases    把 @{别名} 换成明文
Encrypt/Decrypt                 给插件自己的配置文件用（密钥由内核管）
Available()                     变量库是否可用
```

**去重后的分工**：

| 谁 | 存什么 |
|---|---|
| 内核 | **值**（加密）、派生别名、调用插件前回填 |
| 插件 | 「可读名 → 内核别名」**映射**（不含值）、来源解析、argv 保护 |

★ **为什么插件还需要一层映射**：内核别名是 HMAC 派生的
（`sec_0cb468e2…`），这是「同明文自动合并」的前提（S11），**不能改成用户自选**。
但人需要 `@{gitea-local}` 这种能读懂的名字 ⇒ 插件存一层名字映射。
**映射里只有名字，没有值** —— 它泄了也没用。

#### ☆ 安全性论证：为什么暴露 `Fill` 不破 S1

S1 说「不提供取回明文的接口」，理由是一旦 LLM 能取回，它就能把明文写回上下文。
那插件为什么可以拿明文？**因为边界不在插件侧**：

1. 内核在**插件 handler 之前**就把工具参数里的 `@{别名}` 填好了
   （`StageHost.ExecuteTool` → `security.FillMap`）—— 插件**本来就能**
   从 args 读到明文，这是设计使然。
2. 明文若被插件**返回**，会被两处拦回别名（`ContextEvent` scrub + provider 装饰器）。

两条都有判据钉住（`TestPremise_PluginReturnedPlaintext*`：插件故意回吐明文，
验证 context 侧与 provider 侧都拦得住）。所以：

> **暴露 `Fill` 不是"开了一个口子"，而是把本来就存在的能力
> 从各插件自己实现改为统一接口。**

⚠️ 但**绝不能**暴露 `Reveal(alias) -> 明文` 这样的按名字直取接口 ——
那会让插件（以及操纵它的 LLM）枚举别名并逐个取值。本接口刻意没有这样的方法。

#### ☆☆ 实测踩到的坑

**`Available()` 不能用 `List` 推断。** 初版让模板的 `procSecrets.Available()`
调 `secrets.list`，看 RPC 是否成功。但**未启用时 List 返回空列表（不报错）**，
于是"库是空的"与"库不存在"在 RPC 层长得一样 ⇒ `Available()` 误判为 true，
接着 `Store` 才失败。错误信息于是变成"存储失败"而非"能力不可用"，
**排查方向完全错**。修法：加 `secrets.available` 专门回答这个问题。
有判据 + 变异钉住。

**`go:embed` 第三次咬人。** `hmapdev` 用 `//go:embed` 把模板编进二进制。
我改了 `templates/proc_main.go.tmpl` 源码，但跑的是**旧二进制**，
于是生成出的 shim 缺方法、报 `missing method GetSecret` —— 而报错指向
生成文件，看不出真因是"模板没重新编译进二进制"。
修法：`go build ./tools/hmapdev/` 重建并安装。

**两处校验器字符集不一致。** 引用正则 `credRefPattern` 允许 `.`，
而别名校验器 `validName` 不允许 ⇒ 「能被引用却存不进去」。
修法：secret 别名用自己的校验器（`validSecretName`），
**不混用**项目/通道名的校验器 —— 两者是两个命名空间。

#### 验证

`internal/plugin/proc/e2e_secrets_test.go` 3 条（**真实模板**编译的插件跑全链路）；
`internal/agent/core/leakback_test.go` 2 条（插件回吐明文被拦）；
`internal/security/alias_shape_test.go` 1 条（别名是派生值、不能自选）；
插件 `encstore_test.go` 重写为 7 条（含「值只存一处」不变量）。
三层零失败、`-race` 通过、`hmapdev build` 成功。

---

### 5.6 ★ 拦截范围盘点（「到底拦什么」） ［实测 · 已固化判据］

判据：`internal/security/survey_test.go`（会拦 / 刻意不拦，两个清单）。

#### 会拦（→ 换成 `@{sec_xxx}`）

| 类别 | 形态 |
|---|---|
| GitHub token | `ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_` + 36 位 |
| GitHub fine-grained | `github_pat_` + 60 位 |
| GitLab | `glpat-` + 20 位 |
| OpenAI | `sk-`（含 `sk-proj-`）+ 20 位 |
| Anthropic | `sk-ant-` + 20 位 |
| AWS 访问密钥 | `AKIA`/`ASIA` + 16 位大写 |
| Slack | `xox[baprs]-` |
| Bearer 头 | `Bearer ` + 30 位以上 |
| **私钥块（7 类）** | `-----BEGIN PRIVATE KEY-----`（PKCS#8）｜`RSA PRIVATE KEY`｜`DSA`/`EC PRIVATE KEY`｜**`OPENSSH PRIVATE KEY`**（ssh-keygen 现代默认）｜`ENCRYPTED PRIVATE KEY`｜**`PGP PRIVATE KEY BLOCK`**｜**`SSH2 ENCRYPTED PRIVATE KEY`** |
| 赋值语句 | 键名含 `api_key`/`access_token`/`auth_token`/`secret_key`/`client_secret`/`password`/`passwd`，**分隔符 `=` 或 `:` 都可**，值 16 位以上 |

#### 刻意**不拦**（每条都有理由 —— 边界比能力更重要）

| 不拦 | 原因 |
|---|---|
| **40 位 hex（Gitea token 形态）** | 与 git commit SHA **形态完全相同** ⇒ 拦它会误伤每个 commit id（S4） |
| 裸 32 位十六进制 | 与 UUID / 会话 id / 哈希撞车，太泛 |
| JWT | 形态宽泛，误伤风险高于收益（带 `Bearer ` 前缀时那条会拦） |
| 密码短语 / 中文口令 | 无固定字符集，无法与正常文本区分 |
| PIN / 短口令 | 太短，任何数字都会撞 |
| 手机号 / 身份证 | PII 不是本模块目标，且误伤面极大 |
| **URL 里的密码**（`https://user:pass@host`） | ⚠️ **已知缺口**，需专门规则，当前未覆盖 |
| SSH 私钥**文件路径**（`.ssh/id_rsa`） | 拦路径不拦内容 —— 文件内容被读出时会命中私钥块规则 |
| **SSH 公钥**（`ssh-ed25519 AAAA…`、`ssh-rsa AAAA…`） | 公钥**不是秘密**（可公开）。拦它会让正常运维操作坏掉 |
| `authorized_keys` / `~/.ssh/config` / `known_hosts` | 不含密钥本体（config 只有路径；known_hosts 与正常 hostname 无区别） |
| `SSH_AUTH_SOCK` | 只是 ssh-agent 的 socket 路径，不含密钥 |

#### ☆ SSH / 密钥专项：实测补上两个漏报

用**本机真实密钥**（`~/.ssh/id_rsa`、`id_ed25519`）+ 各类 PEM 头验证时发现，
初版正则漏了 **两类真实存在的私钥格式**：

| 漏掉的 | 为什么 |
|---|---|
| `-----BEGIN PGP PRIVATE KEY BLOCK-----` | 尾部是 `KEY BLOCK`，不是 `PRIVATE KEY` |
| `-----BEGIN SSH2 ENCRYPTED PRIVATE KEY-----` | 头部是 `SSH2 ENCRYPTED PRIVATE KEY` |

**根因**：初版正则在 END 侧**硬写**了 `PRIVATE KEY`：
```
-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]{64,}?-----END [A-Z ]*PRIVATE KEY-----
                                                            ^^^^^^^^^^^ 写死了
```
而真实密钥的尾部词因格式而异。修法：两侧都用 `[A-Z0-9 ]*PRIVATE KEY[A-Z ]*`。

**长度下界 64 的用意**（防误伤）：真实私钥体远超 64 字节，而
「文档里只引用头尾两行做说明」的情形中间没有内容 ⇒ 不会被拦。
有判据双向钉住（长体必拦 / 短引用不拦）。

#### ☆ 盘点本身抓到一个真实漏报

写这份盘点时发现 `password: "hunter2hunter2hunter2"`（**冒号**形态）**没被拦**。

**根因是我加 prefilter 时引入的**：`assigned_secret` 的正则分隔符有
两个备选（`:` 与 `=`），而 prefilter 只写了 `"="` ⇒ 冒号形态被
**静默挡住**。

更值得记的是**为什么既有判据没发现**：样本恰好都用了 `=`，
测试全绿 —— 判据覆盖了"能用的形态"，漏了"另一种同样合法的形态"。

修法：
1. `Prefilter` 支持**多候选**（`"a|b"`，任一命中即放行）
2. 新增 `TestPrefilterCoversAllAlternatives`：**逐形态**验证
   「正则支持的每种写法，prefilter 都必须放行」

> 教训：**盘点"什么没被拦"比盘点"什么被拦了"更能发现问题**。
> 正向清单告诉你能力边界，负向清单才暴露洞。

---

## 6. 待办与未决（续）

> 上面 §5 的 T1–T10 是**安全模块本体**的遗留项；
> 本节记录**配置加密**（§5.2）落地后新暴露的项。

| # | 事项 | 说明 |
|---|---|---|
| **E-1** | 密钥轮换 | ✅ **已完成**（2026-10-15）：`security.keyring`（0600）持久化「当前钥 + 历史只读钥」；Store 支持 `RotateKey`（**已有别名不变**、密文重加密落盘、旧钥留在环里）；Codec 支持 `InitConfigCodecWithOldKeys` + `Rekey`/`NeedsRekey`；config 侧 `RekeySecrets` 收口。7 条判据 + 4 条变异 |
| **E-2** | 压缩/备份工具的兼容 | ✅ **已完成**（2026-10-15）：`deploy-plan.sh backup` 打印密钥位置与「密钥须与数据分开存放」提示；内核启动时 `AuditSecrets` 审计密文可读性，发现不可解即告警并给出修复动作（3 条判据 + 2 条变异） |
| **E-3** | WebUI 的「密钥可见范围」 | ✅ **已完成**（2026-10-15）：设置列表默认以占位符屏蔽敏感值；新增单键 `/api/v1/settings/reveal?key=`，核心/当前 WebUI 插件可显式揭示，其他插件一律 403；揭示动作留痕。2 条判据 + 2 条变异 |
| **E-4** | `HOMED_SECURITY_KEY` 生产方式 | ✅ **已完成**（2026-10-15）：优先用 systemd `LoadCredential`（tmpfs、0400、**不进 environ**），降级 env，再降级磁盘文件。unit 已配 `Type=exec`。4 条判据 + 2 条变异，真实 systemd 实测 |

---

## 7. 与既有文档的关系

| 文档 | 关系 |
|---|---|
| `commondev-agent-runtime-design.md` | 前稿的插件侧拦截（`§2.7` 之后）**降级为卫生措施**；安全责任移到本文 |
| `resident-subagent-design.md` | 驻留子的 `KernelSource` L4 模型是**另一个**安全机制（权限隔离），与本文正交 |
| `kernel-modularization-design.md` | **★ 本文 §3 的装配点约束是该文档 §2.3「Phase 0」的实例**。安全模块必须在任何读 `core.llm.*` 的操作之前装配——实测踩过：装配晚了只会打一行 warning，然后 key 变空一路走到 401（症状指向"key 配错"，实际是顺序问题）。该文档把这类约束从"注释"变成"机器校验" |
| 本仓 `internal/plugin/proc/corehandler.go:279` 的 L4 夹取 | 同属"内核不信插件自报"的**同一设计哲学**，可参考其实现风格 |

---

## 8. 实测教训：安全模块的装配顺序（2026-10-16）

> 这一条加到本文，因为它**直接暴露了"顺序只写在注释里"的危险**。

**故障**：安全模块装配晚于 `cfgReg.SetLLMSnapshotFile()`／`cfgReg.ToConfig()`，
而这两者启动即读 `core.llm.*`（需主密钥解密）。

**后果链**：

```
密钥未装配 → 解密失败（只打一行 warning）→ key 变空串
→ initLLMProviders 拿到空 key → 所有 LLM 调用 401 invalid api key
```

**为何难查**：401 看起来像"key 配错了"。而实际上：
- key 值在库里是**对的**
- 直连上游 llmsproxy **HTTP 200**
- 只有内核 agent 401

⇒ 查 key 值、改 key、重配 provider **全部无效**，因为问题是**启动顺序**。

**修法**：把整个安全块前移到*任何*读 `core.llm.*` 的操作之前，并加**结构性判据**
（`cmd/homed/bootstrap_order_test.go`，断言真实的相对行号）。

**★ 一个中途犯的错（值得记住）**：修复第一版只把安全块移到 `ToConfig()` 之前，
**漏了 `SetLLMSnapshotFile()` 也在它之前读密文** ⇒ 判据变绿而线上照旧报错。
教训：顺序类判据**必须覆盖所有读取点**，不能只盯一个。

完整设计见 `kernel-modularization-design.md` §1.2 与 §2.3。

---

## 8. 给接手者的三条提醒

1. **安全模块的位置决定它是不是安全**。初稿以为"唯一出网点"在 task 层，
   实测扫出 **7 处**构造请求 —— 放在 task 层会漏 4 处。
   教训：**落点要靠扫仓实测确定，不能靠读一处代码推断**。
2. **"能引用"与"能读取"必须分开**。前者是可用性所需，后者是泄露。
   把它们混在一起，就是前稿最大的设计错误。
3. **模式库宁漏勿误伤**。误报会搞坏正常任务，而任务坏了人会来查——那时代价远大于漏报一次。
