package plugins

// 编译期内置插件清单。
//
// ★ 顺序无关（各插件在 init() 里自注册），但**排序保持一致**：
// 这样 diff 里能一眼看出增删，而不是被 import 重排掩盖。
//
// ## timer 已移出本清单（2026-10-19）
//
// timer 原先是这里的编译器内置插件（internal/plugins/timer + RegisterFactory），
// 后果是改一行定时器逻辑要重编整个 homed（87MB）并重启内核 ——
// 而它是**最该独立演进**的部件之一：有自己的持久化状态、自己的触发语义、
// 自己的并发模型，却被绑死在主进程的发版节奏上。
//
// 现在它作为**外部插件**（plugins/timer，.hmap 安装）运行：
//   - 换实现只需重装插件 + 重启单个插件进程，不动主进程；
//   - 插件崩了自己被拉起，不影响 agent 主循环；
//   - 工具名 / 配置表（config_timer）/ 注入通道（timer）保持不变，
//     模型侧与已设的定时器都无需迁移。
//
// ⚠️ 不要再把 timer 加回这里：父子两份同时存在会导致同名工具注册冲突，
// 而「谁赢」取决于加载顺序 —— 那是最难排查的一类故障。
//
// ## 判定标准：只有「必须用内部 SDK 能力」的才留在本清单（2026-10-19）
//
// 内部 SDK（internal/sdk）比公开 SDK（third_party/homeagent-sdk/sdk）
// 多出一批**不对外授予**的能力面。外部插件用的是 proc 桥的收窄能力集，
// 这些接口在类型层就不存在（见 internal/plugin/proc/capability.go 的
// withheldCapabilities）。所以：
//
//   插件需要 withheld 里的能力  ⇒ **必须内置**（外置会静默缺功能）
//   插件只用公开 SDK 能力      ⇒ **应当外置**（.hmap 安装，独立演进）
//
// 正例（留内置，各有非它不可的理由）：
//   cli / webui     TrackerAPI/StatusAPI/SupervisorAPI/AdapterAPI
//   healthcheck     SelftestAPI（能构造虚拟内核实例）
//   pluginmgr       failback 恢复链必需（webui/pluginmgr/recoverydiag）
//   seq             ToolAPI（工具表直接操作）
//   remotedevice    RegisterChannel（内核 IO 通道注册面）
//   clawhubadapter  InjectInputSync（OC 主动注入）
//   agentcli        ★ EventPublish —— 见下
//   cfgmgr / skillmgr / seemedia  （保留）
//
// 反例（已外置，因为它们只要公开能力）：
//   cmd        仅 internal/plugin 的 RegisterFactory → 换成 NewPluginFactory 即可
//   localuse   仅 RegisterTool + Settings
//   mcp        仅 RegisterTool + Settings + RegisterStage
//
// ★ 终审翻案（2026-10-19，逐方法对照公开 SDK 后加回两个）：
//   kbtree     KnowledgeAPI.Subtree/CategoryCounts/SearchIn/Categories ——
//              公开 KnowledgeAPI 只有 Search/Add/List，且公开 SDK 没有
//              KnowledgeTreeView/KnowledgeCategoryCount 类型 ⇒ 必须内置
//   multimodal LLMAPI.Chat + LLMMessage/LLMContentBlock（多模态消息）——
//              公开 LLMAPI 只有 ListSources/SetSource/CurrentSource，
//              没有 Chat，也没有消息类型 ⇒ 必须内置
//   （agentcli 的 EventPublish 同理，见上）
//
// 教训：判「只用公开 SDK」不能只 grep 方法名前缀，要**逐方法对照公开接口**。
// s.Knowledge() / s.LLM() 两边同名，但接口面不同；粗看就会误判成可外置。
//
// ## ★ agentcli 为什么不能外置（2026-10-19 核实）
//
// 它靠 s.Publish(&sdk.Event{Type: EventTerminalOutput, ...}) 上报
// 终端输出与生命周期（创建/关闭/退出）。而 **EventPublish 在
// withheldCapabilities 里被明确扣留**，公开 SDK 连 Publish 方法都没有 ——
// 外置后终端输出会静默消失：WebUI 的终端面板、CLI 的 /terminal，
// 全都失去数据源，而且**不报错**。
//
// 想让 agentcli 外置，前提是先把「终端输出」从事件改为公开的输出通道
// （RegisterOutputChannel 是公开能力）。在那之前必须内置。

// ## ai_image / files 已移出本清单（2026-10-19）
//
// 与 timer 同理：它们是**纯工具型**插件（agi_image_generate / files_read 等），
// 只在该被调用时做事，没有任何「外部事件驱动主动注入」的行为，
// 因此没有必须常驻的理由，也没有随内核发版的必要。
//
// 二者都由 SDK 仓 `example/` 下的同版本外部插件提供（ai_image 1.3.0、
// files 1.0.0），工具名与配置表不变（config_ai_image / config_files），
// 存量配置无需迁移。
//
// ⚠️ 同一个坑：不要为「怕插件没装」而把内置版加回来。两份并存时
// loadOne 先查 factories ⇒ 内置版永远赢，外部版**装了也不生效**，
// 而且 plugin_status 会显示 builtin=true（见 PLUGIN_DEV.md 的同款说明）。
//
// ## kbtree 已归位到本清单（2026-10-19）
//
// 它本就是一个注册了 RegisterFactory 的编译期内置插件，却一直被
// `cmd/homed/main.go` 单独空白导入 —— 绕过了「内置插件清单」这个唯一入口。
// 后果：任何按 all.go 盘点内置插件的工具/文档都会漏掉它（本次文档校正
// 就踩了这个坑：先把数量写成 17，实际运行时是 18）。
// 归位后 main.go 不再单独导入它。

import (
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins/agentcli"
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins/cfgmgr"
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins/clawhubadapter"
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins/cli"
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins/healthcheck"
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins/kbtree"
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins/multimodal"
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins/pluginmgr"
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins/remotedevice"
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins/seemedia"
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins/seq"
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins/skillmgr"
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins/webui"
)
