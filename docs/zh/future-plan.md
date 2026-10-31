# 未来计划（Future Work）

> **本文是唯一登记「尚未实现 / 已规划未做 / 已知缺口」的文档。**
>
> 建立于 2026-10-31。此前这些内容散落在各设计文档的「未做」「待补」「遗留」小节里，
> 时间一长就与代码脱节（有的已完成却仍写着未做，有的未做却被当成已完成）。
>
> ## 收录规则
>
> **只收两类：**
> 1. **代码里确实没有的能力**（已用 codegraph / 实测确认）
> 2. **有机制但零生产调用方**（本仓最常见的缺陷形态，见 §0）
>
> **不收：**
> - 已经在做的事（那不是"计划"，是"进行中"，写在对应设计文档里）
> - 历史决策与被否决的方案（那是归档，写在 `legacy-issues.md` 等历史文档里）
>
> ## 状态标记
>
> | 标记 | 含义 |
> |---|---|
> | 🔴 | 有真实影响，应当优先做 |
> | 🟡 | 该做，不紧急 |
> | ⚪ | 有价值但优先级低 / 依赖前置条件 |
> | ⏸ | 主动搁置（有明确理由，不是忘了） |

---

## 〇、先说清一个反复出现的缺陷形态：**机制 ≠ 接线**

本仓已有至少 **5 例**「代码写对了、判据全绿、生产里从不执行」的缺陷。
它们比 bug 更难发现，因为：

- 策略表存在、判定函数正确、日志里一条拒绝记录都没有 ⇒ 看起来「没人违规」
- 判据全绿，因为判据**直接调用**被测量的函数 ⇒ 测不到"有没有人调它"

| # | 能力 | 机制 | 曾有的缺陷 | 现状 |
|---|---|---|---|---|
| 1 | 工具黑白名单 `Policy.CheckTool` | 完整 | `Agent.security` 恒 nil，闸门恒放行 | ✅ 2026-10-30 修复（依赖图补 `FacSecurity`） |
| 2 | 主体策略 `CheckToolFor` | 完整 | 0 生产调用方 | ✅ 2026-10-30 修复（父为子声明） |
| 3 | 插件命名档位 `ActivateProfile` | 完整 | 曾 0 生产调用方 | ✅ 2026-10-31 修（见 §1.1，两次修才修对） |
| 4 | 敏感词 / 注入预警 `ScanContent` | 完整（254 行） | **0 生产调用方** | 🔴 **未修**，见 §1.2 |
| 5 | 沙箱 `CheckPath` / `CheckNetwork` | 完整 | **0 生产调用方** | 🔴 **未修**，见 §1.3 |

**做任何新能力之前，先问一句：这条链的每一跳，生产里真的有人调吗？**

---

## 一、安全（security）

### 1.1 ✅ 插件命名档位已接通（2026-10-31，两次提交）

> **本节已结案，保留是因为它的两次修法本身是教训。**
> 最初的诊断是「`activateSecurityProfile` 零调用方，在 `prepareInputTask`
> 里加一次调用」—— **那个诊断方向是错的**。

**真正的缺陷分三层：**

```
第 3 层（表面）：activateSecurityProfile 零调用方
                 ↑ 补一次调用「看起来」就好了

第 2 层：agent/core 在替 security 做**策略决策**
         · 自己从 payload 抠 security_profile（解析 security 的协议）
         · 自己判断 parentID != "" → 走 CheckToolFor，否则 CheckTool
         · 自己拼 PrincipalPolicy{...} 字面量
         · 自己写 Kind: "resident" —— **声明自己是哪类主体**（提权口子）

第 1 层（根因）：security 缺少「输入接入点」
         判定原语是细粒度的，要求调用方自己知道"我是谁、该走哪条链"。
         于是每个消费方都得理解策略语义，三条输入路径各接一遍。
```

**收敛后的形态**（`5194a96`）：

```
RoundInput{AgentID, IsChild, Payload, Source}   ← 只有事实，无 Kind
rt.BeginRound(in)  → security 自己解析档位 + 自己判定主体类别
round.CheckTool(t) → 本轮判定
round.End()        → 归位
```

**再进一层：每 agent 一个实例**（`8293282`，用户裁定「每个 agent 一个，
分为 sessionProfile 和 longtimeProfile」）：

```
共享（进程级 1 份）          per-agent（N 份）
─────────────────────      ─────────────────────
gate / policy / content      sessionProfile   ← 轮次级，End 即清
profiles / principals        longtimeProfile  ← agent 生命期，跨轮保留
scopes / eventPolicy
```

优先级 **session > longtime**，这个顺序本身是安全语义：
session 是"这一轮的事实"，必须能盖住 long-time 默认值（否则访客在长期
为 owner 的 agent 上无法收窄）；反向就是提权。

**判据**：`TestAgentScopesDoNotShareSessionProfile` /
`TestLongtimeProfileSurvivesRounds` / `TestReleaseAgentDropsLongtimeProfile` /
`TestAgentCoreHoldsNoPolicySemantics`（源码扫描，禁 agent 再做策略决策）/
`TestEveryInputPathDeclaresRound`（三条输入路径都要声明轮次边界）。

**顺带暴露的一个仍未解决的限制**：`Round` 是 per-round 句柄，但
`Runtime.active`（旧路径）仍是进程级。per-agent 视图已经解决**并发 agent 串档**，
但**同一 agent 内**若将来出现并行子任务，仍需按 task 隔离。
当前所有路径都是串行的，故不构成缺陷。

---

### 1.2 🔴 敏感词 / 提示词注入预警从未运行

**现状**：`internal/security/content.go`（254 行）实现了完整的
`ContentPolicy{Scan, BlockedBy}`，`Runtime.ScanContent` 也写好了，
但**全仓零生产调用方**（只剩 2 条测试在调）。

**影响**：

- 配置里写的敏感词一条都不生效
- 模型被提示词注入时内核不预警、不阻断
- 插件通过 `SecurityProfile.SensitiveWords` 声明的档位敏感词同样不生效

**要做的**：在**入向边界**接线。合理位置是 `executeToolCallOutcome` 的 defer
（那里已经有 `security.Global().Replace(...)` 做凭据脱敏，同一个位置、
同一条 defer 分支），以及插件注入的输入在进 `prepareInputTask` 之前。

**注意**：不要只接一处。入向有两条路——工具结果、插件输入事件。只接一条会
留下「工具的返回被扫了、插件消息没扫」的缺口，而后者才是注入的主要入口。

### 1.3 🔴 沙箱约束从未运行

**现状**：

| 能力 | 机制 | 生产调用方 |
|---|---|---|
| `Policy.CheckPath(方向, 路径)` | 完整，拦 `..` 逃逸 | **0** |
| `Policy.CheckNetwork()` | 完整 | **0** |
| `Runtime.CheckPath` / `Runtime.CheckNetwork` | 薄包装 | **0**（只被测试调） |

配置项 `core.security.sandbox.*` 全套已注册（`enabled` / `read_roots` /
`write_roots` / `allow_network`），`ApplyPolicyFromConfig` 也确实把它们装进了
`Policy` —— 但**没有任何模块拿它判路径**。

**当前唯一真实生效的路径约束**在 files 插件自己的 `pathWithinSandbox`
（`internal/plugins/files/plugin.go`）—— 这正是设计文档批判的
「每个插件各写一套」形态。

**要做的**：两个选择，需先定方向。

- **A（内核）**：在工具参数校验之后、分派之前，对带路径/URL 的参数做 `CheckPath` / `CheckNetwork`
- **B（插件）**：把 `CheckPath` 经 SDK 暴露给插件，让每个 IO 插件自己调（**不推荐** ——
  会重新长回"各写一套"）

倾向 A，但要先解决"哪些参数算路径"的判定（不能瞎猜 `command` 里的字符串）。

### 1.4 🟡 插件无法声明主体策略

**现状**：公开 SDK 的 `SecurityAPI` 只有 `DeclareProfile` / `DeclareProfiles`
（命名档位）。`Runtime.DeclarePrincipalPolicy` 存在但**未暴露给插件**。

**区别**：档位是「按输入携带的参数选」，主体策略是「按主体 ID/标签授予」。
QQ 需要前者（owner vs guest 由发送者身份选），但一个需要「按具体用户 id 授予
工具」的插件需要后者。

**注意**：暴露前要想清 scope。`scope="tag"` 是**部署级**语义，让插件声明 tag 策略
会让插件有能力影响全站 —— 那是个提权口子。倾向只暴露 `scope="id"`。

### 1.5 🟡 凭据库（变量库）的六个未决项

从 `kernel-secret-firewall-design.md` §5 迁来（原 T5–T10）。**T1–T4 已落地**，
不再重列；下面六项代码里确实没有。

| # | 事项 | 为什么要做 |
|---|---|---|
| T9 | **不自动淘汰的规模上限** | 变量库落盘文件只增不减。需要**可观测**的降级（超 N 条时告警），而不是静默删某条 |
| T10 | **提交工具的泄露面** | "主动提交"要把明文交给内核 ⇒ 明文会进会话/日志。应优先支持 `${ENV}` / `@file:` 两条**不明文入会话**的路径 |
| T5 | **压缩对别名的影响** | 上下文压缩的摘要可能把 `@{sec_1}` 当普通文本；压缩后别名是否仍有效未验证 |
| T6 | **明文在内存中的擦除** | Go 字符串不可变 ⇒ 只能擦可写副本。要先定"努力擦除"还是"不承诺" |
| T7 | **引用外部文件、内核只持路径** | SSH 私钥这类"内核拿不到字节"的场景，可能需要只持路径的形态 |
| T8 | **入向回填的安全边界** | 回填后 agent 可把 `@{别名}` 写进**任意**命令参数 ⇒ 等于"在任意命令里用明文"。这**是必需的**（否则没法用密钥），但"能做什么"因此回到**命令本身**。要定：要不要限制哪些工具允许触发回填 |

### 1.6 🟡 硬私有的判定目前靠插件自己

**现状**：QQ 的 `hard_private_tools` 由插件读配置后通过 `DenyTools` 声明
（而不是内核的 `HardPrivateTools` 语义）。

差异：`DenyTools` 是「这个主体不能用」，`HardPrivateTools` 是「**任何**用户主体不能用」。
后者防的是「用户偷看私人数据」，而内部执行体（父的委派）本就有完整权限 ——
即使用错了也不会误伤内部，但语义是对的。

**要做的**：QQ 插件把 `hard_private_tools` 从 `DenyTools` 改成
`HardPrivateTools`。纯插件改动，声明链路已贯通。

---

## 二、内核模块化

### 2.1 🔴 device 模块是空壳 —— 工具执行仍在 agent

**现状**：

| 项 | 设计意图 | 实际 |
|---|---|---|
| 工具枚举 | device 拥有 | ✅ `device.Runtime.Tools()` 有，但**唯一调用方是它自己的 ToolCount()** |
| 工具路由 | device 拥有 | ❌ `toolcall.go` 实际调的是 `toolroute.Dispatch`，不是 `device.Dispatch` |
| 工具执行 | device 拥有 | ❌ `device.Runtime.Execute` **0 生产调用方** |
| inputch / outputch | device 拥有 | ❌ 仍在 `internal/agent/io` |
| 安全过境 | device 是唯一落点 | ❌ device 拿到的 `ks.Security` 没有任何执行路径会用到 |

`ks.Device` 与 `Agent.MountDevice` 的产物没有任何读取方 ——
`agent.Device()` 除了自己没人调。

**这不是"漏接线"，是刻意的分阶段**（见 `internal/kernelmod/device/mod.go` 包注释）：
第一阶段先立边界与闸门，第二阶段再搬实现。**文档应当如实这么写，而不是写成已完成。**

**要做的（第二阶段）**：把 `tooldefs` / `toolcall` 的实现迁进 device。前置条件：

- device 对 Agent 内置处理器的窄接口（`api.BuiltinToolHandlers` 已有雏形）
- 工具执行中的上下文/超时/取消/panic 恢复要一起搬
- 记忆策略（`ContextPolicy` / `RecallPolicy`）的归属要定清
- 独立工具（memgc 等）无 device 时的退化路径

### 2.2 🟡 `device` 设施无人消费

`device` 模块 `Provides FacDevice`，但**没有任何模块 `Requires` 它**（实测）。
它是图的叶子。

**含义**：漏挂 device 模块时，所有判据仍绿（见 `bootgraph_test.go` 里
`TestEveryRegisteredModuleIsCompiledIn` 的注释已经写明这类盲区）。
在第二阶段完成前，这属于预期状态；完成后应有模块依赖它。

### 2.3 🟡 声明↔真实读取的漂移判据缺失

**历史**：设计文档 §2.7.3 点名了 `TestDeclarationsTraceToRealCallSites`
（「声明与真实签名不漂移」），它随 `3864f36`（删死包 `kernelboot/modules`）
迁包时**没有搬过来**，现在仓库里不存在。

**为什么它重要**：`kernelmod/agent` 的 `Requires()` 曾经少了 `FacSecurity` ——
代码读 `ks.Security` 却不在图上声明。**这条判据正是用来抓这种漂移的**，
它缺失期间，`agent→security` 这条边断了整整一轮发布。

**要做的**：写一条判据，扫描各模块 `Init` 及同目录非测试代码里对
`ks.<Field>` 的读取，与 `Requires()` 的设施集合比对，报告「读了但没声明」。

**已知的例外**（判据要放行，不能误报）：

- **Group 0 产物**（`Cfg` / `CfgReg` / `DataDir` / `AgentWorkDir` / `CLISocket` /
  `HTTPAddr`）—— 它们不由任何可调度模块提供，靠"阶段前提"保证。
  这类靠各模块自己的 nil 检查守（见 `nilcheck_test.go`）
- **自产自读**（如 text-memory 读自己的 `ks.TextMemory`）
- **KernelState 的方法**（`AddCloser` / `AddQuiescer`），不是字段

### 2.4 ⚪ `deploy/depgraph-decls.json` 已无消费者

手写声明清单，生成器早已改为读模块的 `Requires()/Provides()`。文件仍在仓库里，
是误导性遗留（新人会以为要维护它）。

**要做的**：删文件，或改成明确的 deprecated 说明。

---

## 三、记忆

### 3.1 🟡 `OversizeToolReports` / `oversizeHintFor` 零消费者

**现状**：`toolresult_budget.go` 统计了「本任务有多少次超大工具结果」，
但除了它自己没人读。`offloadToolResult`（超长结果转存 + 句柄回读）**已经接好**了，
所以主路径不受影响。

**缺的是**：把统计接进调度器/下一轮决策，让模型知道"你这次查询太宽了"。

### 3.2 ⚪ 图库 `memory_blocks.data` 单表承载媒体

**现状**：媒体字节直接存在 `memory_blocks.data` BLOB（块 union 化，CAS 已淘汰）。
生产实测：330MB 库中 299MB 是这个列（563 个有负载的块，去重后 556 个 —— 重复率低，不是问题）。

**观察，非缺陷**：这是刻意的 union 化设计（元数据与负载同行）。若将来库膨胀，
优化点是分表或外置 blob 存储，不是"加回 CAS"。

---

## 四、日志与运维

### 4.1 🔴 日志保留策略只清 1/4 的归档类型

**现状**（实测，见 `internal/log/retention.go`）：

`cleanupBeforeMonths`（默认 `n_months` / 3）**只处理 `month_` 和 `year_`**，
完全不碰 `week_` 和裸 `homed_*.log`。

探针实测（cutoff 3 个月）：

```
输入:   month_2026-18  month_2026-02  week_2026-W18  week_2026-W44  year_2026
保留后: month_2026-18  week_2026-W18  week_2026-W44
       （month_2026-02 与 year_2026 被删，符合预期；半年前的 week_ 永远留着）
```

**更硬的证据：`month_*.tar.gz` 的命名根本没有月份。**

`compressMonthly` 用 `key := fmt.Sprintf("%s-%s", m[1], m[2][:2])`
（m[1]=年，m[2]=ISO 周）⇒ 产出 `month_2026-27` / `month_2026-38`。
而 `cleanupBeforeMonths` 用 `time.Parse("2006-01", f[6:13])` 解析 ⇒
**月=27 越界解析失败 ⇒ `continue` ⇒ 一个都不删**。

生产目录里 `month_2026-27` ~ `month_2026-39` 全都是这种畸形名，
且 `compressMonthly` 遇到同名文件会 `continue` —— 这 13 个文件永远不会被合并或清理。

**为什么判据没抓到**：`retention_test.go` 的 5 条判据是**手写**合规文件名
（`month_%04d-%02d`），从没跑过 `compressMonthly` 的真实产物。

**要做的**：

1. `compressMonthly` 用真实月份做键（从周名推出所属月，或改用 `month_YYYY-MM`）
2. `cleanupBeforeMonths` 覆盖 `week_` 与裸 `homed_*.log`
3. 判据必须**先跑 `compressMonthly` 拿产物、再跑 retention 删它** ——
   手写文件名的判据测不到命名不一致

### 4.2 🟡 生产日志存量未清

生产 `/home/hologramagent/log`：491 个裸 `homed_*.log`（75MB），其中 **459 个是
2026-10-26 崩溃循环**留下的。`week_2026-W43` 只含 1 个文件（应为 22 个）——
因为那批崩溃日志在周压缩跑之前就被 `fileExists` 判断挡住了。

retention 修好之后这些会被逐步清掉，但不会立刻（要等跨月压缩）。

---

## 五、文档与工程卫生

### 5.0 🟡 两个测试用例存在「固定时间假设」导致偶发红

**2026-10-31 记录。** CI 的 `-race` 门两次因偶发失败变红（提交 `merge: pr31`、
`style(webui)`），本地却复现不出来。

#### 已修：`TestRTChild_InterruptTruncatesOutput`

失败信息 `等待超时：开始输出`（正好 5.00s）。查 CI 日志：`[rtchild] t started`
之后**一轮都没跑到 provider** —— 不是输出丢了，是轮次没启动。

原写法把「前置条件（输出已开始）」和「被测行为（Stop 后输出冻结）」塞进同一个
固定 5 秒的 `waitFor` 里，超时时看不出卡在哪一步。已改成两段**等状态**：
先等 `p.callCount() >= 1`（轮次真进入 ChatStream），再等 chunk 到设备。
超时直接指认卡在哪步。同 2026-10-24 `TestStress_Nesting...` 的修法。

#### 未修：`TestResidualDropNotifiesSyncCaller`

本地整包 `-race` 8 轮里偶发 1 次（`应处置 1 条，实际 0`），CI 从未出现。

根因已定位：`scheduler.waitSchedulerStopped()` 只等 **`running == nil`**
（没有任务在跑），**不等调度循环退出**。于是 `child.cancel()` 之后仍有一个窗口：
循环可能还停在下一轮顶部、或正走到 `pumpInbox`/`nextRef`，把测试刚入队的任务
取走执行 ⇒ 残余任务为 0 ⇒ 断言失效。

**要修得给生产代码加一个「循环已退出」的信号**（`schedulerLoop` 退出时 close
一个 channel，`waitSchedulerStopped` 改为等它），不是改测试能解决的。
因此先登记、不改：它在 CI 里从未出现过，而改调度器退出信号属于生产路径改动，
收益与风险不匹配。

### 5.1 🟡 README / ARCHITECTURE 的工具名陈旧

实际工具名是 `stablemem_*` / `coldcontext_*`，但对外文档仍写 `memory_recall` /
`memory_commit` / `doc_query`。

| 文件 | 处数 | 性质 |
|---|---|---|
| `assets/docs/zh/ARCHITECTURE.md` | 6 | **对外文档，必须改** |
| `assets/docs/zh/OVERVIEW.md` | 1 | 对外文档 |
| `README.md` / `README_EN.md` | 3 / 3 | 对外文档 |
| `docs/zh/resident-subagent-design.md` | 2 | 活设计文档 |

**2026-10-31 状态**：已修正（见 §六 变更记录）。

### 5.2 🟡 plan.md 不进 git，却被 43 个 .go 文件当"单一事实来源"引用

- `.gitignore` 第 101 行忽略 `/plan.md`
- 43 个 `.go` 文件的注释里写着「见 plan.md §X」
- 而 plan.md 的状态表**已经谎报过一次**（把 `SetSecurity` 记成已完成，
  实际生产 0 调用方）

**要做的**：要么把 plan.md 纳入 git 并建立"状态表必须有判据背书"的规则，
要么把引用改为指向具体设计文档而不是 plan.md。

### 5.3 ⚪ CI 不跑 gofmt

`make lint` 有 golangci-lint（warn-only，本机未安装），但 `ci.yml` 没有 gofmt 步骤。
2026-10-30 曾检测到 1 处漂移（`rtchild_route_test.go`），已顺手清掉。

### 5.4 ⚪ 工作区体积

`build/` 7.2G + `dist/` 6.6G + `.npm-cache` 5G = 约 20G（git 只跟踪 1,313 文件）。
`dist/_old-20261017` 3.2G 是历史发行包。

---

## 六、分散在各设计文档里的待办（索引）

下面这些待办**留在原文档**里更合适（它们与那份文档的实验/设计强耦合），
这里只做索引，避免"想找待办要翻 30 篇文档"。

| 待办 | 在原文档 | 性质 |
|---|---|---|
| **弃答：泛指词检测未实现** | `abstention-criterion.md` §未完成 | 🟡 拒答主路径已接（`RecallBlocksGuarded`），泛指词这一支没做 |
| **弃答：n=10 判据样本偏小** | 同上 | ⚪ 方向确定（确定性事实+完全分离），样本可扩 |
| **遗留表退役：生产仍在用旧三表** | `legacy-table-retirement.md` | 🔴 迁移工具已就绪（`internal/memory/migrate_entities.go`），生产未切换。2026-10-31 实测：entities 1292 / relations 978 / sentences 66 行仍在。方案原文已随计划文档删除，剩余读方清单见 `legacy-table-retirement.md` §八/§十二 |
| **场景：现网垃圾场景清理 + 收口验证** | `future-plan.md`（原计划已删） | ⚪ 代码侧 7 步里 5 步已完成（归一化/ScenePolicy/R3/R5/R4 覆盖面）；剩「清现网垃圾场景」与「收口验证」属现场操作 |
| **中断子系统独立化** | `interrupt-subsystem-split-plan.md` | ⚪ 计划未实施；当前中断实现仍在 `agent/core`（`interrupt.go` + `scheduler.go`） |
| 测试模板库（压力/接口/编译预置模板） | `commondev-design.md` §4「尚未做」 | ⚪ 机制已落地，模板按需补 |
| 记忆召回「形态 3 / 形态 4」两个缺口 | `recall-capability-tiers.md` §「待补的能力」 | 🟡 需新实验，方向已列 |
| 判据口径与契约对齐 | `abstention-criterion.md` §「待办」 | 🟡 |
| 生产召回验证的 3 条遗留 sentence→块 边 | `production-recall-validation.md` | ⚪ 非迁移产生 |
| 遗留问题清单 | `legacy-issues.md` §二「真正待办」 | ⚪ 长期清单 |

---

## 七、变更记录

| 日期 | 变更 |
|---|---|
| 2026-10-30 | 工具策略闸接线（依赖图补 `agent→security`）；父为子声明主体策略 + 撤销 |
| 2026-10-31 | 建立本文；全量校订 35 篇文档的状态声明与失真表述；修正对外文档的工具名；从 `kernel-secret-firewall-design.md` §5 迁入 T5–T10 |
