# 工具调用并行化改造：执行计划

> **设计依据**：`docs/zh/toolcall-contract-and-sequence-design.md`（本文只讲**怎么一步步做**，
> 设计取舍与实证依据在那份文档里，不重复）。
>
> **状态：全部阶段已完成**（2026-10-31 复核）。
>
> 阶段 0 → 0.2 → 0.5 → 1 → 2 → 2.5（内核主线）与 3/4（序列插件）
> 均已完成。原先这里写着「阶段 0.5 起为待办」，是计划初期的状态，已过时。
> 遗留项状态见文末「遗留项状态」小节（全部已解决）。
> **纪律**：每个阶段的「判据」必须**先写且必须能失败**，再改实现；
> 判据通过前不进入下一阶段（见文末「阶段纪律」）。

---

## 阶段总览与依赖

| 阶段 | 内容 | 依赖 | 状态 |
| --- | --- | --- | --- |
| **0** | 修 map 迭代顺序（flush 乱序） | — | ✅ **已完成** |
| **0.2** | **工具错误类型化（已完成）** | — | ✅ **已完成** |
| **0.5** | 补多 tool_call 批内路径判据 | — | ✅ **已完成** |
| **1** | 结果契约 + 参数预校验 | — | ✅ **已完成**（1a/1b/1c/1d） |
| **2** | 并行执行层 | 1 | ✅ **已完成**（2a/2b/2c/2d） |
| **2.5** | 提示词改为「默认并行」 | 2 | ✅ **已完成** |
| **3/4** | 序列插件 | 2 | ✅ **已完成**（P1–P4，见下） |

**主线（内核）0 → 0.2 → 0.5 → 1 → 2 → 2.5 已全部完成**；
**插件线 P1 → P2 → P3 → P4 也已完成**。两部分详见下文。

### ⚠️ 顺序不可调换的两处

- **2.5 必须在 2 之后**：先改提示词说「默认并行」而内核仍串行 = 提示词对模型说谎。
- **1 必须在 2 之前**：并行化需要「诚实的 Success」与「结构化值」来判断结果与做条件；
  先并行后补契约，等于把两处改动叠在同一段逻辑上，回归时无法定位是哪一处引起。

---

## 阶段 0 ✅ 已完成

**问题**：`flushToolCall` 由 `for idx := range accs` 驱动，Go map 迭代顺序随机化
⇒ 同一批并行 tool_call 进入 `resp.ToolCalls` 的顺序**每次运行都可能不同**。
对 `output_send__` 这类用户可见通道 ⇒ 分段消息到达顺序不可复现。

**改动**：`process.go` —— 抽出闭包 `flushAll`，收集 index 后 `sort.Ints` 再 flush。
两个调用点（`ch` 关闭、`ctx.Done()`）统一走它。

**判据**：`stream_flush_order_test.go` —— 8 工具 × 200 轮，断言输出严格按投递顺序。
**已做变异验证**：退回 map 遍历后判据于 round 0 即 FAIL（实际顺序 `[tool_02…tool_00 tool_01]`）。
修复后 core 包全绿。

---

## 阶段 0.2 ✅ 工具错误类型化（已完成）

**问题**（两条，第二个是静默 bug）：

1. 内核用 `strings.Contains(err, "not found in any plugin")` 判别「工具不存在」
   （原 `toolcall.go:104`）——**约定**不是契约。插件错误文案若含该子串即被误判。
2. ⚠️ `IOManager` 向父兜底时**吞掉父的执行失败**（`channel.go:655-660`），
   误报为「工具不存在」。后果放大：设备离线这类**本该 retry** 的失败被判为
   「工具没了」⇒ 整组被跳过，与「插件真没加载」无法区分。

**改动**：

- `io/channel.go`：新增哨兵 `ErrToolNotFound` + `ToolNotFound(name)` + `IsToolNotFound(err)`
  （沿用仓内 `ErrInputChannelUnknown` 的先例）。`IOManager.ExecuteTool` 的父兜底
  改为**只传递「确实不存在」**，其余错误如实上抛。
- `core/stages.go`：`StageHost` 的 not-found 改用 `agentIO.ToolNotFound`。
- `core/toolcall.go`：字符串匹配 → `agentIO.IsToolNotFound`；「不存在」时给出
  **可执行**文案（提示 `get_plugin_tools` / `output_list_channels`），
  而非含糊的「执行失败」——后者会让模型反复重试同一个不存在的名字。

**判据**：`toolcall_error_test.go`（类型化 vs 诱饵子串、%w 穿透、执行期文案）+
`channel_error_test.go`（父失败不吞、真的不存在仍可判别）。

**变异验证**：退回父兜底吞噬后，`TestExecuteTool_DoesNotSwallowParentFailureAsNotFound`
FAIL（`父的执行失败被误报为『工具不存在』`）；修复后全绿。

⚠️ **过程中的一次自伤**：我先改了 `channel.go` 却漏了 `stages.go` 的 import，
导致整包 build 失败。已修。另：第一次写判据时只覆盖了类型化，**没覆盖父兜底吞噬**，
变异后仍绿 —— 说明「判据通过」不等于「修的东西被测到」。补了 `channel_error_test.go` 才闭合。

---

## 阶段 0.5 ✅ 补批内路径判据（已完成）

**前提更正**（此前我写「无任何测试直接驱动批内路径」——**不准确**）：
`scheduler_critical_test.go:125` 的 `TestBatch_NotAbandonedWithoutPreemption`
**已经**驱动了「同批两个工具按序执行」。漏查是因为我只 grep 了
`PendingTools` / `ToolIdx` 这两个**字段名**，没查断言**内容**。

**真正缺的是**该测试**未覆盖**的三条（已核实全仓无对应断言）：

| 缺口 | 风险 | 新增判据 |
| --- | --- | --- |
| `tool_call_id` 配对完整性 | 阶段 2 改消息落法时，配对一旦断裂上游直接报错 | `TestBatchToolCallIDsAllPaired` |
| `ContentOnce` 批内语义 | 同一段 assistant 文本在批内重复 N 次，撑爆上下文 | `TestBatchAssistantTextAppearsOnce` |
| denied 后**继续**批内 | 改成 abort 会丢掉本可执行的后续调用 | `TestBatchContinuesAfterDeniedTool` |

另加 `TestBatchExecutesEveryToolCall`（批内全序列 + 顺序 + ToolResults 完整性）。

**判据**：`toolbatch_test.go`（4 条）+ 复用既有 `TestBatch_NotAbandonedWithoutPreemption`。
全部**确定性**断言（阶段 0 已消除 map 随机性）。

**变异验证**：令 `stepToolBegin` 跳过批内最后一个工具后，**4 条判据同时 FAIL**
（既有那条也 FAIL），报错直指 `ToolsUsed = [tool_alpha]`、`tool_call_id "c2" 被声明 0 次`。

⚠️ **过程中三次自伤**（都靠"判据先写"暴露）：

1. 臆造了不存在的 helper（`sdkToolDef` / `sdkStageCtx`）⇒ build 失败
2. 把 `a.stageHost = nil` 后又用它注册 stage handler
3. 给 `newTaskFrame` 传 `nil` ⇒ `stepPrepare` 于 `task.go:519` **nil 解引用 panic**
   ⇒ 改用仓内既有的 `a.stageCtxFromInput(...)`（与 `scheduler_preempt_test` 一致）
   ★ 顺带记录：生产路径两处 `newTaskFrame` 调用（`task.go:228/422`）都传真实 ctx，
     但 `stepPrepare` 对 `f.StageCtx` **无 nil 兜底**。本次不修（无生产触发路径），
     记为潜在健壮性缺口。

## 阶段 2 ✅ 并行执行层（已完成）

| 子项 | 内容 | 状态 |
| --- | --- | --- |
| **2a** | 消息落法：一条 assistant 带全部 tool_calls | ✅ `28b42bc` |
| **2b** | `ConsumeToolBlocks` 改 per-call 归档 | ✅ `25f5c53` |
| **2c** | `StageContext` 拆 per-tool | ✅ `3d12e82`（判据由 2d 闭合） |
| **2d** | 批次调度与保序（`ParallelSafe` + 同通道保序） | ✅ `34c0df2` |

**改动要点**（细节见设计文档 §6）：

- **2a** 现状每工具一对消息；改为一条 assistant 带全部 tool_calls。
  这本身是协议上更正确的形态（现状不表达「这是一批」）。
- **2b** `ConsumeToolBlocks` 原本是 IOManager 级单队列，并发下会互相抢媒体 ⇒
  破坏「媒体必须紧跟自己 toolMsg」那条三轮实测的结论。改按 `call_id` 归档。
- **2c** `f.StageCtx` 原本是单槽；改为每工具一份，`Extra` 逐份浅拷贝
  （`output_channel` 被 `stage.go:18` 依赖）。
- **2d** 全批 `ParallelSafe` 才并发，否则**整批**降级串行（不做部分并发——
  收益不抵不可预测性）；同 `output_send__<通道>` 多次发送**保序**。

**并发规则**（三条全满足才并发）：批内 >1 个工具；**全部**声明 `ParallelSafe`；
不含需保序的同通道输出发送。

**结构**：`StepToolBatch` —— fan-out（每工具一 goroutine，各写自己的
`toolCtxs[i]`）→ join → **按索引顺序**串行收尾。收尾必须串行且按索引：
`f.Msgs` 是共享切片，且按索引落才能让模型读到的上下文顺序与它自己发出的
顺序一致。

⚠️ **修掉一个我自己引入的竞争**：`resolveTurnScenes` 会把结果记进**共享**的
`f.sceneDone` / `f.turnScene`。最初在每个 goroutine 里各调一次 —— 既是数据
竞争，又会各自触发一次 `EnterSceneWithHint`，**重复计入场景强度**
（正是 `sceneDone` 注释警告过的问题）。改为 fan-out **之前**解析一次。

### 2c 判据缺口：已由 2d 闭合

2c 落地时做过变体验证（`toolCtxFor` 退回单槽），**两条判据仍全绿** ——
因为串行路径下「单槽」与「per-tool」行为完全一致，差别只在并发下显现。
当时据实记为「实现已就位、判据未闭合」。

2d 落地后补了**并发版**判据（`TestBatchConcurrentEachToolSeesOwnContext`）：
退回单槽时触发 **6 处 DATA RACE 报告 + 串味断言失败**。缺口至此关闭。

### 遇到的一处**既有**测试竞态（非本次引入，但会污染回归信号）

`TestResidualKeepReturnsTasksToParent` / `TestResidualDropNotifiesSyncCaller`
偶发失败，报「应处置 2 条，实际 1」。

**根因**（已核实）：`offload_test.go:473` 的 `SpawnResident` 会启动**子 agent 的
调度器 goroutine**，而测试随后 `child.sched.enqueue(...)` 两条任务、
立刻 `ApplyResidual` 去读同一队列——**全程无任何同步**。调度器与测试读并发
同一份 `sched.queue`，条数可能已被取走。

**为什么以前没暴露**：干净基线（阶段 2 之前）连跑 3 次恰好全绿，是**运气**，
不是确定性。`-race` 单跑该用例也过（无并发源）。本次改动让 core 包耗时略增、
调度时序变化，才把它翻出来。

**处置**：属测试侧缺陷，不在本阶段范围内，**记为待修**（修法：测试里改用
不启动调度器的子 agent，或给 enqueue/读取加同步）。记录在此以免后续误判为
「并行化引入的回归」。

### 2c 的诚实记录：判据在串行下测不出差别

`toolCtxFor` 退回单槽后，两条 2c 判据**仍然全绿**。原因是：
**串行路径下「单槽」与「per-tool」行为完全一致**——每个工具跑完才进下一个，
不存在交错。差别只在**并发**下显现（互相覆写 / after 读到别人的结果）。

⇒ 因此 2c 的真正判据**必须与 2d 一起写**：并发执行批内多工具时，
断言每个工具的 ctx 只带自己的 ToolCalls、after_toolcall 读到自己结果，
并以 `go test -race` 确认无数据竞争。
**在 2d 落地前，2c 只能算"实现已就位、判据未闭合"**，不得记为已验证。

---

## 阶段 1 ✅ 结果契约 + 参数预校验（已完成）

对应设计文档 §4。**动机**：`Success` 硬编码 `true`（`task.go:757` 唯一赋值点）、
`required` 无消费方、结果被降级为 `string`——这三项让阶段 2/3 都缺地基。

### 1a. SDK 侧新增（纯新增，无签名变更）

`third_party/homeagent-sdk/sdk/plugin.go`：

```go
// ToolError 描述失败原因。存在的理由：失败若只表达为文本，模型无法定位到字段，
// 只能原样重试（实测 cmd_run 失败率 34%~48% 源于同一成因）。
type ToolError struct {
    Field  string `json:"field,omitempty"`
    Reason string `json:"reason"`   // required/type/unauthorized/timeout/not_found
    Detail string `json:"detail,omitempty"`
    Hint   string `json:"hint,omitempty"`
}
```

- `ToolDef` 增 `ParallelSafe bool`（零值 `false` = 不可并行 = 现状行为，
  刻意让存量插件升级后得到**保守**行为）。
- 仓内 `internal/sdk/plugin.go` 补对应别名再导出。

### 1b. 诚实化 Success

`Success = (err == nil) && !isToolError(result)`。

**判据必须同时覆盖新旧两种形态**：存量插件返回 `map[string]interface{}{"error": ...}`
（如 `files/plugin.go:228`）要判为失败，而返回普通 map/string 仍算成功。
**漏了后者 = 升级会把存量插件的成功误判成失败**，这是本阶段最大的回归风险。

### 1c. 参数预校验

在 `executeToolCallInner`（`toolcall.go:47`）分派**之前**执行：逐项查 `required`、
逐项查 `properties.type`。失败返回结构化 `ToolError`，不进分派。

⚠️ **`getBool`（`utils.go:26`）的注释记载 bool/string/float 三种都出现过** ⇒
校验**不能**把合法的 `"true"` 判为非法。判据必须覆盖这一点。

### 1d. 保留结构化值

`executeToolCall` 内部保留 `rawResult interface{}`（不降级为 string），
渲染交给统一函数：**紧凑 JSON，禁止 `fmt.Sprintf("%v")`**（那会产出
`map[status:sent id:123]` 这种模型读不懂的 Go 语法）。

**顺带修一个静默失效**：`task.go:771` 的 `ToolResults[0].Result.(string)` 断言
几乎恒失败（插件返回的多是 map）⇒ `after_toolcall` 阶段插件对结构化结果的改写当前无效。

### 阶段 1 判据

- `toolerror_test.go`：`isToolError` 对新旧两种形态的判定（各 3 例：失败 map /
  成功 map / 成功 string）
- `argvalidate_test.go`：缺 required、类型不符、`"true"` 宽松放行
- 回归：core 包全绿；**存量插件 smoke**（`internal/plugins` 不得新增 FAIL）

---

## 阶段 2.5 ✅ 提示词改为「默认并行」（已完成）

⚠️ 有**硬性顺序约束**：必须在阶段 2 落地**之后**。反序（先说"并发"、内核仍
串行）会让提示词**对模型说谎** —— 模型据此推断安全性，写出真正依赖顺序的
调用。宁可晚改，不可错改。

新增【工具执行顺序】段，讲清四件事：

1. 同一条回复里的多个工具调用**默认并行**（同时跑），不是依次执行
2. **不要依赖执行顺序** —— 参数依赖前一个结果就分两轮
3. **例外一：同通道 `output_send__` 保序**（用户可见消息顺序敏感）
4. **例外二：不并发安全的工具整批退回串行**（写类工具 / 未声明者）

措辞刻意与 `batchRunnable` 的**真实**判据一致，而不是理想化表述 ——
**提示词与实现不符，比不说更坏**。

顺带改掉 `spawn_child` 的落空表述（原文「应并行 spawn，不要自己串行逐个执行」
在并行化之前是落空的）。

判据 `prompt_parallel_test.go`（5 条），其中**负向**那条最关键：
只讲并行、不讲例外 ⇒ FAIL（"提示词说谎"的失败模式已被覆盖）。

## 阶段 3 / 4 ✅ 工具序列插件（已完成）

| 阶段 | 内容 | 提交 |
| --- | --- | --- |
| **P1** | AST 解析 + 静态校验 | `76030ae` |
| **P2** | 执行引擎（组内并行 + 具名槽 + 条件求值） | `7532af7` |
| **P3** | 存储 + 跨序列调用图 + missing 策略 | `3d75312` |
| **P4** | 六个 `seq_*` 工具 + 插件装配 | `71c894c` |

**插件线复用内核并行面，但不要求内核开任何新接口**（见设计文档 §7 边界声明）。

### 顺带补上的内核两处缺口

P3 落地时暴露的**真实缺陷**（不是新需求）：

- `GetAllTools` **丢 `ParallelSafe`** ⇒ 插件看到的设备工具一律"不可并发"，
  设备工具的并发声明对插件**不可见**。
- `ToolAPI` **缺按名查** ⇒ 新增 `ToolDefByName`。插件需在**运行前**判断
  目标是否存在（动态注册下"不存在"是常态），而 `GetAllTools` 只能拿全量列表。

### 本阶段解决的两个设计问题

1. **跨序列目标存在性**：`Save` 若要求"目标必须先存在"，互调的两条序列
   谁也存不下来（A 要 B 先在、B 要 A 先在）——**依赖在设计上无解**。
   ⇒ `Save` 只校验同序列内的 group 引用；跨序列目标由 `CheckGraph` 兜底。

2. **黑名单分层**：我一度把 `seq_call`/`seq_when_call` 也禁掉，但那正是
   本包的核心能力（按名调用），禁掉序列就退化成单层脚本。
   ⇒ 黑名单只管**对外发消息 / 起子 agent / 改插件表 / 再跑整条序列**；
   `seq_call` 系列留给序列内部组合，递归由 `maxCallDepth` + 环检测负责
   （设计文档 §8.3 本来就这么定，是我把两层混了）。

### 遗留项状态

| 项 | 内容 | 状态 |
| --- | --- | --- |
| **D4** | 设备授权闸在 `ToolAPI` 路径失效 | ✅ **已解决**（`994f198`）。`ToolAPI` 新增 `CanUse`，由 bootstrap 注入 agent 的授权判据；两条路径判定一致性由 `TestCanUseAgreesWithInnerPath` 钉住 |
| 端到端 | 真机跑一次 `seq_create → seq_run` | ✅ **已完成**（`3d31037`）。并抓出「传参方式完全不可用」的真 bug |
| 提权 | `seq` 是否默认启用 | ✅ **无需决策**：仓内已有 `IsPluginDisabled` / `AddDisabledPlugin` 机制，任何插件（含内置）都可被用户禁用，`seq` 无需特殊处理 |

### 仍需注意的两点

1. **缺 `device_id` 时是 fail-open**（放行）。这是内核既有语义
   （`TestDeviceToolAuth_*` 依赖它），本次**未擅自改**；已由
   `TestCanUseMatchesInnerFailOpenOnMissingDeviceID` 钉住现状。
   若要改成 fail-closed，**必须内核与 `ToolAPI.CanUse` 两处同时改**，
   否则两条路径判定不一致本身就是漏洞。

2. **`TestResidual*` 既有竞态**（`offload_test.go`）：`SpawnResident` 起了
   子调度器 goroutine，而测试 enqueue 后无同步就读同一队列。
   与本次改动无关，已定位根因，待修。

## 阶段纪律 ［沿用本仓既有教训］

1. **判据先写，且必须能失败**。写完先跑一次确认 FAIL，再改实现。
2. **变异验证**：改完把修复回退一次，确认判据重新 FAIL。
   「判据全绿」不等于「判据有效」——本仓 T23 教训（fixture 照臆测字段编，
   判据全绿而实现全错）就是跳过这一步。
3. **判据只断言代码真实产出的字段**。我本次就栽过一次：先写了
   `assert tc.StreamIndex == i`，而 `flushToolCall` 根本不给 `ToolCall` 赋 `StreamIndex`
   ⇒ 恒假信号。判据必须对着**实际输出**写。
4. **期望值由独立来源算出**，不引用被测代码。
5. **每阶段结束跑全包** `go test ./internal/agent/core/ -count=1`，
   并检查存量插件 smoke 无新增 FAIL。

---

## 附：更新前后全面压测结果（2026-09-27）

方法：两版内核（旧 `85e3d66` / 新 `d3eaff4`）在**隔离 netns** 内各起一个实例，
共用同一个 mock LLM（保证 LLM 行为完全一致，消除变量），驱动脚本
`scripts/kernel-stress/cmp.py`。规模 3 = 24 连接 × 12 输入 = 288 条。

### 批内工具调用（核心差异）

| 场景 | 旧版 中位/工具数 | 新版 中位/工具数 |
| --- | --- | --- |
| `!slowbatch2` | 0.221s / **0 个** | 0.380s / **2 个** |
| `!slowbatch4` | 0.222s / **0 个** | 0.399s / **4 个** |
| `!slowbatch8` | 0.220s / **0 个** | 0.409s / **8 个** |

★ **旧版"更快"是假的**：它的 `openai.lua` 缺 `stream_index` 透传，多个分片
并到槽 0、argsRaw 混拼 ⇒ 每个工具报「参数不是合法 JSON」而**一个都没真跑**。
这正是「只看耗时会被骗」的典型，所以 cmp.py 每轮都记录**处理数**并把它当作
通过条件之一。

### 并发 vs 强制串行（新版内部对照）

`!slowbatchN`（全部 ParallelSafe ⇒ 并发）vs `!serialbatchN`（混入
`knowledge_create` ⇒ 整批降级）：

| N | 并发 | 强制串行 | 加速 |
| --- | --- | --- | --- |
| 2 | 0.378s | 0.532s | 1.41× |
| 4 | 0.389s | 0.864s | 2.22× |
| 8 | 0.409s | 1.585s | **3.88×** |

并发批耗时**几乎不随 N 增长**，串行批严格线性 ⇒ N 越大加速比越贴近上限。

### 通过率与稳定性

| 维度 | 旧版 | 新版 |
| --- | --- | --- |
| 调度器并发轰炸（288 输入） | 288/288 **100%** | 288/288 **100%** |
| 连续稳定性（20 轮无错误） | 20/20 | 20/20 |
| 内核队列 / 拒绝 / panic | 空 / 0 / 无 | 空 / 0 / 无 |

规模 1（32 输入）与规模 3（288 输入）结果**完全一致** ⇒ 可复现，非偶然。

### 本次改动与 ONNX 无关

79 个改动文件全部集中在：内核并发调度（22）、seq 插件（15）、Lua 适配器（17）、
压测脚本（4）、SDK（3）、io（3）、cmd 插件（2）。**未触及** `distill.go` /
`onnx.go` / `nlp`，而工具调用路径本身不经过 ONNX ⇒ 上面的结论对生产成立。

### ✅ 部署（已完成 2026-09-27）

生产实例已更新到 `d3eaff4`+ 并验证通过：

| 项 | 结果 |
| --- | --- |
| 二进制 | 86,496,624 → **86,784,400** 字节（onnxruntime） |
| 服务 | `active`、`kernel ready`、LLM 可达（unreachable=0） |
| 多模态空间 | `provider=chineseclip dim=512 modalities=[text image]` |
| `seq_*` 工具 | **7 个全部注册** |
| 适配器 | md5 **完全一致**（`bf1dff86…`）—— 无 `.bundled` 清单 ⇒ 首次升级不覆盖已有文件 |
| 备份 | `/var/tmp/homed-backup-20260927-194554`（含 `ROLLBACK.sh`） |

> 注：部署前生产二进制构建于**当天 06:36**，而 `seq` 引入于 `71c894c`（更晚）
> ⇒ 旧实例的 `strings /usr/local/bin/homed | grep -c internal/plugins/seq` 为 **0**。
> 它在 QQ 上如实回答"没有编排工具"**并不是说谎**，是确实没有。
> 这类"实例自述与代码状态不一致"应先查二进制构建时间，别急着怀疑提示词。

### ⚠ 部署前置条件

生产二进制是 **`-tags=onnxruntime`** 构建（strip 后 75MB、`.rodata` 62.5MB），
普通 `go build` 只有 28MB。`deploy/packaging/package-linux.sh:139` 会显式拒绝
非 onnxruntime 构建：

    if ! go version -m "$homed_bin" | grep -Eq 'build[[:space:]]+-tags=.*onnxruntime'; then
        echo "ERROR: homed 不是 onnxruntime 构建，拒绝打 server/full 包" >&2

⇒ **必须走 `deploy/packaging/build.sh`（需 `libonnxruntime.so` 与
`CHINESECLIP_BUNDLE_DIR` 资产）才能部署**，否则依存句法分析与多模态向量化失效。

---

## 附：设备命令白名单改为可配置（2026-09-27）

与上面的 toolcall 并行是同一次排查的**另一条线**，记在这里是因为它同样属于
"能力声明不该硬编码"这个主题。

### 起因

agent 通过 `device_ctl_cmdrun` 下发命令，命令在**设备侧**执行
（`cmd/waiter/device.go` 的 `exec.CommandContext`），而白名单是**源码里
硬编码的正则**（18 个命令：`ls/pwd/cat/df/…`）。`waiter.yaml` 里**没有任何键
能改它** ⇒ `find` / `grep` / `sed` / `sort` / `tr` 这些排查问题最常用的
**只读**命令一律被拒：

    device_ctl_cmdrun  device_id:waiter-fnnas  error: command not in whitelist

注意 `device_authorized` 当时**已经是 `true`**、两台 token 相同、进程正常 ——
所以"没开启设备桥授权"这个判断是错的，问题在白名单。

### 改动

`waiter.yaml` 新增 `device_cmd_allowlist`（字符串数组）：

```yaml
device_cmd_allowlist:
  - ls
  - find
  - grep
  - sed
```

- **替换**默认集而非追加：避免"以为加了 find、结果还留着 `python3 -c` 任意执行"
- 留空 ⇒ 用内置默认集（★ **绝不能变成"全放行"**，那等于静默拆掉闸门）
- 只取命令名**第一段**再整词匹配：`grep -rn x .` 能过，而 `grepXxx` / `mygrep`
  不会因 `contains` 蒙混过关

### ★ 一次真实的疏漏

waiter 有**两条**设备桥启动路径：

| 路径 | 场景 |
| --- | --- |
| `main.go` 的 `startDeviceBridge` | 交互 / 一次性模式 |
| `daemon.go` 的 `startDaemonDeviceBridge` | **`waiter --daemon`（生产两台都这么跑）** |

最初只在 `main.go` 里赋值 ⇒ daemon 路径不经过那里 ⇒ 配置**完全不生效**。
症状极难定位：**配置写了、启动也打了招呼、命令照样被拒** ——
看起来像"配置没读到"，实际是"那条路径没接线"。
已加 `TestDaemonPathAppliesAllowlist` 守住。

### ★ 已知局限：只匹配命令名，不看参数

实测（22 条白名单下）：

| 命令 | 结果 | 实际副作用 |
| --- | --- | --- |
| `find . -name x.go` | 放行 | 只读 ✓ |
| `find . -delete` | **放行** | ★ 删文件 |
| `find . -exec rm {} ;` | **放行** | ★ 执行删除 |
| `sed -i s/a/b/ f` | **放行** | ★ 原地改文件 |
| `sort -o out.txt in.txt` | **放行** | ★ 写文件 |

即：**白名单是"命令名清单"，不是"只读保证"**。用户已知悉并选择先下发
（`ship_now`），参数级拦截（拒绝 `-i` / `-delete` / `-exec` / `-o` / `> 重定向`）
作为后续项。

⇒ 写文档时不要把这一层叫"只读白名单"，那会让人以为写操作被挡住了。

### 部署

| | 106 (fnnas) | 30 (mainnas) |
| --- | --- | --- |
| 二进制 | 11,388,177 → **12,691,402** | 同 |
| 版本 | 8月27日 → `1.4.0` | 同 |
| 白名单 | 22 条（启动日志确认读到） | 同 |
| 备份 | `waiter.bak-20260927-194730` | `waiter.bak-20260927-194750` |

**顺带解答了一个悬案**：106 此前一直没有 `online` 日志，而 30 正常。
两台配置与 token 完全相同 ⇒ 差异只可能在旧 waiter 二进制。8月27日那版
落在"未 bind 时收到 ping 会关连接"的缺陷窗口里 ⇒ 更新后已正常：

    19:46:12  device waiter-fnnas online → 输出通道 device-waiter-fnnas
    19:47:30  device waiter-fnnas offline → online   （更新二进制时重连）

### 部署脚本的一个坑

`deploy/scripts/deploy-waiter.sh 里 `ssh` 会从 stdin 读，把后续 `read -p "确认更新"` 的输入吃掉：

    bash deploy/scripts/deploy-waiter.sh deploy <ip> <<< "yes"   # 喂了 yes 却打印「已取消」

脚本本身完全正常、备份逻辑没问题，只是"明明喂了 yes 却什么也没发生"。
已给 5 处 `ssh` 统一加 `-n`。

---

## 附：生产日志里的两个 toolcall 告警（2026-09-27 21:0x 排查）

部署后逐条核对了 `homeagent.service` 的告警。结论：**适配器无缺陷；
toolcall 侧一个真缺陷（已修）、一个插件侧 bug（内核自愈，非内核缺陷）。**

### 适配器：干净

| 检查项 | 次数 |
| --- | --- |
| `finish_reason=length`（输出截断） | **0** |
| `unmarshal unified response` 失败 | **0** |
| 流式分片解析错误 | **0** |
| `非法 JSON 帧` | 11，**全在 19:46:03–09 启动握手期**，此后 3 小时零发生 |

`stream_index` 透传亦已核实：生产 `adapters/openai.lua:122` 与仓库版一致。

### ① `has empty arguments` —— 真缺陷，已修

原始响应里参数**完好**：

```json
"tool_calls":[{"function":{"arguments":"{}","name":"clawhubadapter_list"},...}]
```

根因：诊断条件用 `len(tc.Arguments)==0 && RawArguments==""`，而
`parseToolArguments("{}")` 返回**非 nil 的空 map** ⇒ 零参数工具
（`seq_list` / `*_list` / `seq_help`，其 `properties` 本就是 `{}`）全部误报。
部署后共 **14 次**。

**危害不是"日志吵"**，而是这条诊断的本职是抓「上游/适配器真的丢了参数」——
真发生时会被这堆噪音淹没。**诊断日志失去信噪比就等于没有。**

修法：新增 `argsLookDropped(rawArgs)`，判 `RawArguments` 原文而非解析后的 map：
空串/空白 ⇒ 真丢；能解析成 JSON（哪怕是 `{}`）⇒ 没丢；解析失败（半截 JSON）⇒ 等同丢失。

判据 3 条，其中 `TestArgsLookDroppedEndToEnd` 用**日志里出现过的真实 body**
走 `normalizeOpenAIToolCalls` 到判定的完整接缝 —— 单测过了但接缝不对的情况，
只有端到端才抓得到。

### ② `重复申请 stage 锁` —— 插件侧 bug，内核自愈正常

```
stage.go:106   qq stage before_toolcall 失败后强制释放其持有的 stage 锁
stages.go:260  before_toolcall handler error: 插件 qq 重复申请 stage 锁（handler 内不应嵌套加锁）
```

**不要当内核缺陷去修。** 链路是：

1. `proc_main.go.tmpl:1569` —— SDK 生成的模板在**每个** stage handler 入口
   **自动**调 `callCoreVoid("stage.lock", nil)`（跨进程写锁，内核仲裁）
2. `lock.go:51` —— 锁**不可重入**：`l.held && l.owner == plugin` 即报错
3. 所以只要**同一次 `before_toolcall` 被触发两次且首次未释放**，就会命中

已排查并排除的可能：qq 的 `beforeToolcall`（`plugin.go:1303-1350`）函数体里
只有 `ctx.Lock()`（SDK **数据**锁，与 proc stage 锁是两把锁）与
`currentToolAllowed` / `clearPreviousDenial` 等纯本地调用，**无任何再次触发 stage 的路径**。

⇒ 成因在**插件进程侧的运行时**（编译进 `plugin.bin`），不在 example/qq 的业务代码里。
生产 `plugin.bin` 是 **9月14日**的独立构建产物，**不随 homed 部署** ——
要修需改 SDK 模板并重编该二进制，改动面比内核大得多。

**内核这边的行为是正确的**：`stage.go:102-106` 在插件持锁失败时强制释放，
注释写明这是"锁仲裁回内核"的自愈机制（实验 9），目的正是**避免后续插件死锁**；
`stages.go:258` 把错误收进 `ctx.Errors` 而不中断流程，所以那轮 212 秒正常跑完。

⇒ 5 次告警全部有惊无险。**唯一风险**是：哪天自愈逻辑变动，就是真死锁。
