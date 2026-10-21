# CommonDev：主管式多仓库开发流水线（设计 · 未实现）

> **状态：架构已变更（见 §12）。** **git 托管平台专有的工具**已移出插件、进 skill；
> 其余机制（台账、工作目录、pi 会话与通道、判据与变异复核、冲突预检、凭据）保留。
> §4.1 里的平台工具清单**不再是实现目标**；§13 保留历史实现审计供追溯。
> 接手者请从 §12 开始读。
>
> **续篇（必读）**：`docs/zh/commondev-agent-runtime-design.md` —— 运行时与内核接入
> （工程对话四元组、内嵌/SSH 两形态、inputch 路由、L4 归属、语义缺口）。
> **本文 §6.1 的结论已被续篇修正**（只论证了 cwd 一维）。
>
> **★ 安全模块（计划追加）**：`docs/zh/kernel-secret-firewall-design.md` ——
> 敏感信息拦截**下沉为内核安全模块**（用户纠正：LLM 调用本身不可信，插件拦截不算边界）。
> 该文成立后，**本插件侧的加密 shell 会大幅变薄**（不再自带加密库/别名表）。
>
> **★ 设计总纲 ［已定｜2026-10-13］**：插件对 pi 做的事，本质就是
> **「把给 pi 发消息包装一下、把 pi 返回包装一下」，并深度融合内核的 IO 抽象与中断系统**
> —— 即 **pi = 一个 Device**。**不自己造调度器/会话路由/中断**，全部接到内核已有的那套上。
> 详见续篇 §2.5 / §3.1。
>
> **标记约定**（沿用本仓惯例）：
> - **［已核］** = 本文写作时已在本机实测/源码核对确认，附证据
> - **［待定］** = 需要决策，本文给出建议但未拍板
> - **［风险］** = 已知会让整套机制失效的点
>
> 撰写时间：2026-10-13。事实基线：TrueAgent 工作区 `/home/program/TrueAgent`，
> 生产实例 `/home/newqqagent`（homed PID 2966649 guard / 2966664 agent）。

---

## 1. 背景：这套机制要解决什么

### 1.1 用户的原话（原样引用，避免转述走样）

> 「其实我想的是以后你就是开发主管，我以后只跟你对接，然后你有一个 commondev 插件，
> 包含在远程 git 仓库全套操作支持添加多个账号搜索账号仓库等一系列能力）然后有项目列表
> 每个项目有独立的一系列（分支，对应分支分配给的 agent，分支进行的工作与验收与否）
> 然后具体实现是通过临时目录（插件数据目录下）有 program/hash-name 目录，每个目录下是
> 独立仓库的独立分支，每次修改包含（更新，bugfix 等）都是拉取仓库，拉出分支，修改，
> agent 自审，你复审，你调用插件提 pr 合入（注意审核这部分是插件提供一组让你定义单元
> 测试的工具，你定义过单元测试后，对应 agent 完成修改后你直接让插件进行复核即可）
> 插件中包含压力测试，接口测试，编译测试等的多个测试模板。」

关于多账号，用户后续澄清：

> 「我说的多账号是指插件持有多个 git 平台的访问凭据」

关于 agent 如何进目录，用户后续澄清：

> 「是 homeagent 负责把仓库拉下来，开出分支，然后在目录中拉起 agent 注入提示词，
> 这好像需要进一步完善优化 a2a/acp 或者直接调 cli」

### 1.2 角色模型 ［已定］

| 角色 | 是谁 | 职责 |
|---|---|---|
| **人（用户）** | 老板 | 只与「开发主管」对接，不直接指挥执行 agent |
| **开发主管** | 长期驻留的 HologramAgent 主 agent | 拆任务、**定义判据**、复审、决定合入 |
| **执行 agent** | 按任务拉起的编码 agent（pi / dsh） | 在某棵工作树里干活 |
| **commondev 插件** | 新增 Go 插件 | 仓库/凭据/分支/PR/测试执行，全部机械化操作 |

### 1.3 为什么这件事值得做

现状是「在项目文件夹里开 agent」，导致同项目的多个 agent **无法各占分支并行**。
用户要的是：一个项目可同时有多条分支在被不同 agent 推进，而合入点由主管把控。

---

## 2. 目标机制（用户描述 + 本文补全）

```
项目列表（每项目一条）
  └─ 分支列表（每分支：名字 / 分配给哪个 agent / 工作内容 / 验收状态 / base_sha）
       └─ 临时工作目录（插件数据目录下 program/<hash>-<name>/）
            └─ 独立仓库的独立分支（本地 clone）
                 └─ ★ 拉起 pi 并拿到 session_id
                      └─ ★ 才有通道：插件注册 inputch commondev/<id>
                                             + outputch commondev_<id>
                           └─ 主 agent 建驻留子并划入这对通道
                                └─ 子调 output_send__commondev_<id> 下任务
                                     └─ agent 自审 → 主管复审 → 插件复核 → 提 PR → 合入
```

> **顺序要点（容易搞反）**：**先有 session，才有通道**。通道是“与某个具体 pi 会话
> 对话的入口”，没 session 注册出来就是空壳。所以：
> **建工程 → 拉 pi（拿 session）→ 注册通道 → 建驻留子并划入 → 投递**。
> 详见续篇 §2.2。

**插件应提供的能力面**（用户列举）：

1. 远程 git 仓库全套操作（clone / fetch / branch / commit / push / PR / merge）
2. 多平台凭据（GitHub / GitCode / Gitea …）［已按用户澄清修正：多凭据，非多 agent 账号］
3. 账号/仓库搜索
4. 项目列表与分支状态台账
5. **判据定义与复核**（主管定义单元测试 → 插件在 agent 改完后执行）
6. 测试模板：压力 / 接口 / 编译

---

## 3. 已核实的事实（接手者可直接依赖）

### 3.1 仓库与平台

| 事实 | 证据 |
|---|---|
| TrueAgent 自身 remote 为 `gitcode` + `origin`(github) | `git remote -v` |
| 本机有 **Gitea 1.24.2** 在 `192.168.2.106:3000` | `curl /api/v1/version` → `{"version":"1.24.2"}` |
| 本机无 git 库依赖（go.mod 无 go-git/git2go） | 插件现只能用 `os/exec` 调 git |
| `~/.git-credentials` 存在 | `ls ~/.git-credentials` |

### 3.2 凭据现状 ［风险］

| 事实 | 证据 |
|---|---|
| 插件配置存于 `config_<plugin>` 表，key→TEXT，**可存 JSON** | `config_qq.group_tool_allowlists` 即 JSON |
| 凭据**明文**存储 | `config_vikunja.token = tk_2b7ecac1...` |
| `config.db` 权限 **644（全局可读）** | `ls -l /home/newqqagent/config.db` |

> **［风险］** commondev 的凭据是**能 push 用户仓库**的凭据，与现有 token 不同量级。
> 按现有 644 明文惯例存，等于把仓库写权限暴露给同机任何可读文件的东西
> （本容器跑着数十个子进程插件、浏览器、MCP 服务）。详见 §7.1。

### 3.3 插件开发基建（可复用的现成东西）

| 事实 | 证据/位置 |
|---|---|
| 已有 `plugindev` 示例插件，含 `plugindev_status/init/build/sdk/projects` | `third_party/homeagent-sdk/example/plugindev/` |
| 插件数据目录 API | `sdk.Settings().DataDir()`（`internal/sdk/settings_impl.go:24`） |
| `hmapdev` 工具链已装 | `/usr/local/bin/hmapdev` |
| SDK 无 HTTP/exec 能力面，但插件是编译型 Go，可自行 `os/exec` | `internal/sdk/plugin.go` 方法清单 |

> commondev 可从 `plugindev` 长出来，不必从零起。

### 3.4 「把 agent 拉进目录」——三条路的真实能力 ［已核，关键］

| 路径 | 能否指定 cwd | 结论 |
|---|---|---|
| `pi -p`（无头） | ✅ **cwd = 进程 cwd** | **实测**：在 `proj-b` 下运行，正确读出 `marker.txt` = `BBB` |
| `dsh headless` | ✅ 逻辑同 CLI（cwd 继承） | 实测被**凭据**挡下（非 cwd 问题） |
| `a2a_query` / `acp_query` | ❌ **参数里没有 cwd** | 见下 |

**`a2a_query` / `acp_query` 的确切形状**（已读源码）：

- `a2a_query` 参数：`agent_url` / `query` / `session_id` / `timeout` —— **无 cwd**
- `acp_query` 参数：`server_url` / `prompt` / `session_id` / `timeout` —— **无 cwd**
- pi-acp 扩展的 `session/new` payload：`{"request": {"text": text}}` —— **无 cwd**
  （`/root/.pi/agent/extensions/pi-acp/index.ts:127`）

**它们只能把文本发给「已在运行的」 agent**，而该 agent 的工作目录是**启动时**固定的。

> **这是用户所说「需要进一步完善优化」的准确位置。**
>
> A2A/ACP 是**长期驻留 agent** 的模型（peer-to-peer，一个 agent 一个进程一个目录）；
> 而本需求是**按任务拉起**（一次性、每任务一个目录）。两者模型不匹配。
>
> - 若走 A2A/ACP：必须**扩建协议**增加 `cwd`（改动扩展 + 可能改动 ACP 实现）
> - 若走 CLI：`pi -p` **天然贴合**（进程 cwd 即工作目录），零协议改动
>
> **本文倾向 CLI 路线** ［待定］，理由见 §6.1。

### 3.5 端口占用：A2A/ACP 不是通用能力 ［已核］

`127.0.0.1:14010`（A2A）与 `14011`（ACP）由 **PID 819 `pi-web-sessiond`** 监听，
**不是** general pi 的能力。普通 `pi -p` 启动时只打印：

```
[pi-a2a] A2A server not listening on 127.0.0.1:14010 (EADDRINUSE)
```

`pi --help` 里**没有** `--serve` 之类参数——服务由 `~/.pi/agent/extensions/pi-a2a|pi-acp`
两个扩展提供，且扩展内有「子进程不继承监听端口」的守卫。

### 3.6 拉起 agent 当前**不工作** ［风险 · 最高优先级］

实测三条拉起尝试全部失败：

| 尝试 | 错误 |
|---|---|
| `pi -p --provider llmsproxy --model AUTO` | 成功（同目录测试通过），但另一轮撞 `model "claude-opus-5" is not allowed for this key`（403） |
| `dsh headless` | `AUTH: Authentication Fails, Your api key: ****d63d is invalid` |

且与 QQ 会话中 agent 自身遭遇的失败**同源**（`/home/newqqagent/memory/context.json`
第 23–28 条）：

```
[23] 403: model "claude-opus-5" is not allowed for this key
[24] 503: all 6 auto providers failed
[26][27][28] 503: all 6 auto providers failed（codebuddy_kimi / vela_deepseek …）
```

> **［风险］这是整套设计的致命前提。** 主管模式、worktree 隔离、PR 流水线、变异复核——
> **每一环都要求「能成功拉起一个 agent 并让它在指定目录干活」**，而这一步当前成功率
> 无法保证。**在解决它之前，不应继续构建上层机制。**
>
> 注意：失败集中在 `llmsproxy/AUTO` 与 `justworker/claude-opus-5`。而本机大量其他
> agent 调用（子代理、go test、端到端验证）当日是成功的 ⇒ **可用 provider/model 组合
> 需要被枚举出来，而不是继续用 `AUTO` 去赌。**

### 3.7 内核侧 `spawn_child` 与本题无关 ［已核］

`spawn_child` 是内核自有的**工作式轻量子**（goroutine），参数只有 `task` 与 `max_turns`
（`internal/agent/core/spawn.go:49`），**不是**拉起外部编码 agent。
用户已明确：本需求是「homeagent 拉起本机的编码 agent（pi 和 dsh）」，与此无关。

### 3.8 值得保留的历史结论（前序 git worktree 讨论）

在 QQ 会话（message_id 1685590448）中，agent 给出的 git worktree 方案，本文复核：

| agent 的说法 | 复核结果 |
|---|---|
| worktree 隔离有效、互不污染主仓 | ✅ 实测通过 |
| 「同一分支不能被两棵 worktree 同时挂，git 会直接拒绝」 | ✅ 实测 `fatal: 'feat/a' is already used by worktree at '...'` |
| worktree 共用对象库、很轻 | ✅ |
| 「给专属 agent 配 `core.agent.workdir`」 | ❌ **错**。见下 |

**`core.agent.workdir` 的两处错误理解**（`internal/config/registry.go:863`）：

1. 它是 `Type: "string"` 的**全局单值**，不是 per-agent 配置 ⇒ 配了影响所有 agent
2. 它只是 `cmd_run` 的 **fallback**，agent 显式传 `workdir` 时被覆盖（`internal/plugins/cmd/plugin.go:149`）

> **教训**：worktree 方案本身没错，但「怎么把某个 agent 绑到某棵树」当时**没有机制**，
> 只有「在任务描述里写明」这种提示词层面的软约定。这正是 §3.4 的问题。

---

## 4. 设计：目标架构

### 4.1 commondev 插件的工具面（建议）

```
# 凭据与平台
commondev_account_add         添加一个平台凭据（kind: github|gitea|gitcode）
commondev_account_list        列出已配凭据（**不返回 secret 本身**）
commondev_account_test        验证某凭据可用（调平台 API 而非只看字符串非空）
commondev_repo_search         按关键字搜账号下/平台上的仓库

# 项目台账
commondev_project_add         登记一个项目（本地名 → 远程 URL + 平台 + 默认分支）
commondev_project_list        项目与分支总览（含每分支的 agent / 工作内容 / 验收状态 / base_sha）
commondev_branch_open         开一条分支工作线（分配 agent、写工作内容、记录 base_sha）
commondev_branch_status       单条分支的详细状态（含临时目录路径、最近提交、测试结果）
commondev_branch_close        收尾清理（merge 后或废弃时）

# 工作目录
commondev_workdir_prepare     准备/复用临时工作目录（mirror 派生，见 §4.3）
commondev_workdir_clean       清理临时目录（含凭据残留检查，见 §7.2）

# 内置 pi 的运行时（★ 本质是“包一下”：发消息 / 收返回，见续篇 §2.5 §3.1）
commondev_agent_up            拉起 sidecar（内嵌 pi；remote 时 ssh）并登记台账
commondev_agent_state         查 session / runtime / 健康
commondev_agent_down          收尾（幂等全清，不留孤儿）
# ★ 下任务**不走插件工具**：子调内核原生的 output_send__commondev_<id>(payload=…)，
#   插件写的通道 handler 负责“包一下”→ sidecar.prompt（见续篇 §2.5）。
# “中断/纠偏 pi”走**内核已有的** InjectInterrupt（L3），也不另造工具。
commondev_pi_plugin_install   ★ 为内置 pi 装插件（scope=project 临时 / plugin 全局）
commondev_pi_plugin_remove    卸载（同 scope 语义）
commondev_pi_plugin_list      列出已装（分 scope）
commondev_pi_source_import    ★ 把 HologramAgent 内部配置的 LLM 源导入内置 pi
commondev_pi_source_add       添加一个源（直接写 pi 的 models.json/auth.json）
commondev_pi_source_list      列出可用源（默认**不返回** api_key）
commondev_pi_source_default   ★ 指定内置 pi 默认用哪个源（写 settings.json）
commondev_pi_config_get/set   读写 pi 配置（scope=plugin|project）

# 判据与复核
commondev_test_define         **主管**定义一条判据（落地成可执行脚本 + 期望）
commondev_test_run            执行判据集合（正向）
commondev_test_mutate         执行**变异验证**（反向，见 §4.5）★ 必须有
commondev_review_submit       agent 完成后提交复核（正向 + 变异一起跑）

# 提 PR
commondev_pr_open             提 PR（含 conflict 预检，见 §4.4）
commondev_pr_status           查 PR 状态
commondev_pr_merge            合入（**仅主管授权的凭据可调**）
```

### 4.2 项目/分支台账（建议 schema）

```
projects
  id, name, remote_url, platform, default_branch, mirror_path, created_at

branches
  id, project_id, name, agent_name, work_desc,
  base_sha,            # ★ 用户原描述中缺失，必须补（理由见下）
  head_sha, status,    # status: open|review|merged|abandoned
  worktree_path, created_at, closed_at

criteria                # 主管定义的判据
  id, project_id, branch_id, name, kind,   # kind: unit|interface|stress|build
  script, expect, definition_hash, created_at

review_runs
  id, branch_id, criteria_id, phase,       # phase: forward|mutate
  result, evidence, started_at, finished_at
```

**为什么必须加 `base_sha`**（用户原描述没有）：
没有它，插件无法判断「两个 PR 改了同一文件时第二个该不该 rebase、能不能自动合」，
出问题也无法重放。建议在 `commondev_pr_open` 前做 conflict 预检。

⇒ **刀 3 已落地**（见 §4.6）。

### 4.3 临时工作目录：mirror + **本地 clone（硬链接）** ［已定 2026-10-13］

**用户拍板**：每个工程对话一个独立目录 `program/<hash>-<name>`（`name` = **仓库名**），
**该分支修改提交合入后直接删除该目录**。因此需要「删掉即彻底」，这直接排除了两个
候选方案（均实测）。

```
<插件数据目录>/program/
  mirrors/<platform>-<repo>.git/   # git clone --mirror，每仓库一份，定期 fetch
  <hash>-<name>/                   # 每个工程对话一份；合入后整目录删除
```

`<hash>` 用「项目名 + 分支名」的短哈希，**保证可重算**（便于清理孤儿目录），
并且 **hash 是目录唯一性的来源**。

#### 目录映射：★ 同一仓库的不同分支必然是不同目录 ［已定 2026-10-14］

用户明确要求：

> 「即使是同一个仓库，只要是不同的分支都拉到 hash-name 下使用 hash 区分，
>   插件通过哈希表映射工程分支与文件夹」

```
branchKey(project, branch) = project + "\x00" + branch
   └─ shortHash() → <hash>（8 位 16 进制，可重算）
        └─ program/<hash>-<name>      其中 name = 分支名派生（可读性）
```

**两条职责必须分清**（这是本节能防住的事故）：

| 要素 | 承担什么 | 为什么 |
|---|---|---|
| **hash** | ★ **唯一性** | 一个仓库可并行多条分支；唯一性只能由 hash 承担，**不依赖 name** |
| **name** | 可读性 | 清理孤儿/审计时纯 hash 无信息量 |

若只按仓库名建目录，两条分支会**共用同一个工作树** —— 那是灾难性的：
agent A 的未提交改动会出现在 agent B 的 `git status` 里，且「独立 clone」的前提（§4.3）不成立。

**验证**（已实施）：`branchDir("TrueAgent","feat/a") != branchDir("TrueAgent","feat/b")`，
且同输入可重算。变异（hash 只用项目名）→ 报红。

**⚠️ name 不能承担唯一性**：分支名经派生后可能撞（如 `feat/a` 与 `feat.a` 都派生为 `feat_a`），
所以目录名必须带 hash。

**映射是双向的、且只有一处真相**：目录名可由 `(project, branch)` 重算；
反查靠台账的 `branch.Workdir`（`resolveBranchDir`）。
**绝不要在映射函数之外另算目录名**。

**孤儿检测**：`program/` 下符合 `<hash>-<name>` 形态、但台账里无记录的目录 = 孤儿
（`listOrphanDirs`）。非本插件形态的目录不碰。

#### 为什么是本地 clone，而不是 worktree 或 `--reference`（均实测）

| 方案 | `rm -rf` 之后 | 外部依赖 | 结论 |
|---|---|---|---|
| **worktree** | **`.git/worktrees/<name>` 残留**，状态变 `prunable`；必须再跑 `git worktree prune` 才清 | 无 | ❌ 删除不彻底；中央仓需持续维护 |
| **`--reference`**（本稿初版建议） | 干净 | **★ `.git/objects/info/alternates` 指向 mirror** | ❌❌ **mirror 一旦被清理，work tree 彻底损坏**：实测 `error: unable to normalize alternate object path`，之后**每次** git 操作都报错 |
| **本地 clone** | **干净**（只剩源仓） | **无** | ✅ **对象是硬链接** ⇒ 省磁盘（实测两侧对象 inode 完全相同）；删掉即彻底 |

```bash
git clone <mirror> <workdir>          # 本地 clone：硬链接共享对象、无 alternates
```

> **为什么硬链接这件事重要**：本地 clone 复制对象时用硬链接（实测 inode 相同，如
> `5836713`），因此并行 N 份工作树**几乎不额外占磁盘**，同时**每个目录都是完整自洽的仓**
> —— 既无 worktree 的中央元数据，也无 `--reference` 的外部依赖。
> 两个要求（「删除彻底」与「不是每任务全量 clone」）同时满足。

#### 已考虑并否决的替代方案

> **用 worktree + 只删分支（不删目录）**：即「删掉分支名但留下 worktree 目录」。
> **不采纳**：残留的 checkout 会持有一个已删除分支的引用，下一次同名分支的
> worktree 创建可能冲突（实测 `fatal: 'feat/a' is already used by worktree`）；
> 且目录永远删不干净，与用户要求的「合入后直接删除目录」相悖。

#### ⚠️ 失去的一条保护，必须用台账补回

worktree 天然禁止「同一分支被两棵工作树同时挂」—— 实测
`fatal: 'feat/a' is already used by worktree at '...'`。**改用独立 clone 后这条保护消失了**：
两份 clone 可以在同名分支上各自 push，后者会被拒（非 fast-forward）而暴露问题，
但**在 push 之前**两边都以为自己是唯一。

⇒ **必须在台账层补一道唯一性闸**：

- `branches` 表对 `(project_id, name, status IN ('open','review'))` 建**唯一索引**；
- `commondev_branch_open` 遇到同名且仍开着（或已进 review）的分支必须**响亮拒绝**，
  不是静默复用。

（用户为「一个仓库并行开发」选择独立 clone 是**正确的** —— 代价只是这道补偿闸。）

#### 目录生命周期

```
branch_open
  └─ 建 <hash>-<name>/（由 mirror 本地 clone）
       └─ 干活 → 提交 → push → PR
            └─ 合入（或废弃）
                 └─ ★ 整目录删除（不保留、不归档）
```

> **删除是收尾动作的一部分**：合入后不删，下次同名分支会与残留目录混淆；
> 并行开发的意义就是「用完即清」。与内核「子不得比父活得久、无残留」同一条纪律
> （见续篇 §4 孤儿清理）。

### 4.4 目录命名与并行开发

- 目录：`program/<hash>-<name>`，`<hash>` = 项目名+分支名短哈希（★ **唯一性来源**），
  `name` = 分支名派生（**只做可读性**，不承担唯一性，见 §4.3）
- **一个仓库可并行开发**：每分支一份独立 clone ⇒ 互不污染、可同时推进
- 目录内记录 `_commondev.json`（项目/分支/agent/base_sha/凭据指纹），便于事后审计
- **不要**把凭据写进目录（见 §7.2）

> **并行是有前提的**（见 §8.4）：并行的必须是**互不相干的模块**。
> 若任务间有依赖（A 改接口、B 用接口），两棵独立 clone 互不可见，
> 问题会集中到 merge 时爆发 —— 那时应先拆边界，再谈并行。

### 4.5 ★ 复核必须是两步：正向 + 变异（本文最重要的设计主张）

用户原话：

> 「我定义过单元测试后，对应 agent 完成修改后你直接让插件进行复核即可」

**这里的隐含假设是「插件 GREEN = 改对了」。该假设不成立。**

插件只能跑**主管预先写下的判据**。若判据没覆盖那条 bug，插件照样打勾。
自动化**不会**发现假绿，**只会让假绿跑得更快、更可信**。

**本仓当日的实证**：2026-10-13 下午新写的协议一致性判据组，变异测试中
**M5 未报红**——把 `json:"phase"` 改成 `json:"phaseX"`，判据仍全绿，
因为它只查了前缀。是**变异测试**把它咬出来的（该轮修复的最终判据已改为匹配完整 tag）。

**因此插件的复核必须包含两个阶段**：

| 阶段 | 做什么 | 不通过的后果 |
|---|---|---|
| **正向**（forward） | 跑主管定义的判据 → 期望通过 | 实现有 bug，驳回 |
| **反向**（mutate）★ | 自动改坏被测代码（按主管给的变异规则或自动生成）→ 期望判据**报红** | **判据太松，驳回** |

**没有反向阶段，就是在给「测试通过」这个信号不断充值信用，直到它骗一次大的。**

变异规则由主管随判据一起定义（`commondev_test_define` 可迭代幂等），存入 `criteria` 表。
注意本仓纪律：判据类写「某标识符是否出现在函数体」时**必须先剔除注释再匹配**（曾因注释含
`offsetParent` 导致假绿）。

### 4.6 三层审查的分工（建议明确，避免主管被 diff 淹没）

用户描述三层：agent 自审 → 主管复审 → 插件复核。

**建议定义清楚「主管复审」审什么**，否则主管会变成更贵的高级码农：

| 层 | 审什么 | 不审什么 |
|---|---|---|
| agent 自审 | 代码正确性、风格、实现细节 | — |
| **主管复审** | **这次的改动，判据够不够**：引入了哪些新失败方式？判据覆盖了吗？变异咬得住吗？ | 逐行读 diff |
| 插件复核 | 机械执行正向 + 变异 | 语义判断 |

---

### 4.7 ★ 刀 3：conflict 预检与 base_sha ［✅ 已完成 · 真实 git 实测］

#### 为什么这一刀必须做

并行开发（§4.4）让冲突从"可能"变成"必然"：每分支一份独立 clone，
两棵树**互不可见**。冲突不会在开发时暴露，而是**全部堆到 merge 时爆发** ——
那时判据已经变绿、复审已经通过，却发现合不进去。

⇒ 必须在**提 PR 之前**就发现。

#### 做法：`git merge-tree --write-tree`（**完全不碰工作区**）

实测 git 2.53 的退出码语义：

| 退出码 | 含义 |
|---|---|
| `0` | 可干净合并 |
| `1` | 有冲突 |
| 其它 | 命令本身失败（**不能当成"无冲突"**）|

★ **它不写索引、不动 HEAD、不留 MERGE_HEAD** ⇒ 可以在任何时候安全地跑，
不需要 stash/checkout，也不会破坏正在进行的开发。这一条有判据钉住
（跑完预检后断言 HEAD 未变、无 MERGE_HEAD、工作区零改动）。

#### 三类结果必须分开（防静默降级）

| 结果 | 语义 |
|---|---|
| `Mergeable: true` | 检查过，可合并 |
| `Mergeable: false` + `Conflicts` | 检查过，有冲突（列出文件）|
| **`Skipped: true`** | **没检查**（目标分支本地不存在 / 非 git 仓库 / merge-tree 不可用）|

★ **`Skipped` 时绝不声称可合并** —— 把"没检查"当成"检查通过"是最危险的
静默降级，调用方会以为安全。有判据**逐条覆盖全部 Skipped 出口**
（初版只测了其中一条，于是另一条的变异不报红 —— 判据覆盖不全等于留洞）。

#### `base_sha` 的捕获时机

在 `runPrepareWorkdir` 里、**目录就绪后立刻**记 —— 必须在 agent 开始改之前。
agent 一旦提交，HEAD 就不再是"这条线从哪出发"。

★ **只记第一次**：重开同一分支时若覆盖基点，"这条线从哪出发"就永久丢失，
冲突判定与事后重放都失去依据。

#### 新增工具

| 工具 | 作用 |
|---|---|
| `commondev_conflict_check` | 提 PR 前预检可合并性（只读，`ParallelSafe`）|
| `commondev_parallel_status` | 盘点并行推进的分支线（按项目分组），派活前确认边界 |

#### ☆ 实测发现

**冲突解析有两条独立路径**，缺一不可：

1. `CONFLICT (content): Merge conflict in <path>` 行
2. `100644 <oid> <stage>\t<path>` 的 ls-tree 阶段行

前者覆盖常见情形，后者覆盖**不打印 CONFLICT 行**的冲突类型（如 rename/rename）。
实测：只保留路径 ①、把 ② 的判定前缀改坏，**"仅 ls-tree 形态"的判据会报红** ——
说明 ② 是真·载重路径，不是冗余。

#### 判据与变异

`conflict_parse_test.go` 8 条（含**真实 git 仓库**的端到端：可合并 → 合入后冲突
→ 工作区零影响）；变异 6 条全部咬住（含 1 次暴露判据覆盖不全、1 次暴露
"冗余路径其实是载重路径"）。

---

### 4.8 ★ 刀 4：判据与复核（正向 + 变异） ［✅ 已完成 · 真实 go test 端到端］

#### 核心命题：插件 GREEN ≠ 改对了

设计 §4.5 的主张在本刀**落地并用真实数据验证**。我在 `/tmp` 复刻过最小场景：

```go
func Max(a, b int) int { if a > b { return a }; return b }
```
- 松判据只测 `Max(5,3) == 5`（漏了 `b>a`）
- 变异 `return b` → `return a`（**真 bug**）⇒ **判据仍全绿**
- 补一条 `Max(3,5) == 5` ⇒ 同一变异**立刻报红**

⇒ **没有反向阶段，就是在给「测试通过」这个信号不断充值信用，
直到它骗一次大的。**

#### 两阶段

| 阶段 | 做什么 | 不通过意味着 |
|---|---|---|
| **正向** forward | 跑判据 | 实现有 bug |
| **反向** mutate ★ | 把文件里 `old` 换成 `new`，期望判据**报红** | **判据太松**（该变异没被看见）|

#### 结论三档（**不合并成一个绿勾**）

| verdict | 含义 |
|---|---|
| `pass` | 正向全过 **且**全部变异被咬住 |
| `pass_with_gaps` | 正向过，但**有变异未被咬住** 或**有判据没定义变异规则** |
| `fail` | 有判据未通过 |

★ `pass_with_gaps` **不是"通过"** —— 它是「判据没覆盖到」。
把它说成通过，就是本刀要消除的那种"信号充值"。

#### 新增工具

| 工具 | 作用 |
|---|---|
| `commondev_test_define` | 主管定义判据（**幂等**：同定义重复登记不改动）；必须同时给 `mutations` |
| `commondev_test_list` | 列出判据（含变异条数 —— **为 0 的是复核缺口**）|
| `commondev_review_submit` | 跑复核（后台作业：判据可能含编译/压测，超 60s 工具上限）|
| `commondev_review_result` | 取结论与历史（含逐条证据）|

#### ☆ 实现要点（每条都对应一个可致错的点）

| 点 | 为什么 |
|---|---|
| **必须 `-count=1`** | Go 默认缓存测试结果。实测：变异后若命中缓存，测试**根本没跑**，"报红"无从谈起 |
| **变异必须恢复文件** | `defer` 保证 panic 也恢复；且恢复后**校验内容一致**——恢复不回去比变异本身危险（agent 会基于坏代码继续改）|
| **`old` 必须恰好出现一次** | 出现 0 次 ⇒ 变异无效，却被误报成"判据太松"（**假警报**）；出现多次 ⇒ 可能改错地方，却报"咬住"（**假通过**）|
| **通过由退出码判定** | 不是看输出里有没有 `PASS`（输出可被伪造/污染）|
| **固定判据顺序** | map 遍历随机 ⇒ 报告不可对照 |

#### 判据与变异

`criteria_test.go` 10 条 + `e2e_criteria_test.go` 2 条（**真实项目 + 真实 go test**）；
变异 5 条对**复核机制本身**做变异（元层级），全部咬住 ——
包括「变异后不恢复文件」「变异未咬住却报 ok」「无变异规则却给 pass」。

#### ⚠️ 尚未做（原计划的"模板库"部分）

设计 §6.4 原写「测试模板库（压力/接口/编译）」，本次落地的是**机制**
（判据定义 + 复核执行），**预置模板库**仍未做。理由：
机制不成立时模板是摆设；现在机制已用真实数据证明，模板可以后续按需补。

---

## 5. 与现有系统的关系

| 现有物 | 关系 |
|---|---|
| `plugindev` 示例插件 | commondev 的**起点**（可复用其 hmapdev 调用、workspace 管理） |
| `plugindev` 的 `projects` 工具 | 可参考其「列工程 + 构建产物」的形态 |
| `core.agent.workdir` | 全局单值 fallback，**不要**把它当 per-agent 机制用（§3.8） |
| `spawn_child` | 内核工作式轻量子，**与本需求无关**（§3.7） |
| `a2a` / `acp` 插件 | 现无 cwd，需扩建或绕过（§3.4） |
| Gitea 1.24.2（本机） | **建议作为第一刀的唯一平台**（见 §6.2） |
| `tracker`（changeset/回滚） | 已有的文件系统变更追踪，与 git 分支台账**无关**，勿混 |

---

## 6. 关键决策建议（未拍板，供接手者与用户确认）

### 6.1 拉起 agent：走 CLI 家族，不走 A2A/ACP ［本节已由续篇修正］

> **⚠️ 修正（2026-10-13）：** 本节初稿只论证了 **cwd** 一个维度就下了结论，
> **论据不全**。用户明确要求「投递必须落到**同一个会话**，不能前一个没做好就投给新会话」，
> 而 `pi -p` 每轮一个进程，**无法保证**这一点（实测：漏传 `--session-id` 会静默开新会话；
> 并发传同一个 id 会写坏会话）。
>
> 正确的两轴（会话粒度 × cwd 由谁承担）与形态取舍，见续篇
> **`docs/zh/commondev-agent-runtime-design.md` §1.2 / §3**。
> 本节保留，只作为「A2A/ACP 为何被排除」的论据（该结论本身没变）。

| 方案 | 优点 | 缺点 |
|---|---|---|
| **`pi -p`（CLI）** | 进程 cwd 即工作目录（**已实测**）；一次性进程，天然按任务隔离；零协议改动 | **无法保证续投同一会话**（见上）；每次冷启动；无中途纠偏 |
| **`pi --mode rpc`（常驻）** | 同 cwd；同会话续投；`steer`/`follow_up` 可中途纠偏；`get_state` 可断言 session 未变 | 要实现/复用 RPC 客户端；stdout 必须持续读 |
| **Node SDK（进程内）** | 最贴身；`createAgentSession({cwd})` 可控 cwd 与会话 | 需 Node 宿主 |
| `dsh headless` | 同上 | 本机凭据失效，需先修 |
| 扩建 A2A | 复用现有插件 | 要改扩展 + payload；**参数里没有 cwd**（§3.4） |
| 扩建 ACP | 同上 | 同上；且 ACP 的 session 模型本就假设长驻，加 cwd 是**违背其设计意图** |

**修正后的建议**：**内嵌 sidecar 为默认形态**，远程 SSH 由配置切换
（两形态共用同一 sidecar，差异只在传输层）。`pi -p` 仅作侧车不可用时的降级，
**不是正式形态**。A2A/ACP 依然排除（「无 cwd」这条事实没变），
排除理由与本需求无关：它们是**常驻 peer** 的模型，而本需求是**按工程对话拉起**。

**选型已定 ［2026-10-13 用户拍板］**：**默认内嵌（进程内 pi 实例）**，
配置可切到**远程 SSH**；远端同样**创建目录并在该目录拉起 pi 实例**。
远端环境缺失时**询问用户、不得擅自安装**（见续篇 §3.4）。

### 6.2 第一刀只做一个平台、一个项目、一个任务

理由：这套机制有**至少四个独立的失败面**（凭据 / 拉起 / 判据 / 合入），
一次全上会无法归因。先用 Gitea（本机就有，API 可控）跑通**一条真实链路**：

```
登记项目 → mirror → 开分支 → 拉起 pi -p（cwd=工作目录）
  → agent 改 → 主管定义判据 → 插件跑「正向 + 变异」
  → 提 PR → 主管合入
```

**这一刀的目的不是功能完整，是证明「假绿检测」这一环真的能工作。**

### 6.3 权限模型：让「必须有主管复审」成为机制而非礼貌 ［✅ 已完成 · 真实 Gitea 实测］

若执行 agent 所用的凭据能 push 主干，则「主管复审」只是**流程约定**——想绕过随时能绕过。

**机制**（已在真实 Gitea 1.24.2 上逐条实测）：

| 动作 | 执行侧凭据 | 主管侧凭据 |
|---|---|---|
| push `main` | ❌ `pre-receive hook declined` | （不需要）|
| push `feat/*` | ✅ | ✅ |
| 开 PR | ✅ | ✅ |
| **自合 PR（无审批）** | ❌ `Does not have enough approvals` | ❌ |
| 审批（带 commit_id）| ❌（角色不符） | ✅ |
| 合 PR（有审批）| — | ✅ |

★ **限制的真身是平台的分支保护**（`enable_push:false` + `required_approvals≥1`），
不是插件里的判断 —— 插件在信任边界外，改一行代码就能绕过；平台的
pre-receive hook 不能。这是刀 2 与「流程约定」的本质区别。

**实现落点**：

| 件 | 作用 |
|---|---|
| `Account.Role`（`executor`/`supervisor`） | 标注凭据能做什么；空值按 supervisor（兼容老数据）|
| `commondev_protect_branch` | 幂等建立分支保护（只改必要项，不反复改用户仓库）|
| `commondev_pr_approve` / `commondev_pr_merge` | 主管侧动作，按角色选凭据 |
| `accountForRole` | 角色不符时**明确报错**（而非让平台报一个看不懂的错）|

**`defaultProtection()` 的四条**（每条堵一条绕过路径，均有判据钉住）：

| 字段 | 值 | 堵住的绕过 |
|---|---|---|
| `enable_push` | `false` | 执行侧直接推 main |
| `required_approvals` | `≥1` | 无人审批也能合 |
| `block_admin_merge_override` | `true` | **admin 绕过**（主管账号的后门）|
| `dismiss_stale_approvals` | `true` | 审批后追加提交偷换内容 |

#### ☆ 实测踩到的三个 API 坑（都不是文档里写着的）

| 坑 | 现象 | 真因 |
|---|---|---|
| **`event` 必须是 `APPROVED`** | 传 `APPROVE` **返回 200 不报错**，但 review 落成 `PENDING` ⇒ 合并报 "Does not have enough approvals"（看起来像权限问题）| 枚举少了个 D |
| **审批必须带 `commit_id`** | 不带时同样落成 `PENDING`（与 event 是两个独立条件）| 审批要绑定具体提交 |
| **`required_approvals` 也管 admin** | admin 没有绕过审批的特权 | 这是**正确的** —— 否则"复审"对 admin 形同虚设 |

附带（正确行为但要知）：`REQUEST_CHANGES` 会**阻断**合并，即使同时有 `APPROVED`。

#### 判据与变异

`protection_test.go` 11 条；变异 9 条全部咬住（含 1 次暴露判据测错层次 ——
只测 `platformMessage()` 没测 `MergePR` 是否调用它）。

### 6.4 落地顺序

| 刀次 | 内容 | 证明什么 |
|---|---|---|
| **0** | **修「能拉起 agent」**（§3.6） | 前提是否成立 |
| **1** | 单平台单项目：mirror → 分支 → 拉起 → 判据+变异 → PR | 机制成立，假绿可检测 |
| 2 | 接 Gitea 分支保护 + 执行/主管双凭据 | ✅ **已完成**（真实 Gitea 1.24.2 实测，见 §6.3）|
| 3 | 多项目 + 并行 + conflict 预检 | ✅ **已完成**（真实 git 实测，见 §4.6）|
| 4 | 判据与复核（正向 + 变异） | ✅ **已完成**（真实 go test 端到端，见 §4.8）|
| **S** | **★ 内核安全模块（Secret Firewall）** —— 见
  `docs/zh/kernel-secret-firewall-design.md`。
  插在 **provider 层**（已核：构造 `CompletionRequest` 有 7 处，task 层拦不全）。
  **它先行的理由**：插件把密钥明文直接投给 LLM（用户拍板）—— **没有这道闸，
  密钥就真的发出去了**。而 2/3/4 刀都要用凭据（push / PR / 测试），
  它们建立在“提交密钥是安全的”这个前提上 ⇒ **安全模块是它们的前提**。 | 凭据不泄露 |

> **第 S 刀的位置说明**：它不排在 2/3/4 之后，而是**与刀 2 并行且先行**。
> 这与刀 0 同理：**前提不成立时，不要继续构建上层**。

> **第 4 刀为什么放最后**：它最容易写，也最容易变成摆设。先让「变异能咬住」
> 这一环成立，模板才有意义。

---

## 7. 安全设计（必读）

### 7.1 凭据存放 ［风险］

**现状**（§3.2）：`config_<plugin>` 明文、`config.db` 644。
commondev 的凭据是**能 push 用户所有仓库**的凭据，性质完全不同。

建议：

1. **不放 `config.db`**。放独立文件（如 `<插件数据目录>/credentials.json`），权限 `0600`，
   **启动时校验权限，不对就拒绝启动**（本仓纪律：响亮失败，不静默降级）
2. **优先 SSH key 而非 PAT**：key 可按平台单独生成/吊销，不随账号密码轮换失效
3. **每平台用独立、最小授权的凭据**（GitHub fine-grained PAT 支持单仓授权），
   **不要一个 token 通吃**
4. 接 `git push` 时走 **credential helper / 环境变量注入**，**不要**写进 git config 文件

#### ★ 变量填充引用 ［已定 2026-10-14 · 用户要求］

用户明确要求：「gitea / gitcode / github 等主流系统的 ackey 保存与**通过变量填充引用**」。

**核心设计：按名字引用 + 值可写 `${ENV_VAR}`**

```
commondev_account_add(name="gitea-local", platform="gitea",
                      token="${GITEA_TOKEN}",          ← ★ 引用，不是明文
                      base_url="http://192.168.2.106:3000")

之后一切操作只带**凭据名**：
commondev_branch_open(..., account="gitea-local")   ← 参数里永远不出现 secret
```

**为何要变量填充**（三个真实好处）：

| 好处 | 说明 |
|---|---|
| 明文不落盘 | 文件里只有 `${GITEA_TOKEN}` |
| 环境可切换 | 同一份配置，dev/prod 换环境变量即可 |
| 轮换不需改数据 | 改环境变量 + 重启即可 |

**两条不可逆的纪律**：

1. **变量解析不到值 ⇒ 响亮报错**，绝不静默当空串。
   后者会退化成匿名访问，表现成 **401** 而非“凭据没配” —— 极难排查。
   实测：`${NONEXISTENT_VAR_ZZZ}` → 报“凭据引用的环境变量未设置: …”。
2. **secret 绝不进任何返回值/日志**：`account_list` 只报 `token_ref`（引用本身不是秘密，
   对排查有用）与 `token_set` 布尔。实测：明文 token 在日志中出现 **0 次**。

**平台形状差异**（必须抽象，不能一个头部走天下）：

| 平台 | API 前缀 | 认证头 |
|---|---|---|
| Gitea | `<base>/api/v1` | `Authorization: token <T>` ← **不是 Bearer** |
| GitHub | `https://api.github.com` | `Authorization: Bearer <T>` |
| GitCode | `https://api.gitcode.com/api/v5` | `Authorization: Bearer <T>` |

未知平台**必须报错**（不猜默认形状）。

**凭据可用性要真验**：`account_test` 调平台的 `/user` 端点，不是只看字符串非空。

### 7.2 临时目录的凭据残留 ［风险］

任务跑完删目录后，**clone 里可能仍留着 token**（remote URL 带 token 是常见做法，
`git remote -v` 就能读出来）。

**规矩**：clone 出来的 remote URL **不带凭据**；push 时临时注入
（Gitea/GitHub 均支持 `http.extraheader` 或 `GIT_ASKPASS` 一次性注入）。
这样临时目录删掉就是真删掉。

建议 `commondev_workdir_clean` 内置一道**残留检查**（扫 remote URL / config / 已知凭据指纹）。

### 7.3 插件自身不是沙箱

本仓已知：插件是编译型 Go 子进程，能做 `os/exec`、读写文件。commondev 持有高价值凭据，
**它自己的权限边界需要单独评估**（用户在主机的进程隔离、文件权限是否足够）。
本文不展开，但接手者应至少确认：commondev 的数据目录权限、以及宿主对插件进程的隔离手段。

---

## 8. 未决问题（需用户拍板）

### 8.1 「独立仓库」还是「worktree」？［已解决 2026-10-13］

**用户拍板：独立仓库（本地 clone）。** 目录 `program/<hash>-<name>`（`name` = 仓库名），
**合入后直接删除目录**。

理由：worktree 的 `rm -rf` 不彻底（`.git/worktrees/` 残留，需 `prune`），
而 `--reference` 有外部依赖（mirror 被清理即损坏）——**两者都不满足「删掉即彻底」**。
实测与取舍见 §4.3。

**代价与补偿**：失去 worktree 的「同分支不可双挂」保护 ⇒ 台账层补**唯一性闸**（§4.3）。

### 8.2 主管（主 agent）自己的模型可用性

主管也要调 LLM（拆任务、定义判据、复审）。若主管所用模型正是 §3.6 里失效的那些，
整套机制同样停摆。**需要为「主管」单独确认一个稳定可用的 provider/model。**

### 8.3 判据由谁写、写在哪

用户说「你（主管）定义单元测试」。但判据的**落地形态**未定：
- 写进被测仓库（成为仓库的一部分，随 PR 一起走）？
- 还是留在 commondev 台账里（`criteria` 表）？

两种各有取舍：前者可被仓库自身复用，后者与实现解耦但要维护两处。
**建议**：留在台账，但把生成的脚本**落盘到工作目录内**执行，并在 PR 描述里附判据摘要。

### 8.4 冲突与依赖

若并行任务之间有**依赖**（A 改接口、B 用接口），worktree/独立 clone 会**放大**问题：
两棵树互不可见，A 的改动 B 编译不过。

用户需要明确：**并行的是「互不相干的模块」还是「一个功能的几个部分」？**
- 前者 → 本设计正合适
- 后者 → 会集中在 merge 时爆发，建议**先拆分边界**（谁拥有哪些文件）再谈并行

### 8.5 责任边界

用户说「以后只跟主管对接」。那么当判据打勾、PR 合了、线上仍出问题时，责任如何划分？
这决定**判据里必须包含什么**：若主管负责，则光有单元测试不够，
需要纳入**可观测性/回归判据**（真跑一遍看真事件流，而非只看测试绿）。

---

## 9. 接手者请从这里开始

### 第一步（必做）：验证前提是否成立

写一个最小探针，回答一个问题：**「能否稳定拉起一个 agent 并让它读到指定目录？」**

要求：
- 在临时目录放 marker 文件，拉起 `pi -p`，要求输出 marker 内容
- **跑 20 次，统计成功率**（不要只跑一次成功就下结论）
- 记录每次使用的 provider/model，据此**枚举可用组合**
- 若成功率低，先解决 §3.6，**不要继续上层设计**

### 第二步：只做 §6.2 的第一刀

用 Gitea。链路：mirror → 分支 → 拉起 → 判据 + **变异** → PR。
**重点验证「变异能咬住判据」**——故意写一个太松的判据，确认变异阶段能把它驳回。

### 第三步：把本篇文档的每一条事实重新核对

本文的 ［已核］ 项都有命令可复现，见 §10。**若发现与本文不符，先更新本文再动手。**

---

## 10. 自检清单（逐条可复现）

接手者应能执行以下命令并得到与本文一致的结论：

```bash
# §3.1 平台
git -C /home/program/TrueAgent remote -v
curl -s http://192.168.2.106:3000/api/v1/version          # 期望 {"version":"1.24.2"}

# §3.2 凭据现状（注意：不要打印完整 token）
ls -l /home/newqqagent/config.db                          # 期望 644
sqlite3 /home/newqqagent/config.db "select key from config_vikunja;"   # 含 token

# §3.4 拉起 agent 的 cwd 行为
mkdir -p /tmp/probe-a && echo AAA > /tmp/probe-a/marker.txt
cd /tmp/probe-a && pi -p --provider <可用provider> --model <可用model> \
  "执行 pwd 和 cat marker.txt，只输出这两行原始内容"
# 期望输出 /tmp/probe-a 与 AAA

# §3.4 a2a/acp 无 cwd
grep -n "agent_url\|query\|session_id" \
  /home/program/TrueAgent/third_party/homeagent-sdk/example/a2a/plugin.go
grep -n "cwd" /root/.pi/agent/extensions/pi-acp/index.ts   # 期望无结果

# §3.5 端口归属
ss -tlnp | grep -E "14010|14011"                           # 期望 pid=819 pi-web-sessiond

# §3.8 core.agent.workdir 是全局单值
grep -n "core.agent.workdir" /home/program/TrueAgent/internal/config/registry.go
# 期望 Type: "string"

# §3.7 spawn_child 参数
grep -n "max_turns\|task" /home/program/TrueAgent/internal/agent/core/spawn.go | head

# §4.5 变异能咬住判据的现成范例
cd /home/program/TrueAgent
go test -count=1 -run TestStageFrameFieldsMatchServer ./internal/plugins/webui/
grep -n "phase,omitempty" cmd/waiter/state.go              # 判据匹配的是完整 tag
```

---

## 12. ★ 架构变更：平台知识进 skill（2026-10-15）

> **本节修正本文 §4.1。** 原清单里的**平台专有工具**不再实现在插件里；
> 其余机制（台账、工作目录、pi 会话与通道、判据与变异复核、冲突预检、
> 凭据保存与安全执行）**保留**。

### 12.1 用户拍板的边界

> 「我说的是 **git 部分 skill 化**啊，其他部分还是需要插件的」

| 层 | 负责 |
|---|---|
| **插件 commondev** | 项目台账、工作目录（mirror + 独立 clone）、pi 会话与通道、判据与变异复核、冲突预检、`enc_*` 凭据保存与安全执行 —— **40 个工具** |
| **skill** | **各 git 托管平台的认证头、API 端点、PR 与分支保护流程** |

### 12.2 移出插件的部分（10 个工具 → skill）

| 原工具 | 现在怎么做 |
|---|---|
| `commondev_account_add/list/test/remove` | `enc_store` / `enc_list` + skill 里的认证头对照表 |
| `commondev_repo_search` | `enc_shell` + `curl <API>/repos/search?q=` |
| `commondev_pr_open` / `pr_status` | `enc_shell` + `curl -X POST .../pulls` |
| `commondev_pr_approve` / `pr_merge` | `enc_shell` + `curl .../pulls/{n}/reviews`、`/merge` |
| `commondev_protect_branch` | `enc_shell` + `curl -X POST .../branch_protections` |

删除的文件：`platform.go`、`protection.go`、`pr_workflow.go`（约 1270 行）。
`credentials.go` 保留但**去掉了平台语义**（`platformSpec` / `Role` / `BaseURL`）。

### 12.3 为什么平台部分不该在插件里

1. **每接一个托管平台都要改 Go 代码** → 重编译 → 重打包 → 重启服务。
   而平台知识（哪个端点、什么认证头）本该和文档一起演进。
2. **平台形状被硬编码**：`platformSpec` 的 switch 每加一家就长一截，
   **未列出的平台直接报错**——实测 GitLab / Gitee / Bitbucket / Azure DevOps 全不支持。
   而 agent 用 `curl` 本来什么平台都能打。
3. **工具面膨胀**：50 个工具让模型选择成本上升。

⇒ 平台知识进 skill（改文档**立即生效**）；插件只保留机器必须保证的机制。

### 12.4 保留的纪律（没变）

- 凭据不进 `config.db`（0644 全局可读）、不进 remote URL、不进命令行参数；
- **没有「取回明文」的接口**：只能被 `@{别名}` 引用（S1）；
- 变量库不自动淘汰，泄漏控制靠主动删除；
- `enc_shell` 三条保障：明文不进 argv（stdin）、输出脱敏、超时；
- 「不要把测试通过当作改对了」——复核必须含**变异**阶段（§4.5 主张仍成立）。

### 12.5 判据（钉住不回退）

`alignment_test.go` 新增两条：

- `TestPlatformToolsAreNotInPlugin` —— 那 10 个平台工具不得再注册；
- `TestNoPlatformSpecificCodeInPlugin` —— 源码里不得再出现 `platformSpec` /
  `AuthHeader:` / `DefaultAPI` / `RoleExecutor` 等平台专有符号
  （**排除注释**：说明"已移出"是允许的）。

**为何要判据而不只是文档**：文档说了还会有人（包括将来的我）图省事加回去，
而那种回退**功能上看起来完全正常** —— 只是把可移植性换成了硬编码。

---

## 13. 功能完备性核对（2026-10-15）

逐章节对照设计文档核对实现，**19/19 通过**（脚本化核对，非人工目测）：

| 设计条目 | 实现 |
|---|---|
| §4.1 工具面（扣除已移出的 10 个平台工具） | ✅ 40 个工具 |
| §4.2 台账 schema（Project/Branch） | ✅ |
| §4.3 mirror + 本地 clone（硬链接） | ✅ |
| §4.3 ⚠ 唯一性闸（同名未结分支拒绝） | ✅ |
| §4.3 目录名 `<hash>-<name>` | ✅ |
| §4.4 可并行（每分支独立 clone） | ✅ |
| §4.5 复核两步（正向 + 变异） | ✅ |
| §4.7 `base_sha` 首次捕获 | ✅ |
| §4.7 `merge-tree` 预检（不碰工作区） | ✅ |
| §4.7 `Skipped` 与 `Mergeable` 分开 | ✅ |
| §4.8 三档 verdict | ✅ |
| §4.8 变异必须唯一匹配 | ✅ |
| §7.2 凭据残留检查 | ✅ |
| §7.2 remote 不带凭据 | ✅ |
| 续篇 §2.2 先 session 后通道 | ✅ |
| 续篇 §2.5 投递走内核 output 门 | ✅ |
| 续篇 §3.6.3 直接写 pi 配置文件 | ✅ |
| 续篇 §3.6.5 不静默回退宿主凭据 | ✅ |
| S1 无「取回明文」接口 | ✅ |

### 13.1 LLM 源端到端（本轮新增）

新增 `llmsource_e2e_test.go`（5 条判据），把「内核源 → 插件导入 → pi 配置 →
**真实调用成功**」整链收口：

| 判据 | 层次 | 验证 |
|---|---|---|
| `TestLLMSourceImportProducesValidPiConfig` | 纯逻辑 | models.json 形状、`openai`→`openai-completions` 映射、contextWindow 透传、0600 权限、defaultProvider |
| `TestLLMSourceImportRejectsUnknownAdapter` | 纯逻辑 | 未知 adapter **必须报错**（猜默认会让 pi 用错协议，现象是「模型不回复」）|
| `TestLLMSourceImportFailsWithoutKernelSources` | 纯逻辑 | 无源时报错，不生成空配置 |
| `TestLLMSourceImportFromRealKernelConfig` | **真机** | 读生产 `config.db` 的真实字段形态，3 个源全部导入成功 |
| `TestLLMSourceRoundTripAgainstRealEndpoint` | **真机** | 从**导入产物**（而非测试变量）读回地址与 key，真实 HTTP 调用成功 |

**真机结果**：

```
✅ 真实内核配置导入成功：3 个源，默认=llmsproxy
✅ 端到端通过：内核源 → 插件导入 → 用磁盘配置真实调用成功
```

**为何必须端到端**：导入逻辑正确 ≠ pi 能用。中间三层各自可能出错
（字段映射 / adapter→api 名 / 默认源写入），任何一层错了现象都是
「pi 起不来 / No API key found」，而单元判据全绿。

**变异验证（4 条全咬住）**：

| 变异 | 结果 |
|---|---|
| adapter 映射成错的 api 名 | ❌ 报红（纯逻辑判据）|
| base_url 不写入 | ❌ 报红 |
| **api_key 不写 auth.json** | ❌ **真机 e2e 报红**（真实调用失败）|
| 未知 adapter 猜默认值 | ❌ 报红 |

复现命令：

```bash
cd third_party/homeagent-sdk/example/commondev
K=$(sqlite3 /home/newqqagent/config.db "select value from config where key='core.llm.api_key';")
HA_E2E_LLM=1 HA_E2E_API_KEY="$K" HA_E2E_REAL_DB=/home/newqqagent/config.db \
  go test -count=1 ./...
```

### 13.2 判据与变异总览

| | 数量 |
|---|---|
| 判据 | **62** |
| 变异（累计验证咬住） | 30+ |
| 工具 | 40 |
| 非测试代码行数 | 5437 |

---

## 14. 历史实现审计（2026-10-15 早）

本节是对原始设计清单的逐条回读结果；不要仅依据文档顶部的历史状态判断实现状态。

### 14.1 已实现并有测试/实测

| 设计项 | 实现/证据 |
|---|---|
| 刀 0/1：项目、分支、独立 clone、sidecar、通道、续投 | `plugin.go` / `sidecar.go`；branch/session/channel 端到端测试 |
| 刀 2：双角色凭据、分支保护、审批、合并 | `protection.go`；真实 Gitea 1.24.2 矩阵 + 变异 |
| 刀 3：`base_sha`、`merge-tree --write-tree`、Skipped 分离 | `conflict.go`；真实 git 仓库 + 变异 |
| 刀 4：判据、正向、变异、三档 verdict、后台复核 | `criteria.go`；真实 `go test` 端到端 + 机制元变异 |
| 原 §4.1 工具面 | 项目/分支/工作目录/agent/PR/判据/pi 配置/MCP/仓库搜索工具均已注册 |
| Secret Firewall 与配置加密 | `internal/security`、`internal/config`；race 与三层回归 |
| E-4 生产密钥注入 | `deploy/homeagent.service` 的 `LoadCredential`；真实 systemd 262 实测 tmpfs/0400/不进 environ |

### 14.2 设计文字与实现的取舍

- `commondev_agent_send`：续篇 §2.5 已明确废除；发送继续走内核原生 `output_send__...`，不重复造工具。
- `commondev_test_run` / `commondev_test_mutate`：早期清单的两个单阶段入口已保留；`commondev_review_submit` 是两阶段一键入口。
- `commondev_pi_plugin_*` 与细化稿的 `commondev_pi_package_*`：两套命名均注册为兼容别名；底层调用同一套 `pi install/remove/list/update`。
- `commondev_project_open`：续篇早期别名已补，底层复用 `project_add`。
- skill 双投不是运行时插件行为：主管侧 skill 由 HologramAgent 发布，执行侧 skill 由工作目录/发布包注入；插件不擅自把代码写进用户全局目录。

### 14.3 仍明确未实现的设计项

这些不是本次审计漏掉的隐藏功能，而是设计文档已标出的未决/后续项：

- 远端 SSH runtime（当前仅 local）；
- 预置测试模板库（当前完成的是判据/变异机制，不是 unit/interface/stress/build 模板集合）；
- U4 `steer` 语义降级：当前接受 pi 的插话语义，不伪称为内核式抢占恢复；
- U8 的“随包锁定独立 pi 二进制”仍取决于发布包；当前 agent_up 会回显 sidecar 可解析到的 `pi_version`，无法解析时显式警告，不编造版本。

审计结论：**原始设计的核心机制已实现，但不能宣称所有后续项全部完成。** 本节列出的项目是剩余真实边界。



1. **不要把「测试通过」当作「改对了」**。本文 §4.5 的变异验证不是可选项，
   它是这套机制唯一能防住假绿的手段。本仓当天因此浪费过一整轮排查。

2. **不要用提示词层面的软约定替代机制**。前序 worktree 讨论里
   「在任务描述里写明工作目录」就是这一类——它靠模型自觉，没有任何保证。§3.8 已记录该教训。

3. **前提不成立时，不要继续设计上层**。§3.6 的「拉起 agent 不工作」如果没解决，
   后续所有设计都是纸面的。**先让它能跑，再让它跑得好。**
