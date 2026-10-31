# 旧表清理可行性评估（2026-10-04）

> **状态：计划（收尾中）。** 遗留表退役的执行记录；剩余读方见文内 §八 / §十二。

> 结论：**现在不能清理**。55 处生产调用点仍在读写 `entities` / `relations` / `sentences`。
> 这份评估回答「能不能删」以及「删之前要先做什么」。

## 一、数据侧已就绪

清理的**数据前置**已全部通过（`TestRetire_清理前置`，生产快照实测）：

| 判据 | 结果 |
|---|---|
| ① 每个 entity 都有对应的块 | ✅ 1294 → 1294，缺块 0 个 |
| ② 每条 relation 都有对应的边 | ✅ 980（去重 959）→ 959 |
| ③ 每条 sentence 都有原句块 | ✅ 66 → 66（`55db22c` 修复后） |
| ④ 悬空的 sentence→块 边 | ✅ 0 |
| ⑤ 端点不存在的块边 | ✅ 0 |

## 二、代码侧远未就绪

### 调用点分布

```
internal/memory/social/social.go            7 处
internal/memory/graph.go                    6 处（Commit/Recall 实现）
internal/memory/scene.go                    4 处
internal/memory/light_memory.go             4 处
internal/plugins/webui/handler_memory.go    2 处
internal/plugins/healthcheck/plugin.go       2 处
internal/plugin/proc/corehandler_memory.go  2 处
internal/plugin/lua_plugin.go                2 处
internal/sdk/memory_impl.go                 2 处（Recall + CommitWithMedia）
────────────────────────────────────────
合计 55 处
```

### 三张表各自还被谁依赖

**`entities`**

```go
graph.go:505  Commit 里 SELECT id FROM entities WHERE name = ?   （写入路径）
graph.go:681  hotspots ORDER BY mention_count DESC
graph.go:756  SELECT ... FROM entities WHERE LOWER(name) LIKE ?  （Recall）
```

**`relations`**

```go
graph.go:532  Commit 里查重（source+target+type+session）
graph.go:705  Recall 的关系遍历
graph.go:748  SELECT MAX(r.sentence_id) FROM relations r
graph.go:362  从 sentence_ref 回填 sentences
```

**`sentences`**

```go
graph.go:362  INSERT OR IGNORE INTO sentences ... FROM relations.sentence_ref
graph.go:522  CommitWithMedia 挂接媒体块的落点
（EnsureSentence 已删 —— 它是最后一个写入点，07b8c2a 之后归零）
```

## 三、为什么这三条链路必须一起换

它们不是三张独立的表，而是**一条写入链 + 一条读取链**：

```
写入  Commit ──> entities(name 唯一) + relations + sentences(挂媒体块)
读取  Recall ──> entities(LIKE) + relations(遍历) + sentence_id
```

**只删表不改代码 ⇒ 写入报 "no such table"，读取返回空。**
而 `Commit` 是**在线主路径**（每轮对话都可能调用），
不是可以延后的离线工具。

## 四、清理前必须完成的工作

按依赖顺序：

| # | 工作 | 说明 |
|---|---|---|
| 1 | `Commit` 改写为写块 | 不能再 `INSERT entities`；三元组应直接落 `memory_blocks` + 块边 |
| 2 | `Recall` 改写为读块 | 现在读 entities/relations；应读块 + 块边 |
| 3 | 媒体挂接点迁移 | `CommitWithMedia` 的 `sentence_id` 要换成原句块 ID |
| 4 | social / scene / light_memory 适配 | 它们的调用要改到新接口 |
| 5 | WebUI / healthcheck / proc / lua / SDK 适配 | 对外契约不变，实现换掉 |
| 6 | **回滚演练** | 在快照上跑完 1-5，验证召回不退化 |

★ 第 6 步不是可选项：`Commit` 在线上，改错了**当场影响对话记忆**。

## 五、当前建议

**不要现在清理。** 理由不是"风险高"，而是：

1. 数据侧就绪 ≠ 代码侧就绪（当前 55 处调用点）
2. `Commit`/`Recall` 在**在线主路径**上，不是一次性迁移
3. 相比之下，**清理旧表的收益只是"少三张空表"**，
   而代价是重写一条在线链路

⇒ 真正该做的顺序是：**先让写入与读取都走块**（1-3），
  旧表自然失去写入方，最后再谈删除。

## 附：数据侧的两个真实缺口（已修）

- **66 条 sentence 没有原句块**（`55db22c` 修复）：
  迁移只从 entities 读，从不为 sentences 建载体 ⇒ 直接清理会丢原文
- **关系边 959 vs relations 980**：
  20 组 `(source,target,type)` 完全重复被边表去重（`dd2c996` 已显式报出 `DedupedEdges`）


## 六、⚠️ 一个操作错误：**`cp` 拿不到一致的快照**

验证 `55db22c`（sentences 原句块）时，我用
`cp ${HA_DATA}/memory/graph.db /var/tmp/ha-probe/fix.db` 取副本，
结果：

```
生产库     entities=1294  sentences=66
cp 出来的   entities=1293  sentences=67      ← 差 1，且方向相反
```

第一反应是「迁移改动了源表」，差点去查迁移的写入逻辑。
**实际是 `cp` 的问题** —— 生产库在 WAL 模式下，已提交但未
checkpoint 的数据还在 `-wal` 文件里，`cp` 只拿到主库文件 ⇒ 不一致快照。

用 `sqlite3 .backup` 重取后：

```
.backup 快照  entities=1294  sentences=66  blocks=98   ← 与生产库一致
```

★ 本文档与 `production-recall-validation.md` 里都写过
  「库在写，`cp` 拿不到一致快照，要用 `.backup`」，
  **我自己写下的规则自己违反了。**

⇒ 判据也该加一条：**快照来源必须可验证**。
  最省事的做法是取完快照立刻与源库比对关键计数，不一致就重取
  —— 差 1 就会暴露。


## 七、2026-10-04 进展：写入侧已完成

### ✅ Commit 块化（cdf0726）

`Commit` 现在在旧表写入之外**并行块化**：

    三元组  主语块 --关系--> 宾语块
    原句    原句块 blk_src_<hash>

四条写入路径（媒体桥 / memory_commit 工具 / 驻留子 / 记忆整理流水线）
全部经过它 ⇒ **旧表不再因块化而缺内容**。

### ✅ 边升格为独立单位（52e4596）

    去 UNIQUE(source_kind,source_id,target_kind,target_id,edge_type)
    + confidence / session_id / turn_id / status / merged_into

★ 那个 UNIQUE 才是「报告 980、实际 959」的根因 —— 边表从设计上
  存不下多条同类边。详见该提交说明。

### ✅ scene_refs 完整迁移（07b8c23）

    kind='entity'   450 条 → 'block'
    kind='relation' 268 条 → 'edge'
    生产快照实测：718 条迁移完成，悬空 0

### ✅ EnsureSentence 已删

`sentences` 表的**最后一个写入点**已移除并连同其专属测试一起删除。

## 八、剩余的读方（尚未切）

| 读方 | 调用处 | 状态 |
|---|---|---|
| `scene.go` | 取边、按名找邻居、悬空检查 | ✅ 已改块/边（13c3292） |
| `social.go` | 3 处 Recall（要「按名取实体 + 关系遍历」） | 需要块侧的等价接口 |
| `indexer.go` | 2 处（全量实体名 → 建向量索引） | 需要 `AllBlockNames` |
| `distill.go` | 2 处（全量 → 记忆整理） | 同上 |
| `light_memory.go` | 3 处（透传代理） | 随上面改动 |
| `toolcall.go` | 旧路兜底（块路优先，已是生产主路） | 保留兜底 |

### 需要的块侧能力（其中两项已就绪）

    按名取实体        ❌ 缺（一条 SQL 的事）
    按关系遍历        ✅ BFSBlocks（9df1efb）
    取边 + 两端节点   ✅ AllRelationEdges / RelationEdgesBetween
    全量块名          ❌ 缺（MemoryBlocks 已有，只需薄包装）

⇒ 「旧表退场」的准确说法：**写入侧已全部块化，剩余是读方切换。**


## 九、2026-10-04：读方切换全部完成

### 切了的（每项都有独立判据）

| 读方 | 提交 | 备注 |
|---|---|---|
| `scene.go` 读写两侧 | `13c3292` | 6 处查询 + 补 `contains` 结构边 |
| `Purge` | `258eadf` | 9 处调用方 |
| `PurgeNoise` | `258eadf` | |
| `RecallSorted`（两分支） | `258eadf` `5a6e4a9` | 302 行，`entityIDs` 换键类型 |
| `Introspect` | `258eadf` | |
| `MergeEntities` → `MergeBlocks` | 本轮 | 86 行 → 委托 |
| `social.go` 5 处 | 本轮 | 见下 |

### ★ 「55 处调用点」是虚高的

    indexer / social / distill / light_memory 全都不写裸 SQL，
    它们都走 db.Recall(...) ⇒ 真正的收敛点是 RecallSorted 一个函数。

⇒ **按处数估工作量是错的。** 逐个适配时才发现它们共用一条路。

### ★ 切换期最危险的状态是「混合态」

social 3 个测试当场变红，根因值得写下来：

    写侧（Purge）已切块、读侧（RecallSorted）还没切
      ⇒ 软删的边在**旧表**里仍是 active
      ⇒ 召回照样返回它

**混合态比全旧态更危险** —— 它看起来是「部分成功」，
而全旧态至少是一致地坏。读方必须一次切到位。

### MergeBlocks（块合并）

旧 `MergeEntities` 85 行全在旧表，改名只改 `entities.name`，
块还叫「张先生」⇒ 召回照样命中。

★★ **与旧实现的本质差异**：块 ID 是**内容派生**的
（`blk_ent_<hash(name)>`），「张先生」与「张三」是两个不同的块。
所以合并不是「改端点」，而是**让源块消失并把它的边改指向目标块**：

    ① 关系边重定向（source→target / target→target）
    ② 去自环：同端**同 edge_type** 才是重复
       ★ 只按 (source,target) 去重会把「喜欢咖啡」与「讨厌咖啡」误删一条
    ③ 删指向源块的结构边（否则端点悬空 → graphNodeExists 拒绝后续写入）
    ④ 删源块
    ⑤ scene_refs 把源的引用**换成目标**（不是删 ——
       「张先生那次值班」这个场景仍存在，只是人换了名字）

**源块不存在必须报错**，不能返回 `(0, nil)`：
它有两种原因（已合并 / 名字写错），返回同一个结果会让后者表现为成功。

### ★ social：ID 空间错配是最隐蔽的一类

`Entity.ID`（int64，召回内序号）与 `Relation.SourceID`（块 ID，字符串）
是**两个不同的 ID 空间**。旧代码 `r.SourceID == personID` 永不匹配。

★ 症状极具迷惑性：`GetTrait` 里 `if 匹配 {…} return r.SourceName`
   在「不匹配」时**无条件**执行，于是返回**人物自己的名字**当特质值
   （判据里是 `got "张三"` 而非空串）—— 看不出是 ID 错配，只像数据错了。

修法是**按名字匹配** —— 而且这本来就更对：
「张三的特质」与 ID 无关；按 ID 还有个隐患：
同一个人可能有两个块（历史数据里文本相同），按 ID 只认一个。

### ★ 块没有 type 概念

`Triple.SubjectType` 只写旧 `entities.type`，`memory_blocks` **无类型列**。

`ListPersons` 原来靠 `e.Type == "person"` 筛选，改成**从图结构推断**：

    人物 = 社交关系（非 trait）的任一端点 ∪ trait 关系的源

★ 这不是权宜之计 —— 它比 type 更可靠：type 是写的时候声明的
  （调用方可能不声明、可能声明错），而结构是数据本身的性质。
  旧实现里「`AddRelation` 加了人但没 `SubjectType`」就会漏掉那个人。

---

## 十、生产迁移已完成（2026-10-04 15:36）

### 执行

```
homed-graph-migrate -db ${HA_DATA}/memory/graph.db -apply \
  -embed-provider chineseclip -model-dir /var/tmp/ha-c/models/chinese-clip-vit-b16-onnx

耗时 161.8s，单事务，命令自动快照 graph.db.bak-20261004-153657
```

### 产出

| | 迁移前 | 迁移后 |
|---|---|---|
| 块 | 98 | **2752**（带向量 1391） |
| 块边 | 97 | **2371** |
| 关系边（confidence 非空） | — | **980** |
| scene_refs 旧 kind | 718 | **0** |
| scene_refs 新 kind | — | block 540 / edge 268 / document 2 |
| 悬空端点 | — | **0** |
| 中心向量 | 无 | 已重建（各向异性 detected，双边中心化启用） |

### 验证（七步全通过）

```
① 停机确认      inactive + disabled          ✓
② WAL checkpoint 0|0|0                       ✓
③ 一致快照      integrity ok，五表逐一核对    ✓
④ 报告核对      与副本完全一致，未写库        ✓
⑤ 执行          161.8s，exit=0               ✓
⑥ 结构验证      七项全部通过                  ✓
⑦ 召回探针      7/9                          ✓
```

★ **④ 这一步的价值**：副本上验证过还不够，
生产库的数字必须单独核对一次 —— 因为副本是快照，
而生产库在停机期间理论上不该有任何变化，但**"理论上"不是证据**。

★ ⑥ 的第 1 项（逐表核对旧表）证明了迁移确实没动源表：

```
entities 1294 / relations 980 / sentences 66  ← 与迁移前完全一致
```

### 召回前后对照

| 维度 | 迁移前 | 迁移后 |
|---|---|---|
| casual | 0/4 | **3/4** |
| confusable | 0/1 | **1/1** |
| abstention | 3/3（假象，见 abstention-criterion.md） | **3/3**（真实拒答） |
| coexist | 0/1 | 0/1（BFS 未进召回链） |
| **合计** | 3/9 | **7/9** |

### 回滚

```
主回滚点  /var/tmp/ha-prod-migrate/prod-20261004-153045.db（integrity ok）
迁移自带  ${HA_DATA}/memory/graph.db.bak-20261004-153657
代码回退  e3dea6d 及之前任一提交（全部已提交，未 push）
```

★ 回滚只需换代码：旧表未被改动，迁移前的读方仍能工作。

### ★ 过程中的两次自纠

**1. 迁移报告曾报「关系 0」**（`d4eda47` 已修）

Introspect 的口径在读侧切块后变成数块侧，而迁移报告要的是
「旧表还剩多少」；加上 SQL 语法错误被 `if err == nil` 吞掉 ——
一个语法错误伪装成「库里没有关系」。

★ **诊断误导比报错危险**：报错了有人查，0 不会。

**2. 首条执行命令因中文括号报 shell 语法错误，迁移未执行**

事后确认生产库块数仍是 98 才重试。那次失败无任何影响，
但它说明**执行清单里的命令必须先在目标 shell 上验证语法**。

### 剩余工作

| # | 项 | 说明 | 阻塞 |
|---|---|---|---|
| 1 | 停旧表双写 | `commit()` 里的 `entities`/`relations` INSERT | 无（读方已全切块） |
| 2 | BFS 进召回链 | `coexist` 维度 0/1：泛指提问召不回端口号 | 无 |
| 3 | 观察期后删旧表 | 三张表 + 约 55 处调用点 | 需第 1、2 项完成 + 观察 |
| 4 | 纯中文编造查询 | 契约外，需符号切分改进 | 无 |

★ 第 3 项**不建议现在就做**：删表不可逆，而第 1、2 项都还没做。


## 十一、停旧表双写已完成（2026-10-04 18:xx）

### 做了什么

`commit()` 不再写 `entities` / `relations` / `sentences`。
旧表**停止增长，冻结为历史**（生产 1294 / 980 / 66）。

读方此前已全部切块，所以这一步完全可逆：改回代码即可，旧表数据完好。

★ 判断依据是**行为**不是语句：
  判据 `Test停双写_旧表不再增长` 逐表比对 Commit 前后的行数。

### ★★ 它暴露了六个静默失效

全部是「报成功、实际没做」或「悄悄做错事」，**没有一处报错**。

| # | 函数 | 现象 |
|---|---|---|
| ① | `memory_commit` 工具层 | **每次都告诉模型「全部被拒，请检查实体名」** —— 而写入其实成功了。模型据此改实体名，把内容改坏 |
| ② | `syncIfStale` | 索引器**永远不建立索引**（旧表行数恒 0 ⇒ 连首次都判成「无需同步」） |
| ③ | `ExportTriples` | 驻留子 agent 的**回收失效**（合入 0 条） |
| ④ | `ClearSentenceID` | **一直在清理错误的行**（调用方传边 ID，函数改旧表另一行；而 UPDATE 影响 0 行也是「成功」） |
| ⑤ | `Archive` | 对块体系**完全无效** ⇒「按时间衰减记忆」静默失效 |
| ⑥ | `archiveColdDocs` | 判据 `ec==0 && rc==0` 恒真 ⇒ **L2 文档永不删除** |

★ ① 值得单说：**「报假失败」比「报假成功」危险**，
  前者会诱发模型的破坏性动作（改不该改的内容）。

★ ⑥ 讽刺：它自己的注释写着
  「实测 456 字图片描述得到 0 计数，随后文档被删、内容消失」。
  **同一个原因，方向反了** —— 从「误删」变成「永不删」。

另修复三个附带问题：`PurgeOrphans` 删错对象、`Introspect` 的 hotspots
冻结在迁移快照、`FindRelations` 精确查找失效。

### ★ 一个更隐蔽的：旧表的**读取**也必须停

    SELECT id FROM entities WHERE name = ?

旧表删掉后它返回 no rows ⇒ **Commit 直接失败** ⇒ 记忆完全写不进去。

★ 这是「停双写」时最容易漏的一环：**写要停，读也要停**。

### ★ 边去重：同会话收敛，跨会话并存

    旧 relations 靠 UNIQUE(source,target,type,session_id) 保证
    52e4596 为支持「同类边并存」把边表 UNIQUE 全部移除
    ⇒ 重复提交同一三元组产生重复边（实测同会话两次 → 2 条）

而记忆写入是**高频重试**的（LLM 反复提交同一事实），
重复边会让召回刷屏、场景权重虚高。

### ★★ 判据修正里最危险的一类

**用 `Commit` 造旧表数据**。旧表停写后迁移测试的输入天然为空，
于是「迁出 0 块 0 边」**竟然通过**。

★ 那不是「迁移正确」，是「**测不到**」——
  它让「迁移还对不对」这个问题无法回答。

已加显式的 `Seed*` 测试接口（明确标注仅供测试），
迁移测试现在真的测到了迁移。

### 判据修正汇总（约 15 处）

| 类型 | 处理 |
|---|---|
| 断言旧表计数 `ec`/`rc` | 改用块侧信号 |
| 用 `Commit` 造旧表数据 | 改用 `Seed*` 接口 |
| 靠删旧表造状态 | 改删块侧（如「造孤立实体」） |

★ `Commit` 的返回签名是 SDK 契约（9 处调用方），改签名代价大，
所以保留位置并置 0。
★ **代价要说清楚**：这两个返回值现在没有意义了，
依赖它们判断的调用方（若存在）应改用块侧计数。

## 十二、剩余工作

| # | 项 | 说明 | 风险 |
|---|---|---|---|
| 1 | 启动服务 | 生产库已迁移但服务仍停机 | 需确认 |
| 2 | 观察期后删旧表 | 三张表 + 若干调用点 | **不可逆** |
| 3 | `casual` 3 格 / `coexist` 1 格 | 判据 `want` 口径窄于实际能力 | 无 |
| 4 | `Commit` 返回值语义 | `ec`/`rc` 已无意义，9 处调用方 | 无 |

★ 第 2 项**建议先观察**：旧表已冻结，删它唯一的收益是省几 MB，
而代价是不可逆 —— 万一将来要回滚分析历史数据就没了。
