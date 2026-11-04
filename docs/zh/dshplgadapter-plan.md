# dshplgadapter 插件项目规划书

> **状态**：规划（未开始实现）
>
> **定位**：把 DeepSeek Harness（下称 dsh）的 Cordis 插件生态接入 HologramAgent。
> 参考原型就是本仓的 `internal/plugins/clawhubadapter/` —— 它把 OpenClaw 生态
> 接入 HA，dshplgadapter 把 dsh 生态接入 HA，两者是**同一个模式的两次实例化**。
>
> **建立于**：2026-11-04。
>
> 本文所有事实均来自实测命令，不凭记忆。关键实测附命令原文，便于复查。

---

## 〇、两个前置结论（先钉死，否则后面全是空谈）

### 结论 1：clawhubadapter 的设计可以整套照搬

它解决的问题与 dshplgadapter 的**完全同构**：

| 共同点 | clawhubadapter（OpenClaw） | dshplgadapter（dsh） |
| --- | --- | --- |
| 外部原型 | 一插件一目录，`main.js` 导出 `{register(api)}` | 一插件一包，Node ESM 导出 `apply(ctx)` |
| 插件格式 | JSON 清单 + JS 入口 | `package.json` + `cordis.patch.yml` |
| 运行需要 | Node 宿主 | Cordis 宿主（Node） |
| 拿到什么 | 工具、通道、Provider、阶段钩子 | 工具（`ctx.tools`）、事件（`ctx.on`）、服务 |
| 内核怎么办 | 起 sidecar 子进程 + JSON-RPC over stdio | **完全相同** |

所以**不需要重新设计架构**。要做的只是换掉三样东西：
① 子进程里的"假宿主"（simulator/main.js → 一个 Cordis Context）；
② 注册通知的类型表（OpenClaw 的 `type` → Cordis 的注册面）；
③ 包发现与安装逻辑（`main.js`/`openclaw.plugin.json` → `package.json`+cordis.patch）。

### 结论 2：它**必须是外部插件**（`plugin.bin`），不是内置插件

> **本节 2026-11-04 重写过一次。** 原结论是“必须内置”，依据是“外部插件不能发
> 事件、所以移交 skill 只能靠概率”。该依据**被实测推翻**：外部插件自带的 skill
> 由 **pluginmgr 在安装/启动时自己移交**，与插件发不发事件无关。
>
> 而 clawhubadapter 已经实打实迁成了外部插件并端到端跑通（见 §0.3），
> 所以 dshplgadapter 同形态。

#### 2.1 外部插件确实发不了事件（这条仍然成立，且已实测）

三层证据：

1. `capability_test.go:279` 把 `events.publish` 列进 **`forbiddenPrefixes`** ——
   内核**刻意**不向外部插件提供事件发布，且有测试守着不许加回去。
2. proc 协议表只有 `MethodEventsSubscribe` / `MethodEventsUnsubscribe`，
   **没有** `events.publish`（`internal/plugin/proc/protocol.go:235-236`）。
3. 外部插件模板 `proc_main.go.tmpl` 不注入 event subscriber
   ⇒ `Events()` 恒为 `nil`（`builtin-only.md:25` 权威裁定）。
4. 更早一道：公开的 `EventSubscriber` 接口**没有 `Publish` 方法**，
   所以这是**编译期**就拦住（实测变异：把 `s.Events().Publish` 加回去，
   `hmapdev build` 直接报 `s.Events().Publish undefined`）。

#### 2.2 但“发不了事件”**不阻碍**外部化 —— 因为不用发

实测得到的真实链路（隔离实例，2026-11-04）：

```text
15:06:27 [pluginmgr] clawhubadapter: handed off skill clawhub-tools to skillmgr
15:06:27 [pluginmgr] startup rescan: re-handed off 1 bundled skill(s)
15:06:27 [skillmgr] registered via skill_detected event: clawhub-tools
15:06:27 [plugin] loaded SKILL: clawhub-tools v1.0.0
```

注意这条链上**没有一行来自插件自己**：

- 插件只把 SKILL 放在工程根 `skill/<名字>/SKILL.md`，`hmapdev build` 打进包；
- 内核/pluginmgr 在**安装**与**每次启动**时扫 `<插件目录>/skill/`，
  自己发 `skill_detected`（`pluginmgr/skill_handout.go`）；
- 内置插件走另一条路 `plugin/builtin_skill.go` 的 `SkillProvider`（编译期 embed）。

⇒ **“插件带 skill”与“插件发事件”是两件不相干的事**。前者是分发物约定，
后者是运行期能力。外部插件做前者完全没问题。

#### 2.3 迁移实测：三处差异，全部可绕

把 `internal/plugins/clawhubadapter/` 搬到 `hmapdev` 骨架上，只改 import，
`hmapdev build` 报四个错：

```text
./plugin.go:1103: too many arguments in call to s.InjectInputSync
        have (string, string, string, map[string]interface{})
        want (string, string, string)
./plugin.go:1106: invalid operation: out != nil (mismatched types string and untyped nil)
./plugin.go:1107: out.Payload undefined (type string has no field or method Payload)
./plugin.go:1323: s.Events().Publish undefined (type sdk.EventSubscriber has no field or method Publish)
```

| 差异 | 修法 | 代价 |
| --- | --- | --- |
| `InjectInputSync` 签名不同 | 用外部签名，返回 `string`，空串即“没回复” | 改一处 |
| `Events().Publish` 不存在 | **删掉，不需要** | 见 2.2 |
| `internal/safeclose` 拿不到 | 自写 `safeClose[T any]` | 新文件 40 行 |
| `init()` 自注册不可用 | 改为 `NewPluginFactory` | 改一处 |

#### 2.4 迁移结果（已验证）

```text
go vet / gofmt                          干净
go test -race                           ok（20 条原有 + 4 条外部形态判据）
hmapdev build                           packaged clawhubadapter_bundle.hmap
隔离实例端到端                           loaded: clawhubadapter + 10 个工具全注册
                                      loaded SKILL: clawhub-tools v1.0.0
```

> **决策**：dshplgadapter 做成**外部插件**，与迁移后的 clawhubadapter 同形态。
> 工程放独立目录 + `plg.json`，用 `hmapdev build` 产出 `.hmap`。

### 结论 3：外部插件带 skill 走 `skill/` 目录，不走事件

把这条单列，因为它是上面推翻结论 2 的关键事实，而且很容易搞混：

| 谁带 skill | 放在哪 | 谁负责登记 |
| --- | --- | --- |
| 外部插件 | 工程根 `skill/<名字>/SKILL.md`（打进 .hmap） | **pluginmgr** 安装时 + 每次启动重扫 |
| 内置插件 | Go 源码 embed + 实现 `SkillProvider` | **内核** `InstallBuiltinSkill`（随插件启停出入） |
| 用户自己写 | `<data_dir>/skills/` | skillmgr 启动全扫 |

三者的共同终点都是 skillmgr 的 `loadOne`（幂等），但**入口完全不同**。
写外部插件时只需要第一种。

一条实操纪律（`hmapdev build` 会检查）：**`auto_start=false` 的插件必须带
`skill/` 目录**，否则用户装完不知道该先 `plugin_enable` —— 等价于没装。
clawhubadapter 是 `auto_start` 默认 true，所以不强制；但带上 skill 仍是对的。

---

## 一、参考原型：clawhubadapter 的实际结构

先看清楚要抄什么。它的四个文件是分层的，**每层职责单一**：

| 文件 | 行数 | 职责 | dshplgadapter 是否照搬 |
| --- | --- | --- | --- |
| `plugin.go` | 1526 | 生命周期 + 工具注册 + 包管理 | 照搬骨架，换包发现逻辑 |
| `sidecar.go` | 420 | **JSON-RPC over stdio + pending-id 配对** | **原样照搬** |
| `registry.go` | 326 | 注册通知 → SDK 调用的**转译表** | 换转译表内容 |
| `simulator/main.js` | 422 | 子进程里的**假宿主** | **换成 Cordis Context** |

### 1.1 `sidecar.go` 是最有价值的部分（原样照搬）

它解决了"跨进程调用"的全部脏活，与具体插件格式**无关**：

```text
sidecarRequest / sidecarResponse       JSON-RPC 帧
pendingResp                            每请求一个 channel + 自己的 close 守卫
readLoop                               读 stdout，按 id 派发响应，其余当通知
call(method, params)                   发请求并等响应
NotifyChan()                           把主动通知推给上层
waitReady()                            ping 探活，30s 超时
Close()                                优雅关停
```

**它踩过的坑（照搬时不要重犯）**：

- `pendingResp` 的 `once` **必须每个请求一个**，不能共用全局 `Once` ——
  `close(ch)` 二次即 panic，而 `Close()` 有多条路径会走。
- `waitReady` 超时从 5s 提到 30s（`2915006`）：慢机器上 Node 冷启动会超时。
  注释里写了取法依据：比 systemd `TimeoutStopSec`(25s) 宽，与用户可感知的
  "卡住"阈值(约 30s)持平。
- 读循环里 stdout 只能有 JSON，**任何 `console.log` 都会坏帧**。
  这一条 dsh 侧尤其危险 —— 见 §2.3。

### 1.2 `registry.go` 的转译表（照抄结构，换内容）

它把"子进程发来的注册通知"分派成 SDK 调用：

```go
func (d *RegistryDispatcher) Dispatch(typeStr string, data json.RawMessage, ...) {
    switch typeStr {
    case "tool":     d.toolReg.Dispatch(...)      // → s.RegisterTool
    case "channel":  d.channelReg.Dispatch(...)   // → s.RegisterInputChannel
    case "provider", ...: d.providerReg.Dispatch(...)
    case "hook", "lifecycle", ...: d.stageReg.Dispatch(...)  // → s.RegisterStage
    default: d.capRecorder.Record(...)            // 未知类型：只记账
    }
}
```

**注意 `default` 分支**：未知注册类型不报错，只记进 `CapRecorder`。
这是刻意的 —— 外部生态的注册面比我们的多，硬拒绝会让插件直接不可用。
dsh 侧注册类型更多（见 §3.2），**必须保留这个宽容策略**，否则第一天就崩。

### 1.3 `simulator/main.js` 是"假宿主"（换成 Cordis）

它做的事情本质上是**骗过插件**，让插件以为自己跑在真宿主里：

```js
const entry = pluginEntry.default || pluginEntry;
if (typeof entry !== 'object' || typeof entry.register !== 'function') {
    process.stderr.write(`[simulator] plugin entry must export {default: {register(api)}}\n`);
    process.exit(1);
}
const api = {
    id, name, version, description, source, rootDir, config, pluginConfig,
    logger: { debug, info, warn, error },
    resolvePath, registerTool, registerProvider, registerChannel, ...
};
entry.register(api);
```

dsh 这一层**不需要手写假宿主**，因为 Cordis 的 `Context` 可以无参构造：

```bash
$ grep "export declare class Context" @deepseek-ai/cordis/lib/types/context.d.ts
40:export declare class Context {
60:    constructor();
```

这是个关键利好：**dsh 的宿主是真的能跑起来的**，不用伪造。
但代价是初始化更重（Cordis 有自己的 loader / fiber / service 体系），
需要实测哪些服务必须注册、哪些可以缺失。

---

## 二、dsh 侧的事实（全部实测，2026-11-04 本机）

### 2.1 版本与包布局

```bash
$ python3 -c "import json;print(json.load(open('/usr/lib/node_modules/@deepseek-ai/dsh/package.json'))['version'])"
0.2.1-rc.2
```

- `@deepseek-ai/cordis` **4.0.5** —— 插件框架
- `@deepseek-ai/dsh-*` 约 200 个包（内核按 plugin 拆得极细）
- `@deepseek-ai/dsh-tools` 提供 `defineTool()`

### 2.2 插件形态：包 + cordis.patch.yml

一个真实外部插件（`dsh-mobile-nav`）：

```jsonc
// package.json
{
  "name": "@dsh-external/dsh-mobile-nav",
  "type": "module",
  "main": "lib/index.js",
  "exports": { ".": {...}, "./client": {...}, "./cordis.patch.yml": "./cordis.patch.yml" },
  "dsh": { "client": ... }          // ← 客户端半边的声明
}
```

```yaml
# cordis.patch.yml
- insert:
    - id: dsh-mobile-nav
      name: '@dsh-external/dsh-mobile-nav'
```

```js
// lib/index.js —— 主入口
export function apply(ctx) {
    ctx.effect(() => installResponseCompression(), 'dsh-mobile-nav: response compression');
}
```

**装载链路**（实测 profile 配置）：

```bash
/root/.dsh/profiles/web/package.json
  ├─ dependencies:      @dsh-external/xxx, dsh-mail-bridge, ...
  └─ dsh.profile.bundles: [ "@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app", ... ]

/root/.dsh/profiles/web/cordis.yml      → 空数组（root，由 patch 组合）
/root/.dsh/profiles/web/cordis.patch.yml → 覆盖层
/root/.dsh/profiles/web/node_modules/@dsh-external/*  → 软链到真实目录
```

⇒ **dshplgadapter 的"扫描目标"就是 `<profile>/node_modules/`**
（含 `@dsh-external/`、`@scope/`、无 scope 的），加上 `dsh.profile.bundles` 清单。

### 2.3 ★ dsh 插件与 clawhubadapter 最大的不同：日志会污染协议通道

这是**必须提前设计**的风险，不是实现细节。

clawhubadapter 的 OpenClaw 模拟器把 `logger` 接到 stderr：

```js
logger: { debug: () => {}, info: () => {}, warn: () => {},
          error: (...a) => process.stderr.write(`[plugin] ${a.join(' ')}\n`) }
```

而 dsh 生态的插件**大量使用 `console.log`**，且 dsh 自己的 logger 语义就是
"写到进程输出"。一旦子进程 stdout 被日志污染：

```text
内核 readLoop 收到非 JSON 文本 ⇒ 报「收到非法 JSON 帧」并丢弃
⇒ 该行日志丢了，还多一条看起来像协议故障的报错
```

本仓已经为这个坑付过代价（`hologram-plugin-dev` skill 记载：生产累计 1038 条
这类报错，定位真因花了一整轮排查）。

**设计方案：给协议单独一条通道，不让它和 stdout 抢。**

三条路，优先级从高到低：

| 方案 | 做法 | 代价 |
| --- | --- | --- |
| **A（推荐）** | 子进程用 **fd 3** 做协议通道，stdout/stderr 全部当日志 | 要确认 Node 侧能拿 fd 3（`fs.writeSync(3)` 可以做） |
| B | 起子进程时把 stdout 重定向到文件，协议走另一条 | 需要 sidecar 支持独立管道 |
| C | 不改协议，但在 simulator 里**劫持** `console.log` 指到 stderr | 只能拦住模拟器内的调用，插件用 `process.stdout.write` 仍然漏 |

**A 的可行性要先验证**（这是阶段 0 的第一件事）：
`node -e "require('fs').writeSync(3,'x')"` 在 Go 侧 `ExtraFiles` 注入 fd 3 后是否可用。

### 2.4 dsh 插件的注册面（要转译的东西）

从 `dsh-mail-bridge`（**最重要的参照物**，2586 行，已生产运行）实测：

```js
export const inject = ['agents', 'tools'];   // ← Cordis 要求声明依赖服务
export const name = 'dsh-mail-bridge';
export function apply(ctx, config) { ... }
```

它用到的 ctx 面：

| API | 用途 | dshplgadapter 转译成 |
| --- | --- | --- |
| `ctx.tools.register(defineTool({...}))` | 注册工具 | `s.RegisterTool` |
| `ctx.on('session/event', fn)` | 会话事件 | 阶段钩子 / 事件订阅 |
| `ctx.on('agent/status', fn)` | agent 状态变化 | 阶段钩子 |
| `ctx.on('approval/request', fn, next)` | 权限请求（**有 next 链**） | 需确认 HA 侧对应物 |
| `ctx.on('tools/execute', fn, next)` | 工具执行前后（**中间件链**） | `before_toolcall` / `after_toolcall` |
| `ctx.on('tools/post-execute', fn)` | 工具执行后 | `after_toolcall` |
| `ctx.effect(disposer, label)` | 注册清理函数 | 插件 Stop 时调用 |
| `ctx.agents.create/get/list/resume` | 建/查 agent | 需确认是否暴露 |
| `ctx.logger.info/warn/error` | 日志 | 转 stderr / 内核日志 |
| `ctx.sessionQuery` | 会话查询 | 可选 |
| `ctx.userQuestions` | 向用户提问 | 可选 |

**注意 `ctx.on(..., next)` 的中间件链**：dsh 的 `approval/request` 和
`tools/execute` 都是 `(payload, next) => Promise` 形态，插件可以拦截并决定
是否放行。这与 HA 的 `before_toolcall`（能拒绝/改参）语义接近，但**不是一一对应** ——
dsh 的链是异步 promise 链，可以有多个插件串联；HA 的阶段钩子是同步返回。
⇒ 转译时**不能在链中间阻塞等 HA 的异步判定**，否则会死锁。需要设计成：
"先收集所有插件的判定，再一次性提交给 HA"（类似 §1.2 的记账策略）。

### 2.5 已存在的同类先例（务必读）

`/opt/agentmail/plugins/dsh-mail-bridge/` —— 2586 行 TypeScript，
生产运行中，做的是 **dsh 插件 ↔ AgentMail 平台桥接**。

它不解决"dsh 插件进 HA"，但它解决了**"如何在 dsh 插件里正确使用 ctx 面"**：

- `inject = ['agents','tools']` 的声明方式
- `defineTool` 的正确用法（含一个历史坑：参数 spec 必须转成合法 JSON Schema）
- `ctx.on` 的五个事件、`ctx.effect` 的清理约定
- **`ctx.logger` 不进 journalctl**，日志要用 `console.error`（它踩过，
  注释里写得很清楚）

⇒ 它的 `src/index.ts` 是 dsh 插件侧 API 的**最可靠文档**，比 dsh 官方文档更实在。

---

## 三、设计

### 3.1 总体形态（与 clawhubadapter 逐层对照）

```text
                    HologramAgent 内核（homed）
                             │
                  dshplgadapter（外部插件，plugin.bin 子进程）
                             │
        ┌────────────────────┴────────────────────┐
        │                                          │
   发现与安装                                   运行期
   （扫描 profile）                        （每 dsh 插件一个 sidecar）
        │                                          │
   读 package.json                         fork node <simulator>
   读 cordis.patch.yml                       ↕ JSON-RPC（fd 3）
   判断是否有 apply()                      Cordis Context + 插件
        │                                          │
        └──── 纯 SKILL 条目 → skillmgr ←──────────┘
                （同 clawhubadapter 的职责链）
```

### 3.2 注册转译表（dsh → HA）

| dsh 侧的注册 | HA 侧的落点 | 备注 |
| --- | --- | --- |
| `ctx.tools.register(defineTool(...))` | `s.RegisterTool` | 参数 schema 要转 JSON Schema |
| `ctx.on('tools/execute', fn, next)` | `StageBeforeToolcall` + `StageAfterToolcall` | 见 §2.4 的异步链注意 |
| `ctx.on('approval/request', fn, next)` | 待定（HA 无直接对应） | 阶段 3 再定 |
| `ctx.on('session/event', fn)` | `Subscribe`（只读） | |
| `ctx.on('agent/status', fn)` | 阶段钩子 | |
| `ctx.effect(fn, label)` | 记下来，`Stop()` 时逆序调用 | |
| `ctx.logger.*` | 转 stderr（不要走 stdout） | |
| 其他未知 | 记进 `CapRecorder`，不报错 | 同 clawhubadapter 的 default 分支 |

### 3.3 包发现规则

扫描 `<profile>/node_modules/`，一个目录算"dsh 插件"当且仅当：

1. `package.json` 能解析，且
2. （`package.json` 有 `dsh` 字段）**或**（存在 `cordis.patch.yml`）**或**
   （入口 `lib/index.js` / `main` 里能 static-scan 到 `export function apply`）

第 3 条**不做静态扫描**，改为"试着加载，加载失败就当普通包跳过" ——
理由：ESM 的静态分析在正则层面不可靠，容易把非插件误判成插件（或反之）。
加载失败必须**只打日志、不中断扫描**（同 clawhubadapter `Start` 里 `continue` 的处理）。

**纯 SKILL 条目**（有 `SKILL.md` 但无 `apply`）→ 走 `publishSkillDetected`
交给 skillmgr，与 clawhubadapter 完全一致。

### 3.4 安全（把 clawhubadapter 刚修的两个洞从一开始就堵上）

本仓 2026-11-04 刚修了 clawhubadapter 的两个真实漏洞，**dshplgadapter 必须
从一开始就带**，不要重犯：

| 漏洞 | 教训 | dshplgadapter 的做法 |
| --- | --- | --- |
| ZIP Slip（解压任意路径写） | `filepath.Join(dest, entry)` 不校验 | 复用 `safeArchiveTarget` 思路（可考虑提到公共包） |
| 路径穿越（`name=".."` 删数据目录） | 名字来自模型，拼路径前必须校验 | 包名一律走 `resolveUnderRoot` |
| 无校验和的远程安装 | 下载即执行 | 阶段 1 **只支持本地已安装包**；远程安装推迟到阶段 4 并带 sha256 |

**一条额外的、dsh 特有的风险**：dsh 插件是 **Node 代码**，`apply()` 里可以
`import('node:child_process')` 直接执行任意命令。这与 HA 的沙箱模型直接冲突
（沙箱目前唯一真实生效的是 files 插件自己的 `pathWithinSandbox`，见
`plan.md` 的记录）。⇒ **阶段 1 必须先做"要不要允许 dsh 插件跑任意代码"的决策**，
不能默认允许后再补救。

---

## 四、分阶段实施

每个阶段独立可交付、独立可验证。**不合并交付**。

### 阶段 0：可行性探针（不改产品代码）

目的：把三个"不确定"变成"确定"。**任何一条失败都要回来改设计**。

| # | 探针 | 判据 |
| --- | --- | --- |
| 0.1 | fd 3 做协议通道 | Go `ExtraFiles` 注入 + Node `fs.writeSync(3)` 能双向通信 |
| 0.2 | 裸 `new Context()` 能承载插件 | 加载 `dsh-mobile-nav` 的 `apply` 不报错 |
| 0.3 | `defineTool` 的 schema 能转成 HA 的 JSON Schema | 拿 `dsh-mail-bridge` 的一个真实工具做往返 |
| 0.4 | `inject: ['agents','tools']` 缺服务时的行为 | 确认是"报错退出"还是"降级"，决定要不要伪造服务 |

产物：`docs/zh/dshplgadapter-probe.md`（实测记录 + 结论）。

### 阶段 1：最小闭环（只支持工具）

- `internal/plugins/dshplgadapter/` 骨架（抄 clawhubadapter 的 4 文件结构）
- 扫描 `node_modules/`，找到插件，起 sidecar
- **只转译 `ctx.tools.register`** → `s.RegisterTool`
- 一个测试插件（自己写的最小 dsh 插件）验证"装了 → 模型能调"

判据（行为级，非实现级）：

- `TestDiscoversDSHPlugin` —— 造一个带 `apply` 的包，必须被扫描到
- `TestRegistersToolFromPlugin` —— 插件注册的工具出现在 HA 工具表
- `TestUnknownRegisterTypeDoesNotCrash` —— 未知注册类型只记账不崩
- `TestPluginLoadFailureDoesNotAbortScan` —— 一个坏包不影响其他包

### 阶段 2：事件与钩子

- `ctx.on('tools/execute'|'tools/post-execute')` → HA 的两个工具阶段
- `ctx.on('session/event')` → 只读订阅
- **异步链的语义问题解决**（§2.4 提到的死锁风险）

判据：

- `TestToolExecuteHookCanDeny` —— 插件拒绝后 HA 不执行该工具
- `TestHookChainDoesNotDeadlock` —— 多插件串联不卡死（带超时）

### 阶段 3：纯 SKILL 交接 + 生命周期

- 非插件包 → `publishSkillDetected`（**内置身份**，可用 Events）
- `ctx.effect` 的清理函数在 Stop 时逆序调用
- 热重载

判据：

- `TestSkillHandoffToSkillmgr` —— 造一个只有 `SKILL.md` 的目录，skillmgr 收到
- `TestEffectsRunInReverseOnStop`

### 阶段 4（可选，需单独决策）：远程安装

- 从 npm / dsh 插件市场安装
- **必须带 sha256 校验**，且默认关闭

---

## 五、参考清单（写代码前该读什么）

### 5.1 必读（按顺序）

> `internal/plugins/clawhubadapter/` 已于 2026-11-04 **迁出内核仓**，
> 现位于 `/home/program/clawhubadapter-plugin/clawhubadapter/`。
> 下面的路径都是迁移后的新位置。

| 文件 | 为什么 |
| --- | --- |
| `<ext>/sidecar.go` | **协议层原型**，原样照搬（JSON-RPC + pending-id 配对 + NotifyChan + 优雅 Close） |
| `<ext>/registry.go` | **转译表原型**，换内容照抄结构（含 `default` 宽容分支） |
| `<ext>/plugin.go` | 生命周期、包发现、工具注册的骨架 |
| `<ext>/simulator/main.js` | 假宿主原型，换成 Cordis Context |
| `<ext>/skill/clawhub-tools/SKILL.md` | 随包 skill 的真实写法（外部插件带 skill 的范本） |
| `internal/plugins/pluginmgr/skill_handout.go` | 外部插件随包 skill 的**移交机制**（安装时 + 启动重扫） |
| `internal/plugins/skillmgr/plugin.go` | 交接口终点（`loadOne`、`isNativeSkillPath`、`scanExisting`） |

`<ext>` = `/home/program/clawhubadapter-plugin/clawhubadapter`。

### 5.2 外部参考（dsh 侧）

| 路径 | 为什么 |
| --- | --- |
| `/opt/agentmail/plugins/dsh-mail-bridge/src/index.ts` | **dsh 插件侧 API 的最可靠文档**（2586 行生产代码）。`inject` 声明、`ctx.tools.register`、`ctx.on` 五种事件、`ctx.effect` 清理、logger 不进 journalctl 的坑 |
| `/root/.dsh/profiles/web/package.json` | 装载配置的真实形状（`dsh.profile.bundles`） |
| `/root/.dsh/profiles/web/node_modules/@dsh-external/dsh-mobile-nav/` | 一个最小真实插件（含 `cordis.patch.yml`） |
| `@deepseek-ai/dsh-tools/lib/types/schema.d.ts:248` | `defineTool` 的签名与 `DefineToolOptions` 全字段 |
| `@deepseek-ai/cordis/lib/types/context.d.ts:40` | `Context` 类（`constructor()` 无参，是阶段 0.2 的依据） |

### 5.3 SDK 侧（`third_party/hologramagentsdk/`，即完整 SDK 仓）

| 文档 | 为什么要看 |
| --- | --- |
| `docs/guide/capability-boundary.md` | **先看这个**。决定什么能做、什么只能内置 |
| `docs/api/builtin-only.md` | 仅内置可用的 API 清单（`Events()` 恒 nil 的裁定在此，第 25 行） |
| `docs/guide/capability-boundary.md` §「判定依据」 | 三条权威来源（proc 模板 / internal/sdk / proc 协议表）—— 有疑问就按这三条查 |
| `docs/api/tools.md` / `docs/api/stages.md` | `ToolDef` / 阶段钩子的字段全集 |
| `sdk/plugin.go` | 字段以源码为准，`grep -A30 "type ToolDef struct"` |
| `tools/hmapdev/templates/proc_main.go.tmpl` | **外部插件实际拿到什么能力，全在这一个文件里**（`buildPluginSDK` 的注入清单） |

### 5.4 hmapdev 要问的东西

```bash
hmapdev version          # 工具链 + SDK 模块 + 构建用 Go（实测 1.4.0 / go1.27.1）
hmapdev sdk current      # 当前活跃 SDK（实测 v1.4.0）
hmapdev sdk list         # 已装版本（v1.4.0 / v1.3.0 / v1.2.0 / v0.8.0）
hmapdev --help           # ★ build/debug **不支持** --help，看这个
hmapdev init <name>      # 拿脚手架（会生成 plugin.go + plg.json + go.mod + rawfile/）
hmapdev build            # 产出 dist/<name>_bundle.hmap（ZIP，含多平台 plugin.bin.<os>.<arch>）
```

**两个实测踩过的坑**：

1. `go build` 在插件工程里**不成立** —— 缺 `main`，入口由 `hmapdev build`
   注入生成（`z_proc_gen.go`）。所以"能不能编译"要用 `hmapdev build` 判断，
   不要用 `go build`。
2. proc 运行时调 **`NewPluginFactory`**（不是 `NewPlugin`）。手写入口时名字必须对，
   否则 `undefined: NewPluginFactory`。

> dshplgadapter 是**外部插件**，所以 `hmapdev` 全程参与构建：
> `hmapdev init` 生成骨架 → `hmapdev build` 产出 `.hmap`（内含 `plugin.bin.<os>.<arch>`
> 与随包 `skill/`）。与本仓 `internal/plugins/` 的内置插件（走主仓 `go build`）
> **是两套不同的构建路径**，不要混。

### 5.5 可直接照抄的迁移范本（先读这个再开工）

2026-11-04 已把 clawhubadapter 完整迁为外部插件并跑通，工程在
`/home/program/clawhubadapter-plugin/clawhubadapter/`。dshplgadapter 开工前
建议先读这三个文件，它们是迁移后**已验证可编译、可运行**的真实形态：

| 文件 | 看什么 |
| --- | --- |
| `plugin.go` 开头的 `NewPluginFactory` | 外部入口的正确写法（含注册改工厂的差异） |
| `safeclose.go` | 内核 `internal/safeclose` 的外部替代（泛型覆盖三种 channel） |
| `external_form_test.go` | 四条外部形态判据，**直接照搬改包名** |

还有一个实操细节：`plg.json` 的 `name_en` 决定产物文件名
（`toSnake(name_en)` → `<snake>_bundle.hmap`）。如果 `name_en` 写成
"Dsh Plg Adapter"，产物会是 `dsh_plg_adapter_bundle.hmap`，与包名
`dshplgadapter` 不一致 —— 安装脚本里很容易对不上。**把 `name_en`
设成与 `name` 相同**。

---

## 六、风险清单

| 风险 | 影响 | 处置 |
| --- | --- | --- |
| **stdout 被 dsh 插件日志污染** | 协议坏帧，插件整体不可用 | 阶段 0.1 先验证 fd 3 方案；未解决不进阶段 1 |
| **dsh 插件可执行任意 Node 代码** | 绕过 HA 沙箱 | 阶段 1 前先做**允许与否**的决策（§3.4） |
| dsh 版本漂移（本机 0.2.1-rc.2，RC 版会变） | 插件 API 变，适配层碎 | 适配层按 `inject` 声明**动态**发现能力，不硬编码包名 |
| `@@deepseek-ai/dsh-*` 200+ 包的加载开销 | 内存/启动时间 | 只加载被 `bundles` 声明的，不全扫 |
| 异步中间件链与 HA 同步阶段语义冲突 | 死锁或语义丢失 | 阶段 2 专项；先在探针里复现 |
| 本仓 `channels.lua` 类问题：加了代码但静默退化 | 难查的 nil | 照 `luac` 那次的教训，每条新路径都要有行为判据 |

---

## 七、明确不做

- **不做 dsh 的 client 半边**（`exports["./client"]`、浏览器 UI）。
  那是 dsh 前端的事，HA 没有对应位置。
- **不实现 Cordis 的完整服务树**。只提供插件**声明了 `inject`** 的那几个服务，
  其余缺失即报错退出（fail-loud），不伪造。
- **不在阶段 1-3 做远程安装**（§3.4 第三条）。
- **不为了兼容 dsh 而改 HA 的阶段钩子语义**。转译层负责适配，内核不动。

---

## 八、判据总表（贯穿各阶段）

| 判据 | 钉什么 |
| --- | --- |
| `TestDiscoversDSHPlugin` | 扫描真的能认出 dsh 插件 |
| `TestRegistersToolFromPlugin` | 工具真的进了 HA 工具表（不是只记日志） |
| `TestUnknownRegisterTypeDoesNotCrash` | 宽容策略 |
| `TestPluginLoadFailureDoesNotAbortScan` | 一个坏包不毁全部 |
| `TestProtocolSurvivesPluginLogging` | ★ 插件狂打日志时协议仍通（对应最大风险） |
| `TestPackageNameCannotEscapeRoot` | 路径穿越（抄 clawhubadapter 的教训） |
| `TestArchiveExtractionStaysInsideRoot` | ZIP Slip（同上） |
| `TestEffectsRunInReverseOnStop` | 清理顺序 |
| `TestSkillHandoffToSkillmgr` | 职责链交接 |

**每条判据都要做变异验证**：把对应实现改回错误形态，判据必须变红。
只写判据不做变异，等于没验证（本仓 `agentcli-terminal` 那次教训：
第一版变异"只改一句话"没红，因为关键词在文中另有 13 处，那次变异无效）。
