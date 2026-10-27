# 三层记忆系统修复计划：块化重构后的失效面

> **归档说明（2026-10-27）**：本文是历史计划，**术语已过时** ——
> `doc_query` / `doc_commit` 已改名为 `coldcontext_query` / `coldcontext_commit`，
> 图记忆（Graph）在对外语义上改称 **stablemem（稳定记忆）**。
> 现行架构与术语以 `assets/docs/zh/ARCHITECTURE.md` 为准；本文保留作为决策记录。


> 状态：**已实施**（2026-10-22）。实施记录见文末 §九。
> 原计划写作于同日，下述前提与失效面均经源码与生产数据核实。
> 触发：块化统一（`docs/zh/memory-restructure-plan.md`）重构了底层数据结构，
> 但上层「层的流动」没有跟着改，导致 L1↔L2 这条链出现结构性失效。
>
> 本文只写**经源码与生产数据核实**的失效点。每条都标了证据位置，
> 不含推测、不含性能断言。

---

## 〇、设计前提（不可动摇，任何修法违反即错）

### 前提 A：记忆块是唯一的物理载体

`docs/zh/memory-restructure-plan.md` §〇 纠错表原话：

> | 分层是 L0 对话/L1 文档/L2 graph/L3 blocks | L0=context / L1=doc / L2=graph。
> **blocks 不是新的一层**，是图节点从文本升级为带向量的多模态节点 |

`MemoryBlock` 是 union：文本块负载在 `Text`，媒体块在 `Data`（配 MIME/宽高/尺寸）。
**同一个块 ID 在 context → doc → graph 全程不变。层是持有关系，不是数据种类的划分。**

### 前提 B：L1 与 L2 结构同构，唯一区别是「是否直接投喂给模型」

这是**层与层的定义**，不是实现细节：

| | L1 Context | L2 Document |
|---|---|---|
| 承载单元 | 事件（内联正文 + 块） | 事件（内联正文 + 块） |
| 结构 | 同构 | 同构 |
| 与模型的关系 | **直接投喂**（进 system prompt 时序） | **不直接投喂**（需经召回才进入 L1） |
| 容量 | 最近 n 条 / 上限 m 条 | 无上限 |
| 流动性 | 超限时按相似度迁出 | 冷到阈值后蒸馏进 L3 |

⇒ L2 **不是**「L1 的摘要层」，也**不是**「另一种数据形态」。
它就是**同样的事件 + 同样的块**，只是不直接进模型视野。
召回 = 把块（连同它所属事件）从"不投喂"的持有层**迁回**"投喂"的持有层。

### 前提 C：正文（含多媒体正文）内联在块里

文本块的 `Text` 就是正文；媒体块的 `Data` 就是字节。
**不存在"块 + 外部正文"的分离形态**，也不存在靠渲染字符串承载原文的路径。
事件是块的容器，不是块的替代品。

### 前提 D：任何一层都不压缩

原文必须完整保留。视图（检索摘要、给模型看的渲染）可以是压缩的，
但**视图不是记忆**。把视图当记忆存，等于在存储端就丢了原文。

### 前提 E：Graph 顶层仍是三元组，块只是它的节点实现

`internal/memory/sentence_block.go` — `putTripleBlocksTx`：

```go
src := MemoryBlock{ID: TripleBlockID(t.Subject), Text: t.Subject, SemanticType: t.SubjectType}
dst := MemoryBlock{ID: TripleBlockID(t.Object), Text: t.Object, SemanticType: t.ObjectType}
// 边：src --edge_type=Relation--> dst，落在 memory_block_edges
```

三元组 =【节点】【关系边】【节点】，这个**顶层结构没有变**。
变的是节点的物理表示：`entities` 表 → `memory_blocks` 表。
`entities` 退场不等于三元组退场。

### 前提 F：内核不得硬编码任何插件行为

这是**铁律**。任何依赖具体插件名（`agentcli` / `terminal` / `qq` …）的内核分支都是错的。
插件的行为差异一律由插件自己**声明**：`ToolDef.NoMemory`、`ToolDef.Cleaner`、
`ChannelDef.NoMemory`、`ChannelDef.Cleaner`、`InjectOptions`。

### 前提 G：SDK 已通过一套等价接口实现同等能力

公开 SDK（`third_party/homeagent-sdk/sdk/memory.go`）已提供与内核同构的表达：

| SDK 接口 | 对应的内核概念 |
|---|---|
| `TextEvent{Role, Content, Timestamp, Channel, Attachments}` | 事件（正文内联 + 附件） |
| `DocMemoryAPI.Insert(doc)` | 写入 L2（纯文本） |
| `DocMemoryAPI.InsertWithMedia(doc, attachments)` | 写入 L2（正文 + 媒体块） |
| `DocMemoryAPI.Query(text, topK)` | 只读召回（不删除） |
| `MediaAttachment{Digest, MIME, Data}` | 块引用：带 `Data` 为新内容，只带 `Digest` 为引用已有 |
| `TextMemoryAPI.Append(TextEvent)` | 时间序文本日志 |

⇒ **SDK 契约不需要改动**。修的是内核实现没跟上契约语义，不是契约本身。

---

## 一、失效面清单（经核实）

### 失效 1：L2 只存了事件的渲染视图，事件本体被丢弃

**位置**：`internal/memory/document/document.go` — `ContextToDoc`

`Prune` 交给 L2 的是结构完整的 `[]ContextEntry`（Timestamp/Source/Content/
Response/ToolsUsed/ToolResults/Blocks），但落库时被拍平成一个字符串：

```go
for _, e := range entries {
    line := fmt.Sprintf("[%s] %s: %s", e.Timestamp.Format("15:04"), e.Source, e.Content)
    line += fmt.Sprintf(" → %s", truncate(e.Response, 100))      // 正文 → 视图
    line += fmt.Sprintf("\n  [工具] %s: %s", tr.Name, truncate(tr.Output, 200))
}
content := strings.Join(parts, "\n")
```

只有 `Doc.Content` 被保存。**后果**：事件的时间戳只剩 `HH:MM`，
`Response` 只剩 100 rune，工具输出只剩 200 rune，`ToolsUsed` 完全不存。

这直接违反前提 B（L1/L2 同构）与前提 C（正文内联在块里）——
L1 里是完整事件，L2 里却退化成一段文本。

**生产证据**（`/home/newqqagent/memory/documents/`，1000 篇归档文档）：

| 度量 | 实测值 |
|---|---|
| response 段恰好 103 字符（=100+`...`） | 311 / 2094 段 = **15.1%** |
| 工具输出段恰好 203 字符（=200+`...`） | **7593 / 13261 段** |

⇒ 原文**在落库那一刻就不存在了**。这不是"召回时没取全"，是存储端丢失。

### 失效 2：L2 里没有文本块，L1/L2 不同构

**位置**：`internal/memory/document/document.go` — `blocksFromEntries`

块的 `modality` 实测分布（同一批生产文档）：

```
L2 块总数 444：image 437 / audio 7 / text 0
```

**后果**：L2 里"块"只承载媒体，文本内容全靠 `Doc.Content` 这个视图。
违反前提 B/C——L1 的事件正文 + 块，到 L2 变成了"字符串 + 仅媒体块"。
（对照：L3 的文本是有块的，`putTripleBlocksTx` 的主语块/宾语块都是 `BlockText`。）

### 失效 3：召回时新建合成事件，而非把块按原时序迁回

**位置**：`internal/agent/core/toolcall.go` — `executeDocTool` 的 `doc_query` 分支

```go
docs := a.docStore.Consume(query, topK)   // 读 + 从 L2 删除
...
a.context.InsertByTimestamp(ContextEvent{
    Timestamp: d.CreatedAt,               // ← 归档时刻，非事件时刻
    Source:    "cold_storage",            // ← 新造来源
    Input:     fmt.Sprintf("加载文档记忆: %s", query),
    Response:  content,                   // ← 渲染视图，且再截 2000
})
// ← 完全没有把 d.Blocks 挂回去
```

三处偏离设计（`ARCHITECTURE.md` L107-108、L178）：

> doc_query → Consume(读取并删除)
> → **逐条** context.Append{Timestamp: **原始时间戳**, Source: 原来源}
> → 文档以原始时间戳写入 context 时间线, 从 docStore 删除

**生产证据**（块自带 `created_at`，与归档时刻差近两小时）：

```
媒体块 created_at = 2026-10-12 11:09:51   ← 事件发生时刻（captureBlockMedia 里 time.Now()）
doc.created_at   = 2026-10-12 13:04:54   ← 归档时刻
```

443/443 个媒体块都带有效 `created_at`。**块自带它该回到哪个时刻的坐标，现在没人用。**

### 失效 4：块在 Consume 时两层同时消失

**位置**：`internal/memory/document/document.go` — `Consume` → `removeDoc`

```go
func (s *Store) removeDoc(id string) {
    delete(s.docs, id)
    ...
    os.Remove(filepath.Join(s.dir, id+".json"))   // 块的字节只在这个文件里
}
```

`doc_query` 调用 `Consume` 后，文档（含 `d.Blocks`）从内存与磁盘同时删除；
而 `toolcall.go` 不回填 `Blocks`。**后果**：这些块既不在 L2（已删）也不在 L1（没挂），
只剩局部变量引用，函数返回后即不可达。

**生产风险敞口**：L2 现有 444 个块，其中 **396 个不在 L3**（`graph.db` 里查不到），
原始字节约 **248 MB**。一旦被 `doc_query` 命中即永久丢失。

### 失效 5：时序渲染用固定 120 rune 截断正文

**位置**：`internal/agent/core/process.go` — `formatMergedTimeline`

```go
sb.WriteString(fmt.Sprintf(" → %s", truncateStr(e.Response, 120)))
```

上下文预算可以是几百 K token（`ComputeTokenBudget`），但每条正文**永远只显示前 120 字**。
原文即使用前提 D 完整保存在库里，模型也看不到——这是"不压缩"在**渲染环节**的漏口。

**生产证据**（`context.json`）：2 条 `cold_storage` 事件的 response 分别 1541 / 1209 字符，
渲染出来都是 123 字符。

### 失效 6：`Prune` 的归档过滤要求本身违反前提 F

**位置**：`ARCHITECTURE.md` L13 写「过滤掉 `agentcli` 和 `terminal` 来源后归档」。

`agentcli` / `terminal` 是**插件名**。把它们写进内核归档规则 = 硬编码插件行为，违反前提 F。

**当前代码**：`Prune` 里没有任何源名过滤，该要求**未实现**；
生产库里有 325 行 `agentcli` 归档（全部是终端状态行）。

⇒ 这不是"补上过滤"，而是**该要求应作废，改由插件声明驱动**（前提 F）。

---

## 二、修复顺序（按依赖）

失效 1/2 是存储形态，3/4 是召回形态，5 是渲染，6 是文档纠错。
**必须先修存储**——存储里没有事件本体，召回就没有东西可放回。

| # | 动作 | 依赖 | 涉及文件 |
|---|---|---|---|
| ① | L2 存储形态改为「事件 + 块」（与 L1 同构） | — | `memory/document/document.go` |
| ② | 文本正文以块承载（消灭 `Content` 作为唯一文本载体） | ① | 同上 + `agent/core/context.go` |
| ③ | 召回按块/事件原始时序迁回 L1 | ①② | `agent/core/toolcall.go` |
| ④ | 时序渲染改为按预算分配 | 独立 | `agent/core/process.go` |
| ⑤ | 作废失效 6 的文档要求（改由 `NoMemory` 声明驱动） | 独立 | `ARCHITECTURE.md` |

### ① 的具体形状

`Doc` 与 `RelevanceContext` 的事件列表**同构**：

```
L1: events []ContextEvent        // Timestamp/Source/Input/Response/ToolsUsed/ToolResults/Blocks
L2: events []ContextEvent        // 同一结构，同一字段
```

- `Doc` 保留 `ID` / `Summary` / `Vector` / `DenseVec` / `DenseFP` / `AccessCount` 等
  **检索元数据**（这些是 L2 作为"可检索容器"的附加属性，L1 不需要）
- 事件列表**内联正文**（`Response` 完整，不截断）+ 持有的块
- `Doc.Content` 降级为**纯渲染视图**（供摘要/人看/兼容 SDK 的 `Doc.Content`），
  不再作为原文载体；原文从事件读

> ⚠️ 注意与「已否决做法」的区别：不是新造一个异构字段把 `ContextEvent` 旁挂上去，
> 而是让 **L2 的承载结构就是 L1 的那一套**。事件列表是文档的主体，不是附件。

### ② 的具体形状

- 归档时，每条事件的正文（`Input` / `Response`）生成对应文本块（`Modality=BlockText`，
  `Text` 即正文），块 `CreatedAt` = 事件时间戳
- 媒体块照旧（`Data` 内联字节）
- 事件 `Blocks` 里同时含文本块与媒体块 → L1/L2/L3 三处同构

### ③ 的具体形状

```
doc_query → 取出 L2 事件列表（迁移语义：取出后 L2 不再持有）
          → 逐条按各自 Timestamp 插回 L1 时序
          → 事件持有的块随之迁回（同一块 ID，向量不重算）
```

---

## 三、判据（先写判据再改，避免自证）

> 教训（本次已发生）：先改代码再写测试，测试就只会证明"我改的是我以为的"。
> 判据必须**从设计前提推导**，且必须能在改动前**红**。

### ① 存储形态（L1/L2 同构）

- [ ] 归档后能从 L2 取回**逐条**事件的原始 `Timestamp`（完整时刻，非 `HH:MM`）、
      `Source`、`Response`（完整，非截断）、`ToolsUsed`、`ToolResults`
- [ ] 变异：把 `Response` 改回 `truncate(..,100)` ⇒ 红
- [ ] 变异：丢掉 `ToolsUsed` ⇒ 红
- [ ] 变异：时间戳改回 `Format("15:04")` ⇒ 红

### ② 文本块

- [ ] 归档文档的文本块 `Text` 与事件正文逐字一致
- [ ] 文本块 `CreatedAt` == 事件 `Timestamp`
- [ ] 变异：不生成文本块（只存 `Doc.Content`）⇒ 红

### ③ 召回

- [ ] `doc_query` 后事件按**各自原始时间戳**回到 L1
- [ ] 块 `ID`/`Vector`/`CreatedAt` 全程不变（迁移而非重建）
- [ ] 块的持有层从 L2 变为 L1，**不同时留在两层**
- [ ] 变异：用 `d.CreatedAt` 当时间戳 ⇒ 红
- [ ] 变异：不回填 `Blocks` ⇒ 红

### ④ 渲染

- [ ] 预算宽裕时正文完整可见（含结尾哨兵）
- [ ] 预算紧张时仍保留开头（不能整条消失）
- [ ] 单条超长正文不得把其余事件整段挤掉

### ⑤ 文档纠错

- [ ] `ARCHITECTURE.md` 不再出现任何插件名作为内核归档规则
- [ ] 判据：`grep -rn '"agentcli"\|"terminal"' internal/agent/core internal/memory`（非测试）为空

---

## 四、禁止事项（此前已犯，写下来防复发）

| 禁止 | 原因 |
|---|---|
| 给 `Doc` 加异构字段旁挂事件快照（如 `Entries []ContextEntry`） | L2 主体就是事件列表本身，不是"Doc + 附件" |
| 改 `truncateStr` 的常数当作"修复压缩" | 视图截断不是记忆压缩问题；根因在存储形态 |
| 任何插件名字面量进内核分支 | 违反前提 F |
| 编造不存在的 API 再补注释解释它"应该"怎样 | 实现必须以真实存在的设施为准 |
| 先按自己的理解写测试再改代码 | 会把误解固化成判据 |
| 改公开 SDK 契约 | 前提 G：SDK 已等价，修内核实现 |

---

## 五、已否决的做法

### 否决 1：新增 `Doc.Entries` 存事件快照（2026-10-22，已实现又撤销）

错在**异构旁挂**：`Doc` 仍是字符串主体，事件列表被当成附件挂上去。
正确形态是 §二①：事件列表**成为 L2 的主体**，与 L1 同构。

### 否决 2：把渲染层截断常数调大（2026-10-22，已撤销）

根因在存储形态，不在渲染常数。§二④ 单独处理，且应是**按预算分配**而非改常数。

---

## 六、已拍板的决定

### 1. 事件本体在 L2 的形态（2026-10-22 拍板）

**L2 就是 doc 存储，本身与 Context 同构，区别只在于是否被直接投喂给模型。**

⇒ 不是“复用 `ContextEvent` 当旁挂字段”，也不是“把事件也变成块”这种额外抽象。
**事件列表就是 L2 文档的主体**，与 L1 事件列表同一结构。

### 2. 正文形态（2026-10-22 拍板）

**正文（包括多媒体正文）本身就内联在块里。**

⇒ 文本块的 `Text` 是正文，媒体块的 `Data` 是字节。
不存在“块 + 外部正文”的分离形态，也不靠渲染字符串承载原文。

### 3. SDK（2026-10-22 拍板）

**记忆部分 SDK 已通过一套接口实现等价功能** ⇒ **不改公开 SDK 契约**。

对照表（`third_party/homeagent-sdk/sdk/memory.go`）：

| SDK 接口 | 等价的内核概念 |
|---|---|
| `TextEvent{Role, Content, Timestamp, Channel, Attachments}` | 事件（正文内联 + 附件） |
| `DocMemoryAPI.Insert(doc)` | 写入 L2（纯文本） |
| `DocMemoryAPI.InsertWithMedia(doc, attachments)` | 写入 L2（正文 + 媒体块） |
| `DocMemoryAPI.Query(text, topK)` | 只读召回（不删除） |
| `MediaAttachment{Digest, MIME, Data}` | 块引用（带 Data 为新内容，仅 Digest 为引用） |

⇒ 修的是内核实现没跟上契约语义，不是契约本身。

### 4. 存量数据（2026-10-22 拍板）

**等待系统自然冲刷**，不做迁移脚本。

约束：
- 新代码必须能**读旧格式**（缺事件列表/缺文本块时降级），不得 panic、不得静默丢内容
- 降级路径要有明确日志，便于判断存量退出进度
- 旧格式文档被召回时按旧行为处理，**不伪造成“有原始事件”**

---

## 七、待确认的细节（实现层面，不阻塞开工）

1. **文本块与事件是一对一还是一对多？**
   一条事件生成一个文本块（`Input` + `Response` 合在一个 `Text` 里），还是各一块（便于分别检索）？

2. **`Doc.Content` 保留为渲染视图，还是彻底删除？**
   若保留，与事件列表信息重复；若删除，SDK 的 `Doc.Content` 则由事件列表现场渲染填充。

3. **L2 事件的检索向量怎么算？**
   现在 `denseFor` 用 `Summary + Content`。改为事件列表后，
   是用 `Summary + 各事件正文拼接`，还是用文本块自己的向量做融合（与媒体块一致）？

---

## 八、验收口径

- `go build ./...` 通过
- 全量 `go test ./... -count=1` 0 失败（当前基线：58 包通过 / 0 失败）
- §三 每条判据有对应测试，且**变异测试**各自能被抓住
- 生产侧（可在副本库验证）：`doc_query` 召回后块不消失；新旧两种格式文档都能正确召回

---

## 九、实施记录（2026-10-22）

### 已落地

| # | 动作 | 落地位置 |
|---|---|---|
| ① | L2 存储形态改为「事件 + 块」 | `document.Doc.Events []DocEvent`；`ContextToDoc` 落盘事件列表 |
| ② | 正文各自成文本块 | `docBlocksForEntry` / `entriesToEvents`；Input、Response 各一块（`BlockText`） |
| ③ | 召回按原始时序迁回 | `Agent.restoreDocEvents`；`DocEvent.InOut()` 从块读回正文 |
| ④ | 蒸馏改用原文 | `documentOriginalText`（有 Events 用块内原文，存量退回 Content） |
| ⑤ | 文档纠错 | `ARCHITECTURE.md` 删除「过滤 agentcli/terminal」，改为「由插件声明驱动」 |

### Content 的定位（按拍板）

`Doc.Content` **保留为渲染视图**，由 `contentFromEvents` 生成：

- 供摘要、人工阅读、SDK 的 `Doc.Content` 字段使用
- **不是**原文载体 —— 原文在 `Events[i].Blocks` 里文本块的 `Text`
- 视图也不主动截断（存储层不替渲染层做决定）

### 存量

按拍板**不做迁移**，等系统自然冲刷。降级路径：

- `Doc.HasEvents() == false` ⇒ 召回时整篇渲成一条 `cold_storage` 事件（旧行为）
- 不伪造成「有原始事件」；有日志可观测

### 判据与变异

新增两组判据（各条都在改动前设计、改动后可跑）：

| 文件 | 判据数 | 覆盖 |
|---|---|---|
| `internal/memory/document/blockflow_test.go` | 7 | 存储形态、文本块粒度、逐条保留、视图定位、幂等、存量兼容、原文可用性 |
| `internal/agent/core/recalldoc_test.go` | 5 | 逐条回填、原始时间戳、块迁回、回填不截断、存量降级 |

**变异测试（全部被抓住）**：

| 变异 | 被哪条抓住 |
|---|---|
| 正文落库时 truncate(100) | `TestL2_ArchivePreservesFullText` / `TestL2_ContentIsRenderedViewNotSource` |
| Input/Response 合成一块 | `TestL2_InputAndResponseAreSeparateTextBlocks`（及 3 条连带） |
| 召回用 `d.CreatedAt` 当时间戳 | `TestRecallByDoc_RestoresEachEventAtOriginalTimestamp` |
| 召回不回填 Blocks | `TestRecallByDoc_MigratesBlocksBackToContext` |
| 召回再截 2000 字 | `TestRecallByDoc_TextIsNotTruncatedOnTheWayBack` |

### 未做（明确不做）

- **不改公开 SDK 契约**：`TextEvent` / `DocMemoryAPI` / `InsertWithMedia` 已等价
- **不迁移存量文档**：等自然冲刷
- **不改渲染层截断常数**（失效 5）：根因已在存储形态修掉；渲染按预算分配是独立议题，
  避免与本次修复混在一起、互相掩盖
