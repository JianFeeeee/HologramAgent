# docs/zh 文档索引

> 建立于 2026-10-31。目的：**让读者知道该信哪一篇**。
>
> 此前本目录混着三种时态 —— 当时实测、历史推演、执行计划 —— 外观完全相同。
> 曾有文档写着「`main.go` 仍走串行代码」而实际早已接完（滞后一整轮发布）。

---

## 一、本目录只放四类文档

| 类型 | 回答什么问题 | 有长期价值 | 命名 |
| --- | --- | --- | --- |
| **设计文档** | 为什么这样设计 | ✅ | `*-design.md` |
| **操作手册** | 怎么做 | ✅ | `*-runbook.md` |
| **教训** | 踩过什么坑 | ✅ | `engineering-lessons.md` |
| **未做登记** | 还剩什么没做 | ✅ | `future-plan.md` |

### ❌ 不放：执行计划

执行计划（`*-plan.md` / `*-checklist.md`）**会随实施完结变成误导性读物** ——
它记录"当时打算做什么"，而读者会读成"现在是什么"。

2026-10-31 删除了 9 份已完结的计划，删前把两类内容提出来：

| 内容 | 去向 |
| --- | --- |
| 决策与教训 | `engineering-lessons.md` |
| 未完成的项 | `future-plan.md` |
| 实测数据 | 并入对应设计文档（如压测数据进 `toolcall-contract-and-sequence-design.md` 附录 A） |

**唯一例外**：`legacy-table-retirement.md` 与 `interrupt-subsystem-split-plan.md` 保留 ——
前者被 4 处生产代码注释当作**退役依据**引用，后者记录的探测事实仍被判据引用。
它们不是"执行计划"，是**依据**。

---

## 二、怎么判断一篇文档的可信度

每篇首部都有**状态行**，取值只允许这四种：

| 状态 | 含义 | 能当事实用吗 |
| --- | --- | --- |
| **已实现并部署** | 描述与代码一致，关键处给了取数命令 | ✅ 直接信 |
| **部分实现** | 先说清哪部分生效（附实测调用点） | ⚠️ 只信标了"生效"的 |
| **设计 / 未实现** | 是方案，代码里可能没有 | ❌ 只能当设计意图 |
| **归档说明** | 历史记录，**故意保留当时写法** | ❌ 不要改、也不要信 |

### 判据名的可信度规则

文档里的 `TestXxx` **不保证现在还存在**：

- **已实现并部署**类文档里的 → 应当存在。找不到就是缺陷
- **设计 / 未实现**类文档里的 → 是**规划名**，可能从未实现
- **归档说明**类文档里的 → 记录当时事实，可能已改名或删除

真实案例：`BootCtx` 改名 `KernelState` 后，
`TestBootCtxCloseAllIsReverseOrder` → `TestKernelStateCloseAllIsReverseOrder`，
文档没跟上。

**核对命令**：

```bash
for n in $(grep -rhoE '`Test[A-Z][A-Za-z0-9_]+`' docs/zh/*.md | tr -d '`' | sort -u); do
  grep -rq "func $n(" --include="*_test.go" internal cmd 2>/dev/null || echo "缺失: $n"
done
```

有缺失**不一定**是缺陷，先看它出在哪类文档里。

**当前基线（2026-10-31）**：27 条缺失，全部已知：

| 篇数 | 文档 | 性质 |
| --- | --- | --- |
| 18 | `commondev-*.md` | 「设计 · 未实现」类，判据名是**规划名** |
| 7 | `kernel-modularization-design.md` | §2.7.3 的**对照表左栏**（有意的历史记录） |
| 2 | `multimodal-space.md` | 视频能力未纳入契约，判据为规划名 |

⇒ **在「已实现」类文档正文里找不到，才是缺陷。**

---

## 三、文档清单

### 3.1 设计文档 —— 可以直接当事实

| 文档 | 内容 |
| --- | --- |
| [`kernel-modularization-design.md`](./kernel-modularization-design.md) | **装配架构权威**：依赖声明、BFS 分层、Group 0、实例化粒度 |
| [`kernel-secret-firewall-design.md`](./kernel-secret-firewall-design.md) | 安全防火墙（**部分实现**，见文首接线状态表） |
| [`input-scheduler-design.md`](./input-scheduler-design.md) | 两类别输入 + L1–L4 中断 + 可抢占 |
| [`resident-subagent-design.md`](./resident-subagent-design.md) | 驻留子：轻量内核、两级记忆、父子 L3/L4 |
| [`toolcall-contract-and-sequence-design.md`](./toolcall-contract-and-sequence-design.md) | 工具调用契约（**附录 A 是压测数据**） |
| [`commondev-design.md`](./commondev-design.md) | 主管式多仓库开发流水线（**设计 · 未实现**） |
| [`commondev-agent-runtime-design.md`](./commondev-agent-runtime-design.md) | commondev 运行时与内核接入 |
| [`context-overflow-l4-design.md`](./context-overflow-l4-design.md) | 上下文超页 L4（归档，术语过时） |
| [`triple-distill-llm-design.md`](./triple-distill-llm-design.md) | 三元组蒸馏（已实现并**部分否决**） |

### 3.2 操作手册

- [`ci-cd-runbook.md`](./ci-cd-runbook.md) — CI/CD
- [`deploy-runbook.md`](./deploy-runbook.md) — 部署
- [`site-infra-runbook.md`](./site-infra-runbook.md) — 站点基础设施

### 3.3 参考 / 实测报告

| 文档 | 内容 |
| --- | --- |
| [`engineering-lessons.md`](./engineering-lessons.md) | **踩过的坑与共同形态**（推荐先读） |
| [`kernel-performance.md`](./kernel-performance.md) | 性能实测基线 |
| [`production-recall-validation.md`](./production-recall-validation.md) | 生产召回验证（「探针 PASS 但结论是假的」） |
| [`recall-as-association.md`](./recall-as-association.md) | 召回作为关联 |
| [`recall-capability-tiers.md`](./recall-capability-tiers.md) | 召回能力分层（含 2 个未补缺口） |
| [`multimodal-space.md`](./multimodal-space.md) | 多模态向量空间 |
| [`vector-provider-decision.md`](./vector-provider-decision.md) | 向量 provider 选型（已定） |
| [`abstention-criterion.md`](./abstention-criterion.md) | 弃答判据 |
| [`triple-extract-attention.md`](./triple-extract-attention.md) | 三元组抽取与注意力 |

### 3.4 依据类（不是计划，是退役/拆分的判断依据）

| 文档 | 为什么保留 |
| --- | --- |
| [`legacy-table-retirement.md`](./legacy-table-retirement.md) | 被 4 处生产代码注释引用；剩余读方清单在 §八/§十二 |
| [`interrupt-subsystem-split-plan.md`](./interrupt-subsystem-split-plan.md) | 探测事实被判据引用；拆分本身未实施 |

### 3.5 归档说明 —— 历史记录，不要改也不要信

- [`live-run-evidence-2026-10-04.md`](./live-run-evidence-2026-10-04.md) — 某次实跑证据快照
- [`legacy-issues.md`](./legacy-issues.md) — 遗留问题清单（部分条目已过时）

> 归档文档里的旧工具名（`memory_recall` 等）**故意不改** —— 它们是证据快照，
> 改了就无法判断"这条结论是在哪个版本上得出的"。

### 3.6 单一登记处

| 文档 | 内容 |
| --- | --- |
| [`future-plan.md`](./future-plan.md) | **全仓唯一的「尚未实现 / 已知缺口」登记处** |

---

## 四、写文档的三条纪律

1. **数字类断言必须给取数命令**（如模块数用
   `go run ./cmd/homed-depgraph -print`），**不要凭记忆**。
   本文档集写错过一次（17 模块 vs 实际 20）。

2. **区分「机制存在」与「接线生效」**。本项目头号缺陷形态是
   机制写对了、判据全绿、生产里零调用方。描述能力时写**实测的生产调用点**。

3. **判据要能红**。写判据时先问：把对应实现删掉，这条判据会红吗？
   本仓踩过两次：`security_toolgate_test.go` 手动调 `SetSecurity`
   把缺失的接线补上了；`retention_test.go` 手写合规文件名，
   从没跑过真实产物。两者都永远绿。

详见 [`engineering-lessons.md`](./engineering-lessons.md)。
