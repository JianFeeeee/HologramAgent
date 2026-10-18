# 内核性能指标（实测基线）

本文记录**实测**数据，不是估算。每条都标出测量方法与采集时间，
便于复现与追回归。

> 采集环境：`main` @ `ecffb8a`，Linux amd64，Go 1.27.1，
> 生产实例 `/home/newqqagent`（21.4GB 数据目录），
> 22 个外部插件 + 5 层 / 17 模块内核。
> 采集时间：2026-10-18 18:12（冷启动）。

---

## 1. 冷启动

`systemctl restart homeagent` → `内核装配完成`。

| 阶段 | 时刻 | 累计 | 说明 |
| --- | --- | --- | --- |
| 启动 | 18:12:15 | 0s | 依赖图加载（`depgraph.bin`，digest `2067c913`） |
| 第 0 层完成 | 18:12:15 | ~0.1s | 2 个：`config`, `kernel-api` |
| 第 1 层完成 | 18:12:18 | **3.1s** | 9 个：`doc-store`, `knowledge-migration`, `lua`, `memory`, `migration`, `multimodal`, `onnx-parser`, `supervisor`, `tracker` |
| 第 2 层完成 | 18:12:18 | 3.2s | 3 个：`knowledge-store`, `llm`, `text-memory` |
| 第 3 层完成 | 18:12:18 | 3.3s | 1 个：`stage-host` |
| 第 4 层完成 | 18:12:20 | **5.5s** | 2 个：`agent`, `plugin-host` |
| 装配完成 | 18:12:30 | **15.4s** | 含 22 个外部插件建链 |

峰值并发 4（原为 17 步串行）。

### 耗时分布

```text
第 0→1 层  3.1s  ████████████████  20%   内存/ONNX/文档存储初始化
第 1→4 层  2.4s  ████████████      16%   LLM/知识库/agent 装配
第 4→完成  9.9s  ████████████████  64%   22 个插件子进程建链
```text

**结论**：启动时间主要由**插件建链**决定（64%），不是内核模块本身。
每多一个 proc 插件约 +0.45s。

> ⚠️ 与早期记录（1538–1709ms）的差异：那是**纯内核**（5 层 17 模块）
> 的装配时间，不含插件。两者口径不同，不要混用。

### 复现

```bash
systemctl restart homeagent
journalctl -u homeagent --since "1 min ago" \
  | grep -E "启动顺序来自|第 [0-9] 层完成|内核装配完成"
```text

---

## 2. 资源占用

稳态（启动完成、空闲）：

| 指标 | 值 |
| --- | --- |
| 内核 RSS | **26 MB** |
| 内核 VSZ | 2104 MB（含 Go 保留地址空间，非常驻） |
| 插件子进程数 | 12（其余按需加载） |
| 二进制体积 | 84 MB（未 strip，含 debug_info） |

> 12 而非 22：13 个插件声明了 `auto_start: false`（按需加载），
> 只有被 `plugin_enable` 拉起时才起进程。

---

## 3. 超时与容量

这些是**硬约束**，决定"一个任务最多能占用多久"。

### 工具超时（三层）

| 层 | 值 | 位置 |
| --- | --- | --- |
| `cmd_run` 自身 ctx | 默认 **30s**，可由参数调 | `internal/plugins/cmd/plugin.go` |
| 内核工具等待 | `max(声明值, 参数值)` | `internal/agent/core/toolcall.go:37` |
| 硬上限 | **10m** | `toolcall.go:28` `maxToolTimeout` |

计算规则（`resolveToolTimeout`）：

```text
base = 60s（默认）
     ↓ 若 ToolDef 声明了 Timeout
base = 声明值
     ↓ 若调用参数带 timeout 且 > base
base = 参数值
     ↓ 若 base > 10m
base = 10m
```text

**实测验证**：

```text
cmd_run command="sleep 200"（不传 timeout）
→ 工具 cmd_run 失败: 命令执行超时（30s）
```text

⚠️ **参数可以合法绕过默认值**：模型传 `timeout="2m"` 时，
`cmd_run` 的 30s 保护被提高到 2 分钟。这是设计允许的（长命令确有需要），
但也意味着**单个工具最长可占用 10 分钟**，期间调度器只能等。

### 上下文预算

运行时日志（`task.go:599`）：

```text
tool call loop start, max_ctx=1048576 target=600000 fixed=3004 mem=198998 ctx=397
```text

| 项 | 值 | 含义 |
| --- | --- | --- |
| `max_ctx` | 1048576 | 模型上下文上限（1M） |
| `target` | 600000 | 目标使用量（60 万，留 40% 余量） |
| `fixed` | 3004 | 固定开销（system prompt 等） |
| `mem` | 198998 | 记忆注入占用 |
| `ctx` | 397 | 当前对话占用 |

### 记住-遗忘阈值

| 键 | 值 | 说明 |
|---|---|---|
| `core.agent.offload_busy_after` | **1m** | 忙超过此时长则转投 offload 子 agent |

---

## 4. 安全层开销

| 项 | 值 |
| --- | --- |
| 密钥来源 | `embedded`（构建期注入，指纹 `0db8e9c5`） |
| 变量条数 | 8 |
| 加密算法 | AES-GCM（`enc:v1:` 前缀） |
| 出向/入向扫描 | 每次工具调用与工具返回各一次（`security.Patterns`） |
| 日志脱敏 | `scrubForLog`（幂等，nil-store 安全） |

扫描是**纯内存正则匹配**，未观测到成为瓶颈（工具调用耗时由工具本身主导）。

---

## 5. 插件建链

| 指标 | 值 |
| --- | --- |
| 建链数（本次启动） | 11 |
| `loaded` 事件 | 28 |
| 单插件建链均摊 | ~0.45s（9.9s / 22） |

协议版本：`sdk=1.4.0 core=1.4.0`。

---

## 6. 已知的性能陷阱

以下都是**实测踩过**的，不是理论风险。

### 6.1 长工具执行期间 `stop` 无法生效

`stop` 请求只在 `pre_action` 被消费：

```text
[agent] stop requested by webui (queued=1 will be short-circuited at pre-action)
```text

若 agent 正卡在 `sleep 150` 这类长命令里，**没有 pre_action 可以插入** ⇒
停止按钮要等到当前工具跑完才生效。

现场（2026-10-18）：`task#3` 跑了 10 分钟，期间 `task#4`~`task#8`
全部堵在队列，两次 stop 请求都无效，最后只能重启。

### 6.2 同阶段重复注册 = 静默覆盖

插件侧 `stageHandlers` 是 `map[阶段名]handler`，同阶段注册两次后者覆盖前者。
除了功能丢失，还意味着**权限门可能被覆盖掉**。hmapdev 在构建期告警。

### 6.3 `before_output` 有两条触发路径

一轮结束 vs 单次 `output_send__`。误用会让"正在输入"漏触发，
或让状态归位过于频繁（干活期间反复跳回空闲）。

详见 SDK README「阶段钩子」一节。

---

## 7. 复现与回归

```bash
# 冷启动
systemctl restart homeagent && sleep 20
journalctl -u homeagent --since "1 min ago" | grep "内核装配完成"

# 资源
ps -o rss= -p $(pgrep -f "homed" | head -1) | awk '{printf "RSS: %.0f MB\n", $1/1024}'

# 插件进程
pgrep -c -f "plugin.bin"

# 工具超时（应 30s 失败）
# 经 agent 调：cmd_run command="sleep 200"
```text
