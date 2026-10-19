package sdk

// 插件**启动形态**的声明契约（plg.json 的 auto_start / requires_facilities）。
//
// ## 三种启动形态
//
//	自动启动（缺省）      auto_start 不写或 true
//	启用但不自动启动      auto_start: false   ⇒ 待 agent 经 plugin_enable 拉起
//	禁用                  registry 的持久开关 ⇒ 连 enable 都不允许
//
// 中间那种是本文件的主要目的：**按需加载**。
//
// ## 为什么要有"启用但不自动启动"
//
// 全部插件默认自启有两个具体代价（2026-10-18 实测）：
//
//   · 启动时间：22 个示例插件全部拉起（20 个是子进程），冷启动因此多出
//     数百毫秒到秒级，并常驻 24 个进程。
//   · 上下文占用：173 个工具定义每轮请求都重发，实测约 2,809 tokens。
//
// 对**纯工具型**插件（weather / ocr / music / bili …）这些代价是白付的：
// 它们只在被调用时才做事，没人调用时只是静静占着槽位。
//
// ## ★ 但有一类插件**不能**按需加载
//
// 会**主动向 agent 注入输入**的插件必须常驻。判断依据是它是否调用了
// Inject* 系列 API 且由**外部事件**驱动（而不是 agent 调用触发）：
//
//	qq        监听 NapCat 推送，QQ 消息到达要立刻注入
//	rss       轮询订阅源，新文章要主动推送
//	calendar  定时检查提醒
//	memo      定时检查待办到期
//	a2a / acp 监听 TCP，外部 Agent 请求要应答
//	browser   长命令回调 + ticker
//	commondev sidecar 完成结果回投
//
// 把它们改成按需加载 ⇒ 功能**静默失效**：QQ 不响、提醒不来、
// 外部请求无人应答，而且**不报任何错**。这正是本仓反复吃亏的降级形态。
//
// 所以 auto_start 不是一个"性能调优开关"，而是**行为契约**：
// 声明 false 等于说"没有人调用我时，我不需要做任何事"。
//
// ## 用法
//
// 本文件只提供**常量与说明**，实际声明写在插件的 plg.json：
//
//	{
//	  "name": "weather",
//	  "auto_start": false,          // ← 纯工具型：按需加载
//	  "requires_facilities": ["llm"] // 可选：依赖就绪即可提前启动
//	}
//
// ★ 只声明 auto_start 而没有配套 skill，agent 就不知道"要先把插件拉起来"，
// 结果是把一个能用的插件变成了永远不被使用的插件。**两者必须成对出现**：
// skill 里写清楚"本工具需 plugin_enable 后调用，用完可 plugin_disable"。

// AutoStart 是三种启动形态的取值说明（供文档与工具链共用）。
//
// plg.json 里写的是 **bool**（true/false），这里给的是它的语义名，
// 避免各处用裸 true/false 导致读者不知道 false 到底意味着
// "禁用"还是"启用但不自启"。
const (
	// AutoStartOnKernel 表示伴随内核启动（缺少该字段时的默认形态）。
	AutoStartOnKernel = true
	// AutoStartOnDemand 表示启用但不自启：待 agent 显式拉起。
	//
	// ★ 用它的插件必须配 skill，否则 agent 无从得知要先拉起。
	AutoStartOnDemand = false
)

// OnDemandNeedsSkill 说明 auto_start=false 的插件为什么必须有 skill。
//
// 这条不是"建议"而是**功能完整性的一部分**：
//
//	插件写了 auto_start=false 但没配 skill
//	  ⇒ 该插件的工具不出现在工具的初始清单里
//	  ⇒ 模型不知道它存在
//	  ⇒ 它永远不会被调用，也永远不会被拉起
//
// 也就是说，缺少 skill 的按需插件等价于**没装**，但比没装更糟 ——
// 它占着磁盘和安装记录，让人以为它可用。
const OnDemandNeedsSkill = true
