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
        LLM 编排 · 三层记忆 · 检索 · 调度
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

内核不发 HTTP、不读文件。所有接触外部世界的能力都在插件里，插件崩溃不影响内核，
重启退避 1s / 2s / 3s，5 分钟内第 4 次崩溃停止自动重启等人工介入。

### 上下文不是「截断 + 摘要」

常见做法是把超窗的对话裁掉或让模型自己摘要，两者的共同问题是**丢什么由位置决定**。
这里换成了按相关度取舍：用当前输入给每条历史事件打分，低分事件下沉到冷上下文层。

打分用稠密向量余弦（与媒体、冷上下文共享同一向量空间），模型未就绪时退化到 TF-IDF。
裁剪时无条件保住最近 N 条，但 N 本身受预算约束：

```go
// 保护条数不超过预算能装下的条数
budgetCap = ContextTokens / avgEventTokens
pCount = min(配置值, budgetCap)
```

这条约束来自一次实测：50k 窗口下单条事件约 5800 token（工具回灌型），预算约 40000，
「钉住 10 条」本身就装不下，于是每轮裁剪都撞保护下限，两轮后预算耗尽，任务直接终止。
召回率从 44% 掉到 22%。现在窗口越小越自动收紧，1M 窗口下则能用满配置值。

被裁掉的事件不是丢弃，是迁移：事件连同它持有的媒体块一起进冷上下文层，块的 ID 不变、
只换持有层。回灌时用 `coldcontext_query` 取回，两阶段（先 Peek 确认、再 Remove），
不用「先删后灌」——那会在回灌失败时永久丢数据。

### 三层记忆是迁移，不是复制

```text
L1  上下文 Context       相关性裁剪，最近 N 条恒受保护
      ↓  低分事件下沉（带媒体块）
L2  冷上下文 Cold Context 稠密检索，≤2 次访问且 72h 未访问则继续下沉
      ↓  蒸馏成三元组
L3  稳定记忆 Stable       实体关系 + 场面索引，支持按场面召回
```

块在任一层只有一份持有者，不靠引用计数保活。媒体不是附件：图片、音频是图里的一等节点，
有自己的向量，和文本在同一空间里检索。

### 记忆按场面分，不按会话

普通 agent 通常一条会话一条历史，换个话题就失忆，或者全塞进一个上下文里互相干扰。
这里按「可观察的场所指纹」分：通道、对话对象、工具、话题、时段，权重 1.0/1.0/0.8/0.4/0.2。

同一指纹出现到第 2 次才建场景（`minSceneEvidence = 2`），只发生一次的不算。
归属阈值 0.5、唤起阈值 0.35，唤起比归属宽松——想不起来是损失，多想一条只是多几行上下文。
场景每次重现强度 +1，久不重现按半衰期衰减。

两个来源并行：**声明**（插件在注入点说明"这是哪个场面"）与**涌现**（指纹自己聚类）。
声明场景不吸收整轮指纹，否则它会以接近 1.0 的相似度吃掉后续所有同类轮次，
被动路再也长不出更细的场面。

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

`post_action → [before_toolcall → 执行 → after_toolcall] → post_action` 是内循环，
退出条件：模型不再调工具、全部被拒、或达到上限。

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
