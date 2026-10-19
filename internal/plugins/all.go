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

import (
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/agentcli"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/ai_image"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/cfgmgr"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/clawhubadapter"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/cli"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/cmd"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/files"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/healthcheck"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/localuse"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/mcp"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/multimodal"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/pluginmgr"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/remotedevice"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/seemedia"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/seq"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/skillmgr"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/webui"
)
