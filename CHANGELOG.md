# 更新记录

本文件记录各版本的实质变化。README 只留"现在能干什么"，历史演进在这里。

入口文档：[README](README.md) · [架构](assets/docs/zh/ARCHITECTURE.md) · [未来计划](docs/zh/future-plan.md)


**v1.4.x 线**（进行中，`main`）—— **声明式并发安全** + **序列编排（seq）** + **工具结果诚实化**。

- **同轮工具并行执行**：同一轮里的多个 `tool_call` 默认并发执行，**但以「安全」为前置**——
  工具必须在自己的 `ToolDef` 里显式声明 `ParallelSafe`（内置工具用 `toolDefOptions`），
  未声明的一律串行；`Serial` 优先级更高。**批内只要有一个不安全，整批降级为串行**。
  并发声明写在**工具自身**，内核不做硬编码工具名安全表。同通道输出**严格保序**，
  结果与文本分离（`toolOutcome.Text` / `Raw`）。
- **`seq` 序列编排插件**：AST 解析 + 持久化、具名 group、变量槽、**组内并行 + 组间串行**、
  条件调用（`seq_when_call`）、跨序列调用图与环检测、错误契约。
- **工具结果只统计不裁剪**：结果超出预算时**报告**统计量，不静默截断。
- **结构化工具错误契约**：参数按 schema **预校验**（分派前拦下）、
  「工具不存在」与「执行失败」区分、`Success` 不再恒真。
- **设备命令白名单可配置**（waiter `device_cmd_allowlist`）、
  **站点漂移巡检**、**GUI 增量渲染**（保留全部历史，按 `data-msgkey` 复用节点，
  50/200/400 条实测 8.2/23.5/38.2ms）。

**v1.3.x 线**（v1.3.1–v1.3.13）—— **驻留式子 agent** + **输入调度器重做**。

- **驻留式子 agent**：内核可派驻轻量内核的子 agent（自己的调度器、自己的 temp 稳定记忆、
  共享通道登记表）。父经 `resident_agents`（list/create/send/inspect/compress/reclaim/destroy）
  派活与收活；inputch 可划给子，输入**在进内核之前**就已路由到子。
- **输出通道可寻址到具体 agent**：`AllowedOutputs` 授权集合（三处过滤点一致），
  父/子之间可互相投递；设备能力也 outputch 化（每设备一个 `device/<id>` 通道）。
- **输入调度器**：排队/中断两类别 + 四级中断（L1~L4）+ 抢占/挂起/恢复/中断栈；
  同级不抢占、有饥饿防护与抢占冷却；L4 只归内核与内核级插件（WebUI 终止按钮）。
- **轻量内核 profile**：子的记忆面收窄为「传统上下文 + 稳定记忆（stablemem）」（窄接口，
  主库以 query_only 受限句柄打开，写走自己的 temp 实例）。
- **积压及时反馈**（后续线）：主 agent 长时间忙时，内核把排队输入交给临时**分诊助手** ——
  简单的直接处理并回复，需要主 agent 的立刻回「忙碌中，请稍候」，用户不再干等十几分钟。
- 修掉一批真实缺陷：销毁驻留子时入站 inputch（`child/<id>`）注册残留、
  子的轮次永远显示 0（`info()` 根本没填）、子侧 childIO 空壳（未继承父的输出通道）、
  设备心跳 pong 忘了 Flush（每 60 秒掉线）、Lua 插件桥与 SDK 1.3.0 对齐。

**v1.2.0** — 统一多模态向量空间 + 媒体升为稳定记忆一等节点 + 数据面全量迁到共享内存。

- **模型中立的统一向量空间**：内核不再适配任何具体模型，只提供公共 provider SPI
  （`pkg/embedding`：`Modality` / `Input{Data,MIME}` / `Info{Dimension,Fingerprint,Modalities}`
  - 名字注册表），实现在 `providers/*`。默认 **Chinese-CLIP ViT-B/16** —— text 与 image
  落在**同一空间**（512 维、指纹 `cd2a495cf990`、Apache-2.0；实测加载峰值 1.59GB、静置回收后稳态约 0.89GB）；
  `qwen3vl` 保留（2048 维、常驻约 9.4GB，供内存充足或将来要视频的机器切回）。
  文本检索仍由既有词向量 / TF-IDF 兜底：CLIP 双塔的**纯文本语义弱于 MLLM 型嵌入器**，
  这是已知并写进文档的代价。
- **媒体是图数据库的一等节点与边**：**彻底删除**「用文本描述式索引图片」这套将就机制，
  以及 `media_refs` 与媒体引用计数。记忆块遵循单层不变量——Context → Document → Graph
  是块的**迁移**，不是复制、也不靠引用保活。
- **数据面全部走共享内存**（工具调用帧 / Cleaner / 输入输出通道 / 媒体块 / 文档与知识正文），
  RPC 只传偏移描述符；**RPC 协议升到 2**，fd3 布局改变，**不支持滚动升级**——
  内核与全部插件必须同批重建、同批安装，存量插件须用新版 `hmapdev` 重编。
- 注入可声明 `InjectOptions{NoMemory, ContextPolicy}`（**默认仍记入记忆、默认不裁剪**）；
  裁剪必须显式声明，且先经插件注册的 `Cleaner`。SDK 1.2.0 相对 1.1.0 **纯追加**。
- **发行包默认启用** ONNX 向量空间，并把模型（754MB）与 ONNX Runtime（24MB）随
  server/full 包发布；`homed` 放弃 Windows 原生支持改走 WSL2；jieba 词库内嵌进二进制。
- 修掉三个**安装链静默失败**：`initconfig` 因 `CGO_ENABLED=0` 是空操作（打印凭据却一个字节
  没写）、全新安装被误判「已有配置」而整体跳过默认值播种（装完 0 插件）、deb 的 `postinst`
  查错 unit 路径导致 `enable` 从未执行。
- 自本版起以 **AGPL-3.0-only** 发布（含网络条款；插件静态链接 SDK 故须同许可，见「许可」）。

> 以下历史条目保留原文以呈现演进，其中两条机制**已在 v1.2.0 移除**：
> 「媒体以 `[<mime> <短digest>] <描述>` 标记参与检索」（描述式索引）与「媒体引用计数式 GC」。
>
> 另有**一处许可口径已变更**（2026-09-24）：上面 v1.2.0 条目里的「插件静态链接 SDK 故须同许可」
> **自今日起不成立**。SDK 改以 **MIT** 发布（宽松许可，不继承内核的 AGPL），故外部插件**不是**
> 本项目的衍生作品，作者可自行选择许可（含闭源、商业、私有），亦不受 §13 网络条款约束。
> 原文保留以呈现当时的口径。
>
> 另有**两项性能断言的量纲需要更正**（2026-09-20 实测）：
> 「崩溃到恢复 <1s」不成立 —— 崩溃后是**线性退避重启**，即 1s / 2s / 3s（`procRestartBackoff=1s × 第 n 次`），
> 首次重启就要等 1s。且 5 分钟窗口内第 **4** 次崩溃即停止自动重启待人工介入（`procMaxRestarts=3`，判定为 `n > 3`）。
> 该断言写下时（v1.0.0）退避值已是 1s，故从未成立。
> 「RPC 往返 p50 24.1µs」与当前实测同量级但不吻合：本机 `BenchmarkToolInvoke` 实测
> inline/small **30.4µs**、frame/small 51.5µs、inline/large 767µs、frame/large 398µs。
> 保留原文不修改，以免伪造历史。

**v1.1.1** — 多模态贯通**插件边界**。v1.1.0 让记忆系统支持了二进制多媒体节点，但那条链路只对内核自己开放；本版打通到插件与模型。公开 SDK 新增媒体字段与三个媒体注入接口（配套 [SDK v1.1.0](https://github.com/JianFeeeee/hologramagentsdk/tree/v1.1.0)，整条 1.1.x 线共用），内核实现对应四个 RPC。桥接层此前在**静默裁字段**：插件交进来的 `Confidence`/类型/`SentenceText` 全被丢弃、`Doc` 只留三个字段、`Remove` 不解引用（媒体永久算「被引用」，GC 收不掉）。`processTextInput`/`processMediaInput` 归一成一条 `processInput`，媒体路径由此获得它一直缺的去重、`no_memory`、通道 `Cleaner`、中断语义、`EventRawInput`。修掉三处真实缺陷：**用户发的图从来没出现在 WebUI 聊天记录里**（媒体路径发布 map 而订阅方断言 string）、**`memory_commit` 的 `sentence_text` 从未暴露给模型**（注：该工具现名 `stablemem_commit`，本条保留 1.1.1 发布时的原名）（而它是媒体绑定链的必经环节）、**`PluginSDK` 两处并发竞态**（`-race` 实测 11 处，插件重载瞬间偶发 nil 解引用崩溃）。

**v1.1.0** — 记忆系统支持**二进制多媒体节点**。内容寻址媒体存储（CAS + SQLite 元数据 + 磁盘 blob，`Get` always 重校 digest），贯通 L0（上下文事件）/L2（文档）/L3（图谱句子）三层，引用计数式 GC（有引用者绝不删）。视觉模型生成的描述文本是持久语义记忆，blob 只是可被容量 GC 淘汰的缓存。

**v1.0.0** — 外部插件从 C ABI 动态库迁移到**子进程 + 共享内存**。首个不再加载 `.so`/`.dll` 的版本，与 0.9.x 不兼容（存量插件须用新版工具链重编；该工具链当时名为 `plugindev`，**现名 `hmapdev`**）。外部插件需重编为 `plugin.bin`，**业务代码零改动**）。消除 6 类此前在生产造成故障的缺陷：热重载失效（`DF_1_NODELETE` 让 `dlclose` 成 no-op）、崩溃隔离缺失（插件 panic 带崩 homed）、stage lost update（副本模型丢失 35.8~36.8%）、cgo 超时不可中断（线程线性泄漏）、`output_send` 假成功（模型收到「已发送」而消息未送达）、Windows 能力断层（只见 3 个 stage 字段且无法写回）。三面通信：stdio JSON-RPC（控制）+ 共享内存段（数据）+ 事件环（通知）；权限梯度显式化为三道闸。RPC 往返 p50 24.1µs，崩溃到恢复 <1s。

**v0.9.0** — C ABI v2：外部插件 Stage 回调支持写回（`invoke_stage` 增加 result 输出，插件可在 OnInput/AfterToolcall/PostAction 修改 RawMessage/LLMText/ToolResults 等并同步回内核），ABI 版本随内核 minor 对齐（v0.9.x → ABIVersion=2，`version_min=1` 向后兼容旧插件）。同步修复工具循环 zen 兼容补位误伤首轮 system 上下文的问题。配套 SDK 提供增强版 sanitizer 示例（坏 UTF-8/U+FFFD/ANSI 转义全链路清洗）。**该 ABI 已随 v1.0.0 退场。**

**v0.8.0** — 核心可用，插件系统增强。内置 20+ 插件，外部插件开发见 [hologramagentsdk](https://github.com/JianFeeeee/hologramagentsdk) 仓库。新增输入通道 `NoMemory`/`Cleaner`、`ChannelDef`、插件禁用/启用系统（CLI + WebUI），`plugindev` 工具链完成 C ABI `ChannelDef` 传递。
