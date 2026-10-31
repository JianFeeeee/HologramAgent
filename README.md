> ⚠️ **AI 辅助编程声明**：本项目代码、文档及提交历史中，部分内容由 AI 辅助生成或修改。人工已审阅关键改动，但使用时请自行评估与验证。

# HologramAgent

> **English**: [README_EN.md](./README_EN.md)

自托管 Agent 内核。接上 QQ、WebUI、命令行、文件、浏览器，跑几个月不用重启，还记得住事。

```bash
make build build-cli
./build/homed -data /tmp/ha
./build/waiter
```

API 密钥在 WebUI（`http://localhost:8080`）里填。需要 Linux。

## 能做什么

- **通道**：WebUI、命令行、QQ 群与私聊、A2A、IDE 里的 ACP
- **工具**：文件、命令、代码、浏览器（真实 Chrome）、日程、备忘、RSS、音乐
- **模态**：图片、视频、语音、截图、网页
- **记忆**：三层（上下文 → 文档 → 图）+ 知识库，按场面召回
- **调度**：并行子 agent、中断抢占、可恢复的现场

28 个内置插件、89 个模型可调工具。外部插件一插件一仓，见 [github.com/HologramAgent](https://github.com/HologramAgent)。

## 架构

```go
homed（内核：LLM 编排、记忆、检索）
   ↑ PluginSDK
插件（全部 IO：WebUI、QQ、命令行、文件、网络）
```

内核不发任何 IO 调用。插件跑在独立进程里，与内核之间三条通道：stdio 上的 JSON-RPC 管控制，
共享内存管大块数据，事件环管通知。插件 panic 不会带崩内核。

记忆随时间下沉。最近几轮留在上下文，旧一点转成文档，再旧蒸馏成图里的三元组。

这里记忆按场面划分，不按会话。场面由通道、对话对象、用到的工具和时段构成，
回到同一个话题时上次的结论会被召回。

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

本项目以 **GNU Affero 通用公共许可证第 3 版（AGPL-3.0-only）** 发布，全文见 [LICENSE](LICENSE)。

它是 GPL 家族里**传染性最强**的一档：不仅分发时须提供完整对应源码，
**通过网络提供服务时也要向使用者提供源码**（§13 Remote Network Interaction）。
即：任何人把改过的 HologramAgent 对外提供网络服务，都必须让该服务的使用者拿到改动后的源码。

插件与本项目通过公开 [hologramagentsdk](https://github.com/JianFeeeee/hologramagentsdk) 静态链接
（SDK 源码会进入插件二进制），但那个仓以 **MIT** 发布。MIT 是宽松许可，拿到授权的代码不继承
本项目的 AGPL。因此**外部插件不是本项目的衍生作品**，作者可自行选择许可（含闭源、商业、私有），
既不必同许可、也不受 §13 网络条款约束。第三方插件生态的安全与活跃正建立在这条之上。

边界很清楚：**AGPL 覆盖内核与随包内置插件**（`homed`、`internal/`、`internal/plugins/` 下的内置插件）；
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
