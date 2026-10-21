---
name: commondev-discipline
description: commondev 主管式多仓库开发流水线的使用与开发纪律。当你要为某个工程开一条分支、把工程派给一个下属 agent、或用 pi 拉起执行 agent 干活时必读。核心动作：为每个工程对话新建驻留式子 agent 并把该工程的一对通道划给它（否则所有工程都挤在根 agent 上）。**git 托管平台的操作（Gitea/GitHub/GitLab）不在插件里，而在本文** —— 用 enc_shell + 保存的凭据调 curl/CLI 完成。也含 hmap 插件开发与部署纪律。
version: 2.0.0
author: HomeAgent
---

# commondev 纪律（主管模式 · 工程对话 · 平台操作）

本技能三部分，**都可能与你的任务直接相关**：

- **第一部分：使用者纪律** —— 怎么开工程、怎么派活。
- **第二部分：★ git 平台操作** —— **本文是权威**。开 PR、审批、分支保护怎么做。
- **第三部分：开发与部署纪律** —— 怎么不踩坑。

> ## ★ 架构边界（用户拍板 2026-10-15）
>
> | 层 | 负责 |
> |---|---|
> | **插件 commondev** | 项目台账、工作目录、pi 会话与通道、判据与变异复核、冲突预检、凭据保存与安全执行 |
> | **本文（skill）** | **各 git 托管平台的认证头、API 端点、PR 与分支保护流程** |
>
> **插件里没有平台逻辑。** 操作 Gitea/GitHub/GitLab 等平台一律：
> 读本文 → 用 `enc_shell` + `@{凭据名}` 执行 `curl`/`gh`/`tea`。
>
> ⇒ **新增一个托管平台只需改本文，不需要改插件、不需要重编译、不需要重启服务。**
>
> 原本这些在插件里（`platformSpec` / `commondev_account_*` / `commondev_pr_*` /
> `commondev_protect_branch` / `commondev_repo_search`），代价是：
> 每接一个平台都要改 Go 代码 → 重编译 → 重打包 → 重启，
> 且**未列出的平台直接报错**（GitLab/Gitee/Bitbucket 全不支持）——
> 而 agent 用 curl 本来什么平台都能打。这 10 个工具已移除。

> 看不懂某个词时的权威文档（按顺序读）：
> 1. `docs/zh/commondev-design.md` —— 目标机制、台账 schema、判据与变异、安全（§12 是架构变更）。
> 2. `docs/zh/commondev-agent-runtime-design.md` —— 运行时与内核接入（续篇）。
> 3. `docs/zh/kernel-secret-firewall-design.md` —— 凭据模块（enc_* 的底座）。
> 4. `docs/zh/resident-subagent-design.md` —— 驻留子设计的原始文档。
>
> **本文与文档冲突时，以文档为准，并回来修正本文。**

---

## Setup: 第二部分 git 平台操作（本文是权威，不要凭记忆写 API）

### 11. 所有平台都是同一个形状

「带认证头发一个 REST 请求」。用 `enc_shell` + `curl` 即可：

```
enc_shell(command="curl -s -H \"<认证头>\" <API前缀><路径>")
```

**差异只有两个维度：API 前缀 与 认证头。** 其余是 REST 常规。

### 12. 认证头与 API 前缀（逐平台，★ 四种形状互不相同）

| 平台 | API 前缀 | 认证头 |
|---|---|---|
| **Gitea** | `<base>/api/v1` | `Authorization: token <T>` ← **不是 Bearer** |
| **GitHub** | `https://api.github.com` | `Authorization: Bearer <T>` |
| **GitLab** | `<base>/api/v4` | `PRIVATE-TOKEN: <T>` ← **不是 Authorization** |
| **GitCode** | `https://api.gitcode.com/api/v5` | `Authorization: Bearer <T>` |
| **Gitee** | `https://gitee.com/api/v5` | `access_token=<T>`（**查询参数**，不是头） |

⚠ 头名、前缀、甚至「用头还是用查询参数」都不同。**未知平台先查官方文档，不要猜形状。**

### 13. 先保存凭据，再用它

```
enc_store(alias="gitea-token", value="${GITEA_TOKEN}")     # 值优先走 ${ENV} 或 @file:
enc_shell(command="curl -s -H \"Authorization: token @{gitea-token}\" http://192.168.2.106:3000/api/v1/user")
```

⚠ **优先用 `enc_store`，不要用 `commondev_account_add`** —— 后者已不存在
（它内建了平台语义：platform/role/base_url，那正是被移出的东西）。

**先验凭据再干活**（早失败，别推了一半才发现 token 错）：

```
enc_shell(command="curl -s -o /dev/null -w '%{http_code}' -H \"Authorization: token @{gitea-token}\" http://192.168.2.106:3000/api/v1/user")
# 期望 200；401/403 = 凭据无效或权限不足
```

### 14. 常见操作（以 Gitea 为例，其他平台换前缀与头）

```bash
# 搜仓库
curl -s -H "Authorization: token @{t}" "http://host/api/v1/repos/search?q=myname&limit=20"

# 看仓库 / 分支
curl -s -H "Authorization: token @{t}" "http://host/api/v1/repos/jianf/MyProj"
curl -s -H "Authorization: token @{t}" "http://host/api/v1/repos/jianf/MyProj/branches"

# 开 PR（head=你的分支，base=目标分支）
curl -s -X POST -H "Authorization: token @{t}" -H 'Content-Type: application/json'   -d '{"title":"...","head":"feat/x","base":"main"}'   "http://host/api/v1/repos/jianf/MyProj/pulls"

# 查 PR 状态
curl -s -H "Authorization: token @{t}" "http://host/api/v1/repos/jianf/MyProj/pulls/7"

# 合 PR
curl -s -X POST -H "Authorization: token @{t}" -H 'Content-Type: application/json'   -d '{"Do":"merge"}' "http://host/api/v1/repos/jianf/MyProj/pulls/7/merge"
```

**push 时凭据不进 remote URL**：

```bash
# ✅ 一次性注入（仅本次命令）；clone 出来的 remote 保持干净
enc_shell(command="cd <repo> && git -c http.extraheader=\"Authorization: token @{t}\" push origin feat/x")
```

⚠ 若改用 `https://token@host/...` 形式，token 会**持久化进 `.git/config`** ——
删掉临时目录也删不掉凭据（`git remote -v` 就能读出来）。设计 §7.2 明令禁止。

### 15. Gitea 分支保护：让「必须复审」成为**机制**而非礼貌

**光有流程约定不算机制** —— 执行 agent 可以直接推主干。必须让**平台**拦。

```bash
curl -s -X POST -H "Authorization: token @{t}" -H 'Content-Type: application/json'   -d '{"enable_push":false,"required_approvals":1,"block_admin_merge_override":true,"dismiss_stale_approvals":true}'   "http://host/api/v1/repos/jianf/MyProj/branch_protections?name=main"
```

四个开关的意义（缺一个机制就有洞）：

| 开关 | 作用 | 缺了会怎样 |
|---|---|---|
| `enable_push: false` | 禁止直接推送 | 执行 agent 直推主干，绕过评审 |
| `required_approvals: ≥1` | 至少一个审批 | 自己就能合 |
| `block_admin_merge_override: true` | admin 也不能绕过 | 用 admin token 的人绕过一切 |
| `dismiss_stale_approvals: true` | 新提交作废旧审批 | 先批准再改代码，批准白拿 |

**真机实测（Gitea 1.24.2）**：

| 动作 | 普通用户 token | admin token |
|---|---|---|
| push `main` | ❌ `pre-receive hook declined` | ⚠ 可能成功（见下）|
| push `feat/*` | ✅ | ✅ |
| 自合 PR | ❌ `Does not have enough approvals` | ⚠ 取决于开关 |
| 审批 → 合 PR | — | ✅ |

⇒ **推论：执行侧必须用普通用户 token。** 用 admin token 会**静默绕过分支保护**
（看起来正常，实际机制失效且无报错）—— 这是最容易搞错的一点。

⚠ 分支保护已存在时用 **PATCH**（POST 会 403 `already exist`）。

### 16. 审批：三个必踩的 API 坑（实测）

1. **`event` 必须是 `APPROVED`，不是 `APPROVE`。**
   传 `APPROVE` 时请求**返回 200 且不报错**，但 review 状态落成 `PENDING` ——
   于是 `required_approvals` 永远不满足，合并时报 `Does not have enough approvals`。
   表象（权限不足）与真因（枚举值少个 `D`）毫无关联，极难自查。
2. **必须带 `commit_id`** —— 不带时同样落成 `PENDING`（与 `event` 是两个独立条件）。
3. **`required_approvals` 也约束 admin** —— 别指望用 admin 跳过审批。

```bash
# ① 先取 head sha（审批必须绑定**具体提交**，不是"移动的目标"）
enc_shell(command="curl -s -H \"Authorization: token @{t}\" http://host/api/v1/repos/jianf/MyProj/pulls/7 | grep -o '\"sha\":\"[a-f0-9]*\"' | head -1")
# ② 审批（event 必须 APPROVED，且带 commit_id）
enc_shell(command="curl -s -X POST -H \"Authorization: token @{t}\" -H 'Content-Type: application/json' -d '{\"body\":\"通过\",\"event\":\"APPROVED\",\"commit_id\":\"<sha>\"}' http://host/api/v1/repos/jianf/MyProj/pulls/7/reviews")
```

**失败时保留平台原话**：`Does not have enough approvals` 是区分「机制生效」
与「配置错了」的唯一线索，别把它翻译掉或吞掉。

---

## Usage: 第一部分 使用者纪律

> **★ 设计总纲 ［已定］**：插件对 pi 做的事，本质就是
> **「把给 pi 发消息包装一下、把 pi 返回包装一下」**，深度融合内核的 IO 抽象与中断系统
> —— 即 **pi = 一个 Device**。**不要自己造调度器 / 会话路由 / 中断栈**，
> 全部接到内核已有的那套上（`InjectInputTo` / `RegisterOutputChannel` / `InjectInterrupt*Opts`）。

### 21. 角色与单位

| 角色 | 是谁 | 干什么 |
|---|---|---|
| **人** | 老板 | 只与主管对接 |
| **主管** | 你（长期驻留的 HomeAgent 主 agent） | 拆任务、**定义判据**、复审、决定合入 |
| **执行 agent** | 按工程拉起的一个 pi / dsh 实例 | 在某棵工作树里干活 |
| **commondev 插件** | 外部独立插件 | 仓库/凭据/分支/PR/测试，全部机械化操作 |

**为何能用内核机制**（而不是自己造）：

| 内核概念 | pi 侧对应 | 落点（内核已有） |
|---|---|---|
| **给 pi 发消息** | `pi.prompt` / `steer` / `follow_up` | `InjectInputTo(source, "commondev/<id>", …)` |
| **pi 返回** | sidecar 回调 / `tool_execution_*` / `agent_settled` | `InjectTextTo(source, "commondev/<id>", …)`（按 inputch Owner 路由） |
| **中断 pi** | `steer`（插到当前轮之后） | `InjectInterruptTextOpts(..., Priority: L3)` |
| **音声/图像产物** | pi 可产出的媒体 | `OutputCapabilities()` 声明 |

⇒ **插件只做两件额外事**：① 把 pi 接成 Device；② 台账记 `session_id` 并断言未变。
调度/中断/路由/记忆策略**全是内核已经在做的事**。

### 22. 「工程对话」是一等对象

它是四元组，缺一不可：

```
工程对话 (id = br-<project>-<slug>)
├─ workdir      <插件数据目录>/program/<hash>-<name>
│                ★ hash = H(项目+分支) 是**唯一性来源**
│                  ⇒ **同一仓库的不同分支必然是不同目录**（否则两 agent 共用工作树、改动互相污染）
│                ★ name = 分支名派生，**仅做可读性**，不承担唯一性
│                  （派生后可能撞：`feat/a` 与 `feat.a` 都成 `feat_a`，所以必须带 hash）
│                独立 clone（非 worktree、非 --reference）；合入后**整目录删除**
├─ session_id   pi 会话（★ 续投的唯一依据）
├─ runtime      local（默认，内嵌） | ssh（远端同样建目录拉 pi）
├─ host         runtime=ssh 时的目标主机（**branch_open 时给**，见 §24.1）
├─ agent_cli    执行后端（默认 pi；其他 CLI 需自备 sidecar，见 §24.2）
└─ channels     插件为该工程注册的一对通道
                ├─ inputch  commondev/<id>
                └─ outputch commondev_<id>
```

**一个仓库可并行开发**：每分支一份独立 clone，互不污染、可同时推进。
但并行有前提——必须是**互不相干的模块**；有依赖的任务请先拆边界。

> **为何不能用 worktree / `--reference`**（实测）：worktree 删后有
> `.git/worktrees/` 残留（需 `prune`）；`--reference` 有 alternates 外部依赖，
> mirror 被清即损坏。**本地 clone 对象为硬链接**（实测 inode 相同）⇒ 省磁盘、
> 无外部依赖、刪掉即彻底。代价：失去 worktree「同分支不可双挂」的保护
> ⇒ 台账层必须补**唯一性闸**（同名未结分支响亮拒绝）。

### 23. ★ 必做：为每个工程对话新建驻留子，并把通道划给它

**★ 顺序很关键**：通道是**拉起 pi 拿到 session 之后**才注册的，所以你的动作是：

```
建工程 ──► commondev_agent_up（拉起 pi，拿到 session_id）
            └─► 插件**此刻才**注册通道 inputch commondev/<id> + outputch commondev_<id>
                  └─► ★ 你建驻留子并把这对通道划给它
```

**为何必须先有 session**：通道是“与某个具体 pi 会话对话的入口”，没 session 就是空壳。
顺序错了（先建子）会拿到一个指向不存在通道的 `output_send__…`。

**插件只能把通道登记出来；「划入（assign）」只有内核能做。** 所以这一步**必须由你（agent）
自己调内核工具完成**，插件无法代劳。

```
resident_agents create
    id              = dev-<br-id>
    input_chs       = commondev/<br-id>       ← 划入该 inputch（可用斜杠）
    allowed_outputs = commondev_<br-id>       ← 授权该 outputch（★ 下划线，见下）
    capacity        = 64
    task_prompt     = "你在 <workdir> 推进 <work>。
                       下任务给自己用 output_send__commondev_<br-id>(payload=…)，
                       pi 的结果会回投到你的 commondev/<br-id>；
                       有结论用 notify_parent 汇报。"
```

**★ 下任务不需要新工具**：给 pi 发消息就是子调**内核原生的输出门**
`output_send__commondev_<br-id>(payload="…")` ⇒ 打到插件的通道 handler ⇒ 插件
查台账拿到 `session_id` 后 `sidecar.prompt()`。**插件不提供 `agent_send` 之类的工具**。

**⚠️ 通道命名（很容易踩）**：

| | 能用 `/` 吗 | 为什么 |
|---|---|---|
| **inputch** | ✅ | 内核自己就用 `child/<id>` |
| **outputch** | ❌ **绝对不行** | 它会拼成 `output_send__<name>`，上游要求函数名 `^[a-zA-Z0-9_-]{1,64}$`；违反的后果不是“这个工具不可用”，而是**整条请求 400、agent 整体失语** |

⇒ 所以 inputch 叫 `commondev/<br-id>`、outputch 叫 `commondev_<br-id>`。
`<br-id>` 本身也只能用 `[A-Za-z0-9_-]`。

**为什么必须这么做（不是可选的优化）：**

1. inputch 是**最基本的输入路由单位**，路由按 `Owner` 在**进内核之前**完成。
   划给子之后，**该工程的输入只流向那个子**，根 agent 看不到它 —— 这正是
   多工程并行能做到"互不串台"的原因。
2. **不划会的代价**（如果你偷懒）：所有工程对话的输入都涌向**根 agent**，
   根成了瓶颈、上下文被 N 条工程的细节淹没，多工程并行**等于没做**。
   现场教训（`resident-subagent-design.md`）：子挂着 `inputch=[timer]` 而 timer
   的输入打在父身上，**子的轮次永远是 0** —— 登记了不等于划了。

**落地判据**（怎么知道做对了）：

- 投一条输入到 `commondev/<br-id>`，应只被 `dev-<br-id>` 处理；
- 根 agent 的轮次**不**因这条输入增加；
- `input_channels` 工具（内核自带，单工具多视图）用 `view=by_agent` 能看到划分总览。

### 24. 形态选择、自包含运行时与远端环境

**默认本地内嵌**（进程内 pi 实例）；通过配置 `runtime=ssh` 切到**远程**，
远端**同样创建目录并在该目录拉起 pi**。

**★ 自包含运行时**：插件**自带独立的 node 与独立的 pi**，都装在**插件工作目录**，
**pi 的插件也装到这个目录** —— 不靠宿主机的 node/pi，也不污染用户的 `~/.pi`。
**独立 node/pi 随包分发**（不首次下载、不依赖用户装）。

```
<插件数据目录>/runtime/
  node/  pi/
  agentdir/          ← 每个内层 pi 必须传 PI_CODING_AGENT_DIR 指这里
    settings.json    ← pi 自己的配置（偏好 / 默认 model / 全局包声明）
    models.json      ← pi 自己的配置（★ LLM 源写这里）
    auth.json        ← pi 自己的配置（key / OAuth，0600）
    mcp.json         ← pi 自己的配置（MCP 服务器）
    skills/ prompts/ themes/ extensions/
```

#### 24.1 远端（ssh）怎么开

两个轴**正交**，开线时一起给：

```
commondev_branch_open(project="p", branch="feat/x",
                      runtime="ssh", ssh_host="build@10.0.0.9")
```

- `runtime=ssh` 而**没给 `ssh_host`** → 直接拒绝（不会拖到拉起时才报）
- **未知 runtime**（比如 `k8s`）→ 响亮拒绝，**不静默降级为 local**
  （静默降级会让"我以为跑在远端"一路错下去）
- 远端前置条件（**先配好，不要指望交互输密码**）：
  - 从本机到目标机的**免密**（`BatchMode=yes`，卡在密码提示会超时）
  - 远端有 node（不在 PATH 里就配 `ssh_node_path`，例如 `/opt/node/bin/node`）
  - **必配两个根目录**（缺任一，远端线开不起来）：
    - `ssh_workdir_root` — 远端放**工程工作目录**的根
    - `ssh_runtime_root` — 远端放 **sidecar 脚本**的目录
- **★ 工作目录在远端准备**：本地 `clone --mirror` 出来的目录远端看不到，
  所以远端线走的是**远端建目录 → 远端共享 mirror（`<root>/.mirrors/`）→
  远端 clone → 远端切分支**。同一远端上的多条分支线共享一份 mirror。
  台账里的 `workdir` 会被改写成**远端路径**（不是本地路径）。
  远端拉取要求**远端自己能认证该仓库** —— 本插件**不把凭据推到远端**
  （凭据不进工作目录、不进 remote URL）。拉不动就响亮报错。
- 每次拉起会 `scp` 把 sidecar 推过去（幂等）——**不推就会跑上一次的旧脚本**，
  症状是"改了没生效"，极难归因。推送失败**响亮报错**，不静默用旧脚本。
- 用 `commondev_agent_state` 看 `ssh_host` 确认打到哪台机。

#### 24.2 接其他 CLI（zcode 等）——**不改插件核心**

执行后端是**声明式**的：放一个目录就多一个后端。

```
<插件数据目录>/runtime/<cli>/
    driver.json     ← 描述：脚本名 / 参数 / 环境变量 / LLM 源策略
    sidecar.mjs     ← 实现 sidecar 契约（ping / prompt / state / ...）
```

**脚本查找链**（按优先级，go 侧 `locateSidecarScript`）：

| 顺序 | 位置 | 用途 |
|---|---|---|
| ① | `<插件数据>/runtime/<cli>/<script>` | 用户自备 / 覆盖随包版本 |
| ② | `<插件目录>/rawfile/runtime/<cli>/<script>` | **随包分发**（内置 pi 靠这条） |
| ③ | `<可执行文件同目录>/<script>` | 开发期（hmapdev debug） |

⚠ **② 是内置 pi 的必需项**（不是可选）：`sidecar.mjs` 随插件包分发，
装在 `rawfile/runtime/pi/`。全新机器上不需要任何手工准备 ——
若这条断了，表现是「插件装了、工具都在、但一拉 agent 就报找不到脚本」，
且错误里那个路径用户从没听说过。

自己加 CLI 时：把 `driver.json` + 脚本放在 `<插件数据>/runtime/<cli>/`（①），
两条都行；① 优先。

`driver.json` 关键字段：

| 字段 | 含义 |
|---|---|
| `script` | 脚本文件名（默认 `sidecar.mjs`） |
| `node` | `auto` = 用插件自带的 node |
| `args` | 参数模板，占位符 `{workdir} {sessionId} {sessionName} {dataDir} {pluginDir}` |
| `env` | 环境变量模板（同样支持占位符） |
| `llmConfig` | `pi` = 插件替它写 pi 的配置；**`none` = 它自管凭据，插件不碰** |
| `sessionDirSuffix` | 会话目录后缀 |

**纪律**：

- **未知 `agent_cli` 响亮报错，绝不回退到 pi** —— 回退会用错后端跑任务而无提示。
- `driver.json` **损坏也报错**（不猜默认），否则是"改了没生效"的经典来源。
- 内置 pi **不需要** `driver.json`（有内建描述），老部署不受影响。
- 现成例子看 `examples/custom-cli/zcode/`（**源码核实过**的 ZCode 契约）。

### 25. ★ 两种插件安装粒度（临时 vs 全局）

| 粒度 | 作用域 | 落到实处 | 用什么 |
|---|---|---|---|
| **临时** | 一个工程对话 | `<workdir>/.pi/settings.json` | `pi install -l` |
| **全局** | 插件自带 pi 的**所有**工程 | `<agentdir>/settings.json` | `pi install` + `PI_CODING_AGENT_DIR` |

**支持的源**（实测）：`npm:@scope/pkg@1.0.0`（带版本即钉住）/ `git:github.com/u/r@v1` /
`https://github.com/u/r`（当 git 源）/ `ssh://…` / 本地路径（**不复制**）。

**三条纪律**：

1. **必须传 `PI_CODING_AGENT_DIR`**，否则「全局」安装会写进**用户的** `~/.pi/agent/settings.json`
   —— 那是对宿主的越界。
2. **⚠ 安装是复制，不是引用**：`pi install ./pkg` 会把包**复制**到目标；改源目录
   **不影响已装的那份**（我曾因此得出「插件不生效」的错误结论）。要更新必须重装。
3. **安装是显式一步**，不藏在 `agent_up` 里；来源（npm 包名 / 仓库 URL / 版本钉点）要可见。

### 26. ★ LLM 源与 pi 配置：直接写 pi 的配置文件

⛔ **不要自建第二套配置存储**（本稿前版曾提 `config_commondev`，**已废弃**）。
**pi 本身就有完整的配置系统**，插件只是它的**受控写入入口**：

```
<agentdir>/settings.json   ← 偏好 / 默认 model / 「全局」包声明
<agentdir>/models.json     ← 兼容端点与模型（★ LLM 源写这里）
<agentdir>/auth.json       ← API key / OAuth（0600）
<agentdir>/mcp.json        ← MCP 服务器
```

内核把 LLM 源存在 **`config` 表**（不是 `config_<plugin>`），键前缀 `core.llm.`：

```
core.llm.provider                 = llmsproxy     ← ★ 默认源就是这一项
core.llm.sources.<n>.base_url / .api_key / .model / .adapter / .context_window …
```

**导入时字段一对一对上**：`base_url`→`baseUrl`、`api_key`→`apiKey`、
`adapter=openai`→`api="openai-completions"`、`model`→`models[].id`、
`context_window`→`contextWindow`。

**工具面**（每个工具对应一处 pi 配置）：

```
commondev_pi_source_import   把内核的源批量导入内置 pi
commondev_pi_source_add      添加源（写 models.json / auth.json）
commondev_pi_source_list     列出（默认**不返回** api_key）
commondev_pi_source_default  指定默认源（写 settings.json）
commondev_pi_config_get/set  读写其他 pi 配置
commondev_pi_mcp_add/remove/list
```

**四条实现纪律**：

1. **写的就是 pi 的配置文件**，不造中间层；格式与 pi 自己写出的保持一致
   （否则 pi 重写文件时会把自定义字段抹掉）。
2. **改前备份、改后回读校验**：json 写坏会让内层 pi 直接起不来；失败则回滚 + **响亮报错**。
3. **key 不进日志/不进返回值**。
4. **不改用户全局**：所有写入带 `PI_CODING_AGENT_DIR=<agentdir>` 或显式 scope，
   绡不碰 `~/.pi/agent/`。

**两条红线**：

1. **key 不能明文落盘到无保护位置**：`models.json`/`auth.json` 含明文 key ⇒ 目录 **0700**、
   文件 **0600**。内核那套是明文存 `config.db`（**644 全局可读**）—— **别把这套惯例带进来**。
2. **不得静默回退到宿主凭据**：导入失败/源不可用就**报错**，不要偷偷用 `~/.pi/agent/auth.json`
   跑起来（那是宿主的凭据，用了就是越界）。

### 27. 远端环境缺失：问用户，不要自己装（硬纪律）

```
ssh 不通          → 报错（配置问题，不是缺环境）
无 pi / 无 node   → ★ 注入 L3「远端 <host> 未装 X，是否安装？」→ **你（agent）去问用户**
目录不可写        → 报错（权限问题，需人处理）
```

- **SDK 没有“向用户提问”的 API**（插件是子进程，不能直接弹 UI）。现有机制是插件把
  决策消息注入给 agent（L1–L3），**由 agent 问用户**（先例：`cli` 的 `/stop` 用 L3）。
- **不得擅自安装**：不 `curl | sh` 到别人的机器上。要先说清「装什么、装哪里、要什么权限」。
- **不冒充成功**：环境缺失时 `agent_up` **响亮失败** + 留一条待决策项，
  **不静默降级到本地跑**（那会让人以为活派到了远端）。
- **问完要能续**：安装动作本身是**可见的一步**（可查、可重跑），不是 `agent_up` 的隐式副作用。

### 28. 投递纪律（**这是最容易犯的错**）

**每个工程的投递必须落到同一个 `session_id`。** 实测反例（别重犯）：

| 错法 | 后果 |
|---|---|
| 漏传 `--session-id` | `pi` **静默开新会话** —— 你以为在催上一次，其实是把活派给了陌生人 |
| 两个进程**并发**同 `--session-id` | 会话文件被写坏：`user A`/`user B` 相邻，`assistant B` 在前、`assistant A` 落在最后 ⇒ B 那轮的模型上下文里**根本没有 A 的用户消息**，树链交错 |

⇒ 投递前先拿台账里的 `session_id`，投完**断言它没变**；一个 session **绝不并发投递**。
这两条插件侧已做成结构性保证（单一投递队列 + 断言），你做主管时**不要绕过插件**直接
去敲 `pi -p`。

**★ 另外两条实测硬经验（别再踩）：**

3. **SDK 路径必须显式指定会话 id**。用 `createAgentSession({cwd})` 而不传
   `sessionManager` 时，pi **自己生成 UUID** 作会话 id ⇒ 与台账名字不一致 ⇒
   断言**永远失败**（实测：期望 `br-cdtest-feat_alpha`，实际 `01a13959-…`）。
   正确：`SessionManager.create(cwd, sessionDir, {id})` 再传给 `createAgentSession`。
4. **工具返 `ok` ≠ 消息到了**。内核对成功回执刻意**极简**（防 output loop），
   所以 `ok` 只说 handler 成功了。必须去会话文件里看到 `user: <payload>` 才算数。
   （这条能防住“以为投了、其实没投”这类假成功。）

### 29. 三层审查的分工（别把自己变成贵的高级码农）

| 层 | 审什么 | 不审什么 |
|---|---|---|
| agent 自审 | 代码正确性、风格、实现细节 | — |
| **你（主管）复审** | **这次的改动，判据够不够**：引入了哪些新失败方式？判据覆盖了吗？变异咬得住吗？ | 逐行读 diff |
| 插件复核 | 机械执行「正向 + 变异」 | 语义判断 |

### 30. ★ 不要把「测试通过」当作「改对了」

插件只能跑**你预先写下的判据**。判据没覆盖那条 bug，插件照样打勾 ——
自动化**不会**发现假绿，**只会让假绿跑得更快、更可信**。

因此复核**必须两步**：

| 阶段 | 做什么 | 不通过的后果 |
|---|---|---|
| **正向** | 跑判据 → 期望通过 | 实现有 bug，驳回 |
| **变异** ★ | 自动改坏被测代码 → 期望判据**报红** | **判据太松，驳回** |

**没有变异阶段，就是在给「测试通过」这个信号不断充值信用，直到它骗一次大的。**
本仓实证：一条只查前缀的判据在变异测试下仍全绿，是变异把它咬出来的。

配套纪律：判据写「某标识符是否出现在函数体」时**必须先剔除注释再匹配**
（曾因注释里含 `outputParent` 之类的词导致假绿）。

### 31. 工程的目录与生命周期（一个仓库并行开发）

```
branch_open
  └─ <hash>-<name>/ （由 mirror **本地 clone** 派生）
       ★ hash = H(项目+分支)：**唯一性来源**（同仓库不同分支必不同目录）
       ★ name = 分支名派生：仅可读性
       └─ 干活 → 提交 → push → PR
            └─ 合入（或废弃）
                 └─ ★ **整个目录删除**（不保留、不归档）
```

**要点**：

- **合入后直接删目录** —— 用户明确要求；不删则下次同名分支会与残留目录混淆。
- **一个仓库可并行**：每分支一份独立 clone。
- **★ 同仓库不同分支必然不同目录**：`<hash>` 含分支（`H(项目+分支)`），hash 是**唯一性来源**；
  `name` 只做可读性。实测：`feat/alpha` vs `feat/beta` → `b3426c01-feat_alpha` vs `0566c1f5-feat_beta`。
- **⚠ 慢操作必须异步**：内核把**每次工具调用**硬限制在 **60 秒**（`toolcall.go:62`，
  硬编码无配置项）。`git clone --mirror` 同步做 ⇒ 工具报超时、**但 mirror 其实已建好**
  —— 调用方只看到失败、状态却是“做了一半”。正确做法：工具只登记 + 发起后台作业，
  用状态工具追进度（本插件：`commondev_job_status`）。
- **为什么不是 worktree / `--reference`**（实测）：worktree 删后有 `.git/worktrees/`
  残留（需 `prune`）；`--reference` 有 alternates 外部依赖，mirror 被清即损坏。
  **本地 clone 对象为硬链接** ⇒ 省磁盘、无外部依赖、删掉即彻底。
- **⚠️ 代价：唯一性闸必须补上**。worktree 天然禁止「同分支双挂」，独立 clone **没有**这条
  保护（两份 clone 都能在 `feat/x` 上各自 push，直到被拒才知道）。
  台账必须对「同名且未结（open/review）的分支」建唯一索引，重复 `branch_open`
  **响亮拒绝**，不静默复用。
- **并行有前提**：并行的必须是**互不相干的模块**。有依赖的任务（A 改接口、B 用接口）
  两棵树互不可见，问题会集中到 merge 时爆发 ⇒ **先拆边界再谈并行**。
- 与内核「子不得比父活得久、无残留」同一条纪律（见第 20 节）。

---

## Notes: 第三部分 hmap 插件开发与部署纪律

> 面向「要改 commondev」或「要写另一个插件」的场景。**所有条目都在本机实测过**，
> 不是从文档抄的。

### 32. 先搞清三件事，别猜

```bash
hmapdev version        # 工具链版本 + SDK 模块 + 构建用 Go
hmapdev sdk current    # 当前活跃 SDK 版本（★ 动手前必看）
hmapdev sdk list       # 本机已装的所有 SDK 版本
```

- 核心仓 `/home/program/TrueAgent`（Go module `github.com/JianFeeeee/HologramAgent`）
- **SDK 仓** `third_party/homeagent-sdk`（独立 git 仓、独立版本号）

### 33. 开工流程

```bash
hmapdev init <name>              # Go 脚手架（--lua / --type remotedevice 可选）
cd <name> && vim plugin.go       # 注册工具
hmapdev debug .                  # 不想装就先跑一遍
hmapdev build                    # 产出 dist/<name>_bundle.hmap
```

### 34. ★ 五个必踩的坑（按踩中概率排序）

| # | 坑 | 事实 |
|---|---|---|
| 1 | **SDK 版本** | 插件编译时**校验 SDK 能力**，用旧接口的工程直接构建失败，报错形如「当前 SDK 缺少 sdk.InjectOptions.Priority」，并给出两条出路：`hmapdev sdk install <version>` 或 `hmapdev sdk install --from <本地源码>`。**核心仓有新能力但 SDK 未跟上时用 `--from`** |
| 2 | **`hmapdev build`/`debug` 不支持 `--help`** | 不是打印帮助，而是去读当前目录的 `plg.json` ⇒ 你会看到 `error: read plg.json: no such file or directory`。**这不是故障**。要看 flag 用 `hmapdev --help` |
| 3 | **产物是 ZIP，不是二进制** | `dist/<name>_bundle.hmap` 是 ZIP（魔数 `PK`），用 `unzip` 解，内含 `plugin.bin.linux.amd64` 等多平台二进制。**生产要的是解包后的单个二进制** `/home/newqqagent/plugins/<name>/plugin.bin`（不是 zip） |
| 4 | **`plugin.json` 是安装标志** | 生产目录里没有它，插件不会被识别。`plg.json` 是**构建**标志，两者不同 |
| 5 | **`plg.json` 的 `sdk` 字段必须完整 x.y.z** | 写 `"1.4"` 会被**直接拒绝**（SDK 版本纪律：跟随内核中版本、patch 位恒为 `.0`，一条内核线只有一个 SDK 版本） |

### 35. 注册工具的正确形状

```go
s.RegisterTool(tp+"mytool", sdk.ToolDef{
    Name:        tp + "mytool",     // ← Name 必填，放结构体**首位**
    Description: "……",
    Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
    Handler:     h.mytool,
    NoMemory:    true,               // 声明字段一律放**末尾**
}, s)
```

**并发声明**（同一批多个 tool_call 时）：

- `ParallelSafe: true` —— 可并发。**三个条件都满足才可声明**：handler 线程安全 /
  不与同批工具争抢同一资源（SQLite 写、同一设备、同一输出通道）/ 执行顺序无关。
- `Serial: true` —— 必须串行，**优先级高于 `ParallelSafe`**。
- **默认（都不写）= 整批串行**，是保守设计不是遗漏。

⇒ commondev 的**写操作**（凭据增删、台账写、`workdir_prepare`、`pr_open/merge`、
`agent_send`）**一律 `Serial: true`**；**只读**（list/status）才 `ParallelSafe`。

### 36. ★ 外部插件的能力边界（选外部形态就必须接受）

**公开 SDK 里声明的符号，不等于外部插件拿得到。** 外部插件跑在独立进程里，能力由桥接注入：

| API | 外部插件的实际 | 该用什么 |
|---|---|---|
| `Events()` | **恒为 nil**（桥不注入 subscriber） | 桥接运行时按你的声明完成订阅 |
| `UnregisterOutputChannel` | **静默无效**（只注入 registrar，不注入 unregistrar） | 注销需重载插件 |
| `PriorityL4` | **被夹到 L3** | 用 L1–L3 |
| `SocialAPI` 写操作 | 只有 6 个**只读**方法 | 读用 `GetPerson` 等 |
| `PluginMgr()` | **只有 3 个方法**（`ReloadOne`/`ListLoadedPlugins`/`IsPluginDisabled`） | 就这 3 个 |
| `resident_agents` 等内核工具 | **调不到**（proc 桥无对应方法） | 由 agent 侧调（见第 3 节） |

> **外部形态的 L4 天花板**：commondev 作为外部插件**永远拿不到 L4**。

> **⚠️ 不要用 `ToolAPI` 去调内核内置工具（如 `resident_agents`）**：`ToolAPI` 是
> **单例**，per-agent 数据靠「最后注入者」，而创建驻留子也会注入 ⇒ 你可能打到
> **某个驻留子的表上**。这是内核已自陈的局限，别踩。

**L4 归属表**（这块最容易搞错）：

| agent | 它的"内核" | 谁能产生 L4 |
|---|---|---|
| 根 agent | 内核自身 + **内置**插件 | `raiseKernelInterrupt`、WebUI 终止按钮等 |
| 驻留子 | **父 agent** | 父（父→子消息固定 L4） |
| **外部插件（如 commondev）** | — | **永远不能**（被夹到 L3） |

⇒ 外部形态下，「立即取消整条分支」只能靠 **L3 打断主管 + 主管下令**。

### 37. 安装与验证

```bash
# ① 从 bundle 取出本机平台二进制
unzip -o dist/<name>_bundle.hmap 'plugin.bin.linux.amd64'
# ② 放进插件目录（<name> 必须与 plg.json 的 name 一致）
mkdir -p /home/newqqagent/plugins/<name>
cp plugin.json /home/newqqagent/plugins/<name>/
cp plugin.bin.linux.amd64 /home/newqqagent/plugins/<name>/plugin.bin
chmod +x /home/newqqagent/plugins/<name>/plugin.bin
# ③ 重启并确认工具**真的注册了**
systemctl restart homeagent.service
journalctl -u homeagent.service --since "-2 min" | grep "registering tool: <你的工具名>"
```

**内置 vs 独立**（目录存在 ≠ 有 `plugin.bin`）：

```bash
for p in webui cmd seq qq; do
  printf "%-8s " $p
  ls /home/newqqagent/plugins/$p/plugin.bin >/dev/null 2>&1 \
    && echo "独立（要单独构建部署）" || echo "内置（随 homed 部署）"
done
```

★ **改了内置插件的源码 ⇒ 必须重新构建并部署 homed**，重启服务不生效（`//go:embed` 编进二进制）。
★ 判断线上跑的是哪次构建，看 `commit` 字段（**不要用 `strings`**）：

```bash
K=$(sqlite3 /home/newqqagent/config.db "select value from config_webui where key='api_key';")
curl -s -H "X-API-Key: $K" http://127.0.0.1:8080/api/v1/status | grep -oE '"commit":"[^"]*"'
```

⚠ 必须带 `X-API-Key`：无认证时返回 **200 + 登录页 HTML**，只看状态码会误判。

### 38. 排查「插件装了却不生效」

按顺序查，**别跳步**：

```bash
ls /home/newqqagent/plugins/<name>/plugin.bin          # 1) 内置还是独立
journalctl -u homeagent.service --since "-5 min" | grep "registering tool:" | grep -i <name>   # 2) 注册了吗
journalctl -u homeagent.service --since "-5 min" | grep -E "loaded: <name>"                    # 3) 加载了吗
journalctl -u homeagent.service --since "-5 min" | grep -iE "<name>.*(error|panic|failed)"     # 4) 启动报错
```

### 39. ★ 安全纪律（commondev 拿的是**能 push 用户所有仓库**的权限）

> **★ 架构决定（用户拍板 2026-10-14）：敏感信息拦截下沉为内核安全模块。**
>
> 理由：**LLM 调用本身不可信**，而插件在**信任边界之外** ——
> 插件注册的 stage 拦截是“自愿的”，不是强制的，也算不上边界。
>
> ★ **它不是单纯拦截，而是一个闭环**（用户纠正：“只是简单的拦截的话，
> 绝大部分插件会失去原本的功能”）：
>
> ```
> ① 拦截      LLM 出向前扫到明文 → 换成 @{别名}
> ② 加密暫存  明文 AES-GCM **落盘**存内核（不落明文）
> ③ 别名      ★ **哈希键**：sec_ + HMAC-SHA256(masterKey, plain)[:8]
>               ⇒ **同一明文自动得同一个别名**（同变量合并，天然性质）
>               ⇒ 必须 HMAC 而非裸哈希（裸哈希可字典枚举）；**不加 salt**（否则失去合并）
> ④ 回填      插件调用前把 @{别名} 换回明文 ← 插件无感知
> ```
>
> ★ **变量不自动淘汰**（跨轮/跨子 agent 随时要用）；agent 可**主动**调用
> 安全模块提供的**提交/删除**工具。泄漏风险靠主动删控制，不靠超时。
>
> ★ **因为内核零 IO（一切外部交互都从插件走），只需两根咽喉**：
> · 入向 toolcall：`StageHost.ExecuteTool(name, args)`（`core/stages.go:140`）
> · 入向 outputch：设备 `Execute`（`core/output.go:76`）
> · 出向 LLM：provider 层的 `Chat(req)`（已核：构造 `CompletionRequest` 有 **7 处**，
>   task 层拦不全）
>
> ★ **变量系统是全局唯一的**（用户拍板）：主 agent / 子 agent / 驻留子 agent
> **同一套**别名表。已核依据：驻留子共享父的 `ProviderManager`（`resident.go:185`），
> 而它**全进程只建一份**（`bootstrap.go:249`）⇒ 安全模块天然是进程级单例，
> **不需要跨 agent 同步**。
> ⚠ **变量共享 ≠ 授权共享**：能不能调某工具/通道仍看该 agent 自己的 `AllowedOutputs`
> （与 `KernelSource` 一道是 per-agent 的）。两者**正交**。
>
> ★ **“插件无感知”是硬要求**：替换发生在路由到插件的**之前**；
> 插件代码一行不改、不可分辨、无法绕过 ⇒ 存量插件自动获得该能力，
> 且无需给 SDK 加参数（不破接口兼容）。
>
> **对本插件的直接影响（大幅简化）**：插件可以**把密钥明文直接投给 LLM**，
> 由内核安全模块全权负责；`enc_*` 工具族与加密 shell 应**删减**，
> 不再自带加密库/别名表（前稿那套作废）。
>
> 详见 `docs/zh/kernel-secret-firewall-design.md`（含硬约束 S1–S9）。
>
> **两条不可让步的约束**：
> · **只能替代引用，永远不可取回明文**（用户明确）—— 读接口与用接口是两件事；
> · **模式库宁漏勿误伤** —— 误报会搞坏正常任务。

**凭据：按名字引用 + 变量填充**（用户明确要求的能力）

```
enc_store(alias="gitea-token", value="${GITEA_TOKEN}")   ← ★ 引用，不是明文
enc_list                                                  ← 只列名字，**绝不返回值**
```

之后一切操作只带**别名**：`@{gitea-token}` —— 参数里永远不出现 secret。
详见 **第二部分 §13**（那里是权威）。

**为何要 `${ENV_VAR}` 变量填充**：（①）明文不落盘；（②）dev/prod 换环境变量即可；
（③）轮换凭据不需改插件数据。

#### ★★ 判据与复核：**插件 GREEN ≠ 改对了**

**这是本流水线最重要的一条纪律。** 插件只能跑你**预先写下**的判据；
判据没覆盖那条 bug，插件照样打勾。自动化**不会**发现假绿，
只会让假绿跑得更快、更可信。

**真实数据**（我用最小例子复刻过）：

```go
func Max(a,b int) int { if a>b { return a }; return b }
```
- 判据只测 `Max(5,3) == 5`（漏了 `b>a`）
- 变异 `return b` → `return a`（**真 bug**）⇒ **判据仍全绿**
- 补一条 `Max(3,5) == 5` ⇒ 同一变异**立刻报红**

⇒ 所以**复核必须两步**，你定义判据时**必须同时给变异规则**：

```
commondev_test_define(
  project="MyProj", name="clamp 上下界", kind="unit",
  script="go test -count=1 ./...",
  expect="上下界都被限制",
  mutations=[{ id="M1", file="calc.go",
               old="	if v > hi {\n		return hi\n	}\n", new="",
               note="删掉上界分支（真 bug）——松判据看不见它" }]
)
commondev_review_submit(branch_id="br-...")
commondev_review_result()          ← 取结论
```

**结论三档，注意第二档不是"通过"**：

| verdict | 含义 | 你该做什么 |
|---|---|---|
| `pass` | 正向全过 **且**全部变异被咬住 | 可以进下一步 |
| **`pass_with_gaps`** | 正向过，但**有变异没被咬住**或**判据没变异规则** | **不能当通过** —— 去补判据/变异，再复核 |
| `fail` | 有判据没过 | 打回执行 agent |

★ **`pass_with_gaps` 就是"你的判据有盲区"**。把它当绿色勾，
就是在给自己攒一次大的意外。

**定义变异规则的三个要点**：

1. **`old` 必须在文件里恰好出现一次** ——
   出现 0 次 ⇒ 变异无效（却会被误报成"判据太松"）；
   出现多次 ⇒ 可能改错地方（却会被误报成"咬住了"）。
   多出现时给出更长的上下文。
2. **变异要"真坏"** —— 改的是逻辑，不是格式。改不坏就验不出判据的松紧。
3. **`script` 里带 `-count=1`** —— Go 默认缓存测试结果；
   命中缓存时测试**根本没跑**，"变异后报红"就无从谈起。

#### 双凭据与分支保护：**已移到第二部分**（技能纪律 §15–§16）

原来这里用 `commondev_account_add(role=...)` + `commondev_protect_branch` 两个
插件工具实现。**这些工具已移除**（平台知识不再进插件）。

现在这样做（命令见 **第二部分 §15**）：

1. **在平台上建两个用户**：一个普通用户（执行侧，只能 push `feat/*`）、
   一个有管理权限的（主管侧，能审批与合并）。各自签发 token。
2. **两个 token 都存进凭据库**：`enc_store(alias="gitea-exec", ...)` /
   `enc_store(alias="gitea-sup", ...)`。
3. **给主干上锁**（第二部分 §15 的 curl 命令，四个开关都要）。
4. 审批与合并用主管侧别名（§16）。

★ **执行侧必须用普通用户 token** —— 用 admin token 会**直接绕过分支保护**，
机制失效且无任何报错。这是最容易搞错的一点。

★ **拦你的是平台，不是插件** —— 所以改插件代码也绕不过去。
这才是「机制」与「流程约定」的区别。

**两条不可逆的红线**：

1. **变量解析不到值 ⇒ 响亮报错**，绝不静默当空串 —— 后者会退化成匿名访问，
   表现成 **401** 而非“凭据没配”，极难排查。
2. **secret 绝不进返回值/日志**。

**平台形状差异**（不能一个认证头走天下）：

| 平台 | API 前缀 | 认证头 |
|---|---|---|
| Gitea | `<base>/api/v1` | `Authorization: token <T>` ← **不是 Bearer** |
| GitHub | `https://api.github.com` | `Authorization: Bearer <T>` |
| GitCode | `https://api.gitcode.com/api/v5` | `Authorization: Bearer <T>` |

未知平台**必须报错**（不猜形状）。

**其他：**

1. **凭据不入 `config.db`**：现有惯例是明文存 `config_<plugin>` 表，而 `config.db` 是
   **644 全局可读**。commondev 的凭据性质完全不同 ⇒ 放独立文件
   `<DataDir>/credentials.json`，**0600，启动时校验权限，不对就拒绝启动**
   （本仓纪律：响亮失败，不静默降级）。
2. **优先 SSH key 而非 PAT**；每平台独立、最小授权，**不要一个 token 通吃**。
3. **token 不进 remote URL**：拼接仅用于**单次命令**（`pushURLWithToken`），
   clone 出来的 remote 不带凭据 —— 否则删掉临时目录也删不掉凭据。
4. **不要硬编码 token**：走 `Settings()` 或宿主注入。
5. **拉起 pi 时隔离端口**：内层 pi 会尝试监听 A2A/ACP 端口并打 `EADDRINUSE`，
   显式设 `PI_CODING_AGENT_SESSION_DIR` / `PI_A2A_PORT` / `PI_ACP_PORT`。

### 40. ★ 孤儿清理（插件有自己的硬约束）

内核对自己的驻留子有硬约束：**子不得比父活得久 / 无孤儿 goroutine / 无残留 temp**
（`resident-subagent-design.md` §10，写成判据）。

**插件对自己拉起的进程必须同样硬**：`Stop()` 必须是**幂等的全清** ——
pi 进程、sidecar、ssh 会话、workdir（含凭据残留检查）一个不留。
写成判据：`TestStopKillsAllRuntimes`（变异：删掉某个清理 → 必须报红）。

### 41. 参考资料（**别凭记忆写 API**）

| 内容 | 路径 |
|---|---|
| SDK 源码 | `third_party/homeagent-sdk/sdk/` |
| **可运行的示例插件** | `third_party/homeagent-sdk/example/`（a2a / acp / qq / plugindev…） |
| 能力边界 | `third_party/homeagent-sdk/docs/guide/capability-boundary.md` |
| 并发声明 | `third_party/homeagent-sdk/docs/guide/parallel-tool-declaration.md` |
| 部署手册 | `docs/zh/deploy-runbook.md` |
| sidecar 参考实现 | `internal/plugins/clawhubadapter/sidecar.go` |

★ **写插件前先翻 `example/`**：那里的代码是**编译通过**的，比文档可靠。
**文档与示例冲突时以示例为准。** 注意 `example/` 是**外部插件**，
能被它们编译通过的写法，外部就一定可用。

---

## Notes: 红牌（做了就是错）

1. **不划通道就当派了活** —— 登记 ≠ 划入；不划则所有输入涌向根 agent。
2. **绕过插件直接敲 `pi -p`** —— 漏 `--session-id` 会静默开新会话。
3. **并发投同一个 session** —— 会写坏会话（实测）。
4. **没有变异阶段的复核** —— 等于给假绿充值。
5. **凭据写进 `config.db` 或 remote URL**。
6. **改了内置插件源码只重启服务** —— 必须重编 homed。
7. **手改生成的文档**（`docs/api/*.md` 首行写着「请勿手改」）—— 改 `sdk/*.go` 注释再重新生成。
8. **为了让插件生效去改 homed 的源码** —— 先确认它是不是内置插件。
