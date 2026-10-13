# CommonDev：主管式多仓库开发流水线（设计 · 未实现）

> **状态：设计稿，尚未实现。** 本文只描述目标机制、已核实的事实、未决问题与建议的
> 落地顺序。接手者请从 §9 开始，并按 §10 的自检清单逐条验证本文的每一项事实陈述。
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
| **开发主管** | 长期驻留的 HomeAgent 主 agent | 拆任务、**定义判据**、复审、决定合入 |
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
            └─ 独立仓库的独立分支（clone 或 worktree）
                 └─ 拉起 agent（注入提示词，cwd = 该目录）
                      └─ agent 自审 → 主管复审 → 插件复核 → 提 PR → 合入
```

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

### 4.3 临时工作目录：用 mirror 派生，不要每个任务全量 clone

用户描述「每次修改都是拉取仓库」。直接 clone 的问题：Go 这类仓库一份全量历史可达
数百 MB，并行十个就不合适。

**建议**（**［待定］**，需接手者评估）：

```
<插件数据目录>/program/
  mirrors/<platform>-<repo>.git/     # git clone --mirror，每个项目一份，定期 fetch
  work/<hash>-<name>/                # 每个任务一份，由 mirror 派生
```

派生方式二选一（都要实测磁盘与耗时）：
- `git clone --reference <mirror> <mirror> <workdir>`（`--dissociate` 可选）
- `git worktree add`（注意 §3.8：同分支不可双挂——这反而是好事，能防两个 agent 抢同一分支）

> **注意**：用户原话是「每个目录下是**独立仓库**的独立分支」。若严格按「独立仓库」，
> worktree 就不适用（它是同仓库多工作区）。**这是需要与用户确认的选择**（§8.1）。

### 4.4 目录命名

用户描述 `program/hash-name`。建议：
- `hash` 用 `项目名 + 分支名` 的短哈希，**保证可重算**（便于清理孤儿目录）
- 目录内记录一份 `_commondev.json`（项目/分支/agent/base_sha/凭据指纹），便于事后审计
- **不要**把凭据写进目录（见 §7.2）

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

### 6.1 拉起 agent：走 CLI，不走 A2A/ACP ［本文建议］

| 方案 | 优点 | 缺点 |
|---|---|---|
| **`pi -p`（CLI）** | 进程 cwd 即工作目录（**已实测**）；一次性进程，天然按任务隔离；零协议改动 | 每次冷启动（可接受）；需要自己管进程与超时 |
| `dsh headless` | 同上 | 本机凭据失效，需先修 |
| 扩建 A2A | 复用现有插件 | 要改扩展 + payload；ACP 的 session 模型本就假设长驻，加 cwd 是**违背其设计意图** |
| 扩建 ACP | 同上 | 同上 |

**建议**：第一刀用 `pi -p`。它同时满足「指定目录」与「一次性拉起」两个硬需求，
且不需要动任何协议。A2A/ACP 留给「需要与**常驻** agent 对话」的场景。

### 6.2 第一刀只做一个平台、一个项目、一个任务

理由：这套机制有**至少四个独立的失败面**（凭据 / 拉起 / 判据 / 合入），
一次全上会无法归因。先用 Gitea（本机就有，API 可控）跑通**一条真实链路**：

```
登记项目 → mirror → 开分支 → 拉起 pi -p（cwd=工作目录）
  → agent 改 → 主管定义判据 → 插件跑「正向 + 变异」
  → 提 PR → 主管合入
```

**这一刀的目的不是功能完整，是证明「假绿检测」这一环真的能工作。**

### 6.3 权限模型：让「必须有主管复审」成为机制而非礼貌 ［建议］

若执行 agent 所用的凭据能 push 主干，则「主管复审」只是**流程约定**——想绕过随时能绕过。

建议（依赖 Gitea 分支保护）：
- 执行侧凭据：**只能 push `feat/*`，不能 push `main`，不能合 PR**
- 主管侧凭据：**能合 PR**
- 这样「用户只与主管对接」在用户不在场时**也成立**

### 6.4 落地顺序

| 刀次 | 内容 | 证明什么 |
|---|---|---|
| **0** | **修「能拉起 agent」**（§3.6） | 前提是否成立 |
| **1** | 单平台单项目：mirror → 分支 → 拉起 → 判据+变异 → PR | 机制成立，假绿可检测 |
| 2 | 接 Gitea 分支保护 + 执行/主管双凭据 | 复审是机制不是礼貌 |
| 3 | 多项目 + 并行 + conflict 预检 | 可扩展 |
| 4 | 测试模板库（压力/接口/编译） | ⚠️ **放最后** |

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

### 8.1 「独立仓库」还是「worktree」？

用户原话是「每个目录下是**独立仓库**的独立分支」。
- 独立 clone：隔离彻底、删除干净、无「同分支双挂」冲突 —— 更贴合用户描述
- worktree：省磁盘与网络，但不能叫「独立仓库」

本文建议：**mirror + 派生**（§4.3），本质仍满足「独立仓库」语义（每目录一份 checkout），
但需要用户确认这符合预期。

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

## 11. 给接手者的三条提醒

1. **不要把「测试通过」当作「改对了」**。本文 §4.5 的变异验证不是可选项，
   它是这套机制唯一能防住假绿的手段。本仓当天因此浪费过一整轮排查。

2. **不要用提示词层面的软约定替代机制**。前序 worktree 讨论里
   「在任务描述里写明工作目录」就是这一类——它靠模型自觉，没有任何保证。§3.8 已记录该教训。

3. **前提不成立时，不要继续设计上层**。§3.6 的「拉起 agent 不工作」如果没解决，
   后续所有设计都是纸面的。**先让它能跑，再让它跑得好。**
