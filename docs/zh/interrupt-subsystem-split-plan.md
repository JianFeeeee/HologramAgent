# 中断子系统独立化 —— 探测结论与实施计划

> 状态：**计划**（未实施）
> 起因：中断语义散落在 `scheduler.go` 一个 1476 行的文件里，且入口横跨 5 个文件。
> 修改任何一条中断规则（级别比较、抢占条件、栈深度）都要在多处同步改，漏一处的后果是静默的。

---

## 一、探测结论（实测，非推演）

### 1.1 现状：中断没有独立子系统

`type scheduler struct`（`scheduler.go:330`）一个结构体同时承担四件事：

| 职责 | 字段 | 归属应为 |
| --- | --- | --- |
| 就绪队列调度 | `queue` / `running` / `maxQueue` / `backpressured` | 调度器 |
| 中断分级排队 | `interruptQueues[5]` | **中断子系统** |
| 抢占与让位 | `immediate` / `preemptArmed` / `preemptLevel` / `critical` | **中断子系统** |
| 中断栈（嵌套挂起） | `suspendStack` / `maxInterruptFrames` | **中断子系统** |
| 用户停止配额 | `cancelBudget` / `stopArmed` | **中断子系统**（是"打断"的一种） |

### 1.2 中断逻辑的实际分布

| 文件 | 中断相关职责 | 行数量级 |
| --- | --- | --- |
| `scheduler.go` | 队列/抢占/栈/让位/级别语义（**主体**） | 1476 行中约 205 处引用 |
| `eventloop.go` | 中断注入路径 | 少量 |
| `overflow.go` | `raiseKernelInterrupt` 的一种来源 | 1 处 |
| `resident.go` | 子 contextfull → 父 L4 | 1 处 |
| `task.go` | 安全点让位判定 | 1 处 |

### 1.3 最能说明问题的证据

`scheduler.go:374` 那段注释：

> 链条 = 排队(L0) ← I(L1) ← I(L2) ← I(L3) ← I(L4 运行中)，被挂起 4 帧

这是**中断子系统的核心不变量**（`maxInterruptFrames = int(LevelCritical)`，结构推论、非配置项）。
它现在藏在 1476 行文件的中间，没有任何类型或文件专门承载它。改中断语义的人必须
先读完整份 `scheduler.go` 才能确认自己没漏。

### 1.4 关键有利条件（决定拆分方案）

| 探测项 | 结果 | 含义 |
| --- | --- | --- |
| 中断状态字段是否被跨文件直接访问 | **零**（`interruptQueues`/`immediate`/`suspendStack`/`preemptArmed`/`cancelBudget`/`stopArmed` 全部只在 `scheduler.go` 内读写） | 拆分可用**同包拆文件**方式，无需改包边界、无循环依赖风险 |
| `Task` / `Level` / `TaskClass` / `SchedulerStats` 定义位置 | `scheduler.go`（同包） | 可自由移动 |
| `TaskFrame` 定义位置 | `task.go`（同包） | 不需移动，同包可见 |
| 外部调用面（非 scheduler.go） | `raiseKernelInterrupt` 3 处、其余各 1~2 处，共约 15 处 | 面很小，改签名代价可控 |
| 涉及调度器/中断的测试文件 | 16 个 | 安全网足够 |

**结论**：采用**同包拆文件**（`package core` 内新增 `interrupt.go` 等），不新建子包。
理由：状态字段零跨文件访问 ⇒ 不需要为了封装而改包边界；而同包拆分零循环依赖风险、
零 API 破坏，是纯结构搬迁。

---

## 一点五、按 OS 中断语义重新界定边界（2026-10-25 用户裁定）

> 中断类似操作系统中的中断语义：**只提供中断优先级、中断向量、现场保护能力**。
> 调度器建立在中断和其他子系统提供的**基础**之上。

这把尺子把「中断」与「调度」切得很干净，也让计划里两处归属**必须修正**：

| 能力 | 归属 | 判据 |
| --- | --- | --- |
| 优先级（Level 定义/比较/clamp） | **中断** | 谁打断谁的唯一依据 |
| 中断向量（哪些待处理、按级别排队、immediate 槽） | **中断** | 待处理集合本身 |
| 现场保护（挂起现场、栈、LIFO、帧数上界） | **中断** | 被中断者的上下文保全 |
| 中断屏蔽（critical 临界区） | **中断** | 对应 CPU 的关中断 |
| 「下一个跑谁」的决策 | **调度器** | 建立在向量之上的选择 |
| 就绪队列、队列配额、转投、背压 | **调度器** | 与"打断"无关 |
| 任务 ID 分配、统计计数 | **调度器/观测** | 不是中断能力 |

### 🔴 两处必须修正的归属（原计划写错了）

**① 停止配额（`armStop`/`takeStop`/`consumeCancel`）→ 归调度器，不归中断**

原计划默认归中断，理由是"它也是打断"。按裁定这是错的：

```go
queued := len(s.queue) + pending   // ← 操作对象是**就绪队列**
if queued > s.cancelBudget { s.cancelBudget = queued }
```

它计算的是**就绪队列的长度**，是队列控制（"这批已排队的消息别再跑了"），
既不参与级别比较、也不进中断向量、更不压中断栈。停止**不是**一次中断，
它是调度器对**已排队任务**的一次批量取消。

**② `critical` 的定位要拆开看**

- `critical` 的**语义**（"运行任务处于不可打断状态"）属于中断 → 对应关中断
- `setCritical` 的**调用点**在任务生命周期里（`task.go:316/408`）→ 由调度器调用

所以它落在中断子系统的状态里、由调度器设置，与 OS 的 `cli/sti` 完全同构。

### 🔴 中断函数目前反向依赖了调度器状态（拆分时要一并处理）

实测三个中断核心函数读了这些**非中断**字段：

```
registerInterrupt      → s.running  s.stats  s.signalWake
enqueueInterruptLocked → s.maxQueue  s.allocateIDLocked  s.stats
setImmediateLocked     → s.allocateIDLocked  s.stats
```

按裁定这些都不该由中断子系统拥有：

| 反向依赖 | 正确做法 |
| --- | --- |
| `s.running`（读当前运行任务做级别比较） | 中断只接受**调用方传入**的 running；由调度器注入 |
| `s.stats`（统计） | 统计归观测，中断只发事件/回调 |
| `s.maxQueue`（就绪队列上限） | 中断队列**不该**受就绪队列上限约束；若确有上限应自有常量 |
| `s.allocateIDLocked`（任务 ID） | ID 归任务创建方（调度器），中断只接收入队 |
| `s.signalWake` | 唤醒调度器是**调度器的事**；中断登记完由调度器决定是否唤醒 |

**这说明当前"中断"与"调度"是双向耦合的**，不是单向的"调度建立在中断之上"。
拆分阶段 1 若只搬代码不动这些依赖，只是把耦合搬了个位置。因此计划新增
**阶段 1.5：切断反向依赖**。

---

## 一点六、恢复归属的精确切分（2026-10-25 用户补充裁定）

> 中断子系统就是只负责中断部分：按优先级打断、被打断的任务压栈，这些由中断子系统负责。
> **任务恢复也是中断子系统负责**。调度器是在中断子系统的基础上构筑的。

这条补充纠正了我计划的一处遗漏：原计划把恢复整体留在了调度器侧。实际上
**"恢复"有两个层面，必须分开**：

| 层面 | 内容 | 归属 | 判据 |
| --- | --- | --- | --- |
| **恢复决策** | 该恢复谁：比较「栈顶 vs 最高级待处理中断」，取高者；弹栈；产出 `(task, frame, nextSuspended)` | **中断子系统** | 这是"现场保护的逆操作"——压进去的谁、什么时候取出来，属于同一个语义整体 |
| **恢复执行** | `resumeTask`：`rebaseFramePrefix` 重建前缀 → `runTaskSteps` 跑 → `finishInputTask` → `done` | **调度器** | 这是在**执行一个任务**，与"打断/保全"无关 |

**决定性的论据**：栈的压入（`suspend`）在中断侧，若弹出与选择留在调度器侧，
那么「LIFO 语义 + 栈顶与中断队列的优先级比较」这条规则就被劈成两半——
改中断语义时又要跨两个子系统改，正是本次重构要消灭的形态。

### 因此 `nextRef` 也要按此切分

`nextRef` 目前是单个函数，它做四件事：

```go
if s.immediate != nil { ... }            // ① 中断侧：immediate 优先
qTask, qLevel := s.highestInterruptLocked()  // ② 中断侧：最高级待处理
if n := len(s.suspendStack); n > 0 { ... }   // ③ 中断侧：栈顶 vs ②取高者
if len(s.queue) > 0 { ... }                  // ④ 调度侧：就绪队列
```

①②③ 全是中断语义，④ 才是调度。切法：

- 中断子系统暴露 `nextInterruptSelection() (task, frame, sel, ok)`
  —— 内部按 ①②③ 的顺序决策，返回"该中断侧出谁"或"中断侧无货"
- `nextRef` 变成：先问中断侧；中断侧无货再取就绪队列

这样"谁先跑"的**优先级规则只有一处**（中断子系统内部），调度器只负责
"中断侧没货时取就绪队列"。

---

## 一点七、中断向量系统（本次拆分的**真正目的**）

> 独立成中断子模块就是为了方便添加中断向量系统。

### 7.1 先纠正一处术语误用

我此前把 `interruptQueues` 称作"中断向量"，**错了**。两者是不同层面的东西：

| 概念 | OS 语义 | 本仓现状 |
| --- | --- | --- |
| **中断向量** | 中断号 → 处理程序入口的映射（IDT） | **不存在** |
| **中断队列** | 待处理中断的排队 | `interruptQueues[5][]*Task` |

本仓之所以一直不需要向量表：中断任务本身就是 `*Task`，入队时已带执行体
（`Task.Kind` 决定走哪条执行路径），不需要查表找入口。
但代价是**"有哪些中断、各自什么语义"散落在调用点**，见下。

### 7.2 现状：中断源是三个裸调用点，没有统一登记

实测全部 L4 来源都走同一个函数、同一套参数形状：

```go
raiseKernelInterrupt(source, channel, text)      // 级别固定 L4，不夹取
  ├─ reportTaskPanic      → ("kernel",            "kernel", "任务 #N panic…")
  ├─ raiseContextOverflow → ("kernel/overflow",   "kernel", "上下文超页…")
  └─ handleChildContextFull→("child/"+rc.id,      "kernel", "驻留子…已满…")
```

**问题是**：这三个来源的差异只体现在 `source` 字符串上，它们的语义契约
（谁会发、级别几、是否需要模型响应、递归保护规则）全靠注释和调用方自律。

举例：`reportTaskPanic` 里那段递归保护是**手写的**：

```go
if t.Class == TaskInterrupt && t.Level >= LevelCritical { return }
// 否则同一个 panic 会自我放大成中断风暴
```

这条规则只属于"panic 中断"，却硬编码在 `reportTaskPanic` 里。
新增一种中断源时，没人会记得"要不要也加递归保护"——这就是要向量化的理由。

### 7.3 中断向量表的设计

```go
// 向量编号：稳定的、可被日志与诊断引用的中断源标识。
type VectorID string

const (
    VectorTaskPanic      VectorID = "task_panic"      // 任务 panic
    VectorContextOverflow VectorID = "ctx_overflow"    // 上下文超页
    VectorChildContextFull VectorID = "child_ctxfull"  // 驻留子上下文已满
    // 预留：未来的内核事件源在此登记
)

// interruptVector 是一个中断源的完整契约。
type interruptVector struct {
    ID    VectorID
    Level Level          // 该源的中断级别（L4 源固定 LevelCritical）
    // Build 由源自己构造中断载荷；向量表统一负责登记与让位。
    Build func(a *Agent, arg any) (text string, ok bool)
    // Guard 在登记前做前置判定（如 panic 的递归保护）。
    // 返回 false 表示本次不产生中断。
    Guard func(a *Agent, t *Task) bool
    // ExpectsReply 声明本轮是否期望模型产生回复。
    // false 表示"纯认知输入"——见 §7.5。
    ExpectsReply bool
}
```

向量表本身：

```go
var interruptVectors = map[VectorID]interruptVector{
    VectorTaskPanic:       {...},
    VectorContextOverflow: {...},
    VectorChildContextFull:{...},
}
```

统一入口：

```go
func (a *Agent) raiseVector(id VectorID, arg any) bool
```

### 7.4 收益（可验证的，不是口号）

| 收益 | 现状 | 向量化后 |
| --- | --- | --- |
| 新增一种内核中断 | 找 `raiseKernelInterrupt` 调用点照抄，容易漏规则 | 在表里加一条，规则随条目声明 |
| 递归保护 | 硬编码在 `reportTaskPanic` | 是 `Guard`，只对 panic 条目生效 |
| 级别来源 | 分散在 `interruptLevel`/`parseInterruptLevel`/`isKernelLevelSource` 三段 | 内核源由向量条目声明；插件源仍走原三段（那是外部输入，必须夹取） |
| 诊断 | source 字符串，无稳定标识 | `VectorID` 可被日志/状态面引用 |
| "哪些中断存在" | 全文搜索 `raiseKernelInterrupt` | 读向量表一处 |

### 7.5 附带解决：`ExpectsReply` 与"内核提示引起模型回复"

用户报过一个现象：内核的上下文超页提示会引发模型一整轮回复
（"主人，这条只是内核提示……"）。

根因链：超页 → `raiseContextOverflow` → L4 → 抢占 → **新任务跑完整推理** →
`finishInputTask` 无条件 `emitResponse`。

向量表里的 `ExpectsReply` 字段让"这条中断要不要产生回复"成为**声明**，
而不是靠提示词约定（现状 `tooldefs.go:218` 要求"必须认真处理"、
`offload.go:94` 又写"无需回复"，两套约定并存）。

> ⚠️ 注意：这一步会触及 `finishInputTask` 的 `emitResponse` 分支，
> 属于**行为变更**，必须与拆分分开做、单独验证。**本次拆分阶段不实施**，
> 只是把字段预留在类型里（默认 `ExpectsReply: true`，行为不变）。

### 7.6 实施顺序（在拆分之后）

中断向量系统是**拆分的目的，但不是拆分的第一步**。顺序必须是：

1. 先拆（阶段 0~3）—— 纯结构，行为零变化
2. 再引入向量表 —— 把三个调用点改成查表，**行为仍零变化**
3. 最后才做 §7.5 的 `ExpectsReply` 行为变更 —— 单独提交、单独验证

理由：如果拆文件和改语义同时做，一旦行为出问题，无法判断是搬错了还是改错了。

---

## 二、目标结构

```
internal/agent/core/
├── interrupt.go          ← 新增：中断子系统（语义 + 状态 + 全部操作）
├── scheduler.go          ← 瘦身：只留就绪队列、任务生命周期、调度循环
├── interrupt_level.go    ← 新增：Level 定义、比较、解析（可合进 interrupt.go）
└── interrupt_test.go     ← 新增：中断子系统自身的判据（现有测试保持不动）
```

### 2.1 中断子系统（`interrupt.go`）承载什么

**类型与不变量**

- `Level`（L1..L4）、`TaskClass`（就绪/中断/self）
- `effectiveLevel` / `canPreempt` / `clampPluginLevel`（级别语义**唯一实现**）
- `interruptLevel` / `parseInterruptLevel` / `isKernelLevelSource`（外部信号 → 级别）
- `maxInterruptFrames` 的推导与注释（核心不变量，必须有专门的落点）

**状态**（按 OS 语义，只留与"打断/保全"直接相关的）

- `interruptQueues [5][]*Task` —— 中断向量（按优先级分组）
- `immediate *Task` —— 已判定的最高优先待运行者（而非"抢来的槽"）
- `suspendStack []*suspendedTask` + `suspendedTask` —— 现场保护
- `preemptArmed` / `preemptLevel` —— 让位信号（"有一个更高优先级已判定，请在安全点让位"）
- `critical` —— 中断屏蔽

**明确排除**（归调度器）：`cancelBudget` / `stopArmed` / `stats` / `maxQueue` / 任务 ID 分配

**操作**

- 入队与抢占：`enqueueInterruptLocked` / `setImmediateLocked` / `registerInterrupt` /
  `requestPreempt` / `requestKernelPreempt`
- 选择与弹出：`highestInterruptLocked` / `popInterruptLocked` / `interruptCountLocked`
- 让位协议：`preemptGrantedFor` / `clearPreempt` / `rearmPending` / `canSuspend`
- 中断栈：`suspend` / `suspendDepth`（**压入**）
- **恢复决策**：`nextInterruptSelection()`（含栈顶弹出、栈顶 vs 中断队列比较）
- 唯一入口：`raiseKernelInterrupt`（内核侧 L4）

**不包含**：停止配额（`armStop`/`takeStop`/`consumeCancel`）→ 归调度器

### 2.2 调度器（`scheduler.go`）保留什么

- 就绪队列：`queue` / `maxQueue` / `enqueue` / `next` / `queueLen` / `queueSnapshot`
- 任务生命周期：`done` / `removeTask` / `allocateIDLocked` / `runningTask` / `runningFor`
- 转投：`takeQueuedInputs` / `requeueFront`
- 后台压力：`hasRoom` / `noteBackpressure` / `clearBackpressure`
- **停止配额**：`armStop` / `takeStop` / `consumeCancel`（操作就绪队列，不是中断）
- **任务 ID 分配**：`allocateIDLocked`
- **统计**：`stats`（`bumpInterruptLevel` 由调度器在登记中断后调用）
- 调度循环：`schedulerLoop` / `nextRef` / `pumpInbox` / `waitSchedulerStopped`
- **恢复执行**：`resumeTask`（重建前缀 + 跑任务 + 收尾；它不问"该恢复谁"，
  只按中断子系统给出的选择执行）
- 快照：`DumpScheduler` / `schedulerStatus`

> `nextRef` 是**两者的交界**：它按优先级从 immediate → 中断队列 → 中断栈顶 → 就绪队列
> 取任务。放在 `scheduler.go`（因为它是"下一个执行谁"的决策），但它调用的
> 中断侧查询函数归 `interrupt.go`。

---

## 二点五、实施进展（2026-10-25）

| 阶段 | 状态 | 说明 |
| --- | --- | --- |
| 0 安全网 | ✅ 完成 | `interrupt_invariant_test.go`：I1–I7 共 11 条判据，两条经变异验证确实变红 |
| 1 纯搬迁 | ✅ 完成 | 中断机制迁入 `interrupt.go`（303 行），`scheduler.go` 从 1476 → 1369 行 |
| 1.5 切断反向依赖 | ⏳ 部分 | 已建立**中断栈状态钩子**（`stackNotify`）实现单向通告；`maxQueue` 仍约束中断队列（见下） |
| 2 结构收敛 | ✅ 完成 | 划界落实：机制在 `interrupt.go`，策略在 `scheduler.go` |
| 3 验证 | ✅ 完成 | 全仓 `go test -p 1 ./...` 通过；`-race` 三关键包绿；变异验证通过 |
| 中断向量表 | ✅ 完成 | `interrupt_vectors.go`：三个 L4 源改为查表；panic 递归保护收进 `Guard`；两次变异验证均变红 |

### 最终边界（用户第四次裁定：队列归调度器）

> 「中断队列就该调度器管理啊，中断管理器只负责进入中断的任务」

这条纠正了我第一版的错误：我把中断队列（`interruptQueues` / `immediate`）
也搬进了中断管理器。**排队是调度职责**——判断该不该打断、打断不了先排队、
按什么次序再取出来，全是调度策略。

最终边界：

| 归属 | 内容 |
| --- | --- |
| **中断管理器**（`interrupt.go`） | **中断栈（唯一操作者）**、中断屏蔽（critical）、让位信号原语、**栈状态钩子** —— 只负责"进入中断"这件事本身 |
| **调度器**（`scheduler.go`） | **中断队列**（`interruptQueues`/`immediate`）、优先级语义（`Level`/`effectiveLevel`/`canPreempt`/`clampPluginLevel`）、**恢复决策**（`nextInterruptSelection`）、就绪队列、停止配额、背压 |

字段级判据：`TestInvariant_InterruptManagerHoldsOnlyStackState` 用反射白名单
钉住这一点——`interruptState` 只允许 6 个字段（栈/屏蔽/让位/上界/钩子），
把队列塞回去立刻变红（已变异验证）。

关键设计：**中断栈只有中断管理器能操作**。调度器通过 `stackTopLocked()`
**只读查询**栈顶做优先级比较，真正的弹栈由 `restoreTopLocked()` 在中断管理器内完成——
这条不变量在类型层面就无法被绕过。

**解耦机制**：`stackNotify` 钩子。中断栈变化时通告调度器，调度器据此唤醒
调度循环重新求值。中断管理器不知道调度器会怎么选，调度器也不碰栈内部——
将来给中断子系统加向量表、换多栈策略，都不必改调度器。

### 顺带修掉的真实竞态（阻塞 CI）

`-race` 实测发现两处**真实数据竞争**（非本次拆分引入，`ab27d1a` 上 CI 已红）：

| 位置 | 问题 |
| --- | --- |
| `core/stages.go:withExecuteCtx` | 往调用方共享的 `tc.Arguments` **原地写** ctx |
| `io/channel.go:bindExecCtx` | 同样原地写设备 args |

工具执行在自己的 goroutine 里，超时/取消路径下主 goroutine 仍在读同一份 args。
修法：复制 map 而非原地写。变异验证：退回原地写后 `TestPreempt_DeferredDuringToolExec`
立刻报 `DATA RACE`。

修前 `internal/agent/core` 在 `-race` 下 **30 个测试失败**（整包被判红，掩盖其它真竞争）；
修后归零。

### 阶段 1.5 遗留：`maxQueue` 确实约束中断队列

查证结果（回答原计划里的待定项）：`enqueueInterruptLocked` 读的是
`s.maxQueue`（**就绪队列**上限），并在超限时丢**最老**的中断并计 `Rejected`。

这在 OS 语义上说不通——中断队列容量不该由运行队列长度决定。
但**去掉它是行为变更**，按"结构重构不改语义"的原则本次保持不变。
建议后续单独立项：给中断队列自有常量（如 `maxInterruptQueue`），
并单独验证背压行为。

---

### 中断向量表落地（2026-10-25，本次拆分的真正目的）

新增 `internal/agent/core/interrupt_vectors.go`：

```go
type VectorID string
const (
    VectorTaskPanic, VectorContextOverflow, VectorChildContextFull
)

type interruptVector struct {
    ID     VectorID
    Level  Level                          // 每条声明自己的级别（内核源 L4）
    Source func(vectorArgs) string        // 来源标识（child/<id> 这类动态的在闭包里拼）
    Guard  func(vectorArgs) bool          // 前置判定：panic 的递归保护收在这里
    Build  func(vectorArgs) string        // 正文（模型唯一可见内容）
}
```

三个裸调用点全部改为 `raiseVector(id, args)`：

| 原调用点 | 现在 |
| --- | --- |
| `reportTaskPanic` 里硬编码递归保护 + 调 `raiseKernelInterrupt` | `raiseVector(VectorTaskPanic, …)`，保护在 `Guard` |
| `raiseContextOverflow` | `raiseVector(VectorContextOverflow, …)` |
| `handleChildContextFull` | `raiseVector(VectorChildContextFull, …)` |

**未登记 ID 会 panic**，不静默降级——否则打错 ID 会变成"中断发了但没人认领"。

判据 `interrupt_vectors_test.go`（7 条）：表结构完整性、未登记被拒、
递归保护生效、**调用点确实走向量表**（行为验证 source 来自表）。
后两条都做过变异验证：去掉递归保护 ⇒ 红；调用点绕过表 ⇒ 红
（`source="kernel-direct"`，期望 `"kernel"`）。

> ⚠️ `ExpectsReply` 字段**未实现**。按 §7.5/§7.6，它属于行为变更
> （内核提示不再引起模型整轮回复），必须单独提交、单独验证。
> 本次只完成了"规则集中"这一步，行为零变化。

---

## 三、实施步骤

### 阶段 0：安全网（先做，不可跳过）

1. 跑一次基线：`go test ./internal/agent/core/ -count=1` 记录结果。
2. 记录**中断相关测试的全量清单**（16 个文件里与 preempt/suspend/interrupt 相关的用例）。
3. 补一组**行为快照判据**，专测中断不变量（现有测试偏功能，不变量覆盖不足）：
   - 中断栈帧数上界 = `int(LevelCritical)`，且**绝不丢帧**
   - L4 遇 L4 不抢占（严格大于）
   - 栈内严格 LIFO，不按级别重排
   - 临界区内的中断只入队、不抢占
   - `effectiveLevel` 对 self/就绪任务的封顶行为

### 阶段 1：纯搬迁（零逻辑修改）

1. 新建 `interrupt.go`，把 §2.1 列出的**类型、字段、方法**原样搬入。
   - `scheduler` 的字段搬走后，`interrupt.go` 里需要一个持有者。两个选项见 §四风险 A。
2. `scheduler.go` 只留 §2.2 的内容。
3. **不改任何一行逻辑**。搬完立即跑全量测试，必须与阶段 0 基线逐条一致。

### 阶段 1.5：切断反向依赖（关键，不是可选）

搬迁后立即处理 §1.5 列出的五处反向依赖，让依赖方向变成单向
「调度器 → 中断」：

1. `registerInterrupt` 不再读 `s.running`：改由调度器传入 running 快照。
2. `s.stats` 不再被中断函数直接改：中断返回"发生了什么"，由调度器记账。
3. `s.maxQueue` 不再约束中断队列：中断队列上限若需要，用自有常量
   （`maxInterruptFrames` 同理，它是结构推论不是配置）。
4. `allocateIDLocked` 移出中断：中断只接收"已带 ID 的任务"。
5. `signalWake` 由调度器在登记后调用，不在中断内部。

> 若判断某处改动会变更行为（例如中断队列原本真的受 `maxQueue` 限制），
> **停下来先确认**，不要为了结构干净而悄悄改语义。

### 阶段 2：结构收敛

1. `maxInterruptFrames` 的推导常量与注释搬到 `interrupt.go` 顶部，作为子系统不变量。
2. `raiseKernelInterrupt` 的三个来源（panic / overflow / child-contextfull）在
   `interrupt.go` 里集中登记为注释表格，说明"L4 是统一的抢占/让位机制，
   不同功能只是它的不同用途"。
3. 检查是否有函数可降为 unexported（搬迁后可私有化的先私有化）。

### 阶段 3：验证与文档

1. 全量测试 + `-race`。
2. 在 `docs/zh/` 补一节，说明中断子系统的边界与不变量。
3. 更新 `scheduler.go` 顶部注释，指向 `interrupt.go`。

---

## 四、风险与对策

### A. 字段搬迁的持有方式（**主要决策点**）

中断状态必须与调度器共享同一把锁（`s.mu`）——它们在同一临界区里被一起读写。
两种形态：

**A1（推荐）：`scheduler` 内嵌中断状态结构体**

```go
type scheduler struct {
    mu sync.Mutex
    interruptState          // 内嵌：字段提升，访问方式 s.interruptQueues 不变
    queue []*Task
    ...
}
```

- 优点：零调用点改动（字段提升后 `s.immediate` 等写法照旧），搬迁风险最低。
- 缺点：锁定关系是隐式的（仍是同一把 `s.mu`）。

**A2：中断子系统独立持锁**

- 优点：边界更清晰。
- 缺点：`nextRef` 要同时读两个子系统，双锁必然引入顺序问题；且现有代码在
  同一临界区里同时改两者，拆锁是**行为变更**，不是重构。

**选择 A1**。理由：本次目标是把**语义集中**，不是改并发模型。A2 属于另一次
独立的设计变更，混在一起会让"行为未变"这个前提失效。

### B. 测试大量直接访问 `s.mu` 与内部字段

现有 `scheduler_preempt_test.go` 等直接构造 `scheduler` 并断言字段。
A1 方案下这些测试**不需修改**（同包 + 字段提升）。若选用 A2 则要改写测试，
进一步说明 A1 是对的。

### C. 搬迁时手滑改坏逻辑

对策：阶段 1 严格"只搬不改"，每次搬迁后立即跑测试；用 `git diff --color-moved`
复核搬移是否真的只是移动。

### D. 停止配额的归属 —— 已裁定，不再是风险

见 §1.5：它操作**就绪队列**，归调度器。此条已从"待确认"降为"已定"。

### E. `nextRef` 的归属 —— 已切分

见 §1.6：①②③（immediate / 中断队列 / 栈顶比较与弹栈）归中断子系统
（`nextInterruptSelection`），④（就绪队列）归调度器。`nextRef` 本身留在
`scheduler.go`，但只剩"先问中断侧、无货再取就绪队列"这一层编排。

> 注意：这会让 `nextRef` 的代码量明显变小——那是正确的，因为它原本混装了两件事。

---

## 五、验收判据

- [ ] `interrupt.go` 承载 §2.1 全部条目；`scheduler.go` 不再含中断队列/栈/让位字段
- [ ] 中断不变量有**专门判据**（栈上界不丢帧、L4 不抢 L4、严格 LIFO、临界区只入队）
- [ ] 全量 `go test ./internal/agent/core/` 与基线逐条一致
- [ ] `go test -race ./internal/agent/core/` 通过
- [ ] `git diff --color-moved` 显示阶段 1 为纯搬移
- [ ] 修改一条中断规则时，只需改 `interrupt.go` 一处（**这是本次重构的目的**）
- [ ] 中断向量表落地：三个 L4 来源改为查表，`VectorID` 可被日志引用
- [ ] panic 的递归保护从硬编码改为 `Guard`，只对 panic 条目生效
- [ ] 向量表引入阶段**行为零变化**（`ExpectsReply` 仅预留字段，默认 true）
- [ ] 依赖方向单向：中断子系统**不**读就绪队列/统计/任务 ID 等调度器状态
- [ ] 恢复决策（栈顶弹出、与中断队列比较）在中断子系统内；恢复执行在调度器
- [ ] 「谁先跑」的优先级规则只有一处实现（`nextInterruptSelection`）
- [ ] 停止配额归调度器（不因"也是打断"而误归中断）

---

## 六、不做的事

- 不改中断级别数值（L1..L4 与 clamp 规则保持原样）
- 不改抢占条件、不改栈深度上界
- 不拆包（`package core` 保持不变）
- 不改并发模型（仍共用 `s.mu`）
- 不动 `overflow.go` 的裁剪语义（那是上一轮已完成的工作）
- **不在本次做** `ExpectsReply=false` 的行为变更（§7.5 第 3 步，单独提交）
