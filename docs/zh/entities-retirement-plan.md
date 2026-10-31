# entities 退场迁移方案

> **归档说明（2026-10-27）**：本文是历史计划，**术语已过时** ——
> `doc_query` / `doc_commit` 已改名为 `coldcontext_query` / `coldcontext_commit`，
> 图记忆（Graph）在对外语义上改称 **stablemem（稳定记忆）**。
> 现行架构与术语以 `assets/docs/zh/ARCHITECTURE.md` 为准；本文保留作为决策记录。


> 状态：**方案已定，执行未完成**（2026-10-31 复核）。
>
> 方案是「三表全退」（`entities` / `relations` / `sentences` 并入
> `memory_blocks`）。迁移代码在（`internal/memory/migrate_entities.go`），
> 但**生产库仍在用旧表**：
>
> ```
> entities   1292 行
> relations   978 行
> sentences    66 行
> ```
>
> ⇒ 本计划的「执行中」不能读成"快完成了"，而是**迁移工具就绪、
> 生产尚未切换**。切换前的剩余读方清单见
> [`legacy-table-retirement.md`](./legacy-table-retirement.md) §八 / §十二。
> 范围：含生产实例 ${HA_DATA}（用户 2026-10-02 确认迁移）
> 前置：docs/zh/memory-restructure-plan.md（重构计划）
> 原则：一次迁移，删除所有 entities 遗留，避免后续误导。

## 〇、执行前必须确认的一个决策

**relations / sentences 两张表跟不跟 entities 一起退？**

现状：三元组 = entities + relations + sentences 三表联写（graph.go Commit）。
块形态：节点 = memory_blocks，边 = memory_block_edges。

- **方案 A：三表全退**（彻底）—— relations 是第二套边系统，留着必然误导；
  sentences 的"原句"职责由 text block 承担（memory_block_edges 已支持
  sentence 端点 kind，但句子本身该是块）。
- **方案 B：只退 entities**（保守）—— relations/sentences 暂留，
  改成引用 block id。

**已确认方案 A**（用户 2026-10-02）：留着 relations 就等于留着第二套边，
两套边系统并存正是"浆糊"的来源。波及面：68 处 SQL（26+29+13）+ 10 个测试文件。
生产实例纳入迁移范围，在第 3 步单独执行并快照回滚。

## 一、依赖面清单（已核实）

### 内核（internal/memory）

| 文件 | 依赖 | 处置 |
| --- | --- | --- |
| graph.go | 26 处 SQL，Commit/Recall/merge/delete 全链 | **重写**：Commit→PutMemoryBlocks+edges；Recall→块向量召回 |
| noise.go | IsNoiseEntity / NoiseEntities | 改为块判噪或删 |
| scene.go | TagSceneByEntityGlob 等 6 处 | 改挂块节点 |
| migrate.go | LegacyMediaEntityDigest（旧→块迁移） | 改名/扩为全文实体迁移 |
| block.go | 1 处（端点校验引用 entities） | 改：端点只认 block |
| indexer.go | vectorSearchEntities / buildIndexSummary | 改为块索引 |

### 接口面（对外契约，不能断）

| 调用方 | 现状 | 处置 |
| --- | --- | --- |
| memoryface.go GraphMemory 接口 | Recall/RecallSorted/Commit | **签名保留**，实现换块 |
| sdk/memory_impl.go (Recall/Commit) | 外部插件走这条 | 同上，返回类型适配 |
| corehandler_memory.go | 插件 RPC | 同上 |
| cli / webui / healthcheck 插件 | mem.Recall(...) | 签名不变则无感 |
| toolcall.go memory_recall 工具 | g.RecallSorted | 输出格式改块（模型侧 description 同步） |

### 测试（10 个文件）

recall_order_test / noise_test / graph_test / light_memory_test /
scene_test / scene_dedupe_test / reclaim_test / graph_readonly_test /
indexer_test / medialive_test —— 随实现重写，判据保留（变异自证的成果不能丢）。

## 二、迁移步骤（每步可编译可测试）

### 第 1 步：块召回先行（不动 entities）

- GraphDB.RecallBlocks(queryVec, topK)——vector.SearchScored 走块
- memoryface 加方法（不改旧 Recall 签名）
- toolcall 的 memory_recall 输出切到块结果
- **验收**：真库跨维度探针 ≥4/5（用已验证判据）

### 第 2 步：Commit 换块（写入侧切换）

- distill 产出 → 原句块+字段块+contains 边（重构计划第 2 步）
- memory_commit 工具同样改块
- **旧 Commit 保留但标记 Deprecated**，仅存量读取用
- **验收**：新写入 0 行进 entities（用计数断言）

### 第 3 步：存量实体迁移为块

- 沿 MigrateLegacyMediaEntities 方向写 MigrateLegacyEntities：
  实体名→text block（带向量），关系→block edges
- 可回滚：先快照 graph.db，迁移后探针验证，确认后清理
- 孤儿（49 个无关系无原句）→ 原样转为孤立块，不编造连接
- **验收**：迁移后跨维度 ≥4/5；快照可回滚

### 第 4 步：物理删除（最后一步，不可逆）

- 删 entities/relations/sentences 三表 + graph.go 全部相关 SQL
  （26+29+13 处）+ noise.go/scene.go/indexer.go 的实体路
- 删 Entity/Relation/RecallResult 类型，接口面统一块类型
- 删相关测试，保留判据重写为块版
- **验收**：`grep -r entities internal/` 零命中；全量测试绿；
  知识库 content.md 的记忆系统描述同步更新

## 三、风险与回滚

- 每步一个 commit，第 4 步前任意一步可 revert
- 第 3 步动真库前必须快照（cp graph.db graph.db.bak-<ts>）
- 生产实例 ${HA_DATA} **不在本次范围**（需人工确认后单独做）

## 四、明确不变

- memory_recall 工具名与参数（调用方模型无感知）
- pkg/embedding provider SPI（内核无感边界）
- scene 机制（改挂块节点，机制本身保留）
- vector.Store / qwen3vl 双模式权重（直接复用）
