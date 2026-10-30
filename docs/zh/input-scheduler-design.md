# 输入调度器设计（四级中断优先级 · 两类别 · 可抢占 · 现场保存）

> **状态：已实现并部署。** 两类别、L1–L4、可抢占与现场保存、中断栈、
> 任务级回执、`preemptCooldown` 均在生产运行（`internal/agent/core/scheduler.go`）。
>
> ★ 一个下游的真实差异（2026-10-31 补记）：rtchild（实时子 agent）**不走**这套阶梯，
> 而是用单槽覆盖（最新者胜）。原因：四级中断的目标函数是「公平、不饥饿」，
> rt 要的是「可饿死、只要最新」——两者目标相反，不该共用机制。
> 详见 `plan.md` 十一.三 B。
>
> **未实现项**不在本文维护，见 [`future-plan.md`](./future-plan.md)。
>
> **模型更正（2026-09-13，据用户澄清重写 §2/§3/§4.1/§6.3/§9/§11/§12）**
>
> 本稿早期版本把「四级优先级」当成了**所有任务**的通用优先级，并按通道名
> （qq→L2、cli→L3）由内核推断级别。那是错的。正确模型是**两类别 + 四级**：
>
> | | 中断输入（interrupt） | 排队输入（queued） |
> |---|---|---|
> | 注入 API | `InjectInterrupt*` | `InjectText*` / `InjectInputSync*` / 内核自循环 |
> | 级别 | L1–L4 | **无级别** |
> | 定位 | 需要及时处理 | 不需要及时处理 |
> | 可被谁打断 | 仅**严格更高级**的中断 | **任何**中断 |
>
> 级别（“这项工作有多不能等”）由来源在 `InjectOptions.Priority` 里声明。
> L1–L3 任何插件可声明；**L4 是“立即打断”能力**，只有**内核自身**（panic /
> 内核事件 selfip，经 `raiseKernelInterrupt`）与**内核级插件**（编译期内置插件，
> 如 WebUI 的终止按钮）能用。外部插件的 L4 会被夹到 L3。
> 类别由**用哪个注入 API**决定，与通道名无关——QQ 走的是 `InjectInterruptTextOpts`，
> 所以它是**低级别中断（L1）**，不是排队输入。

> 分支：`feature/input-semantics`
> 状态：**设计稿 v1**（待确认项见 §12，未确认处按 §12 的「默认取值」推进）
> 影响面：`internal/agent/core`、`internal/agent/io`、`internal/sdk`（**仅内部**）
> 公开 SDK：**v1 不改**（`third_party/homeagent-sdk/sdk/` diff 必须保持为 0，理由见 §13）

---

## 1. 背景：现状与要解决的问题

现有两条输入语义（见审查结论），都建立在**串行 `eventLoop`** 之上：

| 语义 | 入口 | 路径 |
|---|---|---|
| 排队 | `InjectText` / `InjectInput*` | `io.inputCh` → `eventLoop` → `processInput` → `process()` |
| 中断 | `InjectInterruptText` / `InjectInterrupt*` | `io.interruptCh` → `interceptLoop` → `cancelLLM` + `a.interceptCh` |

已确认的具体问题（均为源码事实）：

1. **队头阻塞**：`eventLoop` 单 goroutine，`process()` 全程持 `a.mu`（`internal/agent/core/process.go:103-104`），一轮对话（含 N 轮工具）期间后续输入全部排队。
2. **中断只在一种时刻成立**：`interceptLoop` 有三条降级回排队的路径——当前无 LLM 在跑、当前轮是 `_consolidation_`、`a.interceptCh` 满（`internal/agent/core/eventloop.go:74-116`）。它不是独立管线，而是"抢占 + 三次降级"。
3. **回执误投风险**：`ResponseCh` 只由全局 `emitResponse` 写（`eventloop.go:475`），无任务归属。一旦引入抢占，中断的回执会写进被挂起任务的等待者。
4. **断链点静默**：`processInput` 有多条提前 `return` 而不 `emitResponse` 的路径（`resolveInput` 失败 `:309`、10s 去重命中 `:317`、`_consolidation_` `:326`），同步调用方（`cli`、`clawhubadapter` 无超时）永久挂起。
5. **背压策略分裂**：`inputCh` 满 → 阻塞发送方；`selfInputCh` 满 → 静默丢弃；`a.interceptCh` 满 → 降级回 `inputCh`。
6. **假取消**：工具超时只是放弃等待，内层继续执行且副作用照做（`internal/agent/core/toolcall.go:33-45`）。
7. **不可观测**：没有任何"当前在跑什么、谁被挂起、降级了多少次"的统一入口。

**目标**：把上述隐式行为收敛成一个**显式、可 dump、可单测、可断言**的调度器。

---

## 2. 术语与模型

```
Task = { id, class, level, origin, frame, state, enqueueAt, preemptCount, responseCh }
  class: queued | interrupt       // **类别由注入 API 决定，与通道名无关**
    queued    —— 无级别；用于“不需及时处理”的场景；可被**任何**中断打断
    interrupt —— 带级别 L1..L4；仅被**严格更高级**的中断打断（被打断则压入中断栈）
  level: 仅 interrupt 有意义（queued 恒无级别，effectiveLevel 视作 0）
  state:  ready | running | suspended | done

TaskFrame = {
    msgs []Message            // 本任务自己的 LLM 消息序列
    step  Step                // 下一个要执行的 step（安全点游标）
    turn  int                 // 已完成的工具轮数
    toolIdx int               // 当前工具批内的下标
    toolResults []ToolResultItem
    toolsUsed []string
    lastBatchReplyOnly bool
    stageCtx *sdk.StageContext
    budget TokenBudget
    outputChannel string      // 该任务的输出通道
    responseCh chan<- *OutputEvent  // 任务级回执信道（可为 nil）
    startedAt time.Time
    input string              // 触发本任务的输入文本（COMMIT 时写记忆）
    noMemory bool
    originSource string
}

Step（枚举，顺序执行，步与步之间是安全点）：
  S_PREPARE     构建 msgs / 应用中断标记 / 合并 stage 上下文
  S_LLM         LLM 流式调用（**可抢占**：cancel 即丢弃）
  S_POST        StagePostAction
  S_LLM_JUDGE   无 tool_call → 去 S_BEFORE_OUTPUT；有 → 去 S_TOOL_BEGIN
  S_TOOL_BEGIN  replyOnly 判定 + 取当前 tc
  S_TOOL_BEFORE StageBeforeToolcall
  S_TOOL_EXEC   工具执行（**临界区，不可抢占**）
  S_TOOL_AFTER  StageAfterToolcall
  S_TOOL_NEXT   批内下一个 / 批结束 → S_LLM
  S_BEFORE_OUTPUT / S_AFTER_OUTPUT
  S_COMMIT      context.Append + emitMemoryCandidate（**原子，不可抢占**）
  S_FINISH      写 responseCh、发事件
```

四个容器（**不是**“三集合并成一个比较器”）：

| 容器 | 内容 | 取出规则 |
|---|---|---|
| `interruptQueues[1..4]` | **中断队列**，每条队列一个级别 | 从 L4 到 L1 依次扫描；同级 FIFO |
| `immediate` | 刚抢占成功的那一条中断（**至多一个**） | 最先取出——抢占必须**立即生效** |
| `queue` | **排队输入**形成的新任务 | 纯 FIFO（无级别可比） |
| `suspendStack`（**中断栈**） | 被打断、保存了现场的任务 | **LIFO，只比栈顶**；栈内不做重排 |

> 用词（已更正）：它**就是中断栈**。用户明确存在「中断被中断」的场景，被打断的现场必须压栈；
> 因此恢复纪律是**严格 LIFO（只比栈顶）**，栈内不做优先级重排。
> 早期稿把它写成“不是栈、按优先级取”是错的。
>
> 早期稿还让 `immediate` 与别的容器共用同一个比较器，于是出现“抢占成功后，
> 抢占者与被挂起者同级 → 原任务被立刻选回 → 抢占空转”——为此打的
> “同级 pending 优先”补丁已删除：抢占者根本不进队列。

---

## 3. 优先级（只属于中断）

### 3.1 两类别 + 四级

**类别（`TaskClass`）由注入 API 决定，与通道名无关**：

| 类别 | 注入入口 | 级别 | 可被谁打断 |
|---|---|---|---|
| `queued` 排队 | `InjectText*` / `InjectInputSync*` / `InjectInputMedia*` / 内核自循环（`selfInputCh`） | **无** | **任何**中断（L1 也能） |
| `interrupt` 中断 | `InjectInterrupt*` | L1–L4 | 仅**严格更高级**的中断 |

**级别（`Level`）语义是“这项工作有多不能等”**：

| Level | 名称 | 语义 | 典型来源 |
|---|---|---|---|
| `L4` | CRITICAL | 内核紧急 | **内核独占**：panic 中断、内核事件中断（selfip） |
| `L3` | INTERACTIVE | 需及时处理 | 时钟/定时器到达、终端输出、交互输入 |
| `L2` | MESSAGE | 一般提醒 | 插件希望尽快看到、但不紧急的提示 |
| `L1` | BACKGROUND | 完全可等 | 异步消息（QQ/微信）、批量通知 |

- **`queued` 没有级别**：它本就是“不需及时处理”的那一类，
  所以“可被任何中断打断”不是漏洞而是定义（`effectiveLevel(queued) == 0`）。
- **默认级 = `L1`**：未声明一律最低级（“显式才是特权”，新插件不会默认拿到抢占权）。

### 3.2 级别从哪来

| 来源 | 可达级别 | 入口 |
|---|---|---|
| 普通插件（外部，独立进程/动态库） | L1–L3 | `InjectOptions.Priority`（空/非法 → L1；L4 被夹到 L3） |
| **内核级插件**（编译期内置，`init()` 自注册） | L1–**L4** | 同上；L4 用于实现**中断能力**，例如 WebUI 的终止按钮 |
| 内核自身 | L4 | `(*Agent).raiseKernelInterrupt`（panic / selfip） |

- ❌ **不是运维可调项**。不引入 `core.agent.priority.<channel>` 这类配置键，
  也不把 `PriorityLookup` 做成可注入的策略表。
- ✅ 插件**可以声明**自己中断的级别（这不是“把内核内部属性外化”，
  而是调用方声明它自己那件事有多不能等）。
- ✅ **L4 给“立即打断”能力**：内核自身（panic / selfip）与**内核级插件**
  （编译期内置插件，如 WebUI 终止按钮）可声明。为什么必须给内置插件：
  用户按下终止按钮时，内核需要一条能立刻打断当前任务的中断；这条能力不能给
  外部插件，否则任何第三方插件都能随时打断用户的一切工作。
- **判据是“这个插件是不是编译期内置”，不是它自报的名字**：
  - 第一道闸在 **proc 桥**（外部进程的唯一入口）：走它的一律把 L4 夹到 L3。
    在这里夹而不是只按 `source` 判，是因为 `source` 是插件自报字段、可以冒名。
  - 第二道闸在 **core**：`isKernelLevelSource(source)` 查
    `pluginReg.IsBuiltinPlugin`，只有内置工厂才承认 L4（纵深防御）。
  - `source` 的约定是 `插件名` 或 `插件名/实例`（如 `webui/<deviceID>`），
    判据取第一段——否则带设备身份的 WebUI 来源会被误判成外部插件。

### 3.3 抢占判据

```go
effectiveLevel(queued) == 0
canPreempt(incoming, running) = incoming.Class == TaskInterrupt
                              && effectiveLevel(incoming) > effectiveLevel(running)
```

因为 `queued` 的有效级恒为 0，这一个比较同时覆盖两条规则：

```
running 是排队任务      → 任何中断（≥L1）都抢占
running 是中断 Li       → 只有 Lj > Li 的中断抢占（严格大于）
incoming 是排队输入     → 永不抢占
```

**严格大于才抢占**；相等一律入队——这条保证确定性，也是“较低无法打断较高”的字面实现。

## 4. 调度规则

### 4.1 选择函数（四容器 · 固定次序）

任务结束、或运行任务到达安全点且存在待处理抢占请求时，执行：

```
1. immediate 非空           → 取它（刚抢占成功的中断，抢占必须立即生效）
2. 中断队列非空             → 取 L4→L1 中最高级非空队列的队头（同级 FIFO）
3. 中断栈非空（与 2 比高）  → 栈顶有效级 ≥ 队头级别 ? 弹栈顶 : 取队头
4. queue 非空              → 取队头（纯 FIFO）
5. 都没有                  → 空闲（阻塞等新输入 / 新中断）
```

- **中断栈只把栈顶**放进比较（严格 LIFO）——栈内更老的任务即使因饥饿防护
  提升了有效级，也不得越过栈顶；“后被打断的先恢复”才是栈语义。
- 第 3 步就是用户给的规则：“先判断中断队列是否为空，同时判断中断栈中任务的
  优先级，哪个优先级高取出哪个”。栈顶是 `queued`（有效级 0）时，任何中断都赢。
- 第 1 步的存在，使“抢占者与被抢占者同级”这个比较**根本不会发生**：
  抢占者不经队列。这是删除早期“同级 pending 优先”补丁后的正确形态。
- 排队任务只在中断与挂起现场都处理完后才执行——这正对应“排队输入用于
  不需要及时处理的场景”。

### 4.2 安全点（可切换点）

**只有 step 与 step 之间是安全点。** 明确：

- ✅ `S_LLM` 之后、`S_TOOL_BEFORE` 之后、`S_TOOL_EXEC` **之后**、`S_TOOL_AFTER` 之后……
- ❌ `S_TOOL_EXEC` **执行中不是安全点**：工具副作用不可回滚，无法"保存现场"。

### 4.3 临界区

```
CriticalSection：step 标记 nonPreemptible = true，或任务进入声明区间
```

- 实现方式：**调度器在临界区期间不求值抢占**（协作式单线程下即"不 yield"），不使用 `sync.Mutex`。
- 资源互斥不用锁，而是**调度器持有的资源表**（若某资源被 running 占用，则不会选出同样占用它的任务）——纯数据判定，天然无优先级反转。
- **v1 临界区清单**（显式列出，避免"隐式临界区"）：

| 临界区 | 理由 |
|---|---|
| `S_TOOL_EXEC`（单次工具执行全程） | 副作用不可回滚；插件 RPC 不可取消 |
| `S_COMMIT` | 上下文/记忆写入必须原子 |
| 需 ONNX 嵌入的 `S_PREPARE` 片段 | ONNX `Run` 不可取消 |
| 媒体 CAS 落盘 | 同上 |
| 显式声明的 `_consolidation_` 类任务 | 记忆一致性 |

- 临界区期间到达的抢占请求**不丢失**：按级别进入中断队列，在临界区结束后的第一个安全点重新求值。

### 4.4 背压（v1 统一为一种）

- `readyQueue` 有界（默认 256，可配）。
- 满时：**阻塞发送方**（与现状 `inputCh` 一致，避免静默丢用户输入），但必须**计数并打日志**。
- 中断队列合计有界（默认同 `maxQueue`）；满时**丢弃最低级别里最老的一条并计数**（中断是提示性输入，宁可丢旧保新）。
- 中断栈帧数上界是**结构推论 = 4**（见 §6.3），不是配置项。

---

## 5. 中断语义

### 5.1 中断产生线程的职责（钉死）

`interruptLoop` 只做三件事，**绝不触碰任何 TaskFrame**：

```
① 从 io.interruptCh 收中断 → 定级（读 payload["priority"]，插件声明 L1..L3）
② 决策（scheduler.registerInterrupt 内）：
     canPreempt(incoming, running) 且 running 不在临界区
        → 置让位信号 + 把 incoming 放进 immediate 槽，并返回 true（调用方据此
          取消当前可取消的 step，即 LLM 流式）
     否则
        → 按级别进入对应的中断队列
③ 唤醒调度器（scheduler.wake，cap 1）
```

共享面仅三处：让位信号（`preemptArmed`/`preemptLevel`）、中断队列、`critical` 原子标志。
**帧的保存与恢复只能由调度器做。**

### 5.2 三种情形的统一

现状的三条降级路径在新模型里不再需要特殊分支：

| 情形 | 旧模型 | 新模型 |
|---|---|---|---|
| LLM 在跑，正常 | 真抢占（同轮 continue） | 真抢占：`S_LLM` 取消，任务 A **压入中断栈**，中断任务 B 从 `S_PREPARE` 启动 |
| LLM 没在跑 | 降级为排队 | B 按其级别入中断队列（空闲时即被 `wake` 唤醒并选出） |
| `_consolidation_` 中 | 降级为排队 | `_consolidation_` 是后台**临界区**（且它是排队任务）→ B 入中断队列，临界区结束后求值 |
| `a.interceptCh` 满 | 降级为排队 | 不存在该队列；中断队列有界，满则丢最低级别里最老的一条 |

### 5.3 中断任务与被打断任务的关系（**已定：D1 = 方案 B**）

> 用户明确：
> *“中断打断时，上个任务到达以来的所有上下文现场被保护（含 toolcall），
> 然后中断在**上个任务前的那个完整状态**上开始运行。中断运行结束，再把被挂起的
> 任务与其上下文现场**加载回中断任务之上**，并继续运行。”*

因此语义是：

1. **被挂起任务的现场 = 它自到达以来累积的全部上下文（含 toolcall 结果）**，
   原样保存在 `TaskFrame` 里。
2. **中断任务从「上一个任务之前的完整状态」开始运行**——它**看不到**被打断
   任务的任何部分进展。等价于：中断任务就是一个普通新任务，正常走 `S_PREPARE`
   （重建 system prompt + timeline + 自己的输入）。
3. **中断结束后，把被挂起任务与其现场加载回「中断任务之上」再继续**：
   中断已提交的那段上下文留在**下面**（成为重建前缀的一部分），本任务自己的
   现场接回**其上**。

实现对应（`internal/agent/core/task.go`）：

- `TaskFrame.PrefixLen` 记录 prepare 段构建的**基础前缀**长度
  （system + timeline + 用户输入）；其后的 Stage 上下文与工具轮产物都是“自己的现场”。
- `rebaseFramePrefix(f)`：恢复时重建基础前缀（因中断结束已把它的输入/输出提交进
  `a.context`，重建出的 timeline 已含中断效果），再把 `f.Msgs[PrefixLen:]` 原样接回；
  并补回 prepare 段的両处尾部改写（`IsInterrupt` 的 `[中断消息]` 标记、输入多模态块）。
- 调用点：`resumeTask` 在 `runTaskSteps` **之前**调用它。

> 代价（已知且接受）：中断看不到“进行到哪一步”，所以“别搜了改成 X”这类指令
> 只能靠它自己重新理解；换来的是中断起点总是一个**一致的完整状态**。

---

## 6. 保存现场与恢复

### 6.1 保存

在安全点被抢占时：

```
suspendStack.push(Task{frame: running.frame, state: suspended,
                      step: running.frame.step, enqueueAt: running.enqueueAt})
running.state = done_for_now
```

- **只保存数据帧**，不保存 goroutine 栈（这正是"单调度 + 隐式状态机"优于"park goroutine"的地方）。
- `S_LLM` 被抢占时：**不完整的 LLM 请求直接丢弃**（LLM 调用幂等、无持久副作用）；恢复时从 `S_LLM` **重发**，`msgs` 与抢占前一致（即"请求前"的状态）。
- 已提交的副作用（已执行的工具、已 append 的 context）**不回滚**——帧里记录的 `toolResults` 会保留，恢复后继续。

### 6.2 恢复

从**中断栈栈顶**取出后：

1. **重建基础前缀**（`rebaseFramePrefix`）—— 此时中断任务已结束并提交，
   重建出的 timeline 包含中断的输入/输出，即“现场加载回中断任务之上”；
2. 把本任务自己的尾部（Stage 上下文 + 工具轮产物 + 占位）原样接回；
3. 从 `frame.Step` 继续执行。

被丢弃的只有那次**不完整的 LLM 请求**（幂等），已执行的工具与已累积的
`toolResults` 全部保留。

### 6.3 嵌套

- 允许中断任务自身被更高级中断抢占（嵌套）。
- **中断栈帧数上界 = 4，是结构推论而不是配置项**：
  链条 = `排队(L0) ← I(L1) ← I(L2) ← I(L3) ← I(L4 运行中)`，
  被挂起 4 帧；L4 之上没有更高级别，链到此为止。
  （插件可达级别只到 L3，所以插件链最多挂起 3 帧 + 底层排队任务；
  第 4 帧只能由内核 L4 制造。）
- 栈自底向上的**基础级**天然递增（能被抢占者必然级别更高），因此栈顶通常就是最高级任务。
- 超限在正确模型下不可达：`susp` 处只做**防御性计数**（`Rejected++`），
  **不降级、不丢弃帧**——帧丢了会丢副作用记录。早期稿写的“超限转 pendingInterrupts”已删除。

## 7. 回执路由（任务级）

**必须改**：`ResponseCh` 从"全局 `emitResponse` 的对象"上升为 `TaskFrame.responseCh`。

```
emitResponse(task, ...)      // 写 task.frame.responseCh，而不是"当前全局通道"
```

- 抢占场景下，中断任务 B 的 `S_FINISH` 只可能写 `B.responseCh`，绝不会写进被挂起的 `A.responseCh`。
- **不变量**：**每个任务在 `S_FINISH` 必然产生且仅产生一个终态事件**（无论成功、失败、被跳过）。`processInput` 现有的三个提前 return（解析失败、去重、consolidation）在新模型里都必须转成"任务以 `skipped` 终态结束并回执"。
- 这顺带修掉现有缺陷：`cli`（`internal/plugins/cli/plugin.go:242`）与 `clawhubadapter`（`internal/plugins/clawhubadapter/plugin.go:1045`）的同步注入在断链时会永久挂起。

---

## 8. 并发结构（两个 goroutine）

```
schedulerLoop（唯一持有任务状态与帧）
   for {
     if 可切换 && preemptionRequest 有效 → 执行抢占（保存现场）
     if running == nil → pick from 三集合；无候选则等待 inbox
     runOneStep(running)          // 可能是阻塞调用（见 §8.2）
     处理 step 结果 → 推进或结束任务
   }

interruptLoop（不持有任何帧）
   收 interruptCh → 定级 → 决策 → 置 preemptionRequest + cancel + wake scheduler
```

### 8.1 不变量

| # | 不变量 |
|---|---|
| I1 | 任意时刻至多一个 `running` 任务（"一个 running"约束的是**副作用**，不只是 CPU） |
| I2 | 任务帧只由 `schedulerLoop` 读写；`interruptLoop` 只写 `preemptionRequest` / 读 `stepCancel` |
| I3 | 任何跨挂起点的状态都是纯数据，不持有锁 |
| I4 | 安全点只在 step 边界；工具执行中与 COMMIT 不是安全点 |
| I5 | 每个任务恰好一次终态事件（含 `responseCh` 写入） |
| I6 | 调度器本身永不退出（panic 只使当前任务失败） |

### 8.2 关于"调度器不被阻塞"（**待确认 D2**）

> **M3 拆分的理由**：真正的挂起要求帧跨越 `prepare → step… → finish` 全生命周期。
> 若只把 `process()` 改成可挂起，`processInput` 会在挂起返回后继续执行
> `context.Append` 与 `emitResponse`——造成重复提交。故 M3 分为 M3a（所有权重构，
> 行为等价）与 M3b（抢占语义）两步。

v1 采纳：**`S_TOOL_EXEC` / ONNX / CAS 属于临界区，调度器在这些 step 上会阻塞进插件 RPC / 原生调用。** 这是有意的取舍：

- 好处：与"两个 goroutine 就够"一致，实现简单，无临时 goroutine。
- 代价：这些临界区期间**中断只能排队，不能抢占**。换言之，**中断的有效窗口 = `S_LLM`**（与今天的实际行为相同，但现在是显式声明而非隐式结果）。
- 演进（v2）：把 `S_TOOL_EXEC` 改成异步 step（临时 goroutine + 完成事件），并给插件协议加 `tool.cancel`。此路径在文档保留，不在 v1 实现。

### 8.3 panic 隔离与 panic 中断

- `runOneStep` 外包 `recover`：panic → 当前任务标记 `failed`，**调度器继续**。
- panic 同时**产生一条内核 L4 中断**（`reportTaskPanic` → `raiseKernelInterrupt`）：
  内核把自己发生了 panic 这件事作为最高级中断通知给调度器，让 agent 能知情/善后。
- 递归保护是**结构性**的：若 panic 的任务本身就是 L4 内核中断，不再产生新的 L4——
  否则同一个 panic 会自我放大成中断风暴。
- 取代现有 `eventLoop`/`interceptLoop` 的 `recover → sleep 1s → go loop()` 无退避重启（`eventloop.go:19-22,38-42`）。

---

## 9. 失效模式与防御

| 失效 | 防御 |
|---|---|
| 饥饿（高优先级流反复抢占） | `preemptCount` 提升有效级：`effectiveLevel = min(4, baseLevel + min(preemptCount, 2))`；被抢占 +1。**只对中断生效**——排队任务无级别，按定义可被任何中断打断 |
| 无界下潜 | 中断栈帧数上界 4（结构推论 = 中断级数）；超限只做防御性计数，**不降级不丢帧** |
| 中断请求堆积 | 中断队列合计有界，满则丢最低级别里最老的一条并计数 |
| 就绪队列满 | 阻塞发送方 + 计数（不静默丢） |
| 同一任务反复被打断 | `preemptCount` 达阈值后有效级提升；另设**抢占冷却**：刚被抢占的任务在 `cooldown` 内不再被同级/更低级抢占 |
| 任务永不结束 | 每任务 `maxTurns`（主循环目前缺失，见审查 P0）+ 每步超时 |
| 不可观测 | `Scheduler.Dump()` 原子快照 + 事件（切换原因、降级次数、丢弃次数） |

---

## 10. 非目标（v1 明确不做）

1. 工具级取消 / 可抢占工具（`tool.cancel`）。
2. 多 agent 并行（仍是单 agent 单调度器）。
3. 公开 SDK 接口变更。
4. 微抢占（任意指令级）。
5. 跨进程恢复（帧不落盘）。

---

## 11. 测试点、测试方式与预期结果

测试基础设施（先于 M1 落地）：

- **假时钟** `Clock` 接口（`Now()` / `AfterFunc`），生产用真实实现，测试注入可控时钟。
- **假 Provider**：实现 `agentAPI.Provider`，返回脚本化的 `tool_calls` 序列（支持"第 N 次调用时挂起直到放行"）。
- **假工具**：测试内 `StageHost.RegisterTool` 注册，可控制每次执行耗时、是否返回错误、是否触发中断注入。
- **同步栅栏**：测试通过 `scheduler.Inbox` 注入中断并用 `runtime.Gosched` + 显式 `waitFor(state)` 断言，不用 sleep 猜时序。
- **快照断言**：`scheduler.Dump()` 返回 `{running, queue, interruptQueues[1..4], immediate, suspendStack, counters}`，测试对纯数据断言。

### 11.1 优先级与抢占

| 编号 | 测试点 | 方式 | 预期结果 |
|---|---|---|---|
| P1 | 更高中断抢占中断 | running=L2 在 `S_LLM`；注入 L3 中断 | L2 压入中断栈（step=S_LLM）；L3 进 `immediate` 并变 running |
| P2 | 相等级别不抢占 | running=L2 中断在 `S_LLM`；注入 L2 | 不抢占；请求入 L2 中断队列；running 不变 |
| P3 | 更低级别不抢占 | running=L3 中断；注入 L2 | 同上，不抢占 |
| P4 | 逐级抢占嵌套 | 排队任务 → L1 → L2 → L3 → L4，均在 `S_LLM` | 中断栈深度依次 1/2/3/4；每层 step 均为 S_LLM |
| P5 | 抢占后在安全点才生效 | running=排队任务在 `S_TOOL_EXEC`；注入 L4 | 抢占**不立即生效**；工具返回后才保存/切换；`deferredPreemptions==1` |
| P6 | 临界区不可抢占 | running 声明临界区；注入 L4 | 同上；L4 请求留在中断队列，临界区结束立即被选中 |
| **P7** | **排队任务被任何中断打断** | running=排队任务；注入 **L1** 中断 | L1 也抢占成功（排队任务有效级 0） |
| **P8** | **排队输入永不抢占** | running=任意任务；注入排队输入 | 不抢占，入排队队列 |
| **P9** | **外部插件不能声明 L4** | 外部来源声明 `Priority="L4"` | 被夹到 L3（proc 桥 + core 双重） |
| **P11** | **内核级插件可用 L4** | 内置插件（如 webui）声明 `L4` | 得到 L4 并立即打断当前任务（终止按钮） |
| **P10** | **panic 产生 L4 中断** | 任务 panic | 产生一条带 `kernel=true` 的 L4 中断；L4 自身 panic 不再递归 |

### 11.2 保存现场与恢复

| 编号 | 测试点 | 方式 | 预期结果 |
|---|---|---|---|
| R1 | 在 `S_LLM` 抢占后恢复 | 构造 A 在 `S_LLM` 被 B 抢占；B 结束 | A 恢复后**重新发起** LLM 请求；`msgs` 与 A 被抢占前**逐字节相同**；不重复执行已完成的工具 |
| R2 | 在 `S_TOOL_BEGIN` 抢占后恢复 | A 完成 1 个工具批后于 `S_TOOL_BEGIN` 被抢占 | A 恢复后继续**下一批**工具；`toolResults` 长度不变 |
| R3 | 恢复结果与不中断一致 | 同一脚本跑两次：一次中途注入中断，一次不注入 | 两次最终 `context` 事件序列**除"中断任务自身的事件"外一致**；A 的 `toolsUsed` 顺序相同 |
| R4 | 嵌套恢复顺序 | L4→L3→L2 依次抢占后依次结束 | 按有效级/到达序恢复；每个任务的 `frame.step` 与其被挂起时一致 |
| R5 | 不完整的 LLM 请求被丢弃 | 假 Provider 在流式途中触发中断 | 该次请求被 cancel；**不产生任何 `msgs` 追加、不产生 tool_call**；恢复后重发次数 = 1 |

### 11.3 回执路由

| 编号 | 测试点 | 方式 | 预期结果 |
|---|---|---|---|
| X1 | 任务级回执不误投 | A 为同步任务并已挂起；B 为同步中断 | B 的回执只到 `B.responseCh`；`A.responseCh` 在 A 恢复并结束后才收到自己的回执 |
| X2 | 断链路径必有终态 | 分别构造：解析失败、10s 去重命中、`_consolidation_` | 三种都产生 `skipped` 终态事件并回执；同步调用方**不挂起** |
| X3 | 每任务恰一次终态 | 统计 `S_FINISH` 次数 vs 任务数 | 相等（I5），无重复写入 |
| X4 | 无超时同步注入不再永久挂起 | `cli` 路径（无超时）注入一条会被去重的输入 | 返回 `skipped` 回执而非永久阻塞 |

### 11.4 队列与选择

| 编号 | 测试点 | 方式 | 预期结果 |
|---|---|---|---|
| Q1 | 中断队列按级别扫 | 四条中断队列各放一个，入队顺序与级别相反 | 取出顺序 L4→L3→L2→L1；中断耗尽后才是排队任务（FIFO） |
| Q2 | 挂起现场优先于新排队工作 | A 被抢占挂起 + B 为新排队输入 | A（栈顶）先被选中 |
| Q3 | 栈顶 vs 中断队头 | 栈顶 L3 + 队头 L2 / 栈顶 L3 + 队头 L4 / 栈顶为排队任务 + 队头 L1 | 分别取 栈顶 / 队头 / 队头 |
| Q4 | 就绪队列背压 | readyQueue 满后注入排队输入 | 发送方阻塞 + 计数 +1；不静默丢弃 |
| Q5 | 中断队列溢出 | 中断队列合计满后注入更多 | 丢**最低级别里最老**的一条 + 计数；其余保持 |
| **Q6** | **immediate 最优先** | `immediate` 非空且中断队列里有更高级别 | 取 `immediate`（抢占必须立即生效） |

### 11.5 深度、饥饿与并发

| 编号 | 测试点 | 方式 | 预期结果 |
|---|---|---|---|
| D1T | 下潜深度上界（结构推论） | 挂起 3 帧后继续注入；再挂起到 4 帧 | 3 帧时 `canSuspend()==true`；4 帧（全链：排队+L1+L2+L3，L4 运行中）时为 `false` |
| G1 | 饥饿防护（抢占提升） | 对同一 **L1 中断**连续抢占 5 次（同级/高级交替） | `effectiveLevel` 提升至 `min(4, 1+2)=3`；第 3 次后不再被 L1/L2 抢占 |
| G2 | 冷却生效 | 同一中断刚被抢占后立刻再注入同级中断 | 冷却期内不抢占，请求入中断队列 |
| **G3** | **提升也必须只在中断间生效** | 排队任务被连续抢占 | 排队任务有效级恒 0（不被提升；它按定义可被任何中断打断） |
| K1 | panic 隔离 | 假工具 panic | 只有该任务变 `failed`；调度器存活；后续任务正常执行 |
| K2 | 竞态检查 | 全部调度用例加 `-race` | 无数据竞争报告 |
| O1 | 快照一致性 | 在任意 step 边界调 `Dump()` | 返回的 `running/ready/pending/suspend` 三集合互不重叠且总数守恒 |
| O2 | 切换可观测 | 每次抢占/恢复 | 产生一条事件（任务 id、原因、from→to、level） |

### 11.6 端到端

| 编号 | 测试点 | 方式 | 预期结果 |
|---|---|---|---|
| E1 | 真实 provider + 假长工具 | 启动内核，用一个会阻塞 5s 的假工具跑 L1 任务，途中经 `interceptCh` 注入 L4 中断 | 中断**在工具执行期间不被处理**；工具返回后立即抢占；中断任务先完成；原任务恢复并完成 |
| E2 | LLM 流式中断 | 假 Provider 慢速流式返回 | 中断后当前流被 cancel，任务挂起，中断任务完成，原任务恢复并重新请求 |
| E3 | 现有 e2e 回归 | 跑 `internal/plugins/integration_test.go`、`real_plugin_smoke_test.go` | 行为不变（除文档化的语义变化） |
| **E4** | **优先级压力（用户指定形状）** | 固定内容假 provider（**记延迟，且被取消时立刻返回**），100 条排队输入 + 100 条中断（L1/L2/L3/L4 各 25）混合打入；每条中断都等到“该被它打断的受害者正在跑”时才注入 | 200 个任务全部到达终态；`Rejected=0`；各级登记数 = 25；**各级抢占数都 > 0**；排空后 `Suspended == Resumed`；每次“取消流式段”都换来一次挂起 |
| **E5** | **嵌套到结构上限并 LIFO 展开** | 排队任务运行中依次注入 L1→L2→L3→L4（每级都等上一级在跑） | 栈深峰值恰好 **4**（= 结构上限，`canSuspend()==false`）；恢复顺序严格 LIFO `[L3, L2, L1, 排队]`；`Suspended==Resumed==4` |

> E4/E5 的 provider 必须**感知 ctx 取消**：否则抢占只能等任务自然结束，
> 测到的全是"步骤之间让位"，流式段的取消路径（真正的现场保存/恢复）压不到。
> 实测：不感知取消时 `LLM完成 == 任务数`、挂起接近 0；感知后取消次数与挂起次数一一对应。

---

## 12. 待确认决策（含默认取值）

> 未获异议时按"默认取值"实现；每项单独一个 commit，便于回退。

| 编号 | 问题 | 默认取值 |
|---|---|---|
| **D1** | 中断任务的上下文 | **方案 B（已定）**：中断从上一个任务之前的完整状态开始；恢复时把被挂起任务的现场加载回中断之上 |
| **D2** | 阻塞 step 处置：v1 全部声明为临界区（调度器可被阻塞）还是引入异步 step | **v1 = 临界区**；异步 step 留到 v2 |
| **D3** | 中断队列与排队队列是否合一 | **完全分离**：中断按级别分四条队列（L4→L1 扫描），排队队列纯 FIFO，两者不共用比较器 |
| **D7** | 任务类别怎么定 | **由注入 API 决定**（`InjectInterrupt*` = 中断；`InjectText*`/`InjectInputSync*`/自循环 = 排队），**不按通道名推断** |
| **D8** | L4 归谁 | **内核独占**。唯一入口 `(*Agent).raiseKernelInterrupt`（panic / selfip）；`clampPluginLevel` 把插件声明夹到 L3 |
| **D9** | L1–L3 归谁 | **插件在 `InjectOptions.Priority` 里声明**（纯追加字段）；空/非法降级到 L1 |
| **D10** | 抢占者进入队列还是立即运行 | **立即运行**（`immediate` 槽）。这消除“抢占者与被挂起者同级”的比较，删除了早期的“同级 pending 优先”补丁 |
| **D11** | 中断栈帧数上界 | **结构推论 = 4**（排队 L0 + I1 + I2 + I3 挂起，I4 运行中），不是配置项；超限只计防御性计数 |
| **D4** | readyQueue 满时：阻塞发送方 or 返回错误 | **阻塞发送方 + 计数**（与现状一致，避免丢用户输入） |
| **D5** | 饥饿防护：抢占计数提升 or 时间老化 | **抢占计数提升**（确定性、易测）；时间老化留待需要时 |
| **D6** | 主循环 `max_tool_turns` 是否在本特性一并落地 | **是**（审查 P0，且调度器需要"任务可终止"这一前提） |

---

## 13. 与发布纪律的关系

- 本特性在 `feature/input-semantics` 上开发，完成后合回 `main`，**不碰 `release/v1.2.x`**。
- **公开 SDK 在本特性上有意新增**（feature 分支不受 rel 分支的接口冻结约束）：
  `sdk.InjectOptions.Priority` 与 `sdk.PriorityL1/L2/L3`。这是为了让插件能声明
  自己中断的级别（§3.2）。
- **追加是唯一的形态**：不改既有字段、不改签名、不改语义；`Priority` 的零值
  等价于旧行为（L1）。
- 合回 `main` 前需完成的发布动作：
  1. 与 SDK 仓协同升 SDK 中版本（`docs/git-branching.md` §七）；
  2. 遵守“只增不减、签名不改”，并同步 hmapdev 模板接线
     （`docs/git-branching.md` §八）。
- 内核侧接口（`internal/agent/io`、proc 桥的 `injectParams`/`injectMediaParams`）
  同步追加 `priority`，与公开 SDK 字段一一对应。

## 14. 实现里程碑（逐个实现，每个 = 一个可独立验收的提交）

| 里程碑 | 内容 | 验收 |
|---|---|---|
| **M0** | 测试基础设施：`Clock` 接口、假 Provider、假工具、`waitFor`、`Dump()` 骨架 | 新测试可运行；`go vet` 干净 |
| **M1** | **纯重构**：把 `process()` 拆成显式 step 状态机 + `TaskFrame`；仍由现有 `eventLoop` 驱动，无优先级/无抢占 | R3、X3 通过；既有全部 agent 测试通过（行为等价） |
| **M2** | 调度器骨架：单 `schedulerLoop` + `readyQueue`，取代 `eventLoop` 的输入处理；无优先级（全部 L1，纯 FIFO） | Q1/Q4 通过；integration 测试通过 |
| **M3a** | **前置重构（本次拆分引入）**：把一轮对话的所有权从 `processInput` 移到调度器——帧覆盖 `prepare → step… → finish`；同时移除 `process()` 整轮持有的 `a.mu`（挂起不能持锁） | 既有全部 agent 测试 + 既有 e2e 通过（行为等价）；`-race` 干净 |
| **M3b** | `interruptLoop` 重写 + 四级优先级 + 严格大于抢占 + 中断栈 LIFO；只支持 `S_LLM` 抢占 | P1–P4、R1、R5、K1–K2 通过；嵌套 LIFO 判据通过 |
| **M4** | 临界区 + `S_TOOL_EXEC` 声明 + 中断队列 + 深度上界（**后经模型更正重做，见下**） | P5–P6、D1T、Q3、Q5 通过 |
| **M5** | 饥饿防护（抢占计数提升 + 冷却） | G1–G2 通过 |
| **M6** | 任务级 `responseCh` + 断链点统一为终态事件 | X1–X4 通过；`cli`/`clawhub` 不再挂起 |
| **M7** | 可观测性（`Dump()`/事件/状态页）+ 既有回归 | O1–O2、E1–E3 通过；`go test -race ./internal/agent/... ./internal/plugin/...` 全绿 |

每步收尾命令：

```bash
export GOCACHE=/tmp/gocache GOPATH=/tmp/gopath
gofmt -l internal/agent internal/plugin internal/sdk   # 本步新增文件必须为空
go build ./... && go vet ./...
go test -race -count=1 ./internal/agent/... ./internal/plugin/... ./internal/sdk/...
```

### 实现状态（2026-09-13 完成）

| 里程碑 | 提交 | 验收结果 |
|---|---|---|
| M1 | `9a58878` | ✅ agent 全量 + `-race`；新增 `task_test.go` 4 项 |
| M2 | `7082a50` | ✅ 新增 `scheduler_test.go` 6 组（含 O1/K1） |
| M3a+M3b | `c69a1f1` | ✅ 新增 `task_lifecycle_test.go` 5 项、`scheduler_preempt_test.go` 5 项 |
| M4 | `7565248` | ✅ 新增 `scheduler_critical_test.go` 3 项 |
| M5 | `a971fc8` | ✅ 新增 `scheduler_starvation_test.go` 4 项 |
| M6 | `4e4e0ad` | ✅ 新增 `task_terminal_test.go` 3 项 |
| M7 | `f11de37` | ✅ 新增 `scheduler_e2e_test.go` 3 项（压力/可观测/端到端） |

#### 模型更正后的重构（2026-09-13，同一特性分支）

用户逐条澄清后重做调度核心（**行为有意的语义变化**，非等价重构）：

| 项 | 内容 | 验收 |
|---|---|---|
| 类别化 | `TaskClass{queued,interrupt}`；类别由注入 API 决定；`newInputTask`/`newSelfTask` 为 queued，`newInterruptTask` 为 interrupt | `scheduler_kernel_test.go` P7/P8 |
| 级别归位 | `Level` 语义改为“中断级别”；`taskLevel()`（按通道名推断）删除，改为 `interruptLevel(evt, privileged)` 读 `payload["priority"]` | P9、P11、Q1 |
| L4 内核独占 | `raiseKernelInterrupt`（panic/selfip）；`requestKernelPreempt` 不夹取；panic 报告为 L4 且带递归保护 | P10、`TestKernel_PanicRaisesL4Interrupt` |
| 选择结构 | `immediate` + 四条中断队列 + 排队 FIFO + 中断栈；删除统一比较器 `pickTaskIndex`/`taskBefore` 与“同级 pending 优先”补丁 | Q1–Q3、Q6 |
| 栈上界 | `maxSuspendDepth`（配置语义）→ `maxInterruptFrames = int(LevelCritical)`（结构推论）；删除“超限转 pending”降级 | D1T |
| 公开 SDK | `InjectOptions.Priority` + `PriorityL1..L4`；io/proc 桥/插件模板同步透传；`example/qq` 声明 L1、`timer` 声明 L3、`webui` 终止按钮声明 L4 | `go test ./...` 全绿 |
| 分级可观测 | `SchedulerStats.InterruptsByLevel[1..4]` / `PreemptsByLevel[1..4]`（按级别分桶，见 §11.6 E4） | 压力测试按级别断言 |
| 计数修正 | `Resumed` 原本在 `nextRef` 与 `resumeTask` **各计一次**（双计），使"排空后 Suspended==Resumed"失真；现只在 `resumeTask` 计 | E4 断言 |
| 压力测试 | `scheduler_stress_test.go`：100 排队 + 100 中断（各级 25）混合；另加嵌套到 4 帧上限并验证 LIFO | E4/E5 |

实现期与设计的差异（均已回写本文档）：

1. **M3 拆为 M3a/M3b**：真正挂起要求帧跨 `prepare→run→finish`，否则 `processInput`
   会在挂起返回后继续提交。
2. **`a.mu` 整体移除**：它原本只包住整轮 `process()`（同一 goroutine），
   移除后所有任务状态由 schedulerLoop 独占（不变量 I2/I3 可落地）。
3. **`interceptCh` 被删除**：M3b 起中断一律走中断队列（当时叫 `pendingInterrupts`），旧的
   “同行注入 + 三处 drain + 批次放弃” 已无写入者，属死代码（M4 清理）。
4. **v1 未做 M0 的伪时钟**：所有抢占测试用“单次调用阻塞到 ctx 取消”的
   provider 达到确定性，无需注入时钟。时序型判据（老化式提升）留待需要时。
5. **工具执行中不可抢占是被结构保证的**：让位检查只在 step 之间；
   不需要在 step 内部再判一次。

---

## 15. 开放问题（后续版本）

1. 异步 step + `tool.cancel`（真正让工具可抢占）。
2. 帧落盘（跨进程/崩溃恢复）。
3. 多 agent 并行调度。
4. 与 `RuntimeManager + 分组 worker` 方案合并（本设计是其前置；
   该方案的核实过程见 git log --grep=input-scheduler）。

> **已更正**：早期稿写“`InjectOptions.Priority` 进入公开 SDK 已被删除”，
> 前提是“优先级是内核内部属性、不应由插件声明”。用户澄清后该前提被推翻：
> **L1–L3 就是给插件声明使用的**。L4 的归属后来也明确了——不是“只有
> panic/selfip”，而是**内核 + 内核级插件**（编译期内置）都能用，用于实现
> “立即打断”（panic、内核事件、WebUI 终止按钮）。因此公开 SDK 同时导出了
> `PriorityL4`（附“仅内核级插件”的说明）。
>
> 仍**不做**的是“运维可调的策略表”（`core.agent.priority.<channel>`）——
> 那是把调度内部属性外化成配置，与“由调用方声明自己那件事有多不能等”不同。
