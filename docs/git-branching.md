# Git 分支管理规范

> 生效：2026-08-31，2026-09-04 修订（三级发布通道 + 单条发布分支），2026-09-06 修订（SDK 仓版本语义与发版联动，见 §七），2026-09-13 修订（发布线路牌随 patch 推进 + 版本号不得固化落库）。
> 适用：**本仓（TrueAgent/HomeAgent）与 third_party/homeagent-sdk（SDK 仓）**——两仓协作时分支策略必须一致，本规范两仓同用。
> 核心原则一句话：**main 唯一长命、永远可部署；一切新工作在特性分支；一个中版本一条发布分支，alpha/beta/正式由 tag 区分；hotfix 只进发布分支并 cherry-pick 回 main。**

---

## 一、分支类型总览

| 分支 | 生命周期 | 来源 | 去向 | 部署性 |
|---|---|---|---|---|
| `main` | **唯一长命分支** | — | — | ✅ **永远可部署** |
| `feature/xxx` | 短命（本次特性完成即删） | main | 合回 main | ❌ 不部署 |
| `release/vX.Y.x` | 中命（**整个中版本生命周期**） | main | 打 tag → 构建发布 | ✅ **发布产物来源** |
| hotfix（直接提交发布分支） | 随发布分支 | 发布分支 | **cherry-pick 回 main** | ✅ |

```
main ──────────────── E ──────────────── G ────────────────（永远可部署）
      │                                    ▲
      │ feature/xxx                         │ cherry-pick（修复逐个 pick 回）
      ├── A ── B ──(合回)───────────────────┤
      │                                    │
      └── release/v1.0.x ────────────────────────────────────────────────
          │                                │              │
          ├─(tag v1.0.0-alpha.1) 内部验证   │              │
          ├─(tag v1.0.0-beta.1)  小范围试用 │              │
          ├─(tag v1.0.0)         正式发布   │              │
          ├─(hotfix) F ─────────────────────┤              │
          ├─(tag v1.0.1)         patch 发布 │              │
          ├─(hotfix) H ────────────────────────────────────┤
          └─(tag v1.0.3)         patch 发布
```

---

## 二、分支职责

### 1. `main`（唯一长命分支）

- **唯一长期存在且永远可部署**。任何时刻 `git checkout main` 出来都是可构建、可上线的状态。
- 积攒**下一个中版本**的功能：feature 分支完成即合回，main 持续向前。
- **main 上不直接开发**。所有改动经 feature 分支合入；hotfix 经 cherry-pick 注入。
- **main 的 `internal/meta.Version` 始终是下一个未发布版本**，不随 patch 发布变动。
- 合入门禁（**单人直推也遵守**，不强制 PR 但强制验证）：
  - `make test` 全绿
  - 涉及插件/工具链时：接口冻结检查 `git diff third_party/homeagent-sdk/sdk/` 为空
  - `go vet ./...` 无新增告警

### 2. `feature/xxx`（新特性/修复）

- 命名：`feature/<短横线描述>`，如 `feature/plugin-proc-migration`、`feature/memory-media`。
- **从 main 开出**：`git checkout -b feature/xxx main`。
- 完成后合回 main：
  - 单人：直推（`git merge --no-ff` 保留特性边界，或 squash 成一个 commit，二选一在团队内固定）。
  - 多人：走 PR（review 后合入）。
- 合回后删除 feature 分支（避免累积）。

### 3. `release/vX.Y.x`（发布分支：一个中版本一条）

- **命名用 `x` 占位 patch 位**：`release/v1.0.x` 承载 1.0.0 → 1.0.1 → … → 1.0.N 全部发布，
  直到 `release/v1.1.x` 切出为止。**不要按 patch 号建分支**（`release/v1.0.1`、`release/v1.0.3` 各一条会把
  同一发布线切成互不相连的碎片，追溯时无法用一条分支看完整条线的演进）。
- **从 main 的某个可部署点切出**：`git checkout -b release/v1.0.x main`。
- 切出后**冻结功能**——发布分支上只做：版本号 bump、发布准备、bug 修复、文档。
- **发布线的 `meta.Version` 必须跟着该线已发的最后一个 patch 走**（`release/v1.1.x` 末态
  `1.1.1`、`release/v1.2.x` 末态 `1.2.2`、`release/v1.3.x` 末态 `1.3.6`）。
  ⚠️ **不要只用构建参数（`-ldflags -X ...meta.Version`）打版本号而不改源码**：
  二进制自称 1.3.4、源码路牌还停在 1.3.0，追溯时对不上账（2026-09-13 真实踩过，
  1.3.1–1.3.4 四个 patch 都是这么打的，`release/v1.3.x` 的路牌一直没动）。
- **现网部署永远用发布分支上 tag 的构建产物**，不是 main 头部、更不是 feature。

### 4. 三级发布通道（alpha / beta / 正式）

通道**由 tag 区分，不由分支区分**——三者共用同一条 `release/vX.Y.x`。

| 通道 | tag 形式 | 含义 | 受众 |
|---|---|---|---|
| alpha | `vX.Y.Z-alpha.N` | 功能齐了但未充分验证，可能有已知缺陷 | 仅内部/开发者自测 |
| beta | `vX.Y.Z-beta.N` | alpha 问题已修，等待真实环境暴露长尾问题 | 小范围试用、愿意承担风险的用户 |
| 正式 | `vX.Y.Z` | 通过验证，可上现网 | 所有用户 |

- **推进顺序**：alpha → beta → 正式，逐级向前，**每级都是同一条分支上的新 tag**。
  这也是 semver 的标准预发布语义（`1.1.0-alpha.1 < 1.1.0-beta.1 < 1.1.0`），
  包管理器与版本比较逻辑天然认得，无需额外约定。
- **允许跳级**：若改动小、验证充分（如仅一处已定位并有回归测试覆盖的内核修复），
  可直接打正式 tag。跳级要在发布说明里写明理由。
- alpha/beta 的构建产物**可以上传 release 附件**，但必须在 gitcode release 上勾选
  "预发布"标记，且发布说明首行标注通道与已知风险。
- **beta 未清零的严重问题不得进正式**：正式 tag 意味着"我们认为它能上 24/7 现网"。
- **发版动作只在发布分支上做**：版本号 bump、打 tag、构建产物、上传 release 附件，
  全部发生在 `release/vX.Y.x` 上。**main 永远不是发版分支**——即使某个改动刚刚合进 main、
  即使 main 此刻可部署，也不从 main 打 tag、不拿 main 的构建产物发布。
  main 的版本号是「下一个未发布中版本」的路牌，不是任何一次发布的版本号。

### 5. hotfix（发布后发现的严重 bug）

- **场景**：版本已发布后，发现只存在于该版本（或该发布线）的严重 bug。
- **动作**：直接把修复提交到**发布分支** → 该分支重新构建、打下一个 patch tag（如 `v1.0.4`）发布。
- **关键：hotfix 必须 cherry-pick 回 main**：

  ```bash
  # 在发布分支上提交修复（代码部分与版本号 bump 分开提交）
  git checkout release/v1.0.x
  git commit -m "fix(x): ..."                    # ① 修复本身
  git commit -m "chore(release): bump v1.0.4"    # ② 版本号（此 commit 不 pick 回 main）
  git tag -a v1.0.4 -m "..."

  # 回到 main，只挑修复本身
  git checkout main
  git cherry-pick <修复①的sha>                   # 只 pick ①，不 pick ②
  ```

  > **为什么 cherry-pick 而不是 merge**：发布分支只承载该版本特有的补丁，merge 会把
  > 版本号/发布相关改动一并带进 main 造成冲突，并让 main 的 `meta.Version` 变成
  > 已发布的旧版本号。逐个 cherry-pick 让 main 精确地只获得修复本身。
  > **版本号 bump 不要 pick 回 main。**

- **同时存在多个活跃 feature 分支时**：修复也要 pick 到那些分支，否则它们合回 main 时
  可能带回旧代码。实践做法是修复落地当天就 pick 到全部活跃分支
  （如 2026-09-04 的 stage 双重解锁修复同时 pick 到 `main` 与 `feature/memory-media`）。

- **hotfix 已逐个 pick 回 main ⇒ main 已含全部修复 ⇒ 无需再合并发布分支回 main**。
  这是本规范刻意为之——除非发布分支上有 main 想要的**功能级**改动（罕见），
  否则发布分支永不 merge 回 main。

### 6. 发布分支退役

- **下个中版本发布 = 上一条发布分支生命周期结束**（`release/v1.1.x` 出现即 `release/v1.0.x` 退役）。
- 退役后可删可留：
  - 删除：保持仓库干净（tag 已保留全部历史，删分支不丢东西）。
  - 保留：便于追溯该发布线的历史构建（对 24/7 现网友好）。
- **按 patch 号命名的历史发布分支应当合并/删除**：它们是本规范修订前的遗留形态，
  内容已被对应的 `release/vX.Y.x` 完全包含，保留只会让"哪条才是这条线"变得含糊。

---

### 7. 开发者文档的发布归属（以 rel 分支的形态为准）

**规则：面向使用者的开发者文档，先在对应的 `release/vX.Y.x` 上修正成「这一版的实际行为」，
再 cherry-pick 合入 `main`。**（文档属 §二.3 所列的发布分支允许事项之一）

为什么不能直接改 main：

- `main` 的语义是**下一个未发布版本**（§二.1）。在那儿写的文档要么描述尚未发布的行为，
  要么与当前 rel 的实际行为**相反**，而文档的读者（包括模型自身）会把它当事实。
- `assets/docs/**` 会**随发行包分发并在 WebUI 里被阅读**——它服务的是“这一版”，不是“下一版”。
- 版本号、工具名、机制的有无都是**随版变动的**：同一个文件在两个分支上就应该是两种口径。

做法：

```bash
git switch release/v1.2.x
# 按这一版口径修改：版本号、当前工具名（hmapdev）、已移除机制不再写成现行
# ... 编辑 assets/docs/**、README{,_EN}.md、docs/zh/** ...
git commit -m "docs: 按 v1.2.x 口径修正 …"
git switch main && git cherry-pick <sha>   # 遵守 §三：只 pick，不 merge
```

`main` 上若需要描述“下一版才有的行为”，必须显式标注（如「（下一版）」或附版本号），
不得让读者以为它已发布。

**反例（本仓真实踩过，均为“文档当成事实后反向误导”）**：

| 现象 | 后果 |
|---|---|
| 人格卡写死 `v0.9.0（C ABI v2）` | 内核接口/日志报 1.2.0，agent 却向用户自述旧版本（且该机制 v1.0.0 已删除） |
| 架构文档在 1.2.0 后仍把“描述式索引 + 引用计数 GC”写成现行机制 | 读者按已删除的设计理解现行行为 |
| README 停在 v1.1.1 并描述已被删除的机制 | 同上 |
| 人格文本在**播种时**就把 `meta.Version` 插值写进配置库 | 装机那天即冻住版本号：内核 1.3.x 的实例仍向用户自称 `v1.0.3`（2026-09-13 用户当场发现） |
| 发布线只用 `-ldflags -X` 打版本、源码 `meta.Version` 不动 | 二进制自称 1.3.4、源码路牌仍是 1.3.0，溯源对不上账；同类还有给 SDK 误发 patch tag（§七.1 要求 patch 位恒为 `.0`） |

配套硬约束：**任何“模型或用户会当作事实”的文本，都不得写死版本号**——
要么**在渲染时**用 `meta.Version` 插值（系统提示词占位符 `{{kernel_version}}` 即此机制），
要么要求读运行时快照，并用测试钉住（如 `TestDefaultPersonaPromptHasNoVersionLiterals`）。
**"插值"指的是每次组装时现算，不是把算好的结果固化进配置库/文档** ——
固化过的版本号与写死没有区别，而且更难发现。

---

## 三、当前分支对齐（2026-09-13 更新）

### 主仓（TrueAgent）

| 分支 | 状态 | 处理 |
|---|---|---|
| `main` | 含全部回流修复；`meta.Version` = 下一个未发布中版本（现为 **`1.4.0`** —— `1.3.0` 已归发布线所有） | ✅ 保持 |
| `release/v1.3.x` | **本条发布线**，`meta.Version` = **`1.3.6`**（该线最后一个 patch）；承载 `v1.3.1`…`v1.3.6`；vendored SDK 定版 `1.3.0` | ✅ 保持 |
| `release/v1.2.x` | 承载 `v1.2.0` / `v1.2.1` / `v1.2.2`，末态 `meta.Version` = `1.2.2` | 📦 已退役（§2.6），保留供追溯 |
| `release/v1.1.x` | 承载 `v1.1.0-beta.1` / `v1.1.0` / `v1.1.1`，末态 `meta.Version` = `1.1.1` | 📦 已退役，保留供追溯 |
| `release/v1.0.x` | 承载 1.0.x 全部 tag | 📦 保留 |

> - `v1.3.0` 是**已撤回**的坏 tag：设备输出通道名 `device/<id>` 里的 `/` 拼进 LLM 函数名
>   `output_send__device/<id>`，上游按**整条请求** 400，全量对话不可用（修复见 `v1.3.1`）。
> - `feature/*` 分支（`input-semantics`、`multimodal-embedding`、`memory-media`、`plugin-proc-migration`）
>   均已合入并删除。

### SDK 仓（homeagent-sdk）

| 分支 | 状态 | 处理 |
|---|---|---|
| `main` | `meta.Version` = 下一个未发布中版本（现为 **`1.4.0`**）——`1.3.0` 已随核心**正式** tag 定版（§七.3），故路牌推进 | ✅ 保持 |
| `release/v1.3.x` | `meta.Version` = `1.3.0`，承载 tag `v1.3.0`（5 平台 `hmapdev` + `SHA256SUMS` + 4 个源码归档） | ✅ 与核心对应 |
| `release/v1.2.x` | `meta.Version` = `1.2.0`，承载 tag `v1.2.0`；同线的 `v1.2.1` 属**误发的 patch tag**（§七.1 违规），其 gitcode 条目标题已标注「（已撤回）」 | 📦 已退役 |
| `release/v1.1.x` | `meta.Version` = `1.1.0`，承载 tag `v1.1.0` | ✅ 与核心对应（已退役） |
| `release/v1.0.0` | 旧 patch 号命名形态，内容已被 main 完全包含 | 📦 保留（供追溯 1.0 线构建） |

> ❗**SDK 仓不发 patch tag**（§七.1）：一个中版本只发一次 `vX.Y.0`。
> 2026-09-13 曾误发 `v1.3.1`（文档用），**已撤回**（远端 tag 已删，本地 commit `05b7a20` 可恢复）；
> `v1.2.1` 是同一类历史遗留。
> ❗**现网 SDK store 例外**：本机 `hmapdev` store 用 `--from` 装的是 SDK 源码构建的 1.3.0，
> 与 tag 内容一致。

### 1.0.x 发布线 tag 历史

| tag | 提交 | 通道 | 说明 |
|---|---|---|---|
| `v1.0.0` | `9b92a04` | 正式 | 外部插件从 C ABI 迁移到子进程 + 共享内存 |
| `v1.0.1` | `e671a8c` | 正式 | 多模态 bugfix（假成功、能力声明与回退链、see_video 帧数语义） |
| `v1.0.3` | `26dc76f` | 正式 | 内核 stage 协调器双重解锁（直接跳正式：单点修复 + 反向验证 + 全类审计） |

> `v1.0.2` 未使用：该号从未发布也无 tag，留空以免与任何本地构建混淆。

### 1.1.x 发布线 tag 历史

| tag | 提交 | 通道 | SDK | 说明 |
|---|---|---|---|---|
| `v1.1.0` | `579d7db` | 正式 | 1.0.0 | 记忆系统支持二进制多媒体节点（CAS 媒体存储 + L0/L2/L3 贯通） |
| `v1.1.0-beta.1` | `7a57a14` | beta | 不发 | 打包链路验证（GUI 架构污染 + 空壳 node_modules）。按 §七.2，beta 不伴随 SDK 发版 |
| `v1.1.1` | 见发布说明 | 正式 | **1.1.0** | 多模态贯通插件边界；SDK 首次随核心正式版发布 |

> `v1.1.0-beta.1` 的提交序在 `v1.1.0` **之后**（它多含一个打包修复），
> 而 semver 预发布语义里 `1.1.0-beta.1 < 1.1.0`。这是「一条发布分支 + tag 区分通道」的
> 已知代价：beta 是为验证**打包链路**而补打的，不代表源码更旧。发布说明里已注明。

### 1.2.x 发布线 tag 历史

| tag | 提交 | 通道 | SDK | 说明 |
|---|---|---|---|---|
| `v1.2.0-beta.1` | `215804c` | beta | 不发 | 统一多模态向量空间 + 媒体升为图记忆一等节点 + 数据面全量迁到共享内存（RPC 协议 **2**，与 1.x 不兼容）。按 §七.2，beta 不伴随 SDK 发版 |
| （正式 tag 待打） | — | — | — | 试运行 beta 无回退问题后打 `v1.2.0`，并同步 SDK 仓 `release/v1.2.x` + `v1.2.0` |

> 1.2.x 与存量插件**不兼容**：RPC 协议升到 2（fd3 布局改变），存量外部插件必须用
> 新版 plugindev 重编为 `plugin.bin`——**不支持滚动升级**，内核与插件须同批重建、同批安装。
> 按 §2.4，跳级直发正式版需在发布说明里列明「单点修复 / 反向验证 / 全类审计」三项；
> 本次改动面大（统一多模态向量空间 + 协议 2 + 数据面全量迁移），不满足跳级条件。

---

## 四、现网部署与版本对应（运维纪律）

- **现网 homed 永远部署 `release/vX.Y.x` 分支上 tag 的构建产物**，路径见 `Makefile`（`make build` → `build/homed`）。
- systemd 服务（`/usr/local/bin/homed`）替换流程：
  1. 备份旧二进制（`homed.bak.pre<版本>.<时间戳>`）
  2. 备份配置库（**用 `sqlite3 .backup`，不用 `cp`**——WAL 模式下 cp 可能拿到不一致快照）
  3. 记录当前插件建链清单，供重启后逐项比对
  4. `install -m 0755` 替换（原子 rename，不会写坏正在运行的进程镜像）
  5. `systemctl restart homeagent`
  6. 健康检查：版本号、插件清单无缺失、`/api/v1/status`、一次真实对话、`fatal error` 计数为 0
- **改造期间现网不得部署 main 或 feature 的中间态**——只有发版才用发布分支的 tag。
- alpha/beta tag 的产物**不上现网**（现网是 24/7 服务，预发布通道的存在就是为了不拿它冒险）。
- 涉及 SDK 仓时：主仓 `go.mod` 的 `replace => ./third_party/homeagent-sdk` 指向本地 vendored 副本，
  发版前确认 vendored SDK 与 SDK 仓 release tag 一致（**两仓中版本对齐是第一优先级**，见 §七）。
- **客户端版本必须与内核同步**（GUI / 鸿蒙 / waiter 同一个号，当前皆为 `internal/meta.Version`）：
  内核版本是唯一事实源，客户端不得各写一个。拉齐用 `make sync-client-versions`，
  发版前跑 `make check-client-versions` 做漂移门禁。waiter 直接引用 `internal/meta`（无第二份字段）；
  鸿蒙的 `versionName/versionCode` 由脚本写 `AppScope/app.json5`，运行时代码从 `bundleManager` 读，
  不再硬编码。

---

## 五、快速参考命令

```bash
# 新特性
git checkout main && git pull
git checkout -b feature/xxx
# ... 开发 ...
git checkout main && git merge --no-ff feature/xxx   # 或 squash
git branch -d feature/xxx

# 开一条新中版本的发布线
git checkout -b release/v1.1.x main
git commit -am "chore(release): bump v1.1.0-alpha.1"
git tag -a v1.1.0-alpha.1 -m "..."                   # alpha：内部验证
# ... 修问题 ...
git commit -am "chore(release): bump v1.1.0-beta.1"
git tag -a v1.1.0-beta.1 -m "..."                    # beta：小范围试用
# ... 真实环境验证 ...
git commit -am "chore(release): bump v1.1.0"
git tag -a v1.1.0 -m "..."                           # 正式

# hotfix（发布后）——注意是同一条 release/v1.0.x，不新建分支
git checkout release/v1.0.x
git commit -am "fix(x): 严重 bug"                    # ① 修复
git commit -am "chore(release): bump v1.0.4"         # ② 版本号
git tag -a v1.0.4 -m "..."
git checkout main
git cherry-pick <修复①的sha>                          # ③ 只挑修复
# 若有活跃 feature 分支，也 pick 过去
git checkout feature/xxx && git cherry-pick <main 上那个 pick 的 sha>

# 公开 SDK 接口改动（feature，不是 hotfix）：先进 main，再 pick 到发布分支
git checkout -b feature/sdk-xxx main
# ... 改 third_party/homeagent-sdk/sdk/ 与内核桥接层 ...
git checkout main && git merge --no-ff feature/sdk-xxx
git checkout release/v1.1.x
git cherry-pick <feature 的各 sha>                    # 只挑改动，不挑 main 的版本号
git commit -am "chore(release): bump v1.1.1"          # 发布分支自己的版本号
git tag -a v1.1.1 -m "..."
# SDK 仓同步（仅在核心打正式 tag 时，见 §七.2/§七.3）
cd third_party/homeagent-sdk
git checkout -b release/v1.1.x main
git commit -am "chore(release): SDK 1.1.0（1.1.x 线全程共用）"
git tag -a v1.1.0 -m "..."

# 发布分支退役（下个中版本发布后，可选）
git branch -d release/v1.0.x                          # tag 已保存历史，删分支不丢东西
```

---

## 六、本规范与「接口冻结」约束的关系

> **接口冻结已到期（v1.1.x 起）**。冻结是**迁移期**的约束——它要保的是
> 「换运行模型不动业务代码」，靠 `git diff third_party/homeagent-sdk/sdk/` 为空来守。
> 迁移完成（v1.0.0 上生产）后该约束按时失效，取而代之的是 §八的三条演进规则。
> 本节保留历史条款，但**不再作为合回门禁**。

- ~~feature 分支合回 main 的门禁（`git diff third_party/homeagent-sdk/sdk/` 为空）~~
  —— **已失效**。现改为：公开接口的改动必须满足 §八（只增不减、签名不改、模板接线）。
- `internal/sdk` **不受冻结约束**，可自由扩展（此条仍成立）；
  公开 SDK 接口指 `third_party/homeagent-sdk/sdk/`。
- **公开接口的改动本身是 feature，不是发布准备**：它必须走 `feature/xxx` → 合回 main 的路径，
  再 cherry-pick 到发布分支。不允许把接口新增当成"发布分支上的 bug 修复"直接提交进 release
  ——发布分支冻结功能（§2.3），接口是最典型的功能面。

---

## 七、SDK 仓的版本语义与发版联动

### 1. SDK 版本号跟随核心的中版本，patch 位恒为 `.0`

| 核心版本 | 对应 SDK 版本 |
|---|---|
| 1.1.0 / 1.1.1 / 1.1.2 / … / 1.1.N | **1.1.0**（全线共用，不随核心 patch 变动） |
| 1.2.0 起 | **1.2.0** |

- 核心的 patch 位（`x`）专用于 **bugfix 与漏洞修复**，这类改动不触碰公开 SDK 接口，
  因此 SDK 版本号没有理由跟着动。
- **为什么不逐位对齐**：SDK 版本号是插件开发者的依赖声明。若核心每发一个 bugfix 就把 SDK
  也推一个新号，开发者要么被迫跟版、要么怀疑自己版本过时，而接口其实一个字都没变。
  让 SDK 号只在**接口可能变化的中版本边界**上跳，开发者只需关心「我在为哪个中版本写插件」。
- 因此「两仓版本对齐」在本规范里指**中版本对齐**（核心 1.1.x ↔ SDK 1.1.0），
  不是三位全等。核心 1.1.1 配 SDK 1.1.0 就是对齐状态。

### 2. beta 阶段不发 SDK

- **核心的 alpha/beta tag 不伴随 SDK 仓发版**：SDK 仓在这一阶段**不打 tag、不建 release**。
- **为什么**：beta 是核心自己的测试阶段，此时 SDK 接口尚未固定。若此刻给 SDK 发版，
  插件开发者会照着一个还会变的接口写代码——**那是无效开发**。接口没定就没有可依赖的契约，
  发出去的版本号是一个假承诺。
- 这条约束的对象是 **SDK 仓的发版动作**，不是核心二进制里有没有 SDK 代码。
  主仓 `go.mod` 用 `replace => ./third_party/homeagent-sdk`，任何核心构建都必然含 vendored
  SDK 源码，这是构建机制决定的，不在本条约束范围内。

### 3. 正式发布时 SDK 随核心一起发

核心打**正式 tag**（`vX.Y.Z`，无预发布后缀）时，SDK 仓同步执行：

1. SDK 仓也有自己的 `release/vX.Y.x`（与核心同名，一个中版本一条）；
2. 在该分支上把 `meta.Version` 定为 `X.Y.0`；
3. 打 tag `vX.Y.0`（首次进入该中版本时），并建 gitcode release；
4. 上传 5 平台 `hmapdev`（插件开发工具链）产物 + `SHA256SUMS`。

同一中版本内的后续核心 patch（1.1.1 → 1.1.2 …）**不重复发 SDK**——SDK 已经是 1.1.0，
没有新东西要发。只有接口再次变化并进入下一个中版本时，SDK 才发 1.2.0。

### 5. 发版产物清单（可复现）

**推 tag ≠ 完成发版**：还要打包产物、建 gitcode release 条目、上传附件。
2026-09-13 出现过"tag 推了、release 条目和产物都没有"的情况（`v1.3.1`–`v1.3.6`），
事后才补 —— 记录在此以免重犯。

**核心仓**（在 tag 的**干净 worktree** 里构建，不要用带其它会话改动的工作区）：

| 产物 | 生成方式 |
|---|---|
| `homeagent_<版本>_linux_amd64.tar.gz` | `VERSION=<版本> bash deploy/packaging/package-linux.sh amd64` |
| `homeagent-client_<版本>_amd64.deb`、`-server`、`-full` | 同上；server/full 需要 Chinese-CLIP 与 ONNX Runtime 资产目录（`build/model-assets/`、`build/runtime-assets/`） |
| `SHA256SUMS` | **全部产物生成完毕之后**统一计算（边打边算会漏掉后生成的包） |
| 4 个源码归档（`.zip` / `.tar.gz` / `.tar.bz2` / `.tar`） | gitcode 打 tag 时自动生成，无需上传 |

**SDK 仓**：`VERSION=<版本> bash package/build.sh all hmapdev` ⇒
`hmapdev_{linux,darwin}_{amd64,arm64}` + `hmapdev_windows_amd64.exe` + `SHA256SUMS`。

**Windows 安装器（WSL 安装型）**：`homed` **不再装到 Windows**（插件体系依赖 fd 继承与
共享内存区段内偏移解引用，Windows 句柄模型表达不了），安装器的职责是**引导 WSL2 并把
Linux 包送进发行版里安装**。产物 `HomeAgent_v<版本>_{Server,Client,Full}_win64.exe`：

```bash
# 先有 Linux 包（安装器送进 WSL 的就是它），再打安装器
VERSION=<版本> bash deploy/packaging/package-linux.sh amd64
VERSION=<版本> bash deploy/packaging/package-windows.sh server amd64   # 只装内核+CLI 的 WSL 场景
VERSION=<版本> bash deploy/packaging/package-windows.sh client amd64   # 需要 Windows GUI payload
```

- `package-windows.sh` 会按**变体只放对应的那一个 deb** 进 payload。为什么：WSL 侧脚本只取
  payload 里第一个 `.deb`（`install-via-wsl.ps1`），而 server/full 的 deb 各带 ~719MB 模型 ——
  照 `build.sh` 的 `stage_linux_payload`（把所有 deb+tar 全塞）打出来会是 ~2.4GB 的安装器。
- `client`/`full` 变体还带 Windows GUI，需要 electron-builder 产出
  `build/homeagent-gui-win32-x64/`；缺它就**明确失败**，不产出"装完没有界面"的半残包。

**上传**（两仓同一个脚本）：

```bash
# 核心仓
python3 deploy/scripts/upload_assets.py <tag> <token>                # 默认上传 dist/release 下可识别的产物
# SDK 仓（hmapdev_* 没有扩展名，不会被自动识别 ⇒ 必须显式列文件名）
GITCODE_REPO=JianFeeeee/homeagent-sdk ASSET_DIR=<sdk>/dist/release \
  python3 deploy/scripts/upload_assets.py <tag> <token> hmapdev_linux_amd64 ...
```

- 脚本先向 `releases/<tag>/upload_url` 取 **OBS 预签名 URL** 再 PUT ⇒ **release 条目必须先存在**；
- alpha/beta 的产物可以上传，但必须在 release 条目上勾选**预发布**标志（§2.4）；
- 校验和必须覆盖**全部**附件，否则等于没有校验。
- ❗❗**gitcode 的 release 附件是"同名只写一次"**（实测：同名两次不同内容，下载始终是第一次那份；
  且没有可用的删除接口 —— release JSON 不含 `id`，附件列表接口 404，`DELETE .../attach_files/<名>`
  只要数字 id）。**后果**：`SHA256SUMS` 若第一次上传时只覆盖了部分平台，之后**永远改不回来** ——
  1.3.1–1.3.10 都踩了：首次只传了 linux/amd64，后来补 arm64/darwin/win 时合并重传**全部无效**，
  线上那份至今只有 4 项。
  ⇒ **纪律：首次上传 `SHA256SUMS` 前必须已打包全部平台**；分批上传时**先传产物、最后传校验和**，
  且校验和只传一次。补救只能换名（如 `SHA256SUMS.complete`）或重建 release（要重传全部产物）。
- ❗**流水线脚本必须 `set -e`（或显式检查每步）**：否则某一步失败（例如驱动脚本在 tag 里
  不存在）之后它仍会继续跑到上传，把**半成品校验和**推上去覆盖全量的那份。
  （实测：v1.3.10 的校验和被 4 项覆盖掉，只能重建。）
- ❗**在 tag 的 worktree 里构建时，驱动脚本要么已进该 tag，要么支持目录覆盖**：
  新补的脚本只存在于 main，去 tag 的 worktree 里调就是 `No such file or directory`。
  现 `package-windows.sh` 支持 `DIST_LINUX` / `BUILD_DIR` / `DIST_RELEASE` 覆盖，
  可以"用主仓的脚本 + 产物目录指向 worktree"。
- ❗**分批上传时，后一轮必须在全量产物上重算 `SHA256SUMS`**，不能只算本轮那几个文件：
  同名附件会**覆盖**前一轮的校验和（实测：先传 amd64 的 9 个资产，后补 arm64 时
  只算了 arm64 的 4 个，结果 amd64 的校验和从 release 上消失 ⇒ 已下载的包失去校验依据，
  只能把产物全部下回来重算）。要么一次打包全部平台再算，要么后一轮把**已上传的**
  也纳入计算。

### 4. 版本号在两仓 main 上的含义

两仓的 `main` 都遵守 §2.1：`meta.Version` 是**下一个未发布中版本**。
所以在 1.1.x 线发布期间，两仓 main 上的值都是 `1.2.0`——它标记「main 正在积攒 1.2 的东西」，
而不是「1.2.0 已经存在」。已发布的版本号一律看对应 `release/vX.Y.x` 分支与 tag。

**但两仓「同步推进」是有条件的**（这一点曾导致误判，现补写清楚）：
推进的前提是**该中版本已经正式发布过**。具体到当前：

- 核心：切出 `release/v1.2.x` 后，1.2.0 就归发布线所有，main 立即推进到 `1.3.0`；
  **即使 1.2.0 目前只有 beta tag**（beta 不上现网，但发布线已占住这个号）。
- SDK：因为 §七.2 **beta 不发 SDK**，SDK 1.2.0 要等核心的**正式** tag 才定版、
  建 `release/v1.2.x`、打 `v1.2.0`（§七.3）。在那之前，SDK 的「下一个未发布中版本」
  仍然是 `1.2.0`，其 main 不得越过它。

→ 因此在这一阶段，**核心 main = `1.3.0` 而 SDK main = `1.2.0` 是正确的**，
不是遗漏同步。（曾按本节的例子把 SDK main 也推到 1.3.0，等于宣称 SDK 1.2.0 已发布。）

---

## 八、公开 SDK 接口的演进规则

> 本节原在《外部插件接口不变矩阵》（迁移期临时文档，已随迁移完成删除）§九。
> 那份文档记的是**迁移期**的约束（"换运行模型不动业务代码"，靠
> `git diff third_party/homeagent-sdk/sdk/` 为空来守）。迁移完成后该约束**到期**——
> 继续冻结等于让 SDK 永远停在迁移那天的能力面，多模态这类功能永远到不了插件手上。
> 取代它的是下面三条更弱、但仍然硬的规则。

### 1. 只增不减，签名不改

新增字段、新增方法可以；**改已有方法的签名、删字段、改字段语义不行**。

实例：v1.1.0 想让插件能给三元组关联媒体，两条路——改 `Commit` 的签名加一个参数，
或新增 `CommitWithMedia`。选了后者。改签名会让每个调 `Commit` 的插件编译失败，
而那些插件根本不关心媒体。

### 2. 新增方法必须是「插件调用、内核实现」方向

这是**存量插件不需要重编**的技术原因：`IOInjector` 新增方法后，插件只是
*多了可以调的东西*，没有新的实现义务。反过来若在 `Plugin` 接口上加方法，
每个存量插件都会因未实现而编译失败。

### 3. 生成模板必须同步接线，否则是**全体外部插件编译失败**

公开接口加方法时，`tools/hmapdev/templates/proc_main.go.tmpl` 里的实现若不满足新接口，
每个外部插件都**编不过**——是硬失败，不是软降级。

完整接线链共六处：`protocol.go` 的 method 常量 → `capability.go` 的能力归属 →
`corehandler.go` 的分派分支 → `proc_core.go` 的委托 → `proc_main.go.tmpl` 的模板实现 →
测试替身（`fakeCoreSDK`、`injectCapture`、`capability_test.go` 的手工方法清单）。
还要同步 `yaegi/mocksdk`——它没有任何代码对着编译，漂移**不会被编译器抓到**。

### 4. 兼容性只认主版本号（不再有独立的协议号）

**不自定协议版本号**：兼容性由双方版本号的**主版本段**表态
（内核 `meta.Version` ↔ 插件 `sdk_version`）。主版本相同即兼容，跨主版本则
在握手时被拒绝并提示用配套 `hmapdev` 重编。

为什么不要独立协议号：语义版本变动频繁（修 bug、加字段），额外的号必然被
漏升而腐烂（历史实证：曾有一个 `ProtocolVersion` 常量，多次布局变更里只升过
一次，而对应守卫测试长期失败无人察觉）。兼容性只挂在**始终会被发的那个号**上，
就不存在「忘同步」这个失效模式。

布局一致性（如 1.2.0 的 fd3 → 统一共享区）另由**结构派生事实**把关：
统一区魔数 + `field_count`（描述符个数）在握手上显式校验。
它们是二进制里的实际值，不需人去维护，也不同“谁记得升号”。

### 5. 合回 main 前要同步的东西

1. 改动公开 SDK 接口面后，同步 SDK 仓的版本（§七）与 `SDKCompatibleVersion`；
2. 生成模板已接线（跑 `cd tools/hmapdev && go test ./...`，含
   `TestProcTemplate_CoversAllCoreMethods`）；
3. 存量插件源码零改动（逐个 `cd example/<n> && go vet ./...`）；
4. 并发安全（`go test -race -count=5 ./sdk/`）。
