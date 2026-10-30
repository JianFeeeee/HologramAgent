# HologramAgent 遗留问题清单

> **状态：长期清单（部分条目已过时）。** 本文按「仍未完成且经核实」筛选，
> 但清单会随代码演进滞后 —— 引用某条前请先用源码复核。
> 它**不是** future-plan 的替代：future-plan 收「能力缺失」与「机制在但零调用方」，
> 本文收「已知问题与收尾项」。两者有交叉时以 future-plan 为准。

> **本文定位（2026-09-25 重写）**：只写**仍未完成、且经源码/实测核实确属本仓库**的问题。
>
> **判定方法**：不继承任何旧档的复选框，回到源码与构建现场逐条重验
> （`grep` 定符号、`go build`、`go test -count=1`、交叉编译试验、双构建试验）。
> 旧档标记「未做」但代码已落地的项、以及引用别仓代码的项，统一进 §三/§五 并附证据。
>
> **本文不写**：已完成项的定位过程（那是 git log 与 commit message 的职责）、
> 无实测支撑的性能断言、任何凭据/生产路径。

---

## 〇、本次重写相对上一版（b6027f3）的三个更正

上一版是源码核实版，结论基本可靠；但本轮**现场实测**推翻了其中三处，记在此处防复发：

1. **「`go test ./...` 有 3 个 FAIL」是环境相关的，不是稳定复现** ——
   本机 12:50 实测 `go test -count=1 ./...` **57 包：38 ok + 19 无测试 + 0 FAIL**；
   但 11:45 同一命令确实报过 PTY 三例失败（当时 `/dev/ptmx` 报
   `permission denied`）。环境恢复后不再复现。
   ★ **真正的缺陷不是「有无 FAIL」，而是那个 skip 是坏的**：PTY 用例**有**
   `t.Skipf("PTY not available: ...")`（`integration_test.go:284/337/395`），
   但它查的是 `resp["status"] == "error"`，而 `terminal_create` 失败返回的是
   `{"error": "创建终端失败: ..."}`（`agentcli/plugin.go:496`）——**键名不匹配**，
   所以环境退化时走的是 `t.Fatalf` 而非 skip。
   ⇒ 「探测形同虚设」比「没有探测」隐蔽，这才是该修的点（见 §六）。
2. **P2-8「Windows 真机验证」整条作废** —— Windows 支持**已被设计性放弃**
   （`cmd/homed/platform_windows.go`：原生 Windows 拒绝启动，改走 WSL2）。
   拿「Windows 下 homed 加载插件往返」当验收目标，等于要求一个明确不存在的产物。
3. **新增最高优先级项：C 化 WIP 的三处构建回归** —— 已 **修复**（2026-09-25，见 §二 P0-1）。
   原始症状：`linux/arm64` 发布目标必然失败、clean clone 编不出 `homed`。
   ★ 修复过程中又发现一个更隐蔽的陷阱：**Go 构建缓存不跟踪包外 `#include` 的 C 文件**
   （改 C 源码但缓存命中时会静默沿用旧代码）——这对「逐步推进 C 化」是致命的，
   已改用包内符号链接避坑。

---

## 一、结论速览

| 类别 | 数量 | 去向 |
|---|---|---|
| 真正待办（本仓库、代码层可动） | 7 项 | §二 |
| 已修复（本轮） | 1 项 | §二 P0-1 |
| 生产部署后验证（代码已就绪，需现场跑） | 4 项 | §四 |
| 已关闭 / 已实现（旧档误标为 TODO） | 8 项 | §三 |
| 跨项目工单（不属本仓） | 2 项 | §五 |
| 明确「不做」（防反复挂账） | 2 项 | §六 |

当前健康度（实测）：

- `go build ./...`（宿主）通过；`go test -count=1 ./...` **57 个包：38 ok + 19 无测试文件 + 0 FAIL**
- `CGO_ENABLED=0 go build ./cmd/homed` **必然失败**（`gojieba` 是 cgo-only）——
  这是**既有事实**，非本轮问题；`homed` 历来只有 cgo 构建。
- 编解码层双路径（cgo / !cgo）均绿：`make check-codec-paths`。
- `linux/amd64` 与 `linux/arm64` 发布目标均可构建：
  `bash deploy/packaging/build.sh linux/arm64 homed` → ELF aarch64（已修，见 P0-1）。

---

## 二、真正待办

按「影响面 × 可验证性」排序。每条给出**证据**（file:line / 实测命令）与**验收**。

### ✅ P0-1　C 化第一刀的构建回归 —— **已修复（2026-09-25）**

**原始问题**（并行 agent 的 `feature/c-core` 工作区改动引入）：
1. clean clone 编不出 homed：cgo `LDFLAGS` 硬指向 `csrc/build/libha_codec.a`，
   而该 `.a` 既不入库（`.gitignore` 的 `build/` 命中）又不被发布脚本生成
2. `linux/arm64` 发布目标必然失败：宿主 x86-64 的 `.a` 被链进 aarch64 产物，
   报 `file in wrong format`
3. `Makefile` arm64 目标缺 CC/CXX，且无 `.syso` 隔离

**修复方式**（判据：不是「能跑」，而是「不能被静默绕过」）：

| 问题 | 修法 |
|---|---|
| 链接预构建 `.a` | 改为**包内符号链接** `internal/agent/api/ha_codec.{c,h}` → `csrc/` 权威源；cgo 直接编源码 |
| 交叉编译架构错 | 编源码按目标重编，天然正确 |
| arm64 缺工具链 | `Makefile` 补 `CC_ARM64`/`CXX_ARM64` 与 `.syso` 隔离（照抄 `build.sh` 已有做法）|
| 双构建无覆盖 | 新增 `make check-codec-paths`（cgo 与 !cgo 两条都跑）|

★ **为何不用 `#include "../../../csrc/src/ha_codec.c"`（包外相对包含）**：
**Go 构建缓存不跟踪包外被 `#include` 的 C 文件**。实测：在包外源里把返回值从 7
改成 8，`go test` 依然通过（缓存命中、静默沿用旧代码）；同样改动落在包内文件时
立即判红。对「逐步推进 C 化」这是致命的——改 C 源码却不生效且无任何报错。
（包内 shim `#include` 包外源同样漏跟踪，已实测排除。）

**实测证据**（每条都可复现）：

- `make build-linux-arm64` → **ELF 64-bit LSB executable, ARM aarch64**（82MB）
- `bash deploy/packaging/build.sh linux/arm64 homed` → **ELF aarch64**（79MB）
- 移走 `csrc/build/` 后，homed（cgo）与 waiter（CGO=0）**均能构建**
- 变异 C 源（`result = 131072` → `777`）后**同一缓存**下 `go test` **立即 FAIL**
  （改前：仍报 ok = 漏跟踪）
- `make check-codec-paths` 两条路径 OK；`make csrc-test` C 契约测试 100% 通过
- 全量 `go test -count=1 ./...` → **57 包：38 ok + 19 无测试 + 0 FAIL**

**遗留（不阻塞，已记入 `docs/zh/c-core/llm-orchestration-c.md` §七）**：
- 下一个切片选谁（L1 剩下的协议编解码，依赖 JSON 解析 ⇒ 先定 `ha_json.c` 复用还是新写）
- 符号链接是本仓**首例**（先例数=0）。已验证 git 往返保留（mode 120000），
  且 `core.symlinks=false` 降级时会**响亮报编译错**（非静默错误）；扩张前应有意识

---

### P0-2　§11.6 reload 语义仍在说谎

**现状**：`plugin_install` 依旧回 `reload_required`，而**不可热重载的插件
（Go `plugin.Open` 路径、`DF_1_NODELETE`）会被假装重载成功**。

**证据**：
- `internal/plugins/pluginmgr/plugin.go:528 / :548 / :755` —— 仍返回 `reload_required`
- 全仓库 `grep -rn "DF_1_NODELETE\|NODELETE" --include=*.go` **为空**（无检测）
- `internal/plugin/registry.go` 注释已自认：`Go plugin.Open 路径（dynamicPlugin）不可卸载，跳过`

**子任务**：
1. ELF 检测 `DF_1_NODELETE` → 标记插件「不可热重载」（`internal/plugin/dynamic_loader_unix.go`）
2. `ReloadOne` 对这类插件返回「需重启 homed」，停止假装成功
3. `plugin_install` 返回 `restart_required` 替代误导性的 `reload_required`

**验收**：
- [ ] 装一个 `DF_1_NODELETE` 插件后，调用方拿到 `restart_required`，不是 `reload_required`
- [ ] `ReloadOne` 在该插件上返回明确错误，且**摘除旧注册面**（工具/stage/output）仍完成，
      不留悬空闭包

---

### P0-3　§13.7 RuntimeManager + 分组 worker（唯一架构大件）

**现状**：`RuntimeManager` / `worker_group` / `WorkerGroup` / `RuntimeClient`
在全仓库**零引用** —— 该能力从未实现。当前是**一插件一子进程**
（`internal/plugin/proc/plugin.go:134` 的 `Spawn`）。

**为什么要做**：N 个外部插件 = N 个常驻进程 + N 份 transport，进程数随插件线性涨。
目标是一个 RuntimeManager + 少量 worker + 多插件共享 transport + 每插件独立
PluginContext（独立身份，共享管道）。

**证据**：
- `internal/plugin/proc/plugin.go:134`（`Spawn`，逐插件起进程）
- manifest 无 `worker_group` 键（`third_party/homeagent-sdk/example/a2a/plugin.json`
  的键集合为 `author/description/entry/name/name_en/name_zh/platforms/tags/version`）

**子任务**：
1. `RuntimeManager` 类型：worker 池 + 调度
2. `Worker` 类型：一个进程承载多个插件，共享 `RuntimeClient`
3. `PluginContext` 类型：每插件独立身份（能力门、ownerID、工具命名空间）挂在同一 transport
4. manifest 增 `worker_group`（缺省 = 全部归同一 worker，兼容迁移）
5. 高风险插件可声明独立 `worker_group`

**验收**：
- [ ] 缺省分组行为与现状逐字节等价（现有 proc 测试全绿，不改协议）
- [ ] 两个插件同 worker 时，各自 `ReclaimOwner` 只回收自己的 arena 块
- [ ] 一插件崩溃不带走同 worker 的另一插件（或明确：带走，并写进文档）
- [ ] manifest 无 `worker_group` 的旧插件可原样加载（向后兼容）

> 注：旧档称 `.pi/subagents/missions/` 里有一份针对本项的只读架构评审可作设计输入——
> **经核实该评审实际失败了**（`0475d2e9`：`EADDRINUSE 127.0.0.1:14010` 进程崩溃，
> `ok:false`、`output:""`，`exitCode:1`）。**没有可用结论**，本项要从零开始设计。

---

### P1-4　§13.2 arena 容量固定，无 Grow/Shrink

**现状**：内核独占的变长分配器已落地（first-fit + 邻块合并 + owner 校验），
但容量**固定 4MB**，用尽即调用失败，不退回内联。

**证据**：
- `internal/plugin/proc/arena.go:99`：`const arenaDefaultCapacity = 4 * 1024 * 1024`
- `arena.go:263 / :283`：超限直接报错

**为什么当时不做**：跨进程 `remap` 会让正在读的对端 SIGSEGV。所以这不是
「补个函数」，而是要先解决**对端可见的地址稳定性**。

**可选路径**（择一，需先定方案再动手）：
- a) 预映射大虚拟区间（未触碰页不占物理内存），容量「逻辑无限」
- b) 段表 + 多段拼接：新块分配在新段，不必 remap 旧段
- c) 维持固定容量，但**把超限错误做成可执行指引**（告诉插件怎么分片）

**验收**（按所选路径定）：
- [ ] 超过当前 4MB 的单次 payload 仍能送达，或收到带指引的明确错误
- [ ] 任何情况下对端不 SIGSEGV

---

### P1-5　§12.3 事件环：机制完成，零真实负载检验

**现状**：事件环（区内 segment + eventfd）与 `events.subscribe` 能力已完成，
但**没有一个真实外部插件订阅 `stage` / `tool_call` 事件**，`dropped` 在长跑下的
行为无人观察。

**证据**：
- `internal/plugin/proc/capability.go:149-150`（subscribe/unsubscribe 已授权）
- `internal/plugin/proc/corehandler.go:46-58`（`EvtRingSubscriber` 接口）
- `third_party/homeagent-sdk/example/` 下 21 个 example 中无事件订阅样例

**子任务**：
1. 写一个订阅 `stage` / `tool_call` 事件的 example 插件，跑真实负载
2. 长跑观察 `dropped` 是否异常增长（环 cap 溢出告警是否够用）

**验收**：
- [ ] example 插件能稳定收到事件并正确反序列化
- [ ] 压测下 `dropped` 有上界且可观测

---

### P1-6　§11.2 工具超时措辞误导 + browser 插件超时聚集

**现状**：内核侧工具超时文案仍写「已取消」，但**进程内 cgo 插件根本取消不了**
（OS 线程永久占用）——措辞与事实不符。

**证据**：
- `internal/agent/core/toolcall.go:42`：
  `fmt.Sprintf("工具 %s 执行超时（60秒），已取消", tc.Name)`
- `internal/plugin/proc/process.go` 注释自认 cgo 路径「超时后 OS 线程永久占用」

**子任务**：
1. 措辞改「已放弃等待（插件仍在后台运行，其占用的线程无法回收）」
2. 排查 `browser` 插件为何频繁 60s 超时（旧档记录 22/26 次集中于此）；
   真取消能力依赖子进程模型（与 P0-3 相关）

**验收**：
- [ ] 超时文案不再暗示「已取消」
- [ ] browser 超时率下降到可解释水平，或给出根因

> 与 P0-3 的关系：把内部 cgo 插件也迁到子进程后，这条的严重性自动消除。

---

### P1-7　WebUI `handleAgentAction` 直接 501

**现状**：WebUI 的 agent 操作接口**所有 action 一律返回 `501 Not Implemented`**，
是明确的未接线桩。

**证据**：
- `internal/plugins/webui/handler_agents.go:239`：
  `writeJSON(w, http.StatusNotImplemented, ..."action not implemented by supervisor")`
- 路由已注册（`handler.go:404` 的 `/api/v1/agents/`）
- **但 UI 未暴露入口**：`dashboard.js` / `cmd/gui/renderer/app.js` 均无对该端点的调用

**子任务**：决定该端点该做什么——接线到 supervisor 的真实动作
（stop/restart/snapshot），或**直接删掉路由**（UI 既然没用，留着只是待爆的债）。

**验收**：
- [ ] `/api/v1/agents/<id>/<action>` 要么真的能动作，要么不再存在（无 501 桩）

---

### P2-8　§12.4 Lua 仍走独立 ABI（进程内解释器）

**现状**：Lua 插件在**内核进程内**跑 gopher-lua，不走 proc 通道，是三套 ABI 里
唯一没收敛的。代价：Lua 插件崩溃 = 内核崩溃，且无共享内存数据面。

**证据**：
- `internal/plugin/lua_plugin.go:947`（`makeStageHandler`，进程内）
- `internal/plugin/lua_plugin.go:334 / :1270`（`register_stage` 直接注册到内核 SDK）

**子任务**：评估「Lua 走 proc 通道」的代价（解释器进程启动开销 vs 隔离收益），
据此决定收敛还是明确保留为独立 ABI 并写进文档。

**验收**：
- [ ] 有明确决策（收敛 / 永久保留），且文档与实际一致

---

## 三、已关闭 / 已实现（旧档误标或本轮更正，防复活）

逐条给出「旧档怎么说」与「实际怎样」。

### 1. §13.12 L3 原生多模态 —— 已实现
- 实际：`internal/memory/graph.go:169` 起建 `memory_blocks`
  （含 `modality/payload_digest/mime/vector/fingerprint/scene`）+
  `memory_block_edges`（`source_kind/target_kind/edge_type`），以及
  `scenes`/`scene_features`/`scene_refs`；`internal/agent/core/graphmedia.go`（310 行）
  实现 `migrateLegacyGraphMedia`/`attachBlocksToSentence`/`linkBlocksToDocument`/
  `commitTriplesWithMedia`；`graphmedia_test.go` 21 个测试。
- 结论：**关闭**

### 2. §13.9 llmsproxy 上下文溢出感知 —— 不属本仓（见 §五.1）

### 3. §13.10 AgentMail 三个 bug —— 不属本仓（见 §五.2）

### 4. §11.4 Lua stage 快照缺读锁（DATA RACE）—— 已修
- 实际：`internal/plugin/proc/shmcodec.go:42` 的 `WriteAll` 已在 `captureLocal`
  前后持 `sc.RLock()/RUnlock()`；`go test -race ./internal/lua/... ./internal/plugin/...` 全绿
- 结论：**关闭**

### 5. §12.2 `io.setToolBlocks` 内核侧是桩 —— 已实现
- 实际：`internal/plugin/proc/corehandler_inject.go:132` 实现
  `MethodIOSetToolBlocks`；模板 `putArena` → `blocks_ref`；
  `e2e_template_test.go:371` 用**真实 SDK 模板**验证
- 结论：**关闭**

### 6. §13.11 WebUI 修复清单 —— 主体已实现，仅剩 P1-7
- 逐项核实：`Last-Event-ID` 重放（`handler_chat.go:710`）、请求超时
  （`handler_chat.go:592` 等 300s）、XSS 消毒（`dashboard.js:252` DOMPurify）、
  `renderAll` 增量（`dashboard.js:34 / :452 / :1361` 增量游标 + 流式增量）、
  `handleKnowledge` 不再吞错（`handler_memory.go:122` 起逐分支返回错误）、
  CSS/DesignSystem（`dashboard.css:3` 起 sakura/frost 令牌）、
  GUI 重构（`cmd/gui/renderer/app.js`）—— 均已落地。
- 仅 `handleAgentAction` 501 是真缺口（已列 P1-7）

### 7. Windows 支持 —— **已设计性放弃**（旧档 P2-8 与 C 化 §2.3 的前提均据此更正）
- 旧档说：「Windows 桩已收敛，但缺真机验证」（把它当待办）
- 实际：`cmd/homed/platform_windows.go` 明确**原生 Windows 拒绝启动**并给 WSL2 指引。
  原因写入注释：插件体系依赖「继承的 fd」+「统一共享内存区的段内偏移解引用」，
  Windows 句柄模型无法表达；强适等于再维护一套平台专属 ABI（C ABI 时代三套 ABI
  并存曾致改写型插件静默失效）。
- 配套：`internal/plugin/proc/shmalloc_windows.go` 的 `allocShm` 直接报错不返回半可用段；
  `deploy/packaging/windows/install-via-wsl.ps1`（新）引导 WSL2 并复用 Linux 包；
  `build.sh` windows 目标**只构建 waiter + gui**，homed/initconfig 明确拒绝
  （见 `build.sh:257-267`）。
- 实测佐证：`GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/waiter` 成功
  （12MB .exe）；`homed` 无论如何都编不出 Windows（`internal/memory` 依赖 cgo-only 的
  `gojieba`）。
- 结论：**关闭**（该项不是待办；Windows 的正确验收 = WSL2 内按 Linux 路径跑，
  与 Linux 目标同一条流水线）

### 8. 仓库卫生：PTY 三例的 FAIL —— 环境相关 + skip 判据失效（旧档 P2-10）
- 旧档说：`go test ./...` 有 3 个 FAIL（PTY 三例 / 端口 9890 冲突 / `system_test` 写 `/etc`）
- 本轮实测（分时）时：
  - `go test -count=1 ./...` → **57 包：38 ok + 19 无测试文件 + 0 FAIL**
  - PTY 三例**本就有 skip 意图**（`integration_test.go:284/337/395` 的
    `t.Skipf("PTY not available: ...")`），且 `/dev/ptmx` 可用时正常通过（连跑 3 次均 ok）
  - **但该 skip 的判据是坏的**：它查 `resp["status"] == "error"`，而插件失败时返回的是
    `{"error": "创建终端失败: ..."}`（`internal/plugins/agentcli/plugin.go:496`）——
    **键名不匹配**，于是真遇到无 PTY 权限的环境会走到 `t.Fatalf` 而非 skip。
    这是「探测存在但失效」的典型：比没有探测更隐蔽
  - `TestRestoreFileFromBaseline` 在 `/etc` 可写时 **PASS**（`system_test.go:83` 确实
    写真实路径且不检查 err——**代码确实不干净**，但它不构成「稳定 FAIL」）
  - `9890` 端口**确有占用**（本机 homed 常驻监听），但测试用 `setupIntegration`
    起的实例未与之冲突（连跑 3 次均 ok）
- 结论：**旧档的记录在当时是真的**（环境退化：ptmx 无权限 + 端口被占），
  环境恢复后自然全绿。但**两个真缺陷存留**：① skip 判据键名不匹配（探测失效，
  退化时硬 FAIL）；② 测试依赖固定端口。两条已列 §六，不列为「待办功能项」。

---

## 四、生产部署后验证（代码已就绪，本就无法在仓库内完成）

这些**不是待开发项**，是「必须落到生产实例才能确认」的验收。仓库内有
`scripts/verify_deploy.sh [data_dir]` 可一键检查前两条。

- [ ] **§0.1 healthcheck 隔离**：部署后 `knowledge/`、`memory/graph.db`、
      `memory/documents/` 不再出现 `_hc_*` 残留
- [ ] **图记忆去重**：`relations` 重复率归零，跑一周不新增重复
- [ ] **§13.5 / §13.6 QQ 端到端**：真实 QQ 消息注入与输出经共享内存通道正常
      （小 payload 内联、大 payload 走 `text_ref`/`frame`）
- [ ] **§0.2 agentcli 不泛滥**：QQ 消息在 agentcli 无自喂送风暴时能被正常响应

---

## 五、跨项目工单（**不属本仓**，旧档误并入）

旧 plan.md 把别仓的工单写成本仓 TODO，导致「查无此代码却挂着未完成」。移出并说明归属：

### 1. §13.9「llmsproxy 上下文溢出感知」
- 旧档写：补 `OVERFLOW_PATTERNS`（"Context window is full"）、AUTO 截断宽度 `80→160`、
  `go test ./internal/ai/...`
- 事实：本仓**没有 `internal/ai/`**；相关符号在 **`/home/program/llmsproxy`**
  （`internal/gateway/chat.go`）。本仓 `internal/agent/api/provider.go` 只**消费**
  该网关（注释里提到 "llmsproxy 的 AUTO 链"，:361）
- 现状：该仓已把宽度改成 160
- 归属：**llmsproxy 仓**

### 2. §13.10「AgentMail 三个 bug」
- 旧档写：提示词修正 / `InReplyTo` / `relay_key ≤ 64 字节`
- 事实：AgentMail 是独立仓 **`/home/program/agentmail`**
  （`relay_key` 见 `server/internal/handler/permission.go`）。本仓
  `grep relay_key\|InReplyTo` **零命中**
- 归属：**agentmail 仓**

> 若这两仓也要纳入统一管理，应各自建 plan，不要塞进本仓文档。

---

## 六、明确「不做」与「建议修但不阻塞」

### 不做（防反复挂账）

- **§13.13 反向结果入共享内存**：本仓**不存在 `llm.chat`**（`llm.*` 只映射
  listSources/setSource/currentSource）；唯一可能返回大结果的 `doc.query` 被
  `CapDocMemory` 能力门挡着，且无外部插件使用。**不做**。
- **Windows 原生适配**：见 §三.7，**设计上不做**。

### 建议修但不阻塞（测试卫生，非当前 FAIL）

- `internal/system/system_test.go:83`：`target := "/etc/RestoreFileFromBaseline.test.tmp"`
  写真实系统路径，且两处 `os.WriteFile(...)` **不检查 err** → 在 `/etc` 不可写的
  环境里静默失败，报 `expected restore to happen`（根因是测试，不是实现）。
  建议改用 `t.TempDir()` + 保留 `IsProtectedPath` 语义所需的显式前缀，并检查每步 err。
- ✅ **（已修，2026-09-25，`17e7094`）固定端口冲突**：webui `:8080`、
  `pluginmgr` `127.0.0.1:9876`（曾是包级可变全局 `var HTTPAddr`，多实例互相踩）、
  `remotedevice` `127.0.0.1:9890`。
  修法：pluginmgr 改为实例字段；remotedevice 改显式 `net.Listen`（失败同步可见、
  `:0` 能回报真实端口）；测试经新增的 `ConfigRegistry.SetPluginConfig` 在插件
  加载前预置 `127.0.0.1:0`，三个插件各自绑 OS 分配的空闲端口。
  验证：`internal/plugins` 连跑 **30/30**（修前干净树 18/20）。
- ✅ **（已修，2026-09-25，`17e7094`）`TestRealPlugin_DeepSearchInvoke` 把上游限流当回归**：
  插件在上游限流时返回的是**正常结果**（err==nil，content 含「未返回结果」+ 无响应
  引擎列表），那是外部条件。现区分「上游不可用（限流/CAPTCHA）⇒ t.Skip 带理由」
  与「其他异常 ⇒ fail」，不再让噪声淹没真回归。
- **PTY 三例的 skip 判据是坏的（真缺陷，不只是卫生）**：`integration_test.go`
  284/337/395 查 `resp["status"] == "error"`，而 `terminal_create` 失败时返回
  `{"error": "创建终端失败: ..."}`（`internal/plugins/agentcli/plugin.go:496`）——
  键名不匹配 ⇒ 真遇到无 PTY 权限的环境**会 `t.Fatalf` 而非 skip**。
  同类型：其他依赖工具错误响应的环境探测（应统一认 `error` 键）。
- 同类审计：其他「写真实系统路径且吞错误」的测试。

---

## 七、C 化：已定事项与待拍板事项

### ✅ 已落地：C 基础设施门禁（2026-09-26，`8070844`）

在推进下一刀之前先把地基建起来 —— 没有门禁，每个 C 切片都在裸奔。

| 门禁 | 内容 | 为什么不能省 |
|---|---|---|
| `csrc-lint` | gcc+clang × `-Wall -Wextra -Wpedantic -Wshadow -Wconversion`，零告警才过 | C 侧没有 Go 的 vet 等价物，告警是**唯一**静态信号；`-Wconversion` 专门盯 cgo 窄化（`size_t→int` 截断默认静默） |
| `csrc-abi` | `ha_abi.h` 的 ABI 主/次版本 + 运行期自述 + Go 侧常量，三方交叉断言 | 「签名冻结」原本只写在注释里，注释不参与编译 |
| `csrc-headers` | 每个 `.h` 能单独编过 | 缺 include 时只在「恰好被别的头先包含」处静默编过 |
| `csrc-sanitize` | ASan + UBSan 跑契约测试 | C 侧内存错误/UBSan 默认静默（不崩、结果看着对），而零 malloc 设计依赖「无越界写」 |
| `csrc-cross` | arm64 交叉编译 | `go build` 不等于 C 代码在该架构上能编 |
| `csrc-fuzz` | libFuzzer：内存安全 + 6 条不变式 | 畸形 UTF-8 等价性正常输入永远测不到，只有随机字节覆盖得到 |

聚合入口 `make check-csrc`（已接进 `make test`）与 `make check-csrc-full`（含模糊测试）。

**地基上线第一小时就抓出 5 个真 bug，全是我自己写的基础设施代码** ——
这本身就是门禁有效的证据：
1. `_Static_assert` 是 C11，而项目 CFLAGS 是 `-std=c99`（`-Wpedantic` 报的）
2. C99 分支 `##msg` 拼接字符串字面量 ⇒ 两个断言共用一个 typedef 名（clang 报的）
3. bench 用 POSIX `clock_gettime`，而 CMake 刻意 `C_EXTENSIONS OFF` ⇒ 未声明
4. 头文件缺 include 的静默通过
5. **Go 不允许在 `_test.go` 里用 cgo**，且 cgo 生成的 `*_Cvar_*` 不是 Go 常量

**验证**（全部当场可复现）：libFuzzer 91s / **3,329,316 次运行 / 零崩溃**；
变异测试证明门禁不摆设（改 C 宏、或让 Go 常量与 C 函数「一起错成一样」均被判红）；
双编译器 × c99/c11 零告警；ASan+UBSan PASS；arm64 交叉编译 0 告警；
全量 `go test -count=1 ./...` **57 包 0 FAIL**；`make build-linux-arm64` → ELF aarch64。

**首次把两类成本分开测**（这是基础设施的核心价值之一）：
C 侧纯函数基准给出的**函数体成本**与 Go 侧基准里的 **cgo 边界成本（~30ns）** 从此可分离 ——
否则看到某场景慢，根本分不清该优化 C 函数体、还是该减少跨语言调用次数。

| 场景 | C 函数体（纯 C，无边界） |
|---|---:|
| `estimate_tokens` ascii_1k | **121.7 ns / 8.4 GB/s** |
| `estimate_tokens` zh_1k | 1434 ns / 0.71 GB/s |
| `truncate_by_tokens` ascii_1k | 134 ns |
| `truncate_by_tokens` zh_1k | 128 ns |

注：`estimate_tokens` 的**中文与 ASCII 差距 11.8×**（与 Go 侧 bench 注释「zh_1k 3097ns」的
量级关系一致）。这是「中文字节校验」的固有成本，不是缺陷；但它是下一个切片的输入 ——
若某热路径以中文为主，优化点在这里，不在 cgo 边界。

### ★ 下一刀的决定：协议编解码层（实测支撑，不再是「哪个看起来底层」）

**为什么是它**：SSE 流式分块解析 `parseOpenAICompatibleStreamChunkFull` 是
**每个流式 chunk 都要跑一次**的最热路径，而它当前每次付费 12–21 次堆分配：

| 输入（真实负载形状） | ns/op | allocs/op |
|---|---:|---:|
| content 块（含中文） | 1937 | 13 |
| toolcall 块 | **3122** | **21** |
| usage 块 | 2464 | 12 |
| 纯字节扫描理论下限 | **133** | 1 |

差距 **15–23×**。会话 1 万块 ⇒ 1–2 万次分配，正是 GC 抖动的来源（C 化的原始动机）。

### ★ `ha_json.c`：**不可直接复用**（原判断「仓库里已有现成实现」已被实测推翻）

SDK 的 `remotedevice/src/ha_json.c`（368 行）经实测有**三个对协议层致命的语义缺陷**：

| # | 缺陷 | 实测 |
|---|---|---|
| 1 | **没有 `\u` 解码** | `{"k":"\u4f60\u597d\ud83d\ude00"}` → `?0?d?d?0`（期望 `你好😀`） |
| 2 | 只有 `ha_json_get_int`，无浮点 | `temperature:0.7` → `0`（注意：`0.7` 的 `int_val` 被**静默**置 0，不是解析失败） |
| 3 | `null` 与「键缺失」不可区分 | 二者都返回 `NULL` / `def` |

外加架构冲突：它是 **DOM + malloc**，与 ha_codec 的「不 malloc / 零拷贝 / 纯函数」约束正交；
在热路径用它等于重建刚测出的「每 chunk 十几次分配」。它的定位是
**remotedevice 设备通道的 JSON**（`ha_json_parse` 注释显示 `\u` 一律写 `'?'`），
不是 LLM 协议层。

**建议方案：`csrc/` 新建专用 `ha_jsonscan.c`（零分配、span 返回、增量）** ——
对齐 ha_codec.h 的既有三条设计约束（只吃指针+长度 / 不 malloc / 无状态纯函数）。

不复用 SDK 版的四条理由：
1. 复用 = 跨仓改动 → SDK 发版 → 两仓版本对齐（违反「双仓绑定」纪律）；
2. 把内核 LLM 协议层耦合到 SDK 的 remotedevice 设备通道库（架构错位）；
3. `\u`/浮点/null 三缺陷必须补，补完已等同于重写；
4. DOM + malloc 与既定约束冲突。

**不建议把内核热路径依赖另一个仓** —— 这是本项的核心理由。

### ✅ 三项已裁决（2026-09-26，jianf）

1. **C 实现放哪里 → 留在主仓 `csrc/`。** 用户澄清：C 实现本来就是**替换**内核的
   Go 实现（`csrc/` 是唯一权威源，Go 侧符号链接 + cgo 绑定，`codec_pure.go`
   已降级为黄金对照的规格基准）。SDK 从头到尾**未被触碰**
   （`git diff main -- third_party/homeagent-sdk/sdk/` = 0 行）。
   所谓「进 SDK」只对**跨端复用**（鸿蒙/嵌入式/C SDK 也想调同一份实现）才有意义，
   而它们并不调用内核编解码层 ⇒ 该问题**关闭**，不作为待办。
2. **`ha_json.c` 复用还是新写 → 新写。** 见上（三个致命缺陷 + 架构正交）。
3. **下一刀选谁 → 协议编解码层，立即开工。** SSE 单块解析实测 1.9–3.1µs / 12–21 allocs。



第一刀（L1 纯函数层）已落地并闭环（见 §二 P0-1 与
`docs/zh/c-core/llm-orchestration-c.md`）。基础设施门禁已建成（见上）。
下阶段扩大前，**三处需 jianf 拍板**（建议已附，见上）：

1. **C 实现放主仓 `csrc/` 还是 SDK `third_party/homeagent-sdk/`？**
   - 当前已在主仓 `csrc/`（`ha_codec.{c,h}` 是权威源，Go 侧符号链接过去）
   - 若 SDK / 鸿蒙 / C SDK 侧也要复用，需定同步机制；搬进 SDK 则要走 SDK 冻结流程
2. **`ha_json.c` 复用还是新写？**（复用会动 SDK 目录结构）
   - 这是下一个切片的**前置**：L1 剩下的协议编解码（`parseOpenAICompatible*`、
     `normalize*ToolCalls`）全部依赖 JSON 解析，不定就推不下去
3. **下一个切片选谁？**（已有基准数据支撑，见下）

### ★ 已定：编解码层「完全 C 化」（2026-09-25，jianf 裁定）

初版基准一度得出「C 比 Go 慢」，**该结论已被推翻** —— 根因是我的 Go 绑定与 C 实现
写得烂（每次调用 `CString` malloc+拷贝 + C 侧 `strlen` + `lower_dup` malloc +
逐字节扫描），把自找的 82% 开销误当成了 cgo 的固有成本。拆解实测：

| 场景 | ns/op |
|---|---:|
| cgo 边界（零拷贝 + 空函数体）| **31.9** ← cgo 真实固有成本 |
| + `C.CString` + `strlen` | 105–111（多出 ~75ns）|
| 初版 `ModelContextWindow` | 175 |

优化后（零拷贝传指针+长度、栈缓冲折叠、字级 ASCII 检测、位运算 UTF-8 校验、
截断返回字节数、提前短路；纯 Go 侧也去掉 `[]rune` 分配）：

| 基准 | 初版 C | 优化后 C | 纯 Go | 提升 |
|---|---:|---:|---:|---:|
| `ModelContextWindow` | 175 | **76.5** | 46.8 | 2.3× |
| `EstimateTokens` / 1KB ASCII | 2318 | **80.8** | 326 | **28.7×** |
| `TruncateByTokens` / 1KB ASCII | 2594 | **71.7** | 411 | **36×** |
| `TruncateByTokens` / 1KB 中文 | 3923 | **70.1** | 3097 | **56×** |

**裁定：完全 C 化，不做按长度分派。** 我一度加了「短串走回 Go」的分派，
被否决 —— 那会同时存在两份语义可能分叉的实现。代价如实记录：
`EstimateTokens("qq")` 这类极短串上 C 约 47ns vs Go 约 3ns（几乎全是边界成本），
绝对值纳秒级可忽略；若某循环对极短串高频调用，**正确应对是 C 化那个循环
（批量传一次）**，而不是退回 Go（见下条）。

**结论性变化**：
- C 侧新增**与 Go `utf8.DecodeRuneInString` 等价的完整校验**（含过长编码、
  代理对、超 U+10FFFF、截断序列）。这使中文密集输入比初版慢（1467 vs 840），
  但初版对畸形序列会与 Go **分叉**（rune 计数偏差 ⇒ token 预算/截断点偏移，
  只影响计数不会崩，不测发现不了）。正确性优先，且仍比纯 Go 快 2×。
  由 `TestGolden_InvalidUTF8`（3000 组随机字节）钉死。
- 包**要求 cgo 才能编译**（删除了 `!cgo` 回退）：CGO_ENABLED=0 下整包构建失败。
  理由：不许存在第二条可能分叉的实现路径；且 `waiter`/`initconfig`/`memgc`/`mock-server`
  实测均**不依赖**本包，无 CI 在 CGO_ENABLED=0 下构建它 ⇒ 不影响任何现有构建。
  Makefile 的 `check-codec-cgo-only` 把「不许有第二条路」变成可执行断言。

**下一个切片的判据**（已从「哪个看起来底层」换成「哪个在真实分布下真能变快」）：
协议编解码（`parseOpenAICompatible*` / `normalize*ToolCalls` / SSE 分片）
处理的正是**长文本**，是 C 的主场，比「把短函数搬过去」更合理。
但其前置是 JSON 解析 ⇒ 先定 `ha_json.c` 复用还是新写。

### 已定事项（不再挂账）

- **不保留回退路径**：编解码层**要求 cgo 才能编译**（无 `!cgo` 文件）。
  CGO_ENABLED=0 下整包构建失败——这是有意的响亮失败，不是缺漏。

  **为什么不做回退**：回退会让两份语义可能分叉的实现同时在产线跑。
  C 侧对畸形 UTF-8 的解码边界一旦与 Go 分叉，只表现为 rune 计数偏差
  （⇒ token 预算与截断点偏移），**不会崩、不会报错**，最难发现。
  只验证过一条路，就不该存在第二条。

  **为什么这不影响任何构建**（实测，别当成风险）：
  - `go list -deps` 实测**只有 `cmd/homed` 依赖 `internal/agent/api`**；
    `waiter` / `initconfig` / `memgc` / `mock-server` 均**不依赖**（逐个验过）。
  - homed 本就强制 cgo（mattn/go-sqlite3 + gojieba）⇒ 恒走 C 路径。
  - 仓库**无 CI**（无 .github/workflows），发布脚本仅在构建 `waiter` 时用
    CGO_ENABLED=0，而 waiter 不依赖本包。
  - `make check-codec-cgo-only` 把「不许有第二条路」变成可执行断言
    （断言 cgo 下全绿 **且** CGO_ENABLED=0 下必须失败）。
- **纯 Go 实现的定位**：`codec_pure.go` 不带 build tag、永远编译，
  但**不是生产路径**——它只作**黄金对照的规格基准**与可读规格。
  （它本身也已零分配化：去掉 `[]rune` 的 4×len 临时分配。）
- **不链接预构建 `.a`，也不用包外 `#include`**（前者架构错 + 产物两头空，
  后者缓存漏跟踪）。用包内符号链接，理由与实测见 §二 P0-1 与设计文档 §2.4。


---

*重写时间：2026-09-25　·　核实方式：源码 grep / go build / go test -count=1 /
交叉编译对照试验 / 双构建试验 / 变异测试*
*旧档备份：`/tmp/plan.md.bak-20260925-124700`（上一版）、
`/tmp/plan.md.bak-20260924-171420`（历史 1474 行混合档）*
