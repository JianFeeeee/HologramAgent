> ⚠️ **AI 辅助编程声明**：本项目代码、文档及提交历史中，部分内容由 AI 辅助生成或修改。人工已审阅关键改动，但使用时请自行评估与验证。

# HologramAgent

> **English**: [README_EN.md](./README_EN.md)

自托管 Agent 内核。跑在自己的机器上，插件以独立进程接入。

```bash
make build build-cli
./build/homed -data /tmp/ha
```

API 密钥在 WebUI（`http://localhost:8080`）里填。需要 Linux。

## 几个设计

**启动顺序在编译期定序。** 20 个内核模块各自声明 `Requires`/`Provides`，构建时算出 6 层
依赖图写进 `depgraph.bin`，digest 用 `-ldflags` 注入二进制。运行期只读图，不重算。
图与二进制不配套则拒绝启动。

**插件按设施就绪提前启动。** 声明 `requires_facilities: [config]` 的插件在第 1 层完成时就起，
不必等第 4 层 agent。实测省 35 秒，那是 agent 层做 dense backfill 750 条事件、重建索引的时间。

**两级输入、四级中断。** 排队输入无级别，任何中断可打断。中断带 L1–L4，高抢占低并将现场
压栈待恢复，同级不抢占。L4 内核独占，插件声明会被夹到 L3。抢占计数可提级，上限两档
（`preemptPromotionCap = 2`），避免低优先级任务长期得不到执行。

**记忆按场面分。** 指纹由通道、对话对象、工具、话题、时段构成，权重 1.0/1.0/0.8/0.4/0.2。
同一指纹出现到第 2 次才建场景（`minSceneEvidence = 2`）。归属阈值 0.5、唤起阈值 0.35，
唤起比归属宽松：想不起来是损失，多想一条只是多几行上下文。

## 架构

```
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

**消息进入后的七个阶段**（`internal/agent/core/stages.go`）：

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

**三层记忆**：上下文（最近 10 条恒受保护）→ 文档（冷数据下沉）→ 图数据库（蒸馏成三元组）。
媒体是图里的一等节点，不是附件。

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

内核以 **AGPL-3.0-only** 发布（[LICENSE](LICENSE)）。改过的版本如果对外提供网络服务，
必须向使用者提供源码（§13）。

自己写的插件不受此约束。插件静态链接的 [hologramagentsdk](https://github.com/JianFeeeee/hologramagentsdk)
是 **MIT**，宽松许可不继承 AGPL，所以外部插件可以闭源、可以商用，也不必开源自改。

随包第三方组件保持各自许可，全文在 `/usr/share/doc/hologramagent/licenses/`。

### 随包分发的第三方组件

| 组件 | 许可 | 位置 |
| --- | --- | --- |
| Chinese-CLIP ViT-B/16（ONNX 产物） | Apache-2.0 | `/usr/lib/hologram/models/chinese-clip-vit-b16-onnx/` |
| ONNX Runtime（`libonnxruntime.so`） | MIT | `/usr/lib/hologram/onnxruntime/` |
| jieba 词库（内嵌进二进制） | MIT | 源码 `internal/memory/jiebadict/` |
| Go 依赖（go-sqlite3、gojieba、bubbletea 等） | MIT / BSD-3 / Apache-2.0 | 均为宽松许可，与 AGPL-3.0 兼容 |

这些组件**保持各自原有许可**，不在本项目的 AGPL 授权范围内；发行包把它们的许可全文放在
`/usr/share/doc/hologramagent/licenses/`，并在 dep/rpm 元数据里声明本包许可为 `AGPL-3.0-only`。
