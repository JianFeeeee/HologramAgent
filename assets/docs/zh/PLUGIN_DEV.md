[English](../en/PLUGIN_DEV.md) | **中文**

# HologramAgent 插件开发指南

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 概述

HologramAgent 的所有外部交互能力都来自插件。插件通过 `PluginSDK`（Go API）与内核交互。

**SDK 仓库**：插件开发工具、模板代码和示例插件统一托管在
[homeagentsdk](https://github.com/JianFeeeee/homeagentsdk) 仓库。

```bash
git clone https://github.com/JianFeeeee/homeagentsdk.git
cd homeagentsdk
```

每个插件实现一个三方法接口：

```go
type Plugin interface {
    Name() string
    Start(sdk *PluginSDK) error
    Stop() error
}
```

### 三种开发方式

| 方式 | 适用场景 | 复杂度 |
|------|---------|--------|
| **子进程插件（推荐）** | 独立分发的第三方插件 | 中等，使用 `hmapdev` 工具链生成 |
| **内置插件** | 随 HologramAgent 一起发布 | 简单，需合入主仓库 |
| **Lua 脚本插件** | 轻量快速原型 | 简单，使用 `hmapdev init --lua` 生成 |

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 一、快速开始：使用 hmapdev 工具链

`hmapdev` 是 SDK 仓库提供的统一插件开发工具链，支持 Go 和 Lua 两种插件类型，
最终产出 `.hmap` 插件包（工具名即来自这个包格式）。

> 改名说明：1.2.0 起工具链由 `plugindev` 更名为 `hmapdev`；SDK 存储目录同时由
> `~/.homeagent/plugindev/sdk` 迁到 `~/.homeagent/hmapdev/sdk`（旧目录会自动继续沿用）。

### 安装

```bash
cd homeagentsdk/tools/hmapdev
go build -o hmapdev
# 将 hmapdev 加入 PATH 或直接使用
# 也可从 SDK 的 release 附件下载预编译二进制（hmapdev_linux_amd64 等）
```

### SDK 版本管理

`hmapdev sdk` 子命令管理本地 SDK 版本：

```bash
hmapdev sdk list       # 列出已安装的 SDK 版本
hmapdev sdk current    # 显示当前使用的 SDK 版本
hmapdev sdk latest     # 显示最新可用版本
hmapdev sdk install v1.2.0  # 安装指定版本
hmapdev sdk use v1.2.0      # 切换使用版本
hmapdev sdk path       # 显示当前 SDK 路径
```

SDK 存储在 `~/.homeagent/hmapdev/sdk/<version>/`，`hmapdev init` 自动读取当前 SDK 版本填充 `go.mod`。

### 源码调试

`hmapdev debug` 直接用解释器执行插件源码并输出调用轨迹，无需编译环境：

```bash
hmapdev debug [dir]   # dir 默认当前目录
```

### 创建 Go 插件

```bash
hmapdev init myplugin
cd myplugin
# 编辑插件代码
vim plugin.go
# 编译打包
hmapdev build          # 默认多平台 bundle（见下节）
# 输出: dist/myplugin_bundle.hmap
# 单平台构建：
hmapdev build --no-bundle
# 输出: dist/myplugin_linux_amd64.hmap (或 windows_amd64)
```

### 创建 Lua 插件

```bash
hmapdev init myluaplugin --lua
cd myluaplugin
# 编辑插件代码
vim main.lua
# 本地测试
lua main.lua
# 编译打包
hmapdev build
# 输出: dist/myluaplugin_lua.hmap
```

### 模板项目结构

**Go 插件**：

```
myplugin/
├── plg.json       — 插件元信息（名称、版本、入口、目标平台 targets）
├── plugin.go      — 插件实现（Plugin 接口 + 导出函数 NewPlugin）
├── go.mod         — Go 模块定义
├── README.md      — 说明文档
└── thirdpart/     — 外部源码存放目录（可选）
```

编译时自动生成子进程运行时文件（`z_proc_gen.go` 等），无需手动创建。

**Lua 插件**：

```
myluaplugin/
├── plg.json       — 插件元信息（entry: "main.lua", targets: "lua"）
├── main.lua       — 插件实现（Plugin 接口的 Lua 版本）
├── sdk.lua        — SDK 模拟层（支持 `lua main.lua` 独立测试）
└── README.md      — 说明文档
```

### 编译打包

`hmapdev build` 会自动完成编译和打包：

```bash
cd myplugin
hmapdev build                      # 默认 bundle 模式（多平台合集）
hmapdev build --no-bundle          # 单平台构建（仅当前 plg.json targets）
hmapdev build --target linux/amd64 # 在 targets 基础上追加一个目标
hmapdev build --outdir dist        # 指定输出目录（默认 dist）
hmapdev build --sdk-path <path>    # 指定 SDK 路径（覆盖 go.mod replace）
hmapdev build --replace <mod@path> # 追加 go.mod replace 指令（可多次）
```

执行过程：
1. 读取 `plg.json` 的 `targets`/`bundle` 字段确定构建目标（bundle 模式优先，见下节）
2. 自动生成子进程运行时代码（`z_proc_gen.go` + `z_proc_shm_unix.go` + `z_proc_shm_windows.go`）
3. **Go 插件**：执行 `go build`（普通可执行文件，`CGO_ENABLED=0`）
4. **Lua 插件**：直接打包源码，无需编译（打包内容：`plugin.json` + `main.lua`，以及可选的 `README.md`、`LICENSE`、`thirdpart/*.lua`）
5. 生成 `plugin.json` 输出清单
6. 打包为 `.hmap` 分发包（zip 格式，内含 `plugin.json` + 二进制）

### plg.json（项目配置）vs plugin.json（输出清单）

| 文件 | 用途 | 关键字段 |
|------|------|---------|
| `plg.json` | 项目元信息，由开发者维护 | `targets` — 单平台构建目标（如 `"linux/amd64,windows/amd64"`）；`bundle` — 多平台合集开关（默认 `true`）|
| `plugin.json` | 构建产物清单，`hmapdev build` 自动生成 | `entry` — 入口文件名；`platforms` — 声明的支持平台 |

每个目标生成单独的 `.hmap`。子进程插件是普通可执行文件，**不分平台后缀**：

| 平台 | 二进制 |
|------|--------|
| Linux / macOS / Windows | `plugin.bin` |

bundle 包内按 `plugin.bin.<goos>.<goarch>` 区分各平台，安装时内核挑当前平台
那份重命名为 `plugin.bin`。

> ⚠️ **v1.0.0 破坏性变更**：外部插件从 C ABI 动态库改为**子进程 + 共享内存**。
>
> - `plugin.so` / `plugin.dylib` / `plugin.dll` **不再被加载**。新内核遇到旧产物
>   会跳过并报可操作错误，不崩溃。
> - **业务代码不需要改一行**——公开 SDK 接口零改动，只需用新版 `hmapdev`（原 `plugindev`）重编。
> - `plg.json` 的 `entry` 字段对 Go 插件**已无意义**（写着 `plugin.so` 也无妨），
>   它现在只用于区分 Lua 插件。
> - 产物不再需要 cgo，交叉编译无需目标平台 C 工具链。
> - Windows 从「只下发 3 个 stage 字段、无写回」升级到 16 字段全可见 + 写回，
>   与 Unix 共用同一套 RPC 实现。

### 构建目标与多平台打包（bundle）

**`hmapdev build` 默认就是 bundle 模式**（`plg.json` 未显式写 `"bundle": false` 时）：一次编译 linux/amd64 + darwin/amd64 + windows/amd64，生成包含所有平台二进制的单 `.hmap`，输出清单自动添加 `platforms` 字段。安装时核心自动选择当前平台的二进制，跳过其他平台。

```bash
hmapdev build              # 默认 bundle，输出 dist/myplugin_bundle.hmap
hmapdev build --bundle     # 显式开启 bundle（同上）
hmapdev build --no-bundle  # 关闭 bundle，按 plg.json 的 targets 逐平台构建
```

注意：
- bundle 模式下 `plg.json` 的 `targets` 字段被忽略，固定构建上述三个平台
- 跨平台交叉编译需要对应工具链（如 Linux 上构建 darwin 需 clang/macOS SDK）；缺少工具链时编译会失败，此时使用 `--no-bundle` 只构建当前平台
- 单平台输出文件名：`{name}_{os}_{arch}.hmap`，如 `myplugin_linux_amd64.hmap`

输出在 `dist/` 目录：
```
dist/
├── myplugin_bundle.hmap           # 默认 bundle：多平台合集
├── myplugin_linux_amd64.hmap      # --no-bundle 后：Linux 版
├── myplugin_windows_amd64.hmap    # --no-bundle 后：Windows 版
├── myplugin_darwin_amd64.hmap     # --no-bundle 后：macOS 版
└── myplugin_lua.hmap              # Lua 插件
```

### 安装部署

通过 PluginMgr HTTP API 安装，支持三种方式：

```bash
# 1. 从 URL 安装（仅支持 http/https，流式下载不落盘）
curl -X POST http://127.0.0.1:9876/plugins \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/myplugin.hmap"}'

# 2. 从本地路径安装（读取指定文件，不移动原文件）
curl -X POST http://127.0.0.1:9876/plugins \
  -H "Content-Type: application/json" \
  -d '{"path": "/path/to/myplugin.hmap"}'

# 3. 直接上传二进制
curl -X POST http://127.0.0.1:9876/plugins \
  --data-binary @dist/myplugin.hmap
```

`9876` 为 pluginmgr 本地监听端口（默认仅监听 127.0.0.1，无鉴权）。

安装后需调用 `/api/v1/plugins/reload` 或重启内核生效。

也可通过 WebUI 插件管理页面上传安装，或走 WebUI 的 HTTP API（端口默认 8080，需 `api_key` 鉴权，内部代理到 pluginmgr）：

```bash
curl -X POST http://127.0.0.1:8080/api/v1/plugins \
  -H "Authorization: Bearer <api_key>" \
  -H "Content-Type: application/json" \
  -d '{"path": "/path/to/myplugin.hmap"}'
```

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 二、Go 插件开发详解

### 插件接口

```go
package main

import "github.com/JianFeeeee/homeagentsdk/sdk"

type Plugin struct {
    name string
    sdk  *sdk.PluginSDK
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
    p.sdk = s
    // 注册配置项、工具、阶段钩子等
    return nil
}

func (p *Plugin) Stop() error {
    // 清理资源
    return nil
}

// NewPluginFactory 创建插件实例（由 main.go 或 Windows bridge 调用）
func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
    return &Plugin{name: name}, nil
}
```

### 入口点

`hmapdev init` 生成的 `plugin.go` 中直接包含 `NewPlugin` 导出函数，它是内核加载插件时的入口：

```go
func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
    return &Plugin{name: name}, nil
}
```

编译时 `hmapdev build` 自动生成子进程运行时代码（`z_proc_gen.go` 平台无关 + `z_proc_shm_unix.go` / `z_proc_shm_windows.go` 平台特定），无需手动编写。三平台共享同一入口与同一套 RPC 逻辑，仅跨进程资源传递机制不同（Unix 继承 fd，Windows 命名内核对象）。

### PluginSDK 核心 API

#### 工具注册 — 让 LLM 可调用你的能力

```go
s.RegisterTool("weather_query", sdk.ToolDef{
    Name:        "weather_query",
    Description: "查询指定城市的天气",
    NoMemory:    false,                            // false=输出参与记忆计算，true=跳过计算
    // Cleaner:  func(output string) string {     // 可选：输出参与向量化/jieba/蒸馏前的清洗
    //     return extractJSON(output, "content")
    // },
    Parameters: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "city": map[string]interface{}{
                "type":        "string",
                "description": "城市名称，如 北京",
            },
        },
        "required": []string{"city"},
    },
}, func(args map[string]interface{}) (interface{}, error) {
    city, _ := args["city"].(string)
    return map[string]interface{}{
        "city":    city,
        "temp":    25,
        "weather": "晴",
    }, nil
})
```

##### NoMemory 与 Cleaner 说明

`NoMemory` 和 `Cleaner` 是 `ToolDef` 上的两个可选字段，控制工具输出在**记忆计算层**（向量化、jieba 分词、蒸馏）中的行为：

- **`NoMemory`**（默认 `false`）：设为 `true` 时，工具输出不参与任何记忆计算（向量、分词、蒸馏），但原文保留在 Context 和 Document 中，LLM 注意力不受影响。适用场景：`cmd_run`（命令输出含不可控噪音）、文件上传/删除等纯操作工具。

- **`Cleaner`**（可选）：函数签名 `func(output string) string`。注册后，工具输出在参与向量化/jieba/蒸馏前先经过此函数过滤。典型用途：SQL 查询去前缀、JSON 包裹提取 `content` 字段。原文始终不变，Cleaner 只影响计算层输入。

决策矩阵：

```
工具输出 → 对 LLM 注意力有信号价值？
  ├── 否 → NoMemory=true（输出保留原文，跳过计算层）
  └── 是 → 有可控噪音？
       ├── 是 → Cleaner 过滤后参与计算
       └── 否 → 正常记忆，无需额外处理
```

> **注意**：`Cleaner` 是 Go `func` 类型（`json:"-"`），不能跨进程序列化，因此 C/C++/Rust 等远程插件无法使用。**Lua 插件不受此限**：def 表中直接传 Lua 函数即可（`cleaner = function(text) return text end`），Go 桥接层会在计算层调用时逐次回调 Lua。

#### 阶段钩子 — 干预消息处理流

7 个阶段：

| 阶段 | 时机 | 用途 |
|------|------|------|
| `on_input` | 消息刚到达 Agent | 黑名单、限流、短路 |
| `pre_action` | 即将调用 LLM | 注入上下文 |
| `post_action` | LLM 返回结果 | 修改输出/工具列表 |
| `before_toolcall` | 工具调用前 | 审计、拒绝、改参 |
| `after_toolcall` | 工具执行后 | 脱敏、改写结果 |
| `before_output` | 输出前 | 格式适配、泄漏清洗 |
| `after_output` | 输出后 | 统计日志 |

```go
// 全局监听：所有插件的阶段事件
s.RegisterStage(sdk.StagePreAction, func(ctx *sdk.StageContext) error {
    ctx.Lock()
    ctx.ContextMsgs = append(ctx.ContextMsgs, map[string]interface{}{
        "role":    "system",
        "content": "注入的上下文内容",
    })
    ctx.Unlock()
    return nil
})

// 仅自己工具：仅监听自己注册的 tool 的 before_toolcall/after_toolcall
s.RegisterStage(sdk.StageBeforeToolcall, myHandler, sdk.StageScopeOwnTools)
```

#### 配置管理

```go
// 注册配置项定义
s.Settings().RegisterDef(sdk.ConfigDef{
    Key:         "plugin.myplugin.api_key",
    Default:     "",
    Type:        "string",
    DisplayName: "API Key",
    Description: "API 密钥",
    Category:    "myplugin",
})

// 读写配置
val, err := s.Settings().Get("api_key")
s.Settings().Set("api_key", "new-value")

// 读取核心配置
s.Settings().GetCore("llm.model")

// 读取其他插件配置
s.Settings().GetPlugin("other_plugin", "some_key")
```

#### 输入投递

```go
// 普通投递（按序处理）
s.InjectText(source, channel, text string)

// 中断投递（可打断当前 LLM 处理）
s.InjectInterruptText(source, channel, text string)

// 不记入记忆
s.InjectTextNoMemory(source, channel, text string)
```

#### 输入通道注册 — 声明外部消息源

```go
s.RegisterInputChannel("qq", sdk.ChannelDef{
    NoMemory: true,
    Cleaner: func(text string) string {
        return strings.TrimSpace(text)
    },
})
```

`ChannelDef` 控制通道在记忆计算层的行为：

| 字段 | 默认 | 说明 |
|------|------|------|
| `NoMemory` | `false` | 通道输入/输出不参与向量化/关键词/蒸馏，原文保留 |
| `Cleaner` | `nil` | `func(string) string` 计算层过滤（不改原文） |

噪声源通道（QQ 群消息、RSS 等）建议 `NoMemory: true`。

#### 输出通道注册 — 声明输出目的地

```go
s.RegisterOutputChannel("email", 1, "邮件发送", sdk.ChannelDef{
    NoMemory: true,
}, func(args map[string]interface{}) (interface{}, error) {
    to, _ := args["to"].(string)
    subject, _ := args["subject"].(string)
    body, _ := args["body"].(string)
    return map[string]interface{}{"status": "sent"}, nil
})
```

参数：`name`（路由名）、`caps`（1=文本/2=富文本/4=文件/8=图片）、`desc`、`def`（ChannelDef）、`handler`（处理函数）。

#### 事件订阅

```go
import "github.com/JianFeeeee/homeagentsdk/sdk"

unsub := s.Events().Subscribe(sdk.EventToolCall, func(evt *sdk.Event) {
    log.Printf("工具被调用: %v", evt.Payload)
})
defer unsub()
```

#### 与其它插件联动 — 走事件总线，不要依赖内核内部组件

内核不承载业务域语义（终端、设备状态、媒体库都是插件的职责），因此插件之间的
联动应当**订阅事件总线**，而不是读内核里的某个状态组件——后者会让同一件事出现
第二份真相，也会把业务逻辑重新塞回内核。

```go
// 面板类插件（如 WebUI/CLI）：订阅终端事件，自己攒视图
// 注意：terminal_output 目前没有公开常量（属内核内部事件类型），
// 外部插件用字符串形式订阅；内置插件可用 internal sdk 的 EventTerminalOutput。
unsubTerm := s.Events().Subscribe(sdk.EventType("terminal_output"), func(evt *sdk.Event) {
    // evt.Payload: terminal_id / command / output / running
})
unsubTool := s.Events().Subscribe(sdk.EventToolCall, func(evt *sdk.Event) {
    // evt.Payload: tool / args / status（cmd_run、terminal_create/close 等）
})
defer unsubTerm()
defer unsubTool()
```

稳定的两条事件：

| 事件 | 发布者 | 用途 |
|---|---|---|
| `EventTerminalOutput` | agentcli | 终端输出与生命周期（创建/关闭/超时） |
| `EventToolCall` | 内核 | 工具调用（含 `cmd_run`、`terminal_create/close`） |

★ **外部插件只能订阅，不能发布**：`Publish` 属于不对外授予的能力
（`withheldCapabilities`），所以「某项能力的所有者需要向总线广播状态」这类插件
（如 agentcli 发终端事件）必须内置。如果你的插件只是消费者，订阅即可。

参考实现：`internal/pluginview`（WebUI 与 CLI 共用的终端视图）。它演示了视图
应当怎么从事件攒出来，以及「视图不是权威、晚启动会缺早期事件」的边界。

#### 能力访问

```go
// 图记忆（实体-关系存储）
entities, relations, err := s.Memory().Recall([]string{"关键词"}, 2)

// 文档记忆（向量存储）
docs := s.DocMemory().Query("查询文本", 3)

// 知识库
results, err := s.Knowledge().Search("查询", 5)

// LLM 源管理
s.LLM().ListSources() // 返回 []string
s.LLM().SetSource("deepseek")
```

#### 事件订阅（内置插件）

```go
// 订阅系统事件，返回取消订阅函数
unsub := s.Subscribe("tool_call", func(evt *events.Event) {
    log.Printf("工具被调用: %v", evt.Payload)
})
defer unsub()

// 发布事件
s.Publish(&events.Event{
    Type:    "custom_event",
    Payload: map[string]interface{}{"key": "value"},
})
```

#### IO 通道管理（内置插件）

```go
// 注册通道（绑定设备驱动），dev 必须实现 agentIO.Device 接口：
//   Name() string
//   Type() DeviceType
//   Description() string
//   Tools() []ToolDef
//   Execute(tool string, args map[string]interface{}) (interface{}, error)
//   Start() error
//   Stop() error
//   OutputCapabilities() OutputCapability
s.RegisterChannel("mydevice", deviceImpl)

// 注销通道
s.UnregisterChannel("mydevice")

// 列出所有通道
channels := s.ListChannels()
```

#### 输入投递（内置插件）

```go
// 排队投递（按序处理）
s.InjectInput(source, channel, eventType string, payload map[string]interface{})

// 同步投递（等待响应）
resp := s.InjectInputSync(source, channel, eventType string, payload map[string]interface{})

// 中断投递（可打断当前 LLM 处理）
s.InjectInterrupt(source, channel, eventType string, payload map[string]interface{})

// 同步文本投递（快捷方式）
resp := s.InjectTextSync(source, channel, text string)
resp := s.InjectTextSyncNoMemory(source, channel, text string)

// 获取输出通道
outputCh := s.OutputChan()
```

> **注意**：`Subscribe`、`Publish`、`RegisterChannel`、`UnregisterChannel`、`ListChannels`、`InjectInput`、`InjectInputSync`、`InjectInterrupt`、`InjectTextSync`、`InjectTextSyncNoMemory`、`OutputChan` 这些方法仅在内置插件中可用（`internal/sdk` 包），外部动态插件无法访问。外部插件请使用 `InjectText`、`InjectInterruptText`、`InjectTextNoMemory` 等公共 API。

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 三、Lua 插件开发详解

Lua 插件适合轻量级快速原型，无需 Go 编译环境，修改后直接重启内核即可生效。

### 执行模型

Lua 插件运行在内核进程内的 gopher-lua 解释器中（单 Lua 状态 + 互斥锁），与 Go 插件的执行模型有本质区别：

- **被动回调模型**：`main.lua` 仅在加载时执行一次，此后插件的工具、阶段钩子、输出/输入通道、注册 API 全部由内核事件驱动回调 Lua 函数；插件不能自己启动后台任务。
- **无并发/无常驻服务能力**：Lua 侧没有 goroutine、协程调度、`os`/`io` 库和 socket 监听能力，唯一主动出站通道是 `sdk.http.get/post`（同步请求）。任何阻塞循环都会持锁卡死该插件的所有调用。
- **常驻服务（如监听端口、后台轮询、定时任务）请使用 Go 插件**（工具链编译的 `plugin.bin`，可自行启动 goroutine，参见 webui/cli 插件）。Lua 插件的等价做法是事件驱动：注册工具/阶段钩子/通道由内核回调，或经 `sdk.http` 与外部进程交互。

### 插件结构

```lua
-- main.lua
local plugin = {
  name = "myluaplugin"
}

function plugin.start(sdk)
  sdk.log("info", "myluaplugin starting...")

  sdk.register_tool("myluaplugin_hello", {
    description = "A hello world tool",
    parameters = {
      type = "object",
      properties = {}
    }
  }, function(args)
    return { content = "Hello from myluaplugin plugin!" }
  end)

  sdk.log("info", "myluaplugin started")
end

function plugin.stop()
  sdk.log("info", "myluaplugin stopped")
end

return plugin
```

### SDK 模拟层

`sdk.lua` 提供纯 Lua 的 SDK 模拟实现，支持 `lua main.lua` 独立测试：

```bash
lua main.lua
# 输出:
# [lua-plugin] info: myluaplugin starting...
# [lua-plugin] register_tool: myluaplugin_hello
# [lua-plugin] info: myluaplugin started
```

在内核中运行时，`sdk.*` 全局变量由 Go 层注入，所有 `-- !impl` 标记的函数会被替换为真实实现。

### Lua SDK API

Lua 插件的 `sdk.*` API 与外部插件（工具链编译的 `plugin.bin` 子进程）能力对齐至 **SDK 1.3.0**（需内核 **1.4.0+**，也在 `v1.3.11` 的 Lua 对齐补丁中回填）：注册类函数调用即时报错（抛 Lua error），数据类函数统一返回 `(result, err)`，`err` 为 nil 表示成功。核心未装配的子系统（如 SocialAPI）返回空值而非报错。

> 历史提醒：1.1–1.3 的媒体/注入标志位/优先级能力曾长期只在 Go 侧，Lua 侧静默缺失。现已全量对齐，并由 `internal/plugin/lua_surface_test.go` 的契约测试守住「mock 承诺的每个函数都有运行时绑定」。

**注册类**

| 函数 | 说明 |
|------|------|
| `sdk.log(level, msg)` | 日志输出 |
| `sdk.register_tool(name, def, handler)` | 注册工具；`def` 支持 `description`、`parameters`、`no_memory`、`context_policy`（`"none"`/`"prune"`）、`cleaner` |
| `sdk.register_stage(stage, handler, scope)` | 注册阶段钩子；`scope` 为 `nil`/`"global"`（默认）或 `"own_tools"`（仅 `before_toolcall`/`after_toolcall` 且工具属于本插件时触发） |
| `sdk.register_api(name)` | 注册 API |
| `sdk.register_output_channel(name, caps, desc, def, handler)` | 注册输出通道；`def` 支持 `no_memory`、`context_policy`、`cleaner` |
| `sdk.register_input_channel(name, def)` | 注册输入通道；`def` 同上 |
| `sdk.unregister_output_channel(name)` | 注销输出通道（随资源生灭的动态通道，如远程设备）；返回 `(nil, err)` |
| `sdk.set_auto_restart(enabled)` | 崩溃时内核自动拉起插件 |

**阶段钩子上下文**

`register_stage` 的 handler 收到完整上下文（与外部插件一致）：`raw_message`、`user_id`、`group_id`、`phase`、`llm_text`、`reasoning_content`、`final_text`、`no_memory`、`context_msgs`、`token_usage`、`memory`、`extra`、`errors`、`response`（已响应时）、`tool_calls`、`tool_results`。

**Stage 写回**：handler 收到的 `ctx` 是引用 table——在 handler 内直接修改可写回字段并同步至内核 `StageContext`（与子进程外部插件能力对齐）：

```lua
sdk.register_stage("on_input", function(ctx)
  ctx.raw_message = "[清洗]" .. ctx.raw_message   -- 修改输入，内核会采用
end)

sdk.register_stage("post_action", function(ctx)
  ctx.llm_text = ctx.llm_text .. "[尾部标记]"       -- 修改 LLM 输出
  ctx.tool_results = { { call_id = "x", result = "改写结果" } } -- 改写工具结果
end)
```

可写回字段：`raw_message`、`llm_text`、`final_text`、`user_id`、`group_id`、`no_memory`、`response`、`tool_calls`、`tool_results`。其余字段为只读。

**IO 与配置**

| 函数 | 说明 |
|------|------|
| `sdk.get_setting(key)` / `sdk.set_setting(key, value)` | 本插件配置读写 |
| `sdk.settings.get_core/set_core/list_core(key)` | 核心配置读写 |
| `sdk.settings.get_plugin/set_plugin/list_plugin(plugin, key)` | 其他插件配置读写 |
| `sdk.settings.list/defs/dump/plugins(prefix)` | 配置查询 |
| `sdk.settings.register_def(def)` | 注册配置项定义（WebUI 展示） |
| `sdk.inject_text(source, channel, text)` | 投递文本消息 |
| `sdk.inject_interrupt(source, channel, text)` | 中断投递 |
| `sdk.inject_text_no_memory(source, channel, text)` | 免记忆投递 |
| `sdk.inject_text_opts` / `sdk.inject_interrupt_opts(source, channel, text, opts)` | 带标志位投递；`opts = { no_memory=bool, context_policy="none"|"prune", cleaner_name=string, priority="L1".."L3" }` |
| `sdk.inject_input_sync(source, channel, text)` | ⚠️ **Lua 中不可用**：恒返回 `(nil, err)`。它要等本轮回复而 Lua 回调持有插件锁，必然自锁。需要同步等待请用 Go 插件，或用下面的异步注入 |
| `sdk.inject_input_sync_opts(source, channel, text, opts)` | 同上（不可用） |
| `sdk.inject_input_media(source, channel, text, blocks)` | 注入文本 + 多模态内容块 |
| `sdk.inject_input_media_opts(source, channel, text, blocks, opts)` | 同上带标志位 |
| `sdk.inject_input_media_sync` / `..._sync_opts(...)` | ⚠️ **Lua 中不可用**（同 `inject_input_sync`） |
| `sdk.inject_interrupt_media(source, channel, text, blocks)` | 带媒体的中断注入 |
| `sdk.inject_interrupt_media_opts(source, channel, text, blocks, opts)` | 同上带标志位 |
| `sdk.set_tool_blocks(blocks)` | 设置下一轮 tool message 携带的多模态内容块（模型据此看图/听音频） |

`blocks` 每项形如：`{ type="text", text="..." }`、`{ type="image_url", image_url={ url="...", detail="high" } }`、`{ type="audio_url", audio_url={ url="..." } }`。`opts` 缺省即零值（记入记忆 + 不裁剪），与三参数版本等价。

**数据类（与子进程外部插件对齐，均返回 `(result, err)`）**

| 子表 | 函数 |
|------|------|
| `sdk.memory.*` | `recall(query, depth)`、`commit({triples})`（triple 支持 `subject/relation/object/confidence/subject_type/object_type/sentence_text/media_digests`）、`introspect()`、`merge(source, target)`、`purge(criteria, hard)` |
| `sdk.doc.*` | `query(text, top_k)`、`insert({id,title,content})`、`insert_with_media(doc, attachments)`、`remove(id)`、`stats()` |
| `sdk.knowledge.*` | `search(query, limit)`、`add(tag, content)`、`list()` |
| `sdk.text_memory.*` | `append({role,content,timestamp,channel,attachments})` |
| `sdk.llm.*` | `list_sources()`、`set_source(name)`、`current_source()` |
| `sdk.social.*`（只读） | `get_person(name)`、`get_network(name, depth)`、`get_trait(name, trait)`、`get_relations(name)`、`list_persons()` |
| `sdk.events.*` | `subscribe(event_type, handler)` → 返回取消订阅函数；handler 收到 `{type,source,timestamp,payload}` |
| `sdk.plugin_mgr.*` | `reload_one(name)`、`list_loaded()`、`is_disabled(name)` |
| `sdk.json.*` | `encode(val)`、`decode(str)` |
| `sdk.http.*` | `get(url)`、`post(url, body, content_type)` |

`attachments` 每项：`{ digest=, mime=, name=, data=<base64> }`；带 `data` 是新内容（落进内容寻址存储），只带 `digest` 是引用已有内容。

> `sdk.events.subscribe` 的回调在内核事件发布 goroutine 上执行，且 Lua 是单状态 + 互斥锁——**回调内只做轻量转发，不可阻塞**，否则会卡死本插件的全部调用。

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 四、内置插件

内置插件使用 `init()` 自注册方式，编译进内核，无需单独部署。

### 目录结构

```
internal/plugins/yourplugin/
    plugin.go       — 插件主文件
```

### 最小插件示例

```go
package yourplugin

import (
    "gitcode.com/JianFeeeee/HologramAgent/internal/plugin"
    sdk "gitcode.com/JianFeeeee/HologramAgent/internal/sdk"
)

func init() {
    plugin.RegisterFactory("yourplugin", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
        return New(name), nil
    })
}

type Plugin struct {
    name string
}

func New(name string) *Plugin {
    return &Plugin{name: name}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
    // 在这里初始化插件：启动 goroutine、注册工具、订阅事件等
    return nil
}

func (p *Plugin) Stop() error {
    // 清理资源
    return nil
}
```

### 注册到内核

在 `internal/plugins/all.go` 中添加空白导入：

```go
package plugins

import (
    _ "gitcode.com/JianFeeeee/HologramAgent/internal/plugins/yourplugin"
    // ... 其他插件
)
```

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 五、最佳实践

1. `Start()` 非阻塞 — goroutine 启动长任务，不要阻塞 Start
2. `Stop()` 清理资源 — 关连接、停 goroutine、取消订阅
3. 工具名唯一 — 建议插件名前缀避免冲突
4. handler 返回 `error` 时 LLM 会收到并可能重试
5. 打断用 `InjectInterruptText`，普通投递用 `InjectText`
6. 配置用 `Settings().Get/Set`，不要硬编码
7. 外部 Go 插件独立编译，不依赖内核版本；内置插件才需随内核重新编译

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 六、插件管理

### CLI 命令

```bash
/plugin list                  # 列出所有插件及状态（已加载/已禁用）
/plugin disable <name>        # 禁用插件（立即生效，不再接收输入）
/plugin enable <name>         # 启用插件（重启后恢复加载）
/plugin reload                # 重载所有插件
```

### WebUI

Dashboard 插件列表的操作列提供「禁用/启用」按钮。禁用 WebUI 自身时会弹出确认对话框，防止误操作。

### 内置插件 API

```go
pmgr := s.PluginMgr()
pmgr.DisablePlugin("qq", "admin")       // 禁用
pmgr.EnablePlugin("qq")                  // 启用
list := pmgr.ListDisabledPlugins()       // 列出已禁用插件
loaded := pmgr.ListLoadedPlugins()       // 列出已加载插件
pmgr.IsPluginDisabled("qq")              // 检查是否已禁用
pmgr.ReloadPlugins()                     // 重载所有插件
```

内部机制：禁用记录存储在 SQLite `disabled_plugins` 表（`name`, `disabled_at`, `disabled_by`），禁用立即生效（插件不再接收输入），完全卸载需重启内核。

> **注意**：`PluginMgr()` 返回的接口**只有 3 个方法**（`ReloadOne` / `ListLoadedPlugins` /
> `IsPluginDisabled`）。启用/禁用/卸载/重载全部（`EnablePlugin` / `DisablePlugin` /
> `RemovePlugin` / `ReloadPlugins`）以及内置插件判定（`IsBuiltinPlugin`）只在内核内部
> 的 `internal/sdk.PluginManager` 上，外部插件拿不到——需重载插件请调 `ReloadOne`。
> 两个接口叫相似的名字但**不是同一个**：`sdk.PluginMgrAPI`（公开，3 方法）与
> `internal/sdk.PluginManager`（内部，9 方法）。

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 七、示例插件参考

### SDK 仓库示例（`homeagentsdk/example/`）

| 示例 | 类型 | 特点 |
|------|------|------|
| [weather](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/weather) | Go | 天气查询（wttr.in），演示 NoMemory/Cleaner/阶段钩子/通道/文本记忆 |
| [luademo](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/luademo) | Lua | Lua 全功能示例，覆盖 v0.8.0 Lua SDK 全部 API 面 |
| [qq](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/qq) | Go | NapCat OneBot 对接，24 个工具，输入/输出通道完整对接 |
| [memo](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/memo) | Go | 备忘管理，PreAction 注入 + 定时打断双提醒 |
| [files](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/files) | Go | 文件系统操作，4 种写入模式，沙箱隔离 |
| [browser](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/browser) | Go | 网络搜索、网页抓取（SSRF）、浏览器渲染（合并自 web/webfetch） |
| [bili](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/bili) | Go | B 站视频下载（yt-dlp） |
| [editdoc](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/editdoc) | Go | Office 文档编辑与格式转换 |
| [a2a](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/a2a) | Go | Agent-to-Agent 协议 |
| [ocr](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/ocr) | Go | 离线文字识别（Tesseract） |
| [sanitizer](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/sanitizer) | Go | 输出清洗过滤器 |
| [calendar](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/calendar) | Go | 日历管理 |
| [rss](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/rss) | Go | RSS 订阅 |
| [ai_image](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/ai_image) | Go | AI 图片生成 |
| [music](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/music) | Go | 音乐播放 |
| [vikunja](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/vikunja) | Go | Vikunja 任务管理对接（项目/任务/标签 CRUD） |
| [vanblog](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/vanblog) | Go | VanBlog 博客发布与管理 |
| [deepsearch](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/deepsearch) | Go | 多轮深度检索（逐层聚焦 + 引用汇总） |
| [acp](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/acp) | Go | Agent Client Protocol 对接（外部编辑器/IDE 驱动本 agent） |
| [recoverydiag](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/recoverydiag) | Go | 故障诊断五件套（分诊/sqlite 校验/日志签名/diff/结论排序），failback 模式的核心插件 |
| [plugindev](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/plugindev) | Go | 插件脚手架：生成工程、构建、安装，供 agent 自助开发插件 |

### 内置插件

| 插件 | 位置 | 特点 |
|------|------|------|
| Healthcheck | `internal/plugins/healthcheck/` | 定时自检（30min）＋ 工具面自报状态，最简的工具型内置插件 |
| CLI | `internal/plugins/cli/` | Unix socket 监听 + 同步请求响应 |
| WebUI | `internal/plugins/webui/` | HTTP 服务 + 依赖注入 |

> **timer 已移出内置插件**，改为外部 hmap 插件提供（`timer_set` / `timer_list` /
> `timer_cancel` 三个工具与 `timer` 输入通道均由外部插件注册）。
>
> ★ **升级顺序**：先安装 timer 插件，再升级内核。反过来会有一段窗口期三个 timer
> 工具全部不存在——而且**不会报错**，只是工具悄无声息地消失（LLM 看不到它们，
> 于是表现为「定时提醒这个功能不见了」，日志里没有任何线索）。
>
> ⚠ **「装上去」不等于「生效」**：在**旧内核**上安装 timer 插件时，插件装载会优先走
> 内置工厂（`loadOne` 先查 `factories[name]`，命中就不再去看 `plugin.bin`），外部版本
> 根本不会被加载——`plugin_status("timer")` 依然是 `builtin=true`、`tools=["timer_set"]`。
> 真正的接管发生在**升级内核并重启之后**（内置工厂已不存在，才会动态加载外部 bin）。
> 所以正确姿势是「先落盘安装 → 再换二进制重启」，中间那次 `plgreload` 是空动作。
>
> 顺带说明外部版比原内置版更强：内置版只有 `timer_set`，进程重启即在途倒计时全丢；
> 外部版额外提供 `timer_list` / `timer_cancel`，且待触发列表跨重启保留。

---

*了解项目整体目标？查看 [OVERVIEW.md](OVERVIEW.md)。*
*了解技术架构？查看 [ARCHITECTURE.md](ARCHITECTURE.md)。*
*SDK 仓库与开发工具？查看 [homeagentsdk](https://github.com/JianFeeeee/homeagentsdk)。*
