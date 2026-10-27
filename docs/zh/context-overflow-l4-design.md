# 上下文超页 L4 中断 —— 设计与落地

> **归档说明（2026-10-27）**：本文是历史计划，**术语已过时** ——
> `doc_query` / `doc_commit` 已改名为 `coldcontext_query` / `coldcontext_commit`，
> 图记忆（Graph）在对外语义上改称 **stablemem（稳定记忆）**。
> 现行架构与术语以 `assets/docs/zh/ARCHITECTURE.md` 为准；本文保留作为决策记录。


> 特性分支 `feat/context-overflow-l4`
> 起因：2026-10-01 记忆召回跑分（v4）实测到HA 上下文管理失控，本文档是那次归因的产出。

## 一、问题现象（实测，不是推演）

同材料、同窗口（50k）、同判据（v4，10 个探针）的两侧跑分：

| | pi | HA |
| --- | --- | --- |
| 主召回 | 4/9 | 4/9 |
| 累计 prompt | 831k | **9.58M** |
| 缓存命中率 | 75.4% | 85.9% |

HA 单轮 prompt 的实测轨迹（`/var/tmp/mem/v4-ha-50k/partial.jsonl`）：

```text
310099 → 182527 ↓41%   ← 修剪
182527 → 206801 → 281597 → 342446 ↑88%   ← 工具回灌堆积
342446 → 185325 ↓46%   ← 修剪
185325 → 347256 ↑87%
```text

**锯齿波，峰值是窗口的 7 倍。** 每轮都在「远超窗口 → 紧急修剪」。

## 二、根因（一）：topK 与窗口脱钩

```go
// internal/agent/core/memorypass.go:152
topK := a.maxContextSize - 1
return a.context.Prune(query, topK, a.docStore)

// internal/agent/core/agent.go:288
MaxContextSize int // 活跃上下文最大条数，超出按相关性裁剪
// 默认 30（core.agent.max_context_size）
```text

`Prune` 的 topK 是**事件条数**，与 `context_window`（token）没有任何换算关系。
于是「30 条」这个容量对上 34 万 token 的实际占用毫无约束力——每轮必然超限、必然修剪。

这直接回答了「1M 上下文能否改善」：**只调 `context_window` 无效**，必须同时让 topK 随窗口变化
（按 token 估算反推条数，或直接改成 token 预算）。

## 三、根因（二）：超页的三条路径没有统一出口

现状（`internal/agent/core/`）：

| 路径 | 现状 | 位置 |
| --- | --- | --- |
| 上游 context_full 错误 | **完全没接**，整轮 `outcomeFailed` | `stepLLM` 的 `llmErr != nil` 分支 |
| 本地积累超限 | 只在轮首 `checkContextFull`，且**根 agent no-op**、一次性 | `resident.go:535` |
| 子 agent contextfull | 已走 L4 上报，但**只推信号不携带动作** | `resident.go:520` |

第三条已经在用 L4，前两条没有——同一类事件三个出口，这正是要收敛的地方。

## 四、既有L4 机制的关键约束（落地前必须知道）

读 `scheduler.go` 得到四条硬约束，直接决定方案形态：

1. **L4 遇 L4 不能抢占**：`canPreempt` 用严格大于，`effectiveLevel` 封顶 L4 ⇒ 第二个 L4 只能排队。
   *推论*：方案不能让「一次超页」反复触发 L4，否则排队堆积。

2. **中断栈深度上界 4**（`maxInterruptFrames` = 中断级数，非配置项），且**绝不丢弃帧**。

3. **挂起现场是整个 `TaskFrame`**（含 `f.Msgs`）。复原后必须重建 `f.Msgs`，
   否则等于把已经超限的请求原样再发一次。

4. **L4 任务看不到被打断者的上下文**（`suspend` 的 `D1=B` 注释明确）。
   ⇒ 「已裁剪什么」必须**显式写进中断消息**，不能指望它自己知道。

## 五、方案

### 5.1 收敛到 L4 唯一入口

`raiseKernelInterrupt` 已是 L4 唯一入口（panic / selfip 都走它）。超页成为**第三个来源**：

```go
// scheduler.go
func (a *Agent) raiseContextOverflow(pruned int) {
 a.raiseKernelInterrupt("kernel/overflow", "kernel",
  fmt.Sprintf("[内核] 上下文超页，已裁剪 %d 条低相关事件到文档记忆。"+
   "被裁内容仍可检索，但需显式查询；查不到不等于不存在。", pruned))
}
```text

### 5.2 Prune 同步做，L4 只负责打断+告知

**关键取舍**：`Prune` 放在 `raise` 之前同步执行，不放进 L4 任务里。

理由（已核实 `Prune` 全路径：`DenseCosine` 无越界、无IO、纯内存排序 + 可选 docStore 写入）：

- 它没有实质失败模式，唯一「0 条」是「本来就装得下」而非错误
- 放 L4 任务里做会多付一次完整任务调度开销
- **避免「L4 里 Prune 失败再触发 L4」的可能**——用户口径：Prune 出错则保存现场并终止，不重试

```go
func (a *Agent) handleContextOverflow(query string) {
 before := a.context.Len()
 pruned := a.pruneByQuery(query)          // 同步，无 IO
 if pruned == 0 {
  // 装得下却报超页 ⇒ 判定逻辑有 bug：保存现场 + 明确终止，**不再触发中断**
  a.abortWithDiagnostics("context-overflow", before)
  return
 }
 a.raiseContextOverflow(pruned)
}
```go

### 5.3 触发条件（两条路径，同一出口）

| 来源 | 判据 | 位置 |
| --- | --- | --- |
| 本地预判 | 积累上下文 > `overflowRatio × 窗口`（默认 1.25） | 每次发 LLM 请求**之前**（覆盖轮内 tool 回环） |
| 上游报错 | `ProviderError.Kind == ErrContextFull` | `stepLLM` 的 `llmErr` 分支，**在 provider fallback 之前** |

本地判据必须用**未裁剪的积累量**（`a.context` 全部事件估算），不能用 `f.Msgs`——
后者被 `buildMessages` 按 `targetUsage = 0.8×窗口` 裁过，**结构上永不成立**（`resident.go:524` 注释已记录该坑）。

### 5.4 错误分类

```go
// internal/agent/api/provider.go
type ErrKind uint8
const (
 ErrUnknown ErrKind = iota
 ErrTransient
 ErrCredential
 ErrContextFull // 新增
)
```go

判别规则**必须拿真网关实测**，不能凭猜：已观察到不同上游把 context_full 报成 400 / 413 /
`invalid_request_error`，只能靠 body 文本匹配 + `StatusCode` 组合判定。

## 六、测试项

| # | 判据 | 类型 |
| --- | --- | --- |
| T1 | 积累量 > 125% 窗口时，轮内 LLM 请求前触发裁剪 | 单测（构造超限会话） |
| T2 | 裁剪后请求成功，且 `f.Msgs` 确实变小 | 单测 |
| T3 | 上游返回 context_full ⇒ 走裁剪恢复，不 `outcomeFailed` | 单测（mock provider） |
| T4 | `pruned == 0` ⇒ 保存现场并终止，**不再触发 L4** | 单测（变异：把 topK 设成超大值） |
| T5 | 同一 TaskFrame 内超页恢复次数 ≤ 上限（默认 2） | 单测 |
| T6 | 中断消息含「查不到不等于不存在」提示 | 单测（断言 payload） |
| T7 | 打断后复原的 TaskFrame **重建** `f.Msgs`，不复用超限那份 | 单测（断言 Msgs 指针/长度） |
| T8 | L4 遇 L4 不产生栈溢出、不丢帧 | 回归（`maxInterruptFrames` 边界） |
| T9 | topK 随窗口换算（1M 下 topK 不再是 30） | 单测 |
| T10 | 端到端：50k 窗口跑v4，HA 累计 prompt < v4 的 1/3 | 跑分判据 |

T4/T5 是**变异验证项**：把防护去掉必须变红，否则说明测试没测到东西。

## 七、不做的事

- 不改 `Prune` 的打分逻辑（用本轮 query 给所有候选打分确有缺陷，但那是独立议题）
- 不做「换入」策略（`docStore` 已有 `doc_query` 召回，但何时主动捞回是另一个设计）
- 不动 panic 的递归保护（超页不需要：Prune 不 panic，且 L4 遇 L4 不嵌套）
