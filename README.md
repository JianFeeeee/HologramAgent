> ⚠️ **AI 辅助编程声明**：本项目代码、文档及提交历史中，部分内容由 AI 辅助生成或修改。人工已审阅关键改动，但使用时请自行评估与验证。

# HologramAgent

> **English**: [README_EN.md](./README_EN.md)

HologramAgent 是一个跑在自己机器上的 Agent 内核。接上 QQ、WebUI、命令行、文件、
浏览器，它就在这些地方干活。

记忆会往下沉：最近几轮留在上下文里，旧一点的转成文档，再旧蒸馏成三元组进图数据库。
所以它能连续跑几个月，而不用把全部历史塞进 prompt。

```bash
./build/homed -data /tmp/ha      # 起内核
./build/waiter                    # 跟它说话
```

需要一台 Linux。API 密钥在 WebUI 里填，存本地 SQLite。开发始于 2026 年 7 月，当前 1.4.0。

---

## 几个设计决定

插件跑在独立进程里。它 panic 了带不崩内核，还会自己重启；换个 `plugin.bin` 就生效。
早先是 C 动态库那套，`dlclose` 被 `DF_1_NODELETE` 变成空操作，cgo 超时杀不掉线程，
插件一崩 homed 陪葬。所以重写成了子进程加共享内存。

内核自己不发 HTTP、不读文件。所有碰外部世界的能力都在插件里：WebUI、QQ、命令执行、
网络搜索、浏览器。这不是为了"干净"好看，是为了换掉整个交互层时不用动内核。

同一个 QQ 插件里，群主和路人可以有不同权限；别的 Agent 发来的指令不会写进你的记忆，
但能触发记忆召回。判断由内核的安全模块做，插件只负责声明。

它按"场面"分记忆，不按会话。你从工作聊到生活，它按通道、对话对象、用到的工具和时段
自动分场。下次同一个场面出现，上次的结论会被想起来，包括它自己说错过的那些。

## 能力

说话的地方包括 WebUI、命令行、QQ 群和私聊、其他 Agent（走 A2A 协议），以及在 IDE 里
当 ACP 服务端。能看图片、视频、语音和网页，也能操作真实的 Chrome。

动手的部分是文件读写、跑命令、写代码、日程、备忘、RSS、音乐。QQ 的文件和语音消息能收发。

它维持着三层记忆加一个图数据库和一个知识库，召回按场面走。
手头的活没干完又来新的，会进调度器排队；用户催了能插队，被打断的活保存现场后能接回去。
长任务可以派给子 agent 并行做，不用占着跟你说话的那条线。

28 个内置插件、89 个模型可调工具。外部插件一插件一仓，
见 [github.com/HologramAgent](https://github.com/HologramAgent)。

## 怎么工作的

```go
homed（内核：LLM 编排 + 记忆 + 检索）
   ↑ PluginSDK
插件（WebUI / QQ / CLI / 文件 / 网络 / 一切 IO）
```

内核和插件之间走三条通道：控制用 stdio 上的 JSON-RPC，大块数据走共享内存，
通知走事件环。大对象不序列化，也就不存在拷两遍的问题。

消息进来先过调度器，不直接喂给模型。这是为了处理"正干着活又来了新指令"的情况：
排队的和插队的分开，插队按紧急程度分四级，高级的能抢占低级，被抢占的保存现场等着恢复。
同级的排队等。

再往下是七个阶段组成的管道，模型可以在里面连续调工具。同一轮的多个工具调用默认串行，
除非工具自己声明了并发安全。一个批次里只要有一个没声明，整批都退回串行。

## 快速体验

```bash
make build build-cli
./build/homed -data /tmp/ha
```

```bash
# 交互模式
./build/waiter

# 或单条消息
echo "你好，记住我喜欢喝咖啡" | ./build/waiter
```

### 后台驻留模式（daemon）

waiter 也支持后台驻留，保持与 homed 的持久连接并等待 TUI 实例接入，适合让 agent 主动召唤用户/设备桥持续存活：

```bash
# 后台驻留（默认连 ~/.homeagent/cli.sock）
./build/waiter --daemon

# 指定 socket
./build/waiter --socket /path/to/cli.sock --daemon

# 随后任意 TUI/一行实例都会自动接入正在运行的 daemon，而不是直连 homed
./build/waiter
```

daemon 监听 `~/.homeagent/waiter.sock`，新客户端连入时会回放缓冲的最近对话（256 行），断连后 daemon 持续存活、自动重连 homed，并保持设备桥（若配置了 `device_gateway`/`device_token`）。

API 密钥通过 WebUI `http://localhost:8080` 设置页配置，持久化在 SQLite 中。

## 代码结构

```
cmd/homed/          守护进程入口，组装所有子系统
cmd/waiter/         CLI 客户端（Unix socket）
internal/
├── agent/core/     Agent 核心：输入调度器（两类别+四级中断）、事件循环、LLM 工具循环、7 阶段管道、驻留子
├── agent/api/      LLM Provider（Lua 适配层：provider.go 调 vm）
├── memory/         三层记忆：Graph(SQLite) / Document(JSON+TF-IDF) / Text(JSONL) + StaticEmbedder(预训练词嵌入/TF-IDF回退) + CleanTemplateText(去模版)
├── knowledge/      知识库（文件系统 + TF-IDF）
├── plugin/         插件注册表 + 子进程加载器（stdio RPC + 共享内存段 + 事件环）
├── plugins/        内置插件（webui / cli / files / cmd / mcp / kbtree / seq / skillmgr …）
├── sdk/            PluginSDK（Tool/Stage/Event 三通道）
├── config/         SQLite 配置中心
├── events/         事件总线
└── internal/lua/adapters/   10 个 LLM 协议适配器脚本
外部插件开发见 [hologramagentsdk](https://github.com/JianFeeeee/hologramagentsdk) 仓库，使用 `hmapdev` 工具链开发，参考 `example/` 目录下的 Go 和 Lua 示例
```

## 文档

**在线文档**：

- 介绍站（项目总览）：<https://introduce.homeagent.jianfgit.xyz/>
- 插件 SDK 文档（快速开始 / API 参考 / 指南 / 示例）：<https://sdk.homeagent.jianfgit.xyz/>
  给 agent 直接读的纯文本入口：[`llms.txt`](https://sdk.homeagent.jianfgit.xyz/llms.txt) /
  [`llms-full.txt`](https://sdk.homeagent.jianfgit.xyz/llms-full.txt)

**仓内文档**（随代码版本走）：

- [项目概览](assets/docs/zh/OVERVIEW.md) | [English](assets/docs/en/OVERVIEW.md)
- [技术架构](assets/docs/zh/ARCHITECTURE.md) | [English](assets/docs/en/ARCHITECTURE.md)
- [插件开发指南](assets/docs/zh/PLUGIN_DEV.md) | [English](assets/docs/en/PLUGIN_DEV.md)
- [Lua Adapter](assets/docs/zh/ADAPTER.md) | [English](assets/docs/en/ADAPTER.md)
- [知识库演示](assets/knowledge/homeagent_architecture/content.md)
- [设计文档索引](docs/zh/README.md) — 32 篇设计文档的状态分类与可信度规则
- [更新记录](CHANGELOG.md) — 每个版本实际改了什么
- [未来计划](docs/zh/future-plan.md) — 没做的东西都记在这

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

```bash
make build build-cli    # 编译守护进程 + CLI
make test               # go test ./...
make install            # 安装到系统
```

依赖：Go 1.25+, CGo (go-sqlite3), Linux。

> `homed` 需 Linux（依赖 fd 继承与共享内存段的段内偏移解引用，见
> `cmd/homed/platform_windows.go`）；Windows 上只构建 `waiter.exe`，
> `homed` 跑在 WSL2 里（见「下载」）。macOS 可构建 `waiter`/`initconfig`，
> `homed` 需在原生 macOS 构建。

## 许可

本项目以 **GNU Affero 通用公共许可证第 3 版（AGPL-3.0-only）** 发布，全文见 [LICENSE](LICENSE)。

它是 GPL 家族里**传染性最强**的一档：不仅分发时须提供完整对应源码，
**通过网络提供服务时也要向使用者提供源码**（§13 Remote Network Interaction）。
即：任何人把改过的 HologramAgent 对外提供网络服务，都必须让该服务的使用者拿到改动后的源码。

插件与本项目通过公开 [hologramagentsdk](https://github.com/JianFeeeee/hologramagentsdk) 静态链接
（SDK 源码会进入插件二进制），但那个仓以 **MIT** 发布。MIT 是宽松许可，拿到授权的代码不继承
本项目的 AGPL。因此**外部插件不是本项目的衍生作品**，作者可自行选择许可（含闭源、商业、私有），
既不必同许可、也不受 §13 网络条款约束。第三方插件生态的安全与活跃正建立在这条之上。

边界很清楚：**AGPL 覆盖内核与随包内置插件**（`homed`、`internal/`、`internal/plugins/` 下 13 个内置插件）；
**MIT 覆盖公开 SDK**（`sdk/`，`go.mod` 零外部依赖、只依赖 Go 标准库，不引用内核任何代码）。
子进程隔离在这里不重要了。决定许可的是被链接的 SDK 代码本身，而它是 MIT。

### 随包分发的第三方组件

| 组件 | 许可 | 位置 |
| --- | --- | --- |
| Chinese-CLIP ViT-B/16（ONNX 产物） | Apache-2.0 | `/usr/lib/hologram/models/chinese-clip-vit-b16-onnx/` |
| ONNX Runtime（`libonnxruntime.so`） | MIT | `/usr/lib/hologram/onnxruntime/` |
| jieba 词库（内嵌进二进制） | MIT | 源码 `internal/memory/jiebadict/` |
| Go 依赖（go-sqlite3、gojieba、bubbletea 等） | MIT / BSD-3 / Apache-2.0 | 均为宽松许可，与 AGPL-3.0 兼容 |

这些组件**保持各自原有许可**，不在本项目的 AGPL 授权范围内；发行包把它们的许可全文放在
`/usr/share/doc/hologramagent/licenses/`，并在 dep/rpm 元数据里声明本包许可为 `AGPL-3.0-only`。
