package sdk

// SystemPromptAPI 是插件**声明**自身系统提示词段的接口。
//
// ## 用户模型（2026-11-04）
//
// > 每个插件都可以声明 systemprompt，可运行时更新，内核每轮重建。
//
// 于是"某个插件想告诉模型怎么用自己的工具"变成**插件自己声明的一段文本**：
//
//	skillmgr 声明 "skillmgr.skills" → 技能索引 + "调工具前先读同名 skill"
//	qq       声明 "qq.identity"    → 身份绑定规则（哪些操作以谁的身份做）
//	任意插件 声明 "<自己>.<段名>"  → 各自的说明
//
// 内核只做两件事：**存**（按段名覆盖）与**渲染**（每轮重建时按 Order 拼进
// system prompt）。内核不认识 skill、不认识 qq，也不判断内容。
//
// ## 分工：插件只声明，内核只渲染
//
//	插件：DeclarePromptSection / RemovePromptSection   （只声明）
//	内核：持有注册表 + 每轮重建 system prompt 时渲染     （只渲染）
//
// ★ 为什么声明必须由**内核**持有，而不是各 Agent 自己存一份
//
// 本仓栽过这个坑：技能索引曾经是内核 Agent 上的一个字段，由启动代码
// 手工接线到**根 agent**。于是 rtchild、驻留子各自构造时都要记得再传一遍 ——
// 漏传不报错、不崩，只是那个 agent 的提示词里一条技能说明都没有，
// 模型只能凭工具名猜用法。
//
// 生产症状（真实日志）：A2A 分诊助手（驻留子）手上没有 cmd_run
// （cmd 插件 auto_start=false，需先 plugin_start），而 cmd-tools skill
// 正文里写着"先 plugin_start cmd"。它看不到这条 skill，于是把
// terminal_create 当成一次性命令执行器 —— 每条 `cd … && echo … && grep …`
// 开一个新终端，命令结束终端就退出，terminal_read 立刻撞
// `read /dev/ptmx: input/output error`，然后为下一条命令再开一个。
//
// 结论：**能力归属内核（共享注册表），插件只声明**。这样任何 agent
// （含未来新增类型）读同一份注册表，零接线。
//
// ## 幂等语义（"可运行时更新"的正确形态）
//
// 同一 Section 重复声明 ⇒ **覆盖**，不是追加。
//
// 这不是风格问题：system prompt 每轮重建，一次任务里 stepPrepare 可能
// 被重入（上下文裁剪后回到该步）。若声明是追加，索引会越堆越多，
// 最终挤掉真正的对话历史。注册表用 map 存段，让"多份"在结构上不可能发生。
//
// ## 典型用法（skillmgr）
//
//	// Start：声明一段静态说明 + 当前索引
//	s.SystemPrompt().DeclarePromptSection(sdk.PromptSection{
//	    Section: "skillmgr.skills",
//	    Owner:   "skillmgr",
//	    Order:   100,
//	    Content: skillIndexText(),
//	})
//
//	// 技能增删/启停后：重新声明同一段即可（自动覆盖）
//	s.SystemPrompt().DeclarePromptSection(...)
//
//	// Stop / 停用：移除，避免模型去调已不存在的 skill
//	s.SystemPrompt().RemovePromptSection("skillmgr.skills")
//
// ## 名字约束
//
// 段名必须是 `<owner>.<名字>`，其中 owner 等于插件名。这条在内核侧强制
// （否则插件 A 能声明 `skillmgr.skills` 并夺走插件 B 的段）。
type SystemPromptAPI interface {
	// DeclarePromptSection 声明或更新一段系统提示词内容。
	//
	// 同 Section 重复声明会覆盖（同一 owner 只能覆盖自己的段，
	// 不能覆盖别人的）。Content 为空串等价于移除该段。
	//
	// 返回 error 的情形：
	//   · Section 不符合 `<owner>.<名字>` 形式
	//   · Owner 与 Section 前缀不一致（试图声明别人的段）
	//   · 内核未提供注册表（系统提示词声明面未装配）
	DeclarePromptSection(s PromptSection) error

	// RemovePromptSection 移除一段声明。
	//
	// 单独提供而不是"传空 Content"：插件 Stop / 被停用时这是常见动作，
	// 写成空串容易漏 —— 漏了就会出现"插件已停用，但它的说明还留在
	// 提示词里，模型去调一个不存在的工具"，比没有说明更糟。
	//
	// 移除不存在或不属于自己的段是静默成功（幂等，便于无条件收尾）。
	RemovePromptSection(section string) error
}

// PromptSection 是一段声明的系统提示词内容。
type PromptSection struct {
	// Section 是段名，必须形如 `<owner>.<名字>`（如 "skillmgr.skills"）。
	Section string
	// Owner 是声明者（插件名），必须与 Section 前缀一致。
	Owner string

	// Order 决定渲染顺序（小的在前）。同 Order 按段名字典序，保证稳定。
	//
	// 约定给个宽松的区段，便于后续插入：
	//	  0–99    内核/框架性质（极少用）
	//	  100–199 工具与技能使用说明（skillmgr 用 100）
	//	  200–299 身份与权限说明
	//	  900+    其他
	Order int

	// Content 是正文（空串 = 移除该段）。
	Content string

	// Volatile 表示内容会随运行时频繁变化（如技能索引）。
	//
	// 只用于诊断（状态面能区分"静态说明"与"每轮重算"），不参与渲染决策。
	Volatile bool
}
