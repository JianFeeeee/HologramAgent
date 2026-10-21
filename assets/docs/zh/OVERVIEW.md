[English](../en/OVERVIEW.md) | **中文**

# HologramAgent — 项目概览

## 这是什么

HologramAgent 是一个持续运行的个人智能 Agent 框架。

核心架构：一个长时间运行的内核进程（`homed`），通过插件系统接入各种 IO 通道（QQ、Web、命令行等）。内核负责 LLM 调用编排、记忆管理、知识检索；插件负责所有外部 IO——收发消息、执行文件操作、搜索网络等。

### 设计要点

**核心域与应用域分离** — 内核（核心域）不执行任何 IO 操作，所有 IO 能力归属插件（应用域）。边界通过 PluginSDK 明确定义：
- 插件向内核注册工具（Tool），供 LLM 调用
- 插件挂入处理管道（Stage），在各阶段拦截/改写消息流
- 插件订阅/发布事件（Event），松耦合通信
- 插件通过 IO API 排队或打断投递输入

这一划分的意义在于职责边界清晰：内核专注于编排与记忆管理，插件负责具体 IO 实现，二者互不耦合。

**业务语义留在插件层** — 内核不定义业务对象：终端、命令历史、设备状态、媒体库
都属于插件域。以终端为例，PTY、会话生命周期与输出缓冲全归 `agentcli`；WebUI 与
CLI 需要终端面板时，**订阅事件总线**（`EventTerminalOutput` + `EventToolCall`）
自己攒视图，而不是去读内核里的某个状态组件。内核只提供事件总线与插件注册面——
新增一类能力不需要改内核，也不会在核心里堆出第二套业务真相。

**三层记忆架构** — 通过分级存储策略管理 Agent 长周期运行中的信息留存：
- **Context 层**：内存中预训练词嵌入评分的事件窗口（StaticEmbedder 词向量 → CosineSimilarity，TF-IDF 回退），实时维护最近上下文，低相关性事件自动下沉到下一层
- **Document 层**：JSON 文件 + TF-IDF 向量索引的临时记忆，支持显式提交和隐式归档，冷数据蒸馏到 Graph
- **Graph 层**：SQLite 图数据库，持久化实体（entities）和关系（relations），BFS 遍历召回，蒸馏管道从对话中提取三元组

三层递进：上下文 → 冷归档 → 长期图记忆，构成从短期到持久的信息衰减与整合管道。

**媒体记忆（v1.2.0 起：一等记忆块）** — 图片/音频不是附属物，而是记忆的一等节点：
- **内容寻址存储（CAS）**：digest 寻址，元数据在 SQLite、blob 在磁盘（两级分桶），相同字节只存一份，
  每次 `Get` 重校 digest（磁盘损坏静默返回脏数据比报错更危险）
- **媒体块直接参与向量检索**：块携带**自己的多模态向量与指纹**，正文里不再有任何媒体标记
- **无独立 GC、无引用计数、无 keep-set**：记忆块遵循单层不变量——Context → Document → Graph
  是块的**迁移**，不是复制、也不靠引用保活；**删除块即删内容**
- **v1.1.1 起贯通插件边界**：插件可通过 `InsertWithMedia` / `InjectInputMedia` 读写媒体，
  模型可用 `memory_commit` / `doc_commit` 的 `media_digests` 参数关联媒体
- **旧机制已整体拆除**：此前把视觉模型生成的描述写进正文、以 `[<mime> <短digest>] <描述>` 标记
  参与检索的**描述式索引**，以及 `media_refs` 引用计数，均已在 v1.2.0 删除——描述是模型生成的
  二手信息，检索「别人转述的图片」不如检索图片自己的向量

## 它实际做了什么

代码位于项目仓库根目录，Go 语言实现。

**内核** (`internal/agent/core/`)：
- `eventloop.go` — 消息循环（`eventLoop`），从 IO 层排队接收输入
- `process.go` / `stages.go` — 处理管道：记忆召回 → 人格注入 → LLM 调用 → 工具执行 → 输出发送，7 阶段钩子
- `toolcall.go` — 工具调度与执行
- `context.go` — 上下文管理（预训练词嵌入评分 StaticEmbedder → CosineSimilarity，TF-IDF 回退），自动剪枝低相关性事件
- LLM 调用通过 Provider 接口抽象，多源自动降级

**记忆系统** (`internal/memory/`)：
- **GraphDB** (`graph.go`) — SQLite，entities + relations 表，BFS 遍历
- **Document Store** (`document/document.go`) — 临时记忆，JSON 文件 + TF-IDF 向量索引，消费即删
- **Text Memory** (`text/text.go`) — 原始对话日志，JSONL 文件轮转
- **Social Store** (`social/social.go`) — 人格特质 + 关系网，包装 GraphDB
- **Memory Indexer** (`indexer.go`) — 自动将 GraphDB 实体向量化，用户输入时召回注入 system prompt

**知识库** (`internal/knowledge/knowledge.go`)：
- 文件系统目录 `knowledge/<name>/content.md`
- TF-IDF 向量搜索，独立于记忆系统的索引实例
- LLM 通过 `knowledge_search` / `knowledge_create` / `knowledge_list` 三个工具操作

**插件系统** (`internal/plugin/`)：
- 内置插件：Go `init()` 自注册，编译进内核
- 外部插件（v1.0.0 起）：编译为普通 Go 二进制 `plugin.bin`，内核 spawn 为**独立子进程**，
  经 stdio JSON-RPC（控制面）+ 共享内存段（数据面）+ 事件环（通知面）通信；也支持 Lua 脚本插件
  （C ABI 动态库通道 `-buildmode=c-shared` 已在 v1.0.0 整体删除）
- PluginSDK (`internal/sdk/`) 定义四通道：RegisterTool / RegisterStage / Subscribe / RegisterOutputChannel
- 阶段钩子 7 个：on_input → pre_action → post_action → before_toolcall → after_toolcall → before_output → after_output

**LLM Provider** (`internal/agent/api/provider.go`)：
- Provider 接口：Name / Chat / ChatStream / MaxContextTokens
- 唯一实现 `LuaAdaptedProvider`：所有源都经 Lua 适配脚本做请求/响应变换；
  适配器脚本位于 `internal/lua/adapters/`，每类协议一个 `.lua` 脚本
- 内置 10 个适配器：deepseek / openai / anthropic / gemini / mistral / groq / github / kimicode / ollama / server
- 源（`core.llm.sources.<name>.*`）由部署时配置，数量不固定，不随内置适配器数绑定

**WebUI** (`internal/plugins/webui/`)：
- 嵌入式 SPA 仪表盘（`dashboard.html` 通过 `//go:embed` 打包）
- REST API：状态查询、配置管理、记忆操作、知识库管理、插件管理
- 兼容 OpenAI API 格式的 `/v1/chat/completions` 端点
- SSE 事件流 `/api/v1/chat/events`

**ClawHub 适配器** (`internal/plugins/clawhubadapter/`)：
- 统一加载 OC 插件（Node.js）、Python sidecar、JS sidecar、SKILL 四种插件类型
- RegistryDispatcher 模式：Tool/Provider/Channel/Stage 注册通知分发
- ClawHub 市场搜索与安装：`clawhubadapter_search` / `clawhubadapter_npm_install`
- 9 种 Provider 类型映射为 LLM 可用工具（图片生成、搜索、语音等）
- OC 通道自动注册为 IO 设备，支持文本/文件/图片/音频能力标志


## 项目状态

核心功能已可运行。插件系统和 SDK 已就绪，可独立开发外部插件。

- 内置插件（13 个）：webui / cli / agentcli / cfgmgr / healthcheck / kbtree / multimodal / pluginmgr / clawhubadapter / remotedevice / skillmgr / seemedia / seq
- 判定标准：**只有必须使用内部 SDK 能力（公开 SDK 没有的接口面）的插件才内置**，其余一律外置为 .hmap 插件（详见 ARCHITECTURE「内置插件的判定标准」）
- 外部插件示例（[hologramagentsdk](https://github.com/JianFeeeee/hologramagentsdk) 仓库 `example/`，含 Go 和 Lua 两种类型）：qq / files / a2a / ai_image / bili / browser / calendar / editdoc / memo / music / ocr / rss / sanitizer / weather / luademo
- 打包分发：`.hmap` 插件包格式，通过 WebUI 安装
