# docs/zh 文档索引

> 建立于 2026-10-31。目的：**让读者知道该信哪一篇、哪些是历史记录**。
>
> 此前本目录 32 篇文档混着三种时态 —— 当时实测、历史推演、设计规划 ——
> 外观完全相同，读者无从分辨。曾有文档写着「`main.go` 仍走串行代码」
> 而实际早已接完（滞后一整轮发布）。
>
> 所以先分类，再读内容。

---

## 一、怎么判断一篇文档的可信度

每篇文档首部应有**状态行**，取值只允许这四种：

| 状态 | 含义 | 能不能当事实用 |
|---|---|---|
| **已实现并部署** | 文中描述与代码一致，关键处给了取数命令 | ✅ 可以直接信 |
| **部分实现** | 先说清哪部分生效、哪部分没有（附实测调用点） | ⚠️ 只信标了"生效"的部分 |
| **设计 / 未实现** | 是方案，不是现状。代码里可能没有 | ❌ 只能当设计意图 |
| **归档说明** | 历史记录，**故意保留当时的写法** | ❌ 不要改它，也不要信它 |

### 判据名的可信度规则（重要）

文档里出现的 `TestXxx` 名字，**不保证现在还存在**：

- **已实现并部署**类文档里的判据名 → 应当存在。找不到就是缺陷（报告它）
- **设计 / 未实现**类文档里的判据名 → 是**规划的名字**，可能从未实现
- **归档说明**类文档里的判据名 → 记录的是当时的事实，可能已改名或删除

历史上有过一次真实漂移：`BootCtx` 改名为 `KernelState` 后，
`TestBootCtxCloseAllIsReverseOrder` 随之变成
`TestKernelStateCloseAllIsReverseOrder`，而文档没跟上。

**核对命令**（任何时候都可以自己跑）：

```bash
# 列出本目录引用的全部判据名里，仓库中找不到的那些
for n in $(grep -rhoE '`Test[A-Z][A-Za-z0-9_]+`' docs/zh/*.md | tr -d '`' | sort -u); do
  grep -rq "func $n(" --include="*_test.go" internal cmd 2>/dev/null || echo "缺失: $n"
done
```

跑出来有缺失**不一定**是缺陷 —— 先看它出在哪类文档里。

**当前基线（2026-10-31）**：27 条缺失，分布如下，全部已知且已说明：

| 篇数 | 文档 | 性质 |
|---|---|---|
| 18 | `commondev-design.md` / `commondev-agent-runtime-design.md` | 「设计 · 未实现」类，判据名是**规划名** |
| 7 | `kernel-modularization-design.md` | §2.7.3 的**对照表左栏**（有意的历史记录），其中 4 条已登记 future-plan §2.3 |
| 2 | `multimodal-space.md` | 视频能力未纳入契约，判据为规划名 |

⇒ **在「已实现并部署」类文档的正文里（非历史对照表）找不到判据名，才是缺陷。**

---

## 二、文档清单（按状态分类）

### 2.1 已实现并部署 —— 可以直接当事实

| 文档 | 内容 |
|---|---|
| [`kernel-modularization-design.md`](./kernel-modularization-design.md) | **装配架构权威**：依赖声明、BFS 分层、Group 0、实例化粒度 |
| [`kernel-secret-firewall-design.md`](./kernel-secret-firewall-design.md) | 安全防火墙（**部分实现**，见文首接线状态表） |
| [`input-scheduler-design.md`](./input-scheduler-design.md) | 两类别输入 + L1–L4 中断 + 可抢占 |
| [`resident-subagent-design.md`](./resident-subagent-design.md) | 驻留子：轻量内核、两级记忆、父子 L3/L4 |
| [`toolcall-contract-and-sequence-design.md`](./toolcall-contract-and-sequence-design.md) | 工具调用契约与顺序 |
| [`ci-cd-runbook.md`](./ci-cd-runbook.md) | CI/CD 操作手册 |
| [`deploy-runbook.md`](./deploy-runbook.md) | 部署手册 |
| [`site-infra-runbook.md`](./site-infra-runbook.md) | 站点基础设施 |

### 2.2 参考 / 实测报告 —— 数据可信，结论看日期

| 文档 | 内容 |
|---|---|
| [`kernel-performance.md`](./kernel-performance.md) | 性能实测数据 |
| [`production-recall-validation.md`](./production-recall-validation.md) | 生产召回验证（含未解决的 3 条遗留边） |
| [`recall-as-association.md`](./recall-as-association.md) | 召回作为关联的机制说明 |
| [`recall-capability-tiers.md`](./recall-capability-tiers.md) | 召回能力分层（含 2 个未补的缺口） |
| [`multimodal-space.md`](./multimodal-space.md) | 多模态向量空间 |
| [`vector-provider-decision.md`](./vector-provider-decision.md) | 向量 provider 选型决策 |
| [`abstention-criterion.md`](./abstention-criterion.md) | 弃答判据 |
| [`triple-extract-attention.md`](./triple-extract-attention.md) | 三元组抽取与注意力 |

### 2.3 设计 / 未实现 —— 只能当设计意图

| 文档 | 状态 |
|---|---|
| [`commondev-design.md`](./commondev-design.md) | 标题即写明「**设计 · 未实现**」；架构已变更，见其 §12 |
| [`commondev-agent-runtime-design.md`](./commondev-agent-runtime-design.md) | 架构已按前篇 §12 调整，部分未落地 |
| [`interrupt-subsystem-split-plan.md`](./interrupt-subsystem-split-plan.md) | 探测结论 + 实施计划 |
| [`toolcall-parallel-execution-plan.md`](./toolcall-parallel-execution-plan.md) | 并行化执行计划（含遗留项） |
| [`memory-blockflow-repair-plan.md`](./memory-blockflow-repair-plan.md) | 记忆块流修复计划 |
| [`memory-restructure-plan.md`](./memory-restructure-plan.md) | 记忆重构计划 |
| [`memory-usability-plan.md`](./memory-usability-plan.md) | 记忆可用性计划 |
| [`scene-memory-fix-plan.md`](./scene-memory-fix-plan.md) | 场景记忆修复计划 |
| [`legacy-table-retirement.md`](./legacy-table-retirement.md) | 遗留表退役（收尾中） |
| [`entities-retirement-plan.md`](./entities-retirement-plan.md) | 实体表退役 |
| [`prod-migration-checklist.md`](./prod-migration-checklist.md) | 生产迁移清单 |
| [`triple-distill-llm-design.md`](./triple-distill-llm-design.md) | 已实现并**部分否决** |

### 2.4 归档说明 —— 历史记录，不要改也不要信

| 文档 | 说明 |
|---|---|
| [`context-overflow-l4-design.md`](./context-overflow-l4-design.md) | 术语已过时（`memory_*` 等旧工具名保留原样） |
| [`live-run-evidence-2026-10-04.md`](./live-run-evidence-2026-10-04.md) | 某次实跑证据快照 |
| [`legacy-issues.md`](./legacy-issues.md) | 遗留问题清单 |

> **为什么归档文档里有旧工具名却不改**：它们是**证据快照**。
> 把 `memory_recall` 改成 `stablemem_recall` 等于篡改当时的记录，
> 让后来的人无法判断"这条结论是在哪个版本上得出的"。

### 2.5 唯一未做登记处

| 文档 | 内容 |
|---|---|
| [`future-plan.md`](./future-plan.md) | **全仓唯一的「尚未实现 / 已知缺口」登记处** |

**规则**：任何设计文档都不再自己维护「未做」小节，一律登记到 future-plan。
历史上各文档各自维护，结果滞后过 —— 已完成仍写未做，未做被当成已完成。

---

## 三、写文档的三条纪律

1. **数字类断言必须给取数命令**（如模块数用
   `go run ./cmd/homed-depgraph -print`），**不要凭记忆写数字**。
   本文档集历史上写错过一次（17 模块 vs 实际 20）。

2. **区分「机制存在」与「接线生效」**。本项目的头号缺陷形态是
   机制写对了、判据全绿、生产里零调用方。描述能力时写**实测的生产调用点**，
   不要只写"已实现"。

3. **判据要能红**。写判据时先问：把对应实现删掉，这条判据会红吗？
   本仓踩过两次：`security_toolgate_test.go` 手动调 `SetSecurity`
   把缺失的接线补上了；`retention_test.go` 手写合规文件名，
   从没跑过真实产物。两者都永远绿。
