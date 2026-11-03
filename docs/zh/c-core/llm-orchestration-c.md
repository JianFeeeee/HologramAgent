# 内核 C 化 · 第一刀：LLM 编排层

> 分支：`feature/c-core`（从 `main` 拉出，遵循 `git-release-discipline`）
> 状态：**第一刀已落地并闭环**（2026-09-25）。L1 纯函数层的三个函数已在 C 侧
> 实现，双路径（cgo / 纯 Go 回退）与黄金对照测试均已在仓库内跑通；
> 构建链已打通至 `linux/amd64` 与 `linux/arm64` 两个真实发布目标。
> 本文保留设计与可行性论证，并在每节标注**落地后的实际情况**。
> 本文只写经实测确认的结论；每个「可行」都附验证方式，每个「不可行」都附证据。

---

## 一、为什么要 C 化，以及为什么先动 LLM 编排

内核当前是**纯 Go 单进程**（`homed`），插件经子进程 + 共享内存与之通信。
C 化的目标不是「换语言重写」，而是把**稳定、高频、无 GC 抖动敏感**的热路径
下沉为可复用 C 库，让内核在保持 Go 编排能力的同时获得：

- 可预测的延迟（无 GC STW 影响热路径）
- 可跨端复用（鸿蒙 / 嵌入式 / C SDK 侧同一份实现）
- 与既有 C 资产统一（见 §三）

**为什么第一刀是 LLM 编排**：这一层是内核最核心的职责（`assets/docs/zh/OVERVIEW.md`
定义内核 = 「LLM 编排 + 记忆管理 + 知识检索」），且它**天然分层**——
上层是有状态的调度/工具循环，下层是**无状态的协议编解码**。
后者是纯字符串进、结构体出，最适合先下沉。

---

## 二、可行性核实结论（实测）

### 2.1 工具链齐备

```
cc/gcc/clang:  /usr/bin/{cc,gcc,clang}
make/cmake:    /usr/bin/{make,cmake}
go env:        CC=gcc  CGO_ENABLED=1  GOOS=linux  GOARCH=amd64
```

### 2.2 已有纯 C 先例，可直接复用

`third_party/hologramagentsdk/remotedevice/` 是一个**零外部依赖**的纯 C 库
（1320 行），已含可复用组件：

| 文件 | 行数 | 能力 |
|---|---|---|
| `src/ha_json.c` | 368 | DOM 风格 JSON 解析器 + 流式构建器 |
| `src/ha_ws.c` | 324 | WebSocket 客户端握手/帧 |
| `src/ha_remotedevice.c` | 628 | 设备通道 |
| `CMakeLists.txt` | — | 静态/动态库、install 规则、可选测试 |

**这意味着 JSON 解析这一 C 化的最大依赖，仓库里已有现成实现**，
不需要引入 cJSON 等外部依赖，与「内核不新增外部依赖」的克制一致。

### 2.3 ~~关键约束：Windows 构建是 `CGO_ENABLED=0`~~ → 前提已消失，改用另一条约束

> ⚠️ **本节结论已因「放弃 Windows 原生」而过时**（见 §2.5）。
> 关键的一点在于，原论证推理的根基“Windows 包需要 CGO_ENABLED=0”已不成立，
> 故「必须保留纯 Go 回退」这个**推论**也不再由它支撑。
> 现在的实际策论是：用 `cgo` / `!cgo` 一组约束（回退实现保留，但理由是
> waiter 等 CGO-free 目标与无 cgo 工具链场景，而不是 Windows），
> 而**不需要额外的 `hacodec` tag** ——因为 ha_codec 是零依赖纯 C99 源码
> 内联编译，不像 onnxruntime 那样需要运行期 `.so`。

原论证（保留作为推理参考）：

```
deploy/packaging/package-windows.sh:69
  GOOS=windows GOARCH="$ARCH" CGO_ENABLED=0 \
```

**这是 C 化最大的部署风险**：若把编排逻辑改成必须 cgo，Windows 包直接编不出来。

**已验证的解法：build-tag 双实现**。实测（最小复现）：

```go
//go:build cgo
// a_cgo.go —— cgo 实现，链接 C 库

//go:build !cgo
// a_pure.go —— 纯 Go 回退实现，行为等价
```

```
CGO_ENABLED=1 go build ./...  → exit 0
CGO_ENABLED=0 go build ./...  → exit 0
```

结论：**C 化必须始终保留纯 Go 回退路径**，且两条路径要有同一组测试钉死
行为等价（黄金对照，见 §五）。这不是可选项——是 Windows 分发的前提。

### 2.4 落地后的形状修正（实测补充，2026-09-25）

实际落地时，三处原设计需要修正：

**① 不链接静态库，也不 `#include` 包外源 —— 用包内符号链接。**
原方案（`LDFLAGS` 指向 `csrc/build/libha_codec.a`）实测会造成两个必然失败：

- `.a` 是构建产物、不入库（`.gitignore` 的 `build/` 命中 `csrc/build/`），
  而发布脚本原先并不产出它 ⇒ 「不入库 + 不生成」两头空，链接报
  `cannot find .../libha_codec.a`。
- 交叉编译 `linux/arm64`（homed 的真实发布目标）时，宿主 x86-64 的 `.a`
  被链进目标产物，报 `file in wrong format`。

改为包内 `ha_codec.c` / `ha_codec.h` **符号链接**到 `csrc/` 权威源：

```
internal/agent/api/ha_codec.c -> ../../../csrc/src/ha_codec.c
internal/agent/api/ha_codec.h -> ../../../csrc/include/ha_codec.h
```

★ **为什么不能用 `#include "../../../csrc/src/ha_codec.c"`（包外相对包含）**：
**Go 构建缓存不跟踪包外被 #include 的 C 文件**。实测：在包外源里把返回值从 7
改成 8，`go test` 依然通过（缓存命中、静默沿用旧代码）；同样改动落在包内文件时
立即判红。对「逐步推进 C 化」这是致命的——改 C 源码却不生效且无任何报错。
（包内 shim `#include` 包外源同样漏跟踪，已实测排除。）

符号链接同时满足两点：文件在包目录内 ⇒ 缓存按内容正确跟踪；
只有一份权威源 ⇒ 无副本漂移、无需同步目标。

**② `csrc/CMakeLists.txt` 的定位变化**：不再是 Go 构建的前置，而是
**C 侧独立复用**（鸿蒙/嵌入式/C SDK）与契约测试（ctest）的入口。

**③ C 源文件会被 Go 工具链视为包的一部分**：故 `.c` 需要 `//go:build cgo` 约束
（C 编译器把该行当普通注释，两侧兼容）。

### 2.5 真正的硬耦合点：Lua 适配器

`internal/agent/api/provider.go` 的 `LuaAdaptedProvider` 在**每次请求**都要
调 Lua VM（`internal/lua/vm.go`，基于 `gopher-lua`，679 行）：

| 调用点 | provider.go 行 | 作用 |
|---|---|---|
| `CallTransformRequest` | :403, :872 | 改写请求体（适配器协议知识） |
| `GetAdapterEndpoint` | :408, :877 | 决定 endpoint |
| `CallTransformResponse` | :439 | 改写响应 |
| `BuildHeaders` / `GetAdapterHeaders` | :487, :489 | 动态签名头 |
| `TransformError` | :900 | 错误归一化 |
| `CallTransformStreamChunk` | :950 | 流式分片改写 |

**结论**：**「把 provider.go 整体 C 化」是不可行的**——它把一个嵌入式 Lua
解释器（带 GC、协程）拖进 C。可行的切法是**只 C 化 Lua 之外的部分**：
把 Lua 当作「回调钩子」，C 侧定义钩子接口，Go 侧注入 Lua 实现。

---

## 三、切分方案：按「无状态 → 有状态」分三层

### L1 · 协议编解码（本轮目标，纯函数，零状态）

最适合先下沉。全部是 `string in → struct out`：

| 目标函数 | 现位置 | 说明 |
|---|---|---|
| `parseOpenAICompatibleResponse` | `provider.go:499` | 非流式响应解析 |
| `parseOpenAICompatibleSSEBody` | `provider.go:541` | SSE body 整段解析 |
| `parseOpenAICompatibleStreamChunkFull` | `provider.go:759` | 流式分片解析 |
| `normalizeOpenAIToolCalls` | `provider.go:628` | 工具调用归一 |
| `normalizeStreamToolCalls` | `provider.go:674` | 流式工具调用归一 |
| `ModelContextWindow` | `provider.go:273` | 模型名 → 窗口（纯映射） |
| `EstimateTokens` / `TruncateByTokens` | `tokenbudget.go` | 纯计算 |
| `ComputeTokenBudget` | `tokenbudget.go:52` | 纯计算（依赖上面两个） |

这些函数**不碰网络、不碰 Lua、不碰 goroutine**，是最安全的起点。
`ModelContextWindow` / `EstimateTokens` / `ComputeTokenBudget` 三个更是
**同一组纯算术**，可作为「第一个能跑通端到端 C 调用」的最小切片。

### L2 · Provider 编排（暂不动）

`LuaAdaptedProvider.Chat` / `ChatStream`：HTTP + Lua 钩子 + SSE 流式，
强耦合 Go 的 `net/http` 与 `context`。C 化的收益低于风险，**暂不动**。

### L3 · 工具循环 / 调度（明确不 C 化）

`task.go` / `toolcall.go` / `eventloop.go` / `scheduler.go`：有状态、
与记忆层和 goroutine 调度深度耦合。C 化会摧毁可维护性，**不做**。

---

## 四、落地结构（**已落地**，2026-09-25）

```
internal/agent/api/
├── provider.go              # 不动（L2）
├── codec.go                 # 统一符号名（调用方只见这里）
├── codec_cgo.go             # //go:build cgo  → 调 C
├── codec_nocgo.go           # //go:build !cgo → 转发到纯 Go
├── codec_pure.go            # 纯 Go 实现（回退 + 黄金对照基准）
├── codec_golden_test.go     # 黄金对照：C 与纯 Go 逐值相等
├── ha_codec.h  -> ../../../csrc/include/ha_codec.h   （符号链接）
└── ha_codec.c  -> ../../../csrc/src/ha_codec.c       （符号链接）

csrc/                        # C 实现（主仓，非 SDK）
├── CMakeLists.txt           # 供 C 侧独立复用与 ctest（不参与 Go 构建）
├── include/ha_codec.h       # 对外 C 接口（冻结契约）
├── src/ha_codec.c           # L1 编解码（当前：窗口推断 + token 估算/截断）
└── test/test_ha_codec.c     # C 侧契约测试
```

★ **为什么是符号链接而不是 `#include` 包外源**：Go 构建缓存不跟踪包外被
`#include` 的 C 文件（实测：改包外源后 `go test` 仍报 ok，静默用旧代码）。
详见 §2.4 ①。

**接口设计原则**（已遵守）：
1. C 接口只吃 `const char*` + 长度，出数值/JSON 串——**不传 Go 指针、
   不回调 Go**（回调留给 L2 的 Lua 钩子层，不在本轮）
2. C 侧**不 malloc 长期持有的内存**；调用方给缓冲区，或用「申请/释放」成对
   API 并在 Go 侧 `defer` 释放
3. `ha_codec.h` 一旦定下就是**冻结接口**，与 SDK 冻结同一标准

---

## 五、验收方式（黄金对照，缺一不可）

C 化的正确性**不能靠「跑起来没崩」**，必须有可复现的对照。三类证据：

1. **黄金对照测试**：同一组输入分别喂 C 实现与 Go 实现，断言输出逐字段相等。
   现有测试可直接复用做基准：
   - `internal/agent/api/sse_body_test.go`（4 个 Test）
   - `internal/agent/api/context_window_test.go`（2 个）
   - `internal/agent/core/stream_accumulate_test.go`（9 个）
   - `internal/agent/core/tokenbudget_test.go`
2. **双构建全绿**：`CGO_ENABLED=1 go test ./...` 与 `CGO_ENABLED=0 go test ./...`
   **都必须通过**（后者走纯 Go 回退）。CI 要同时跑。
3. **契约测试**：`ha_codec.h` 的每个函数有对应 C 单测（参照
   `remotedevice/test/test_ha_remotedevice.c` 的写法，`gcc ... -lpthread` 直编）。

---

## 六、第一步：最小可验证切片（**已完成**，2026-09-25）

**目标**：只 C 化一个纯函数族，跑通「Go → cgo → C → 返回」全链路，
证明结构可行，再谈扩张。

选 `ModelContextWindow` + `EstimateTokens` + `TruncateByTokens`
（三个纯函数，无依赖，逻辑确定，测试齐备）。**下表为实际落地情况**：

| # | 计划项 | 落地 |
|---|---|---|
| 1 | `csrc/include/ha_codec.h` 声明三函数 | ✅（含接口冻结声明与哨兵值约定）|
| 2 | `csrc/src/ha_codec.c` 纯 C 实现 | ✅（表驱动 switch + 手写 UTF-8 步进）|
| 3 | `codec_cgo.go` / `codec_pure.go` 双实现 | ✅（+ `codec_nocgo.go` 转发层）|
| 4 | `csrc/CMakeLists.txt` 产出静态库 | ✅（但 Go **不链接**它，见 §2.4）|
| 5 | Go 侧构建集成 | ✅ 改为包内符号链接 + cgo 编译 C 源 |
| 6 | 黄金对照测试 | ✅ `codec_golden_test.go`（手写用例 + 2000 次随机对拍）|
| 7 | `CGO_ENABLED=0` 下全绿 | ✅ `make check-codec-paths` 钉死两条路径 |

**额外钉死的约束**（原计划未列，实测后补）：

- **变异测试必须真判红**：改 C 侧 `result = 131072` → `777`，
  `go test` 必须 FAIL。这是「C 路径真的生效」的证据（不是「跑起来没崩」）。
  ★ 此测试抓到过两种静默失效：包外 `#include` 漏跟踪、构建缓存隐藏改动。
- **`cc` 交叉编译必须可过**：`make build-linux-arm64` 产出 ELF aarch64。
- **C 侧契约测试**：`make csrc-test`（ctest）与 Go 侧黄金对照互补。

**这一步的价值不在功能**（这三个函数 Go 版没问题），而在**打通链路、
钉死双路径约束、建立黄金对照范式**——后面每扩一个函数都复用它。

---

## 七、已知风险与未决问题

| 风险 | 现状 | 处置 |
|---|---|---|
| ~~Windows `CGO_ENABLED=0` 编不出来~~ | **前提已消失**（Windows 原生已放弃，见 §2.3）| 回退保留但理由换成「CGO-free 目标」|
| Lua 适配器无法 C 化 | 硬耦合 gopher-lua | 钩子化，Lua 留在 Go 侧（L2 做）|
| ~~C 库构建谁触发~~ | ✅ 已解决：Go 不依赖预构建库，直接编包内符号链接的 C 源 | — |
| **包外 C 源会被缓存漏跟踪** | ✅ 已避坑：实测确认，改用包内符号链接 | 扩张时勿改回 `#include` 包外路径 |
| JSON 解析能力不足 | `ha_json.c` 只存 `int`（无 float/long）| 用前须评估；必要时扩该库（会动 SDK，需走 SDK 冻结流程）|
| 接口冻结 | — | `ha_codec.h` 冻结标准对齐 SDK |
| 打包脚本 | ✅ 不再需带 `.a`（编源码，无外部产物依赖）| 扩张到多文件 C 实现时重评 |

**仍未决（需 jianf 拍板）**：
1. C 实现放**主仓 `csrc/`** 还是 **SDK `third_party/hologramagentsdk/`**？
   - 当前已在主仓 `csrc/`；若 SDK 侧也要复用，需定同步机制
   - 放 SDK：天然跨端复用，但要走 SDK 冻结与大版本流程
2. `ha_json.c` 是**复用**（从 remotedevice 复制/提为公共）还是**新写**？
   复用会动 SDK 目录结构。
3. **下一个切片选谁**？L1 剩下的是协议编解码（`parseOpenAICompatible*`、
   `normalize*ToolCalls` 等，见 §三 L1 表）；该层依赖 JSON 解析 ⇒ 先解第 2 题。

### 7.1 ★ 性能：初版结论是错的，根因是我的绑定与 C 实现

**初版结论「C 比 Go 慢」不成立** —— 那是把「我自己的 malloc/拷贝开销」误当成了
「cgo 的固有成本」。拆解实测（同一台机，`-benchtime` 百万次）：

| 场景 | ns/op | 说明 |
|---|---:|---|
| cgo 边界（零拷贝传指针 + 空函数体）| **31.9** | cgo 的**真实**固有成本 |
| + 一次 `C.CString` + C 侧 `strlen` | 105–111 | **多出 ~75ns（70%）** |
| 初版 `ModelContextWindow`（另加 `lower_dup` malloc + 16×strstr）| **175** | 即 **82% 是自找的** |

而初版**违反了自己写在本文 §四 的接口原则第 1 条**：
「C 接口只吃 `const char*` **+ 长度**」—— 它没传长度，让 C 侧 `strlen` 再扫一遍。

#### 优化措施（逐项对应上表的浪费）

| # | 初版做法 | 现在 |
|---|---|---|
| 1 | `C.CString`（malloc + 整串拷贝）| `unsafe.StringData` 传指针 + 长度，**零拷贝** |
| 2 | C 侧 `strlen` 再扫一遍 | 长度由调用方传入，**不扫** |
| 3 | `truncate` malloc 输出缓冲 + `GoStringN` 拷回 | C 只返回**字节数**（结果必是前缀），Go 侧 `s[:n]` 切片 |
| 4 | `lower_dup` 每次 malloc 模型名 | 栈缓冲折叠（超长走零分配回退） |
| 5 | 逐字节 `utf8_next` 函数调用 | **字级（8 字节）ASCII 检测** + 位运算 UTF-8 校验 |
| 6 | `truncate` 扫完整串才判断 | **数满 keep 个 rune 立即返回**（提前短路） |
| 7 | 纯 Go 侧 `len([]rune(s))` / `[]rune(s)`（1KB 分配 4KB）| `utf8.RuneCountInString` / `DecodeRuneInString` 游走，**零分配** |

#### 优化后（完全 C 化：一律走 C，无按长度分派）

| 基准 | 初版 C | **优化后 C** | 纯 Go | 提升 |
|---|---:|---:|---:|---:|
| `ModelContextWindow`（短 ASCII）| 175 | **76.5** | 46.8 | **2.3×** |
| `EstimateTokens` / 短 ASCII | 114.6 | **47.2** | 2.8 | 2.4× |
| `EstimateTokens` / 短中文 | 99.6 | **49.7** | 22.8 | 2.0× |
| `EstimateTokens` / **1KB ASCII** | 2318 | **80.8** | 326 | **28.7×** |
| `EstimateTokens` / 1KB 中文 | 840 | 1467 | 2844 | 0.57×（见下）|
| `TruncateByTokens` / 短中文 | 233 | **40.2** | 25.3 | 5.8× |
| `TruncateByTokens` / **1KB ASCII** | 2594 | **71.7** | 411 | **36×** |
| `TruncateByTokens` / **1KB 中文** | 3923 | **70.1** | 3097 | **56×** |

#### ★ 必须如实说明的两点

**① 中文密集输入比初版慢（1467 vs 840）—— 这是刻意的正确性代价。**
初版的 `utf8_next` **只按首字节推断长度、不校验后续字节**，因此对畸形序列会与 Go
分叉（例：`"\xE4\x41\x41"`，Go 判 3 个 rune，初版判 1 个 ⇒ rune 计数偏差 ⇒
token 预算与截断点偏移）。现在 C 侧做了**与 Go `utf8.DecodeRuneInString` 等价**的
完整校验（含过长编码、代理对、超 U+10FFFF、截断序列）。
换来的能力由 `TestGolden_InvalidUTF8`（3000 组随机字节）钉死 —— 这类偏差
**只影响计数、不会崩**，不测就发现不了。**正确性优先，且仍比纯 Go 快 2×。**

**② 极短串上 C 慢于 Go（约慢一个数量级）—— 这是「完全 C 化」的已知代价。**
`EstimateTokens("qq")`：C 约 47ns（几乎全是 31ns 的边界成本）vs 纯 Go 约 3ns。
绝对值是纳秒级（47ns = 0.000047ms），单次请求尺度可忽略；
但**若某个循环对极短串高频调用**，这一项会累积。

⇒ **正确的应对是「C 化那个循环（批量传一次）」而不是「按长度分派回 Go」**
（后者正是被否掉的混合做法：它会同时存在两份语义可能分叉的实现）。
这也是 §三 L2/L3 把「有状态编排」明确留给 Go、而把「长 payload 编解码」
作为下一步目标的原因 —— 协议编解码（JSON / SSE 分片）处理的正是长文本。

---

---

*实测记录：双路径、缓存跟踪、交叉编译、变异测试、跨语言基准均于 2026-09-25
在本仓实测；工具链与 C 资产核实于本仓*
*初版核实：2026-09-24　·　落地更新：2026-09-25*
