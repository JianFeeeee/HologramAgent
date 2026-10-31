# 记忆系统重构计划：块节点统一载体

> **归档说明（2026-10-27）**：本文是历史计划，**术语已过时** ——
> `doc_query` / `doc_commit` 已改名为 `coldcontext_query` / `coldcontext_commit`，
> 图记忆（Graph）在对外语义上改称 **stablemem（稳定记忆）**。
> 现行架构与术语以 `assets/docs/zh/ARCHITECTURE.md` 为准；本文保留作为决策记录。


> 状态：**已执行完毕（2026-10-04）**。本文是当时的执行计划，保留作为决策记录。
> 日期：2026-10-02（计划） / 2026-10-04（完成）
>
> ## 执行结果摘要（2026-10-04 回填）
>
> ★ **读法**：下面「三、分步执行」里各步骤仍写着 `✗ 未开始` —— 那是
> **计划当时的原始快照**，有意不改（改了就看不出计划与实际的差距）。
> **以本摘要为准。**
>
> | 步骤 | 状态 | 关键提交 |
> |---|---|---|
> | ① 重写设计文档 | ✅ | `6a0d6b7` `971cd72` `7248558` |
> | ② distill 沉库改块节点 | ✅ | `4b54c03` `1c260e3` `0586f6b` |
> | ③ memory_recall 加块召回路 | ✅ | `688ad08` `40a6acb` |
> | ④ memory_commit 止血 | ⚠️ 部分（见下） | `3bafe1c` |
> | ⑤ 存量迁移 | ✅ | `09f31e2` `e0a234c` `5a70452` |
> | ⑥ scene 挂接 | ❌ **取消**（见下） | — |
>
> ### 计划与实际的偏差（都是实测纠正的）
>
> **⑥ scene 挂接取消**：计划假设 blocks 是新层、scene 挂在它上面。
> 但 blocks 不是新层而是图节点升级体，scene 无处可挂。
>
> **蒸馏路线整体否决**（990b824）：生产库 429 条实体里 **424 条（99%）无主语**，
> 而 `Split` 的第五道闸门「无主语不写悬空属性」会把 LLM 已拆对的字段全丢弃。
> 蒸馏命令已改为**只对有主语的实体跑**（b21410b），实际处理 5 条。
>
> **注意力网络路线否决**（`6a0d6b7` ~ `57eca02`）：分域后 token F1 0.918
> 超过门槛，但**跨域不可泛化**（域外零字段），而通用 Agent 无法分域。
>
> ### 计划里未被采纳的替代方案
>
> - 「给块补语义描述」→ 实验否定（中性描述反而更差，排名 26 → 96，`7248558`）
> - 「给 URL 类事实换更强向量」→ 未测，但裸 URL 无文本可供语义匹配（`971cd72`）
>
> ### 现行架构
>
> ```
> memory_blocks        统一节点载体（文本/媒体都进这里）
> memory_block_edges   source_kind → target_kind 多态端点，唯一边表
> graph_centroid       中心向量（迁移/蒸馏后自动重建）
> ```
>
> 原句由 `blk_src_<sha256>` 原句块承载（内容派生、幂等、不带向量），
> `sentences` 表的**写入点已清零**（迁移与蒸馏都不再写）。
>
> 详见 `production-recall-validation.md`（三十三~三十八节）、
> `recall-capability-tiers.md`（能力分层）、
> `vector-provider-decision.md`（provider 决策与缺口）。
> 背景：本会话前期的记忆系统改动（630d3f4 / 978bada / 69150d2 / 25d35f8）建立在对记忆架构的错误认知上，经用户逐条纠正后全部重审。本计划是重审后的执行路线。
> 前置事实核查：全部基于代码与真库实测，标注来源。

## 〇、认知纠错记录（为什么推翻重来）

| 错误认知（当时的做法） | 正确认知（用户纠正 + 代码核实） |
| --- | --- |
| 分层是 L0 对话/L1 文档/L2 graph/L3 blocks | L0=context / L1=doc / L2=graph。**blocks 不是新的一层**，是图节点从文本升级为带向量的多模态节点 |
| 给 entities 加向量字段（graph-vector-design.md） | **entities 该退场**。memory_blocks 已是带 vector+fingerprint 的节点载体（46f833c） |
| LLM 拆句产物 Commit 成 entities+relations（630d3f4） | 拆出的节点放入 block，保留原句；媒体引用是已删除的老设计（mediaref.go 只剩注释） |
| 三元组 = 我发明的 (主语,维度,值) | 三元组 =【节点】【关系边】【节点】；三元组设计文档在知识库 assets/knowledge/homeagent_architecture/content.md |
| provider 是蒸馏的配件 | provider 是内核的模型接入接口，内核对 tfidf 等传统算法与向量化 LLM **无感**，payload 一致才能动态接入 |
| 多模态要"引用"进图 | 多模态节点由 LLM 多模态能力拆分入库 |
| 场景/scene 表忽略不看 | scene 是第三种召回：条件型记忆不吃词法/向量相似，按场面**前缀匹配**直接取回（scene.go:12-38） |

沉痛教训：630d3f4 的 16 三元组端到端验证——验证的是拆分质量，不是正确的落库形态。**验证通过 ≠ 架构正确。**

## 一、目标形态（一句话）

```
入库：拆解器（provider 接口，内核无感）把对话/文档/媒体拆成
      memory_blocks 节点（modality/text_content/vector/fingerprint）
      + memory_block_edges 结构边；原句保留为块。
召回：块向量检索为主 + scene 场景召回补条件型记忆；
      entities+LIKE 是存量过渡路径，逐步退场。
```

## 二、既有设施盘点（不重复实现）

| 能力 | 已有 | 位置 | 状态 |
| --- | --- | --- | --- |
| 块节点存储 | `PutMemoryBlocks` / `MemoryBlocks` | internal/memory/block.go:67/144 | ✔ 可用 |
| 结构边 | `AddMemoryBlockEdge`（端点校验，测试 453/491 行） | block.go:199 | ✔ 可用 |
| 块检索 | `BlocksForNode`（按端点反查） | block.go:254 | ✔ 可用 |
| 向量检索 | `vector.Store`（Insert/SearchScored/Remove） | internal/memory/vector/store.go | ✔ 可用 |
| provider SPI | `pkg/embedding`（Embed/Info/Close，Fingerprint） | pkg/embedding/embedding.go | ✔ 可用 |
| VL 权重双模式 | qwen3vl（Embed 出向量）+ qwen3vlgen（tied head 生成） | 25d35f8 | ✔ 已验证 |
| 文本拆解 | distill.Extractor（schema 约束+闸门，16 三元组/幻觉0） | internal/memory/distill | 拆分逻辑可复用，**沉库去向要改** |
| 旧实体迁移先例 | `MigrateLegacyMediaEntities` | internal/memory/migrate.go:47 | ✔ 方向先例 |
| scene 召回 | `RecallByScene` | internal/memory/scene.go:323 | ✔ 已有 |
| 拆解生成模型 | ollama+qwen3:1.7b（schema 6/6 零幻觉） | providers/ollama | ✔ 端到端验证过 |

## 三、分步执行

### 第 1 步：重写两份设计文档 ✗ 未开始

- [ ] 删 `docs/zh/graph-vector-design.md`（给 entities 加向量——前提错误）
- [ ] 重写 `docs/zh/triple-distill-llm-design.md`：拆解产物=块节点；entities 标注退场路径
- [ ] 修正 `providers/qwen3vlgen` 与 `pkg/generation` 的定位注释：不是"蒸馏配件"，是入库拆解器的模型面
- **验收**：文档里的每张表名/函数名都能 grep 到；无 entities 加向量字段的任何残留表述

### 第 2 步：distill 沉库改块节点（核心） ✗ 未开始

- [ ] `distill.Extractor.Split` 产出改为块结构（新增 `distill.Blocks` 类型，保留 Triple 输出做对照期）
- [ ] 原句 → text block（text_content=原句, vector, fingerprint）
- [ ] 字段 → text block（text_content=字段内容, vector, fingerprint）
- [ ] 边：原句 block —contains→ 字段 block
- [ ] 向量来源走 `pkg/embedding`（默认 provider 由配置决定；无 provider 时 vector 留空+fingerprint 空，**不静默编造向量**）
- [ ] pipeline.go 的 distillBatch 改调块写入
- **判据**：
  - [ ] 拆一条真实记录后，MemoryBlocks/MemoryBlockEdges 能查到原句块+字段块+边
  - [ ] 断电安全：PutMemoryBlocks 是事务（block.go 已有端点校验事务先例）
  - [ ] 无 provider 时块仍入库（vector=''），检索退化为符号路，**不报错不编造**
- **变异自证**：把"contains 边"删掉，块查询测试必须红

### 第 3 步：memory_recall 增加块召回路 ✗ 未开始

- [ ] GraphDB 增加 `RecallBlocks(queryVec, topK)`：走 vector.SearchScored
- [ ] 工具层 `memory_recall`：块向量召回为主，entities LIKE 为存量兜底，输出统一格式
- [ ] 工具名/参数不变（调用方模型无感知）
- [ ] fingerprint 不匹配时**拒绝比较**（报"向量空间已变更，需回填"），绝不拿 2048 维查询比 512 维库
- **判据**（真库 + 跨记录探针，用本会话已验证的判据）：
  - [ ] 跨维度定位 ≥4/5（现状 LIKE 0/5）
  - [ ] 端口类数字串不退化（向量弱、符号强——混合必要性）
- **变异自证**：去掉块召回路，跨维度判据必须退化到现状水平

### 第 4 步：memory_commit 同步止血 ✗ 未开始

- [ ] description 明确要求：拆解后走块形态；禁止 43-50 字符整句实体
- [ ] 工具实现侧加闸门：超长 subject 拒绝并提示（与 distill 闸门同款）
- **判据**：新写入的实体长度分布不再出现 >25 字符主体

### 第 5 步：存量迁移 ✗ 未开始

- [ ] 沿 `MigrateLegacyMediaEntities` 方向写 `MigrateLegacyTextEntities`：
      188 实体 → 原句块（带向量）+ 字段块 + contains 边
- [ ] 独立命令入口（参照 homed-kb-migrate），**可回滚**：导出原库快照→迁移→对比→确认后替换
- [ ] 孤儿实体（49/108 无关系无原句）保持原样，不编造
- **判据**：迁移后跨维度探针 ≥4/5；原库快照可完整回滚

### 第 6 步：scene 挂接（可选，后置） ✗ 未开始

- [ ] 拆解块带 scene 键（memory_blocks.scene 字段已在 schema 里）
- [ ] 验证 `RecallByScene` 对块节点生效
- 前置：第 2-5 步稳定后再做

## 四、每步通用约束

1. **provider 边界**：内核不出现任何 provider 名分支；一切经 `pkg/embedding`/`pkg/generation` 接口
2. **禁止静默错误**：fingerprint/维度不匹配必须显式报；无向量必须显式留空
3. **回填必落盘**（document.go:137 陷阱：不落盘则判定条件永远成立、每次启动重算）
4. **变异自证**：每条关键判据写测试后，手动破坏对应实现，测试必须变红
5. **长任务纪律**：导出/回填等内存密集任务**绝不与测试并行**（本会话三次 OOM 教训）
6. **验证通过 ≠ 架构正确**：每步完成后对照本计划第一节逐条自检

## 五、暂不做

- entities 表的物理删除（存量过渡期内仍服务 LIKE 兜底）
- KV cache（qwen3vlgen 全量前向 O(N²)，生成低频，已注释标明）
- 多模态拆解（第 6 步之后，依赖块路稳定）
- 切换生产实例 /var/tmp/ha-c 的 provider 配置（需人工确认）
