> ⚠️ **AI 辅助编程声明**：本项目代码、文档及提交历史中，部分内容由 AI 辅助生成或修改。人工已审阅关键改动，但使用时请自行评估与验证。

# HologramAgent

> **English**: [README_EN.md](./README_EN.md)

自托管 Agent 内核。跑在自己的机器上，插件以独立进程接入。

```bash
make build build-cli
./build/homed -data /tmp/ha
```

API 密钥在 WebUI（`http://localhost:8080`）里填。需要 Linux。

跟常见 agent 框架的主要差别在上下文管理：不是超窗就截断或让模型摘要，
而是按相关度取舍，低分的下沉成冷上下文、再蒸馏进图数据库。另外两处不太一样的是
启动顺序在编译期定序，以及插件按设施就绪提前启动（详见下方架构）。

## 架构

```text
                    homed（内核）
        LLM 编排 · 记忆 · 检索 · 调度
                       │
        ┌──────────────┼──────────────┐
        │              │              │
   stdio RPC       共享内存        事件环
    控制          大块数据         通知
        │              │              │
        └──────────────┼──────────────┘
                       │
              插件（各自独立进程）
   WebUI · QQ · 命令行 · 文件 · 浏览器
```

内核进程做编排、记忆、检索；HTTP、文件、设备都由插件负责（`internal/plugin`）。
插件崩溃后内核重启它，退避 1s / 2s / 3s；5 分钟内第 4 次崩溃停止自动重启，
等人工介入（`cmd/homed/kmod_runtime.go`）。

### 上下文窗口

`internal/agent/core/context.go`

事件进窗口时算两套向量：稠密向量与媒体、冷上下文共用同一空间；词向量用 TF-IDF
作退路。裁剪时用当前输入给每条历史事件打分，保留 topK，最近 N 条不参与打分直接保留。

N 默认 10，实际取值受预算约束：`min(配置值, ContextTokens / 单条平均 token)`。

这条约束来自一次实测。50k 窗口、单条事件平均 5800 token、预算 40000，固定保留 10 条
要占 58k，比预算本身还大。于是每轮裁剪都能达标却清不出空间，两轮后预算耗尽、任务终止，
召回率从 44% 降到 22%。现在小窗口会自动收紧，1M 窗口下用满配置值。

被裁掉的事件连同它持有的媒体块写入冷上下文层，块 ID 不变，只换持有层。
取回用 `coldcontext_query`：先 `Peek` 确认，再 `Remove` 源副本。
没有「取出即删除」的路径，删除排在确认之后。

### 三层记忆

`internal/agent/core/context.go` · `internal/memory/coldcontext/` · `internal/memory/graph.go`

| 层 | 持有物 | 进模型的方式 | 冷化 |
|---|---|---|---|
| L1 上下文 | 事件 + 块 | 直接进 system prompt | 相关性低分下沉 |
| L2 冷上下文 | 事件 + 块（与 L1 同构） | 召回后插回 L1 时序 | ≤2 次访问且 72h 未读，蒸馏 |
| L3 稳定记忆 | 三元组，节点即块 | 召回后注入 | 不流出 |

一块在任一层只有一个持有者，没有引用计数。图片和音频是 L2/L3 里的块，
有自己的向量，与文本在同一空间检索。

### 场面

`internal/memory/scene_emerge.go`

场面由五个特征构成，权重各不相同：通道 1.0、对话对象 1.0、工具 0.8、话题 0.4、时段 0.2。
同一指纹出现两次才建场面（`minSceneEvidence = 2`），一次性的交互不建。
归属阈值 0.5，唤起阈值 0.35；重现时强度 +1，久不重现按半衰期衰减。

两个入口并行：插件在注入点声明（如 `chan:qq/peer:group_1`），或者指纹自行聚类。
声明场景只从键本身解析特征，不吸收整轮指纹。吸收之后它会以接近 1.0 的相似度
匹配后续所有同类轮次，自行聚类那条路就长不出更细的场面。

### 七个阶段

`internal/agent/core/stages.go`

| 阶段 | 时机 | 插件可做 |
|---|---|---|
| `on_input` | 消息到达 | 黑名单、限流、短路回复 |
| `pre_action` | 上下文就绪，调 LLM 前 | 注入外部数据 |
| `post_action` | LLM 返回文本与工具调用 | 敏感词过滤、强制改向 |
| `before_toolcall` | 单个工具执行前 | 审计、拒绝、改参 |
| `after_toolcall` | 单个工具执行完 | 脱敏、排序 |
| `before_output` | 最终文本就绪 | 格式适配 |
| `after_output` | 已发送 | 统计 |

内循环是 `post_action → [before_toolcall → 执行 → after_toolcall] → post_action`，
模型不再调工具、全部被拒、或达到上限时退出。

## 其余

- **通道**：WebUI、命令行、QQ 群与私聊、A2A、IDE 里的 ACP
- **工具**：文件、命令、代码、浏览器（真实 Chrome）、日程、备忘、RSS、音乐
- **模态**：图片、视频、语音、截图、网页

28 个内置插件、89 个模型可调工具。外部插件一插件一仓，见 [github.com/HologramAgent](https://github.com/HologramAgent)。

## 下载

[Releases](https://github.com/JianFeeeee/HologramAgent/releases) 提供三种变体：

| 变体 | 内容 | 适用 |
| --- | --- | --- |
| **full** | homed + waiter + 桌面 GUI + systemd unit | 单机全功能 |
| **server** | homed + waiter + systemd unit | 服务器（无桌面环境） |
| **client** | waiter + 桌面 GUI | 连接远程 HologramAgent |

- Linux：`.deb`（amd64/arm64）、`.rpm`（x86_64）、`.tar.gz`
- Windows：`HologramAgent_v1.2.0_{Full,Server,Client}_win64.exe`（NSIS 安装向导，含 AGPL 许可页）。自 v1.2.0 起因 `homed` 不再支持 Windows 原生（依赖 fd 继承与共享内存段内偏移解引用），安装器改为引导到 **WSL2**，并把 Linux 包送进发行版里按 Linux 方式安装。
- 免安装：`homeagent-bin-<os>_<arch>.tar.gz`（含 homed/waiter/initconfig）
- 校验：`SHA256SUMS`

macOS 的 `homed` 需在原生 macOS 构建（CGO + sqlite3），发布包仅含 `waiter`/`initconfig`。

## 构建

依赖 Go 1.25+、CGo（go-sqlite3）、Linux。

```bash
make build build-cli    # 守护进程 + CLI
make test               # go test ./...
make install            # 安装到系统
```

`homed` 只能在 Linux 上跑（依赖 fd 继承与共享内存段的段内偏移解引用）。
Windows 上构建 `waiter.exe`，`homed` 跑在 WSL2 里；macOS 要原生构建 `homed`。

## 许可

以 **AGPL-3.0-only** 发布，全文见 [LICENSE](LICENSE)。

随包的第三方组件保持各自许可，全文在 `/usr/share/doc/hologramagent/licenses/`。

### 随包分发的第三方组件

| 组件 | 许可 | 位置 |
| --- | --- | --- |
| Chinese-CLIP ViT-B/16（ONNX 产物） | Apache-2.0 | `/usr/lib/hologram/models/chinese-clip-vit-b16-onnx/` |
| ONNX Runtime（`libonnxruntime.so`） | MIT | `/usr/lib/hologram/onnxruntime/` |
| jieba 词库（内嵌进二进制） | MIT | 源码 `internal/memory/jiebadict/` |
| Go 依赖（go-sqlite3、gojieba、bubbletea 等） | MIT / BSD-3 / Apache-2.0 | 均为宽松许可，与 AGPL-3.0 兼容 |

发行包在 dep/rpm 元数据里声明本包许可为 `AGPL-3.0-only`。
