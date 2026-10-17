# commondev 运行时与内核接入设计（工程对话 · 内嵌/SSH · inputch 路由）

> **状态：架构已按前篇 §12 调整。** 本文描述的 pi 会话包装、通道注册与运行时包装
> **仍在 commondev 插件里**（它们是通用运行时能力，与 git 平台无关）；
> 移出插件的是**平台专有**部分（认证头、PR、分支保护）。
> 只讲两件事：**「工程对话」这个一等对象怎么落到运行时**，以及**插件怎么接进内核
> 已有的调度器 / 驻留子 / 通道体系**（而不是另起一套）。
>
> 前篇讲的是工具面、台账 schema、判据与变异、PR 流程与安全；本文不重复。
>
> **标记约定**（沿用本仓惯例）：
> - **［已核］** = 本文写作时已在本机实测 / 源码核对确认，附证据
> - **［待定］** = 需要决策，本文给出建议但未拍板
> - **［风险］** = 已知会让整套机制失效的点
>
> 撰写时间：2026-10-13。事实基线：TrueAgent `/home/program/TrueAgent`，
> 生产实例 `/home/newqqagent`，SDK 仓 `third_party/homeagent-sdk`。

---

## 0. 与前篇的关系（一处修正）

前篇 §6.1 把「拉起 agent」写成 **CLI vs A2A/ACP** 的二选一，结论是「走 CLI」。
**该结论只论证了 `cwd` 一个维度，漏了「会话续投」这一维度，因此是错的**（不是结论方向错，
是论据不全）：`pi -p` 每轮一个进程，**无法保证「再投一条还是同一个会话」** —— 而那正是
用户明确要求的能力。

正确的两轴是：

| 轴 | 取值 |
|---|---|
| **会话粒度** | 一次性进程（`-p`） / 常驻进程（`--mode rpc`） / 进程内（Node SDK） |
| **cwd 由谁承担** | `-p`/RPC：**进程 cwd**；SDK：`createAgentSession({cwd})` |

A2A/ACP **依然排除**（前篇 §3.4 的「参数里没有 cwd」这条事实没变），但排除理由与本
设计无关：它们是**常驻 peer** 的模型，而本需求是**按工程对话拉起**。

---

## 1. 已核实的事实（接手者可直接依赖）

### 1.1 会话续投 ［已核 · 关键］

| 事实 | 证据 |
|---|---|
| **`--session-id` 就是「续同一会话」的原语** | 两次 `pi -p --session-id X`，第二次准确答出第一次让记的暗号 `紫罗兰-7734`；会话文件 `--tmp-sidprobe--/<ts>_commondev-test-01.jsonl` |
| **不带 `--session-id` 会静默开新会话** | 同一 cwd 下两次 `-p` 生成**两个不同** UUID 会话文件 |
| `--session-dir` 可把会话库搬走且**不污染**全局 `~/.pi/agent/sessions/` | 实测落 `/tmp/sd-store/`，全局 sessions 无新目录 |
| **★ 两个 `-p` 并发打同一 session-id 会写坏会话** | `/tmp/conc`：文件里 `user A`、`user B` 相邻，`assistant B` 在前、`assistant A` 落在最后 ⇒ B 那条 run 的**模型上下文里没有 A 的用户消息**，且 `parentId` 树链交错 |
| 内层 pi **不继承**外层 `PI_SESSION_FILE` / `PI_SESSION_ID` | 外层跑完只有外层会话被写 |

> ⇒ **纪律**：同一个 session_id **绝不允许并发投递**。这条必须做成**结构性保证**
> （sidecar 内单一投递 goroutine + mutex），不能靠"调用方记得别并发"。

### 1.2 两种运行时形态 ［已核］

> ［已定 2026-10-13 用户拍板］**插件内嵌独立的 nodejs 与独立的 pi**，
> **安装在插件工作目录**（不是复用系统 node/pi）；**pi 的插件也装到这个目录**。
> 默认走**内嵌**，配置可切**远程 SSH**。

| 事实 | 证据 |
|---|---|
| `createAgentSession({cwd})` 可行（真·进程内） | `/tmp/sdktest/probe.mjs`：读工作区 `marker.txt` 得 `SDKMARK-42`，同会话追问仍得 `SDKMARK-42`，`session.messages.length === 8` |
| **`PI_CODING_AGENT_DIR` 可重定向 pi 的 agent dir** | 设该变量后 `pi install <pkg>` 把 `packages` 写进了**自定义 agentdir**，用户全局 `~/.pi` **零污染** |
| **新 agent dir 没有凭据** ⇒ 必须一并配齐 | 只重定向 agent dir 时内层 pi 报 `No API key found for the selected model`；补上 `models.json` + `auth.json` 后**同一命令返回 `SELF-OK`** |
| **项目局部安装插件可行** | `pi install -l ./mypkg`（cwd=工作区）⇒ 写 `<workdir>/.pi/settings.json`；内层 pi 带 `--approve` 跑，调用该包的工具返回 `PKG-OK` |
| **全局安装插件可行** | `pi install <pkg>`（不带 `-l`）⇒ 写 `~/.pi/agent/settings.json`；配合 `PI_CODING_AGENT_DIR` 可改为写插件自己的 agent dir |
| ⚠ **安装是复制，不是引用** | `pi install ./mypkg` 会把包**复制**到工作区。改源目录不会影响已安装的那份（**我曾因此得出"插件不生效"的错误结论**）；要更新必须重装 |
| 不批准 trust 时项目包被挡 | `--no-approve` 时项目局部安装的扩展不加载 |
| RPC 常驻 + 同会话两轮 | `pi --mode rpc --session-id rpc-probe-01`：`agent_settled` 1.6s 后第二轮答出 `蓝鲸-99` |
| RPC 事件含**工具级**证据 | `docs/json.md:105-114`：`tool_execution_start`（args）/ `tool_execution_update` / `tool_execution_end`（result, isError） |
| `agent_settled` 才是「这轮真的完了」 | `agent_end` 之后还可能有 retry / compaction / 排队消息（`docs/json.md`） |
| 本机 sshd 在 `:22`，`ssh`/`sshpass` 都在 | `ss -tlnp`、`which` |
| sidecar 模式本仓已有先例 | `internal/plugins/clawhubadapter/sidecar.go`（376 行）：JSON-RPC over stdio + pending-id 配对 + `readLoop` + `NotifyChan` + 优雅 `Close` |

### 1.3 内核可接入面 ［已核］

| 能力 | 插件可用？ | 依据 |
|---|---|---|
| `RegisterInputChannel(name, def)` | ✅ **公开**，一个插件可注册 N 个 | `sdk/plugin.go:665` |
| `InjectText*` / `InjectInterrupt*Opts` | ✅ | `sdk/plugin.go:818-868` |
| `InjectOptions.Priority`（L1–L3） | ✅ | `sdk/plugin.go:152` |
| `RegisterOutputChannel` | ✅ | `sdk/plugin.go:632` |
| `UnregisterOutputChannel` | ⚠️ **外部插件静默无效** | `capability-boundary.md`：桥只注入 registrar |
| `resident_agents`（list/create/send/inspect/compress/reclaim/destroy） | ❌ **插件调不到** | proc 桥 `grep` 无 toolapi/resident 方法 |
| `AssignInputChannel(name, agentID, cap)` | ❌ **未导出到公开 SDK** | 只在 `IOManager`（`io/channel.go:610`） |
| `inputch_note` / `notify_parent` | ❌ 只有驻留子有 | `tooldefs.go:629`（`parentID != ""`） |
| `Events()` | ❌ 恒 nil | 桥不注入 subscriber |

**两个必须避开的陷阱**：

1. **`ToolAPI` 是单例、per-agent 数据靠「最后注入者」** —— `builtin_toolapi.go:117-119`
   自陈该局限，而 `SpawnResident` 也会注入。⇒ **不要经 ToolAPI 调 `resident_agents`**：
   它可能打到某个驻留子的表上。
2. **不要以为插件稳拿 L3**（真实是两跳两道闸，见 §3.2）。

### 1.4 驻留子设计里可抄的纪律 ［已核］

| 纪律 | 出处 |
|---|---|
| `SendToResident` 固定 `Priority: "L4"`，子侧唯一 L4 来源 = 父 | `resident.go:440-455` |
| `KernelSource = parentID`（父在子的阶梯上是唯一 L4） | `resident.go:190` |
| **父 `Stop()` ⇒ 全部子销毁**，登记表清空、不留孤儿 | `resident.go`（S16 判据） |
| `inspect` 是 pull，**不打断**子 | `resident_agents` 动作语义 |
| `compress` = 保留语义（压上下文 + **清单**） | 前篇 §9 |
| `reclaim` = 取消语义（父选 promote → 取消子） | 前篇 §9 |
| `contextfull` → 父侧 **L4**（只推信号，状态靠 pull） | 前篇 §9 |
| inputch 是**最基本的输入路由单位**，路由发生在**进内核之前** | `resident-subagent-design.md §4.1` |

---

## 2. ★「工程对话」是一等对象（本文核心）

前篇把单位定为「项目 → 分支线」。本文把它落到运行时，并给它一个用户可寻址的名字：
**工程对话**。

**一个工程对话 = 四元组绑定，缺一不可**：

```
工程对话 (id = br-<project>-<slug>)
├─ workdir    <插件数据目录>/program/<hash>-<name>
│              ★ hash = H(项目+分支) 承担**唯一性** ⇒ 同仓库不同分支必然是不同目录
│              ★ name = 分支名派生，只做可读性（不承担唯一性，派生后可能撞）
│              独立 clone（本地 clone 硬链接派生，非 worktree、非 --reference）
│              **合入后整个目录删除**
├─ session_id pi 会话（★ 续投的唯一依据；漂移即失败）
├─ runtime    local（默认，内嵌 pi） | ssh（配置切换，远端建目录拉 pi）
├─ host       runtime=ssh 时的目标主机
└─ channels   插件为该工程对话注册的 **一对通道**
              ├─ inputch  commondev/<id>      ← pi → agent（可用斜杠；只流向该子）
              └─ outputch commondev_<id>      ← agent → pi（★ 下划线，会拼成 output_send__）
```

> **目录为何必须是独立 clone**（而非 worktree / `--reference`）：用户要求
> 「合入后直接删除目录」，而实测 worktree 删后有 `.git/worktrees/` 残留（需 `prune`）、
> `--reference` 则有 alternates 外部依赖（mirror 被清即损坏）。**本地 clone 对象为硬链接**
> ⇒ 省磁盘、无外部依赖、刪掉即彻底。详细实测见前篇 §4.3。

### 2.1 为什么必须是"一对通道"，而不是复用一个

［已核］inputch 是**最基本的输入路由单位**，路由按 `Owner` 在**进内核之前**完成
（`inputroute.go:23`）：`lookupInputChannel(ch) → Owner → DeliverRouted`。
未分配或归自己的，才由本内核处理；归属到不存在的 agent 时**兜底给根并留日志**
（不静默吞）。

⇒ **每个工程对话一个独立 inputch**，就得到了：
- **精确投递**：往 `commondev/br-1` 注入只到 br-1 的 agent，别人看不到；
- **互不串台**：两条并行的工程对话不会互相污染上下文；
- **可限额**：`Capacity` 独立；
- **零内核改动**：`RegisterInputChannel` 是公开 API。

### 2.2 ★ 顺序：先拉起 pi 拿到 session，**才**注册通道

［已定 2026-10-13 用户澄清］**通道注册依赖 session 已存在**。完整顺序：

```
① 建工程           commondev_project_open / branch_open
                     → 建 workdir（program/<hash>-<name>；<hash> 含分支 ⇒ 同仓库不同分支不同目录）
② 拉起 pi          commondev_agent_up（内嵌 sidecar / remote 时 ssh）
                     → pi 起来并拿到/恢复 **session_id**
③ ★ 注册通道（此时才有）
     RegisterInputChannel("commondev/<id>", ChannelDef{NoMemory: true})
       ★ 可以用斜杠（inputch 不做工具名）
       ★ NoMemory：通道控制流不该进记忆（避免污染召回/场面指纹）
     RegisterOutputChannel("commondev_<id>", caps, "工程对话回程", def, handler)
       ★ 下划线，不能带斜杠 —— 它会拼成 output_send__<name>
④ 划给驻留子     resident_agents create input_chs=commondev/<id>
                   allowed_outputs=commondev_<id>（这一步由主 agent 调）
⑤ 投递任务       子调 output_send__commondev_<id>(payload=…)
                   → 插件 handler → sidecar.prompt(session_id, text)
⑥ pi 产出        → 插件 InjectTextTo("commondev/<id>", …) → 路由回该子
```

**为何必须先有 session**：通道是“与**某个具体 pi 会话**对话的入口”——
没 session 就没有投递目标，注册出来的通道是空壳；handler 也无法“包一下”
（查台账拿不到 `session_id`）。

⇒ **台账在 ② 写入 `session_id`，在 ③ 补写通道名**，两步不能合并。
⇒ `commondev_agent_up` **要在通道注册之前**、**在子创建之前**——否则子拿到的
`output_send__commondev_<id>` 会指向一个还不存在的通道。

**回投与小中断**：插件用
`InjectTextTo(source="commondev", channel="commondev/<id>", text=…)` 把 pi 的
事件/进度回投给**该工程对话的子**；用 `InjectInterruptTextOpts(..., Priority:"L3")`
做需要抢占的通报。

### 2.3 ★ skill 必须建议 agent：新建驻留子并**划入**这对通道

**注意顺序**：通道是 `commondev_agent_up` 拿到 session **之后**才注册的（§2.2），
所以你的动作序列是：**建工程 → 拉起 pi → 插件注册通道 → 你建子并划入**。

插件只能把通道**登记**出来（`RegisterInputChannel`），**划入（Assign）只有内核能做**
［已核：`AssignInputChannel` 未导出到公开 SDK］。所以"通道 → 子 agent"这一步
**必须由 agent 自己调内核工具完成**。

⇒ 因此配套 skill 的核心建议是：

```
为每个工程对话新建一个驻留子，并把该工程对话的一对通道划给它：

  resident_agents create
      id           = dev-<br-id>
      input_chs    = commondev/<br-id>          ← 划入该 inputch（Owner 变成这个子）
      allowed_outputs = commondev_<br-id>       ← 授权该 outputch（★ 下划线）
      capacity     = 64
      task_prompt  = "你在 <workdir> 推进 <work>。下任务给自己用
                      output_send__commondev_<br-id>(payload=…)，
                      pi 的结果会回投到你的 commondev/<br-id>；
                      有结论用 notify_parent 汇报。"

  ⇒ 此后该工程对话的输入**只**流向这个子内核（根 agent 看不到），
     子在收到父的 send 时按 L4 被打断（KernelSource = 父）。
```

**如果 agent 不建子会怎样**（skill 必须写清代价，否则它不会照做）：
- 所有工程对话的输入都涌向**根 agent**（`routeInputByOwner` 发现 Owner 为空 ⇒ 自己处理）
  ⇒ 根会成为瓶颈，多工程并行的意义消失；
- 根 agent 的上下文被 N 条工程对话的细节淹没（正是前篇 §4.6 要避免的「主管被 diff 淹没」）。

### 2.4 L4 落在谁身上（**能力表，别搞错**）

| agent | 它的"内核" | 谁能产生 L4 | 机制 |
|---|---|---|---|
| 根 agent | 内核自身 + **内置**插件 | `raiseKernelInterrupt`、内置插件（如 WebUI 终止按钮） | `isKernelLevelSource` ① `pluginReg.IsBuiltinPlugin(name)` |
| 驻留子 | **父 agent** | 父（`SendToResident` 固定 `Priority:"L4"`） | `isKernelLevelSource` ② `name == a.kernelSource` |
| **commondev（外部插件）** | — | **永远不能** | proc 桥 `clampExternalPriority` 夹 L4→L3（`proc/corehandler.go:279`） |

> **［风险］** commondev 作为**外部独立插件**，L4 被制度性移除。若 canonical 流程需要
> 「立即取消整条分支」，只能靠 **L3 打断主管 + 主管下令**，或改用内置插件形态。
> 见 §6 未决问题 U1。

### 2.5 ★ 投递路径：用内核原生的输出门，不另造发送工具

［已定 2026-10-13 用户澄清］完整流程是：

```
① 插件为每个工程对话注册一对通道
     inputch   commondev_br_1     ← pi 的进度/事件回这里
     outputch  commondev_br_1     ← ★ agent 发消息给 pi 就走它
② 主 agent 调 resident_agents create，把该 inputch 划给驻留子
③ 之后下任务 = 该子调 output_send__commondev_br_1(payload="…")
④ 插件收到 payload → 查台账 (workdir, session_id) → sidecar.prompt(...)
⑤ pi 产出 → 插件注入 commondev_br_1 → 路由回该子
```

**关键：第 ③ 步不需要新工具。** `output_send__<通道>` 是**内核原生**的输出门
（`output.go:10`）：agent 调它 → 内核查通道能力 → `EmitTextTo` → 打到
**插件注册的输出通道 handler**。插件的 handler 就是那个"包装"。

⇒ **本稿前版设的 `commondev_agent_send`（面向 agent 的发送工具）已废除** ——
它是重复实现。保留的只有：

| 方向 | 机制 | 谁实现 |
|---|---|---|
| **agent → pi** | `output_send__commondev_br_1(payload=…)` | **内核**（插件只写通道 handler） |
| **pi → agent** | 插件 `InjectTextTo(source, "commondev_br_1", …)` | 内核路由（按 inputch Owner） |
| **中断 pi** | `InjectInterruptTextOpts(..., Priority=L3)` | 内核调度器 |

⇒ 插件侧只剩**一个工具族**（配置/仓库/判据）+ **两个通道 handler**。

#### ⚠️ 通道命名约束（**本稿前版写错了**）

输出通道名会被拼进 LLM 函数名 `output_send__<name>`，而上游要求函数名满足
`^[a-zA-Z0-9_-]{1,64}$`（SDK `RegisterOutputChannel` 注释已警告）。

| | 能用 `/` 吗 | 依据 |
|---|---|---|
| **inputch** | ✅ 可以（内核自己就用 `child/<id>`、`sub/<id>`） | `resident.go:253/447` |
| **outputch** | ❌ **绝对不行** | 否则上游 400，**整条请求被拒、agent 整体失语** |

⇒ 本稿前版写的 `commondev/<id>` **作为输出通道名是非法的**。正确用法：

```
inputch  : commondev/<br-id>      （可以用斜杠，路由单位）
outputch : commondev_<br-id>      ★ 下划线，保证 output_send__ 合法
```

（`<br-id>` 本身也必须限定在 `[A-Za-z0-9_-]`，不能出现斜杠/点/空格。）

#### ⚠️ 另一条限制：`BindOutputTarget` 不开放给插件

“输出通道 → 目标 agent 的 inputch”的解析由 `ChannelRegistry.BindOutputTarget`
提供（`io/inputch.go:175`），但它**未导出到公开 SDK**。
⇒ **插件不能自己把输出通道绑到某个子的 inputch**。

好在 canonical 流程**不需要它**：子 → 父/Z 方向走 `notify_parent`（内核已有），
pi → 子的方向走 **inputch Owner 路由**（已可用）。若将来需要“经输出通道直接投给另一个 agent”，
得先把 `BindOutputTarget` 补到公开 SDK（属"只增不减"的追加）。

### 2.6 commondev 与驻留子的能力映射

**总纲 ［已定 2026-10-13 用户拍板］**：插件对 pi 做的事，**本质就是**
> **「把给 pi 发消息包装一下、把 pi 返回包装一下」**，然后**深度融合内核的 IO 抽象与中断系统**。
> ⇒ **pi = 一个 Device**。不是自己造一套调度/会话/中断，而是**接到已有的那套上**。

对应关系因此是**结构上同构**的，不是比喻：

| 内核概念 | pi 侧对应 | 落点（内核已有） |
|---|---|---|
| **给 pi 发消息** | `pi.prompt(text)` / `steer` / `follow_up` | `output_send__commondev_<id>`（内核原生） |
| **pi 返回** | sidecar 回调 / `tool_execution_*` / `agent_settled` | `InjectTextTo(source, "commondev/<id>", …)` |
| **pi 报错/卡住** | sidecar 错误事件 | 同一个 output handler，或注入为输入 |
| **中断 pi**（改主意、叫停） | `steer`（steer 到当前轮之后） | `InjectInterruptTextOpts(..., Priority: L3)` |
| **音声/图像产物** | pi 可产出的媒体 | `OutputCapabilities()` 声明 `CapText|CapImage|…` |
| **工具级证据** | `tool_execution_start/update/end` | 经 output handler 回投，作台账证据 |

⇒ **本质是一次包装适配**：外壳是 Device，里面是 pi 进程 + sidecar。
所有调度/中断/路由/记忆策略都**交给内核**（`ChannelDef` + `InjectOptions` + `inputch` 路由）。

**与驻留子的对照**：

| 内核驻留子 | pi 侧 | 谁提供 |
|---|---|---|
| 轻量内核（调度器/中断栈） | **不需要** —— 用内核自己的 | — |
| pi 会话 | sidecar 持有的 `session_id` | commondev |
| 父→子 = L4 打断 | **沿用**：父给子发消息就是 L4（插件不介入） | 内核 |
| 子→父 = L3 汇报 | **沿用**：`notify_parent`（skill 建议子调用） | 内核 |
| `inspect`（pull 不打断） | `get_state` + 台账 `deliveries` | commondev 经 sidecar |
| `inputch_note` 处理表 | 台账 `deliveries` 表 | commondev |
| `destroy` | 杀 sidecar / pi 进程 | commondev |
| **父 `Stop()` ⇒ 全部子销毁** | **commondev 保证**（进程/ssh 无孤儿） | commondev（**孤儿属插件责任**） |

> **一句话**：**插件只负责“把 pi 接成 Device”与“台账记 session_id”**，
> 其余的调度、中断、路由、记忆策略**全是内核已经在做的事**。

### 2.7 ★ 工具调用有 60 秒硬上限（实现约束） ［已核 · 实测踩到］
内核把**每一次工具调用**限制在 **60 秒**，且**硬编码无配置项**：

```go
// internal/agent/core/toolcall.go:62
case <-time.After(60 * time.Second):
    te := newToolError(ErrReasonTimeout, …)
```

⇒ **凡是可能超过 60 秒的活儿，一律不能放在工具调用里同步做。**

实测踩到的场景：`git clone --mirror` 一个仓库耗时不定，`commondev_branch_open`
里同步 clone ⇒ 工具超时 60s 报 `工具 commondev_branch_open 执行超时（60秒）`，
**但 mirror 其实已经建好了**（磁盘上 `cdtest.git` 存在）——
即：超时后状态是“做了一半”，调用方却只看到失败。这是最坏的一种失败。

**受影响的操作**（都必须异步化）：

| 操作 | 为何慢 |
|---|---|
| `git clone --mirror` | 仓库大小 × 网络 |
| 由 mirror 派生工作目录（即使硬链接，大量小文件仍慢） | 文件数 |
| `git fetch` 更新 mirror | 网络 |
| 跑判据 / 变异测试 | 测试本身耗时 |
| **拉起 pi 并等它就绪** | 进程启动 + 首次初始化 |

**设计结论**：工具只做**快操作**（查台账、登记、发起），慢操作**转后台**，
由状态工具报告进度。形态建议：

```
commondev_branch_open   → 登记台账 + 建目录（快）+ **后台 clone**；返回 job_id
commondev_job_status    → 查后台任务进度（pending|running|done|failed）
```

⇒ 这同时满足内核“工具要快返回”的设计与用户“开完就能用”的期望。

> **教训**（值得单独记）：我曾把同步 clone 写进工具里，端到端跑时才发现超时。
> 若只看单测（不跑真机）根本发现不了 —— 这是“读码能过、真机不过”的典型。

### 2.8 ★ 会话 id 必须**显式指定**（否则续投必失败） ［已核 · 实测踩到］

**事故**（首次投递）：

```
会话漂移：期望 br-cdtest-feat_alpha，实际 01a13959-bcc3-7007-9a92-06483e2150ba
—— 拒绝继续（不静默改投新会话）
```

**根因**：sidecar 初版用 `createAgentSession({cwd})`，pi 会**自己生成一个 UUID**
作会话 id；而 Go 侧台账记的是我们请求的名字。两者本来就不相等 ⇒
“投递后断言会话未变”这条纪律拿两个不同名字比，**永远失败**。

**修法**（已验证）：用 `SessionManager.create(cwd, sessionDir, {id})` 显式指定：

```js
const manager = SessionManager.create(workdir, sessionDir, { id: sessionId });
const { session } = await createAgentSession({ cwd: workdir, sessionManager: manager });
```

实测：`manager id: MYFIXEDID`（不再是随机 UUID）；pi 会话文件名
`…_br-cdtest-feat_alpha.jsonl`，且其中第 4 行就是 `user: <payload>` ——
即 payload **真的进了那个会话**（不是只看工具返 `ok`）。

⇒ 这同时给了“续投同一会话”一个**真保障**（而非运气）：固定 id ⇒
重启/重投能打开同一个会话文件。

> **注**：`pi -p --session-id X` 也是用 X 作会话 id（实测会话 header `"id":"X"`）；
> 两条路径一致。但 SDK 路径**必须手动传**，不会从环境变量推断。

---

## 3. 运行时形态（内嵌为默认，SSH 为配置项）

### 3.1 架构（一次包装适配）

［已定］**默认本地内嵌，配置切到远程 SSH**。两者**共用同一份 sidecar**，
差别只在**传输层**（本地 `exec` vs 远程 `ssh`）：

```
内核 ──InjectInputTo(source, "commondev/<id>", text)──► 插件
                                                      ├─ 包一下 ──► sidecar.prompt(text)
                                                      │             └─ 内嵌 createAgentSession
                                                      │                 （远程时：ssh 到远端同一份）
                                                      └─ 包一下 ◄── sidecar 回调（pi 输出/事件）
                                                                     └─ output handler → 回投内核
```

**两处“包一下”**（这是本设计的全部）：

| 方向 | 做什么 | 内核已有机制 |
|---|---|---|
| **入（给 pi 发消息）** | 找到 `(workdir, session_id, runtime) → sidecar.prompt/steer` | 入站由 `InjectInputTo` 到达，插件只是落地 |
| **出（pi 返回）** | sidecar 回调 → 包成内核输出 | `RegisterOutputChannel` 的 handler |
| **中断** | 不需新机制 | `InjectInterruptTextOpts(Priority=L3)` 走内核调度器 |

⇒ **插件不实现调度器、不实现中断栈、不实现会话路由** —— 那些都是内核的。
**台账只多做一件事**：记住 `session_id` 并在投递后断言它未变（§3.3）。

**为何不让插件自己管 RPC 循环**：那是重复实现内核已有的能力。
sidecar 只需要回答两个问题：
1. “这条消息投给了哪个 session？”（投递前读、投递后验）；
2. “pi 现在在干什么？”（`get_state` / 事件流）。

> **为何不用 RPC 客户端另开一路**：最初设想 C（内嵌）/ B（`--mode rpc`）
> **双模式**。用户拍板后**收敛为「一套 sidecar + 两种传输」**（内嵌为唯一实现）——
> 少一条运行时路径，少一半要维护的边界。RPC 仅在 sidecar 不可用时可作降级，
> 且**不属于本设计的正式形态**。

#### 3.1.1 接线状态与两处实测缺口 ［✅ 已接线 · 真实 sshd e2e］

`runtime=ssh` 的**实现**早已写完（`sidecar_ssh.go` 全套：探测/推脚本/建链），
`startSidecar` 里的 `if b.Runtime == "ssh"` 分派也接好了。**但 `branch_open`
硬编码 `Runtime: "local"` 且从不读 `ssh_host`** —— 于是 `runtime=ssh`
被**静默吞掉**：以为跑在远端，实际跑在本地，且没有任何提示。
（`runtimeOrDefault` 当时更是从未被调用的死代码。）

> **这类「参数被吞」比报错危险**：报错立刻发现；吞掉会一路错下去，
> 而症状（任务跑在本地、文件在本机被改）与预期完全不同却很晚才暴露。

已补：`branch_open` 解析 `runtime`/`ssh_host` → 缺 host 直接拒 →
未知 runtime **响亮拒绝**（不降级）→ 写台账 → `agent_state` 回显 `ssh_host`。

**另一处缺口**：`probeSSH` 硬编码探 `node`，**忽略 `ssh_node_path`**。
于是用户按提示配好了远端 node 路径，探测**仍然失败**并报
「远端不可达或未配免密」——把**配置问题伪装成连通性问题**，排查方向完全错。
已修（用 `t.NodePath`）。

**判据**：
- 接线（不需环境）：`ssh_wiring_test.go` 9 条 —— 含"参数被吞""未知 runtime 不降级"
  "未知 CLI 不回退 pi""新增 CLI 不改核心"。
- **真端到端**（`HA_SSH_E2E=1`，需 sshd + localhost 免密）：`ssh_e2e_test.go` 5 条 ——
  真起 ssh、真 scp 推脚本（并比对本地/远端字节一致）、真握手、真投 prompt
  并断言**会话未漂移**；另含"远端缺 node 响亮报错""无免密响亮报错"。
  实测握手耗时 ~20s（ssh 建链比本地慢，故 `handshakeTimeout` 单独放宽到 20s）。

**变异验证**（4/4 咬住）：退回硬编码 local / 未知 runtime 降级 /
未知 CLI 回退 pi / `llmConfig` 不透传。

#### 3.1.2 ★ 第三处缺口：远端工作目录**从未被创建** ［✅ 已补 · 真实 sshd 闭环］

`prepareWorkdir` 只有**本地一套**（本机 `clone --mirror` + `clone mirror workdir`）。
对 `runtime=ssh` 的分支，远端 pi 的 cwd 指向一个**从未被创建**的远端目录。
症状是「agent 起来了，但一执行命令就说目录不存在」，而 **job 状态显示 done**
（因为本地 clone 确实成功了）—— 两边各自看起来都成功，所以极难归因。

新增 `workdir_remote.go`，与本地路径**逐条对应**：

| 步骤 | 本地 | 远端 |
|---|---|---|
| 建父目录 | `os.MkdirAll` | `ssh mkdir -p` |
| mirror | `<插件数据>/mirrors/<name>.git` | `<ssh_workdir_root>/.mirrors/<name>.git`（同一远端多条线共享） |
| 派生 | `clone mirror workdir` | 远端 `clone mirror workdir` |
| 切分支 | `ensureOnBranch` | `ensureRemoteOnBranch` |
| 记 base_sha | `captureBaseSHA` | `remoteCaptureBaseSHA` |

**★ 三条纪律**：

1. **`runPrepareWorkdir` 必须按 runtime 分派**，且远端分支必须 `return` ——
   不返回就会继续落到本地路径（本地 clone 成功、远端目录仍为空）。
2. **台账 `Workdir` 必须改写成远端路径**，否则 `agent_up` 把远端 pi 的 cwd
   指到一个本地目录名上。
3. **缺 `ssh_workdir_root` 响亮报错，不猜默认值** —— 猜错会把文件写到远端
   别的项目的目录里，很难回滚。

**判据**：`workdir_remote_test.go` 7 条。其中真 sshd e2e 3 条 ——
`TestSSHBranchWorkdirBecomesRemotePath`（远端真建目录/真 clone/真切分支/
远端 mirror 存在）、`TestRemoteWorkdirReuseIsIdempotent`（复用幂等）、
`TestSSHFullLoop`（**完整闭环**：远端建目录 → 远端拉起 pi → 投递 → 断言会话未漂移，21s）。

**变异验证**（5/6 咬住，1 条不适用）：远端 mirror 用本地路径 /
远端不切分支 / 缺配置猜默认 / `branch_open` 不传 runtime / 远端不改台账 Workdir。
**未咬住的那条**：在远端分支里删掉**一个** `return` —— 因为该分支还有别的
`return`，行为仍正确，判据不报红是**对的**。

**★ 判据自身的一次修正**：`TestPrepareWorkdirDispatchesByRuntime` 初版用
`Contains("runtime == \"ssh\"")` 判断分派 —— 而 `if false && runtime == "ssh"`
仍包含该子串，于是**变异照绿**。改为精确匹配 `if runtime == "ssh" {` 并追加
「不得有 `false &&` 前缀」后咬住。这是本仓反复出现的「判据通过 ≠ 检查到目标性质」。

**顺带修的既有缺陷**：`ssh_workdir_root`/`ssh_runtime_root`/`ssh_node_path`
三个配置项**只被 Get 读、从未 RegisterDef**，于是设置页里看不到，
用户只能去改 config.yaml 猜键名 —— 而缺 `ssh_workdir_root` 会让远端线直接失败。
已全部注册。

**另修自己新代码的 bug**：`runSSHCaptureOn` 初版在 `cmd.Start()` 之后调
`CombinedOutput()` ⇒ `exec: already started`（实测被自己的 e2e 咬出）。
改为 goroutine + `Wait()` + 超时 `Kill()`。

#### 3.1.3 ★ 第四处缺口：sidecar 脚本**没有随包分发** ［✅ 已补 · 真实拉起验证］

打包只收 `rawfile/` 与 `skill/`，而 `sidecar.mjs` 在**工程根** ⇒ 没进包。
同时 `<插件数据>/runtime/pi/` 在部署时没人创建。三条查找路径全空 ⇒

> `agent_up` 在全新部署上**必然失败**，报
> 「找不到 pi 的 sidecar 脚本: /home/xxx/plugin_data/commondev/runtime/pi/sidecar.mjs」
> —— 一个用户从没听说过的路径。

表现为「插件装了、工具都在、但一拉 agent 就错」，属于**能装不能跑**。

**修法**（复用既有通道，不新增概念）：

1. `sidecar.mjs` 放进 `rawfile/runtime/<cli>/`，随包分发；
2. `locateSidecarScript` 建立查找链：

| 顺序 | 位置 | 用途 |
|---|---|---|
| ① | `<插件数据>/runtime/<cli>/<script>` | 用户自备 / 覆盖随包版本 |
| ② | `<插件目录>/rawfile/runtime/<cli>/<script>` | **随包分发**（内置 pi 靠这条） |
| ③ | `<可执行文件同目录>/<script>` | 开发期（hmapdev debug） |

③ 全部落空时，错误**列出找过哪里**并给出解决办法 —— 否则用户只能猜。

★ 与 `nodeBin` 走**同一机制**（`rawFiles()`）：rawfile 已是"随包任意资源"
的通道，不必为脚本再开一个约定目录。

**判据**：4 条（随包存在且与工程根一致 / 查找链命中 / 用户自备优先 /
错误可执行）+ 变异咬住（去掉 rawfile 回退、颠倒优先级）。
**真机验证**：用随包脚本真的拉起 sidecar 并完成握手（pi 0.99.2）。

### 3.2 形态对照与硬边界

| | **本地内嵌（默认）** | **远程 SSH（配置切换）** |
|---|---|---|
| sidecar 位置 | 本机 | 远端主机 |
| workdir | `<插件数据目录>/program/<hash>-<name>` | **远端** `~/.commondev/program/<hash>-<name>` |
| pi 实例 | sidecar 进程内 | 远端拉起（cwd = 远端 workdir） |
| 会话库 | 本地 `--session-dir` 指向 workdir | **远端**（同样指向 workdir） |
| 前置条件 | 本机 node + pi | 远端 node + pi（**缺时先问用户**，§3.4） |
| 传输 | `exec`（stdio） | `ssh <host>` + stdio |

**两形态都必须遵守的硬约束**：

- 侧车崩了要能**恢复同一 session**（`SessionManager` 导入已有 jsonl）；
- **stdout 必须持续读**（不读会堵住 pi）；
- 只有 `agent_settled` 才算「这轮真的完了」；
- **同一个 session_id 绝不并发投递**（§1.1 实测会写坏会话）。

**远程额外约束**：启动时校验远端 `pi --version`，不匹配**响亮失败**。
**不要**静默降级到本地跑 —— 那会让人以为活派到了远端，其实在本机跑。

### 3.3 投递纪律（**必须做成机制，不能靠自觉**）

```
agent_send(id, text, deliver_key)
  ① 查台账拿 (workdir, session_id, runtime, host)   ← session_id 是唯一依据
  ② 单一投递 goroutine + mutex（per 工程对话）      ← 结构性禁止并发
  ③ 写 inbox 文件（持久化，进程崩了不丢）
  ④ sidecar 投递（内嵌 prompt / RPC prompt）
  ⑤ ★ 重读会话，断言 session_id 未变 —— 变了即**响亮失败**（不静默改投新会话）
  ⑥ 记 deliveries(deliver_key, 状态, 结果摘要)      ← 幂等键，重复投递不重复执行
```

**两条必备判据**（配变异）：
- `TestAgentSendSerializedPerSession`：同 session 并发投递必须**排队**（变异：去掉 mutex → 必须报红）；
- `TestAgentSendRejectsSessionDrift`：会话漂移必须报错（变异：删掉断言 → 必须报红）。

### 3.4 ★ 远端环境缺失：询问用户，**不得擅自安装**

**已核**：SDK **没有**"向用户提问"的专用 API（插件是子进程，不能直接弹 UI）。
现有机制是：插件把「需要决策」的消息**注入**给 agent（`PriorityL2`/`L3`），
**由 agent 去问用户**（本机先例：`cli/plugin.go:473` 的 `/stop` 用 L3，
`timer/plugin.go:118` 定时器用 L3）。

⇒ commondev 的 preflight 契约：

```
remote_preflight(host)
  ① ssh 连通性          → 不通：报错（属配置错误，不是缺环境）
  ② 远端 pi --version    → 缺 pi  ：注入 L3「远端 <host> 未装 pi，是否安装？」
  ③ 远端 node --version  → 缺 node：同一询问
  ④ 远端目录可写         → 不可写：报错（权限问题，需人处理）
```

**三条纪律**（缺一条就会出事故）：

1. **不擅自安装**。缺环境时只**询问**，并把「装什么、装哪里、需要什么权限」说清楚，
   由人决定。擅自 `curl | sh` 到别人的机器上，是这类工具最容易犯的越界。
2. **不冒充成功**。环境缺失时 `agent_up` 要**响亮失败**并留下一条待决策项，
   不是静默降级到本地（那会让人以为活派到了远端，其实在本机跑）。
3. **问完要能续**。用户答"装"之后，安装动作本身也该是**可见的一步**
   （能查、能重跑），而不是藏在 `agent_up` 里的隐式副作用。

> ［待定 U2］**安装的具体形态**：由 commondev 执行（需在远端跑包管理器）／
> 只输出可复制的命令让人去跑／两者都要。本文倾向**后者默认、前者需显式授权**。

---

### 3.5 ★ 自包含运行时（插件自带 node 与 pi）

［已定 2026-10-13 用户拍板］**插件内嵌独立的 nodejs 与独立的 pi**，都**安装在插件工作目录**；
**pi 的插件也装到这个目录**。这是"自包含"的含义：**不依赖宿主机的 node/pi 版本**，
也不污染用户的 `~/.pi`。

#### 3.5.1 目录布局

［已定 U7：**随包分发**］独立的 node 与 pi **随插件包一起分发**（不首次下载、不依赖用户装）。

```
<插件数据目录>/                     ← sdk.Settings().DataDir()
  runtime/
    node/            # 独立的 nodejs（随包分发）
    pi/              # 独立的 pi（随包分发）
    agentdir/        # ★ pi 的 agent dir（PI_CODING_AGENT_DIR 指这里）
      settings.json  #   ← pi 自己的配置：偏好 / 默认 model / 「全局」包声明
      models.json    #   ← pi 自己的配置：兼容端点与模型（LLM 源写这里）
      auth.json      #   ← pi 自己的配置：API key / OAuth（0600）
      mcp.json       #   ← pi 自己的配置：MCP 服务器
      skills/  prompts/  themes/  extensions/
  program/
    mirrors/<platform>-<repo>.git/
    <hash>-<name>/   # 每个工程对话一份；.pi/settings.json = 「临时」安装
```

> **为什么只用一个 agentdir**：它就是 pi 的「用户级配置目录」，**pi 自己的配置系统**
> 已经给了我们所需的一切（§3.6）。插件**不自建第二套配置存储**。
> 根目录权限 **0700**（里面有 `auth.json`/`models.json`，均含明文凭据）。

#### 3.5.2 ★ 两种安装粒度（用户明确要求）

| 粒度 | 作用域 | 落到实处 | 适用 |
|---|---|---|---|
| **临时（项目级）** | 一个工程对话 | `<workdir>/.pi/settings.json`（`pi install -l`） | 只这个工程需要的插件 |
| **全局（插件级）** | 插件自带 pi 的所有工程 | `<agentdir>/settings.json`（`pi install` + `PI_CODING_AGENT_DIR`） | 通用能力（如 pi-lens、subagents） |

⇒ 工具面：

```
commondev_pi_plugin_install   source, scope=project|plugin, workdir?  # ✅ 已实现
                              # 支持 npm / git / url / 本地路径（见下）
commondev_pi_plugin_remove    source, scope       # ✅ 已实现
commondev_pi_plugin_list      scope?             # ✅ 已实现
commondev_pi_plugin_update    source?, scope       # ✅ 已实现（PI_CODING_AGENT_DIR 隔离）
```

**必须把 `PI_CODING_AGENT_DIR` 传给每个内层 pi 进程**，否则「全局」安装会写进
**用户的** `~/.pi/agent/settings.json` —— 那是对宿主的越界。

#### 3.5.3 支持的源类型 ［已核 `pi install --help`］

| 类型 | 写法 | 备注 |
|---|---|---|
| **npm** | `npm:@scope/pkg@1.0.0` | 带版本号即**钉住** |
| **git** | `git:github.com/u/r@v1` 或 `git:git@github.com:u/r` | tag/commit 钉住；更新不移动 ref |
| **URL** | `https://github.com/u/r` | **当作 git 源**处理 |
| **ssh** | `ssh://git@github.com/u/r` | |
| **本地** | `./path` 或文件路径 | **不复制**，直接引用；文件路径 = 单个扩展 |

**三个安全纪律**（包可执行扩展代码，而这是往工作区/插件里装代码）：

1. **默认不为工程静默安装**：安装是要**显式调用**的一步，不能藏在 `agent_up` 里；
2. **安装前说明来源**：npm 包名 / 仓库 URL / 版本钉点，让人看得见；
3. **来源不可信时不装**：与 §3.4 同一条纪律（先问，不擅自）。

---

### 3.6 ★ LLM 源：默认用 HomeAgent 内部配置，支持添加与选默认

［已定 2026-10-13 用户拍板］插件内置 pi **默认使用 HomeAgent 内部配置的 llms 源**，
**支持添加其他源**，且**支持在插件中配置默认用哪个源**。

#### 3.6.1 源的真实位置 ［已核］

内核把 LLM 源存在 **`config` 表**（不是 `config_<plugin>`），键前缀 `core.llm.`：

```
core.llm.provider              = llmsproxy        ← ★ 当前默认源（这就是"默认用哪个"）
core.llm.sources.<name>.base_url       = http://127.0.0.1:8081/v1
core.llm.sources.<name>.api_key        = sk-gw-...
core.llm.sources.<name>.model          = AUTO
core.llm.sources.<name>.adapter        = openai
core.llm.sources.<name>.adapter_path   = adapters/openai.lua
core.llm.sources.<name>.context_window = 1048576   （可选）
core.llm.sources.<name>.priority       = 100       （可选）
core.llm.sources.<name>.vision|audio   = true|false（可选）
core.llm.sources.<name>.thinking_enabled = false   （可选）
```

内核侧已有现成控制面（**不是新造**）：`llm_list_sources` / `llm_set_source` /
`llm_current_source`（`spawn.go:338` 起），背后是 `ProviderManager`
（`Register` / `SetDefault` / `List`）。

#### 3.6.2 到 pi 的字段映射

内层 pi 要的是 `models.json`（`providers.<name>.{baseUrl,api,apiKey,models[]}`）。
源一字段一对：

| 内核（`core.llm.sources.<n>.*`） | pi（`models.json`） |
|---|---|
| `base_url` | `providers.<n>.baseUrl` |
| `api_key` | `providers.<n>.apiKey` |
| `adapter=openai` | `providers.<n>.api = "openai-completions"` |
| `model` | `providers.<n>.models[].id` |
| `context_window` | `providers.<n>.models[].contextWindow` |
| `vision` | `providers.<n>.models[].input`（含 `"image"`） |

**生成物是一个文件**：`<agentdir>/models.json`（插件自己的，不动用户的）。

#### 3.6.3 落地：**直接写 pi 的配置文件**，不另建存储

［已定 2026-10-13 用户拍板］**pi 配置的源直接写入配置文件**。
**本稿前版提议的 `config_commondev` 已废弃** —— pi 本身就有完整的配置系统，
再建一套只会制造两处真相。

| 需求 | 落到实处 |
|---|---|
| **默认用内核的源** | 首次拉起时，把 `core.llm.sources.<n>.*` 写进 `<agentdir>/models.json`，key 写进 `<agentdir>/auth.json` |
| **支持添加其他源** | **直接追加到同一个 `models.json`**（同文件、同格式），不另存 |
| **在插件中配置默认源** | 写 `<agentdir>/settings.json` 的 `defaultProvider` / `defaultModel`（pi 原生字段） |

⇒ 结果：**内层 pi 看到的就是自己的配置**，插件只是它的**受控写入入口**。
没有第二处真相、没有同步问题、`pi auth check` / `pi list` 等原生命令全部照常工作。

#### 3.6.4 ★ 配套工具：插件作为 pi 配置系统的受控入口

［已定］**插件要提供对应的 tool 进行对应的配置。** 工具与 pi 的配置文件一一对应：

```
# —— 源与模型（→ models.json / auth.json / settings.json）——
commondev_pi_source_add      name, base_url, api_key, model, [adapter, context_window, vision, ...]
                             # 写 models.json（+ key 写 auth.json）
commondev_pi_source_list     [--with-key]     # 列出来源（默认**不返回** key）
commondev_pi_source_remove   name
commondev_pi_source_default  name, [model]    # 写 settings.json 的 defaultProvider/defaultModel
commondev_pi_source_import   from_kernel       # 把 HomeAgent 内部配置的源批量导入

# —— 其他 pi 配置面（同一套纪律）——
commondev_pi_config_get      key, scope=plugin|project, workdir?
commondev_pi_config_set      key, value, scope   # 直接落 pi 配置文件
commondev_pi_package_install source, scope       # 详见 §3.5.2（pi install）
commondev_pi_mcp_add/remove/list                 # → mcp.json
```

**四条实现纪律**：

1. **写的就是 pi 的配置文件**，不造中间层；格式与 pi 自己写出来的保持一致
   （否则 pi 重写文件时会把我们的自定义字段抹掉）。
2. **改前备份、改后校验**：json 写坏了会让内层 pi 直接起不来；
   写后必须回读校验，失败则回滚并**响亮报错**。
3. **key 不进日志/不进返回值**：`source_list` 默认不返回 api_key（与
   `commondev_account_list` 同一条纪律）。
4. **不改用户全局**：所有写入都带 `PI_CODING_AGENT_DIR=<agentdir>` 或显式 scope，
   绝不碰 `~/.pi/agent/`。

#### 3.6.5 ★ 两条安全红线

1. **API key 不能明文落盘到无保护的位置**。`models.json` / `auth.json` 里的 key 是明文
   —— 它们落在 `<agentdir>/`，因此**该目录 0700、文件 0600**；与凭据文件同一套纪律
   （前篇 §7.1）。内核那套是明文存 `config.db`（且该库 **644 全局可读**），
   **不要把这套惯例带进来**。
2. **不得静默回退到宿主凭据**：如果导入失败/源不可用，要**报错**，
   而不是偷偷用 `~/.pi/agent/auth.json` 里的用户凭据跑起来。

> **为何不复用用户的 `~/.pi`**：那是宿主自己的 agent 环境，凭据归宿主。
> 插件作为独立工具，拿宿主凭据 = 权限越界（前篇 §7.1 同一理由）。

---

## 4. 生命周期与孤儿清理

［已核］内核的硬约束是「**子不得比父活得久 / 无孤儿 goroutine / 无残留 temp**」
（`resident-subagent-design.md §10`，写成 S16 判据）。commondev 必须按**同一条纪律**
管住自己拉起的进程：

| 层 | 谁销毁 | 何时 |
|---|---|---|
| pi 进程 / sidecar | commondev `Stop()` | 插件停止、工程对话 close |
| ssh 会话 | commondev | 同上 |
| **workdir（`program/<hash>-<name>`）** | `commondev_workdir_clean` | **合入（或废弃）后立即删** |
| 内核驻留子 | **父 agent**（`resident_agents destroy`） | 主管决定；插件**不能**代劳 |
| 划入的 inputch | 内核在子销毁时归还 | 自动 |

**workdir 删除时机是用户指定的**：该分支的修改**提交合入后直接删除目录** ——
不保留、不归档。目录内不得留有可复用状态（尤其是凭据，见前篇 §7.2）。

⇒ **插件 `Stop()` 必须是幂等的全清**，并写成判据：`TestStopKillsAllRuntimes`
（变异：删掉某个 repo 的清理 → 必须报红）。

---

## 5. 语义缺口（必须显式取舍，不能假装等价）

［已核］内核的两类输入语义与 pi 的两种投递**不等价**：

| 内核 | pi | 差异 |
|---|---|---|
| 中断（有级别，**抢占 + 保存现场 + LIFO 恢复**） | `steer`（插到当前轮之后、下次 LLM 前） | pi **不挂起/不恢复**，是「并入当前上下文继续」 |
| 排队（无级别，FIFO） | `follow_up`（agent 空闲后投递） | 语义接近 |

⇒ 「打断 pi → 事后恢复原任务」这个内核语义，**pi 没有**。三个选项：
- **(i) 接受降级**：steer 即"插话"，不承诺恢复（简单，但主管必须知道断点在哪）；
- **(ii) 插件补保存/恢复**：记下 steer 前的目标，之后用 `follow_up` 补投；
- **(iii) 用 `--fork` 分叉**：纠偏走新分支，原会话留作对照（代价：会话数增长）。

［待定］见 §6 U4。

---

## 6. 未决问题（需用户拍板）

| 编号 | 问题 | 本文建议 |
|---|---|---|
| **U1** | commondev 的 **L4 上限**：接受 L3 / 改内置插件 / 加插件侧"上级"通路 | **先接受 L3 跑通**；L4 等需求真出现再做（`SetKernelSource` 属 ABI 变更，要按"只增不减"+ 同步 SDK 仓 + hmapdev 模板） |
| **U2** | 远端环境缺失时的**安装形态** | 默认只**输出可复制命令**供人去跑；由插件代装需显式授权（§3.4）。环境缺失时**先询问用户，不擅自安装** |
| **U3** | pi 进程**常驻保活** 还是**用完即弃**（靠 session-id 保连续） | ✅ **已定：常驻**（刀 1 已如此：sidecar 持续存活并复用同一会话，`branch_close` 才收） |
| **U4** | steer 的**语义降级**取舍 | (i) 接受降级，但把"断点"写进台账供主管复查 |
| **U5** | skill 的**分发目标** | 双投：主管侧（HomeAgent `<data_dir>/skills/`）+ 执行侧（工作区 `.pi/skills/`） |
| **U6** | 「工程对话」的**用户可寻址名** | ✅ 已实现：sidecar 接收 `--session-name`，agent_up 返回并记账 `session_name` |
| **U7** | **独立 node/pi 的获取方式** | ✅ **已定：随包分发**（不首次下载、不依赖用户装） |
| **U8** | 内层 pi 的**版本锁定** | 部分实现：`pi_version` 已从 sidecar 回显并记账；随包独立 pi 二进制的发布锁定仍待发布流程完成，解析不到时显式警告 |
| **U9** | `auth.json`/`models.json` 中 key 的处理 | ✅ **已定：常驻文件 + 0700/0600**（实测生效：目录 0700、文件 0600 + 回读校验） |
| **U10** | 插件私有源的**存储位置** | ✅ **已定：不存在“私有源存储”** —— 直接写 pi 自己的配置文件（§3.6.3）。插件自身的 Git 凭据存**独立 0600 文件** `<data>/credentials.json`（已实现，启动时校验权限） |

---

## 7. 自检清单（逐条可复现）

```bash
# §1.2 自包含运行时：agent dir 可重定向、不污染用户全局
mkdir -p /tmp/st/{agentdir,work,pkg/extensions}
# 建一个最小包（package.json 带 keywords:["pi-package"] + pi.extensions）
cd /tmp/st/work && PI_CODING_AGENT_DIR=/tmp/st/agentdir pi install /tmp/st/pkg
cat /tmp/st/agentdir/settings.json          # 期望 packages 写在这里
python3 -c "import json;print([p for p in json.load(open('/root/.pi/agent/settings.json'))['packages'] if 'st' in str(p)])"
#                                           期望 [] ⇒ 用户全局零污染

# §1.2 ⚠ 安装是复制：改源目录不影响已装的（我曾因此误判"插件不生效"）

# §3.5.2 两种粒度
cd <workdir> && pi install -l ./pkg         # 临时 ⇒ <workdir>/.pi/settings.json
PI_CODING_AGENT_DIR=<agentdir> pi install /abs/pkg   # 全局 ⇒ <agentdir>/settings.json

# §3.6.1 LLM 源的真实位置
sqlite3 /home/newqqagent/config.db \
  "select key,substr(value,1,60) from config where key like 'core.llm.%' order by key;"
#   期望：core.llm.provider=llmsproxy + core.llm.sources.<n>.{base_url,api_key,model,adapter,...}

# §1.1 会话续投（同一 session-id）
mkdir -p /tmp/p1 && cd /tmp/p1
pi -p --session-id chk-01 "记住暗号：紫罗兰-7734"        # → 记住了
pi -p --session-id chk-01 "刚才的暗号？"                  # → 紫罗兰-7734
ls ~/.pi/agent/sessions/--tmp-p1--/                       # 只有一个 chk-01 文件

# §1.1 并发打同一 session 会写坏（判据的反例）
#   两条 -p 同 --session-id 并发；检查 jsonl 里 user 消息是否相邻、assistant 是否错序

# §1.1 会话库可搬且不污染全局
pi -p --session-dir /tmp/sd-store --session-id sd-01 "…"
ls /root/.pi/agent/sessions/ | grep -i sd-test      # 期望无结果

# §1.3 可接入面
grep -n "func (s \*PluginSDK) RegisterInputChannel" third_party/homeagent-sdk/sdk/plugin.go
grep -rn "resident_agents" internal/plugin/proc/    # 期望无结果 ⇒ 插件调不到

# §2.4 L4 归属
grep -n "isKernelLevelSource" -A20 internal/agent/core/scheduler.go   # 期望两条分支
grep -n "clampExternalPriority" -A10 internal/plugin/proc/corehandler.go

# §2.1 inputch 路由发生在进内核之前
cat internal/agent/core/inputroute.go
```

---

## 8. 给接手者的三条提醒

1. **不要另起一套调度器**。内核已有四级中断 / 两类输入 / 可抢占 / 中断栈 / 驻留子 /
   处理表 —— 插件的活是**把 pi 接进 inputch 路由**，不是重实现。
2. **不要用提示词软约定替代机制**。「让 agent 记得传 `--session-id`」是软约定；
   台账记 `session_id` + 投递后断言未变，才是机制（§3.3）。
3. **孤儿进程 = 插件的责任**。内核对自己的子有硬约束（S16），插件对自己的进程
   必须同样硬（§4）。
