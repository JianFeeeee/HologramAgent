package core

// 内核自身声明的系统提示词段（2026-11-04 统一到 promptsec）。
//
// ## 用户裁定
//
// > 同时把现在内核所有 systemprompt 的相关注入都接入到这个统一的
// > systemprompt 管理器上。
//
// 在此之前的形态是两套并存：
//
//	① 内核在 buildSystemPrompt 里用 appendPromptBlock 硬拼十几块
//	② 插件（skillmgr）走 promptSections 声明自己的段
//
// 两套并存的问题不是"不好看"，而是**顺序不可控**：插件段只能整体插在
// 内核硬拼流程的某个固定位置，没法说"我要排在某两块之间"；而内核那十几块
// 的相对顺序又完全由代码书写顺序决定，改一处顺序得动函数体。
//
// 现在统一走一个注册表：内核把自己那十几块也**声明**进去，由 Order 决定
// 全部顺序。内核只是"另一个声明者"，插件看到的渲染规则与内核完全一致。
//
// ## 与插件段的区别（只有一点）
//
//	插件段：插件声明 → 常驻注册表（跨轮存活，可运行时更新）
//	内核段：每轮根据当前 agent 实例状态实时计算 → 当轮渲染
//
// 内核块大多依赖实例状态（人格卡、记忆上下文、任务提示词、索引器、首启门禁），
// 不能预先存进共享注册表。所以内核走"当轮临时渲染"：
//
//	kernelSections(a, memContext)  →  当轮快照  →  与注册表合并  →  渲染
//
// ★ 合并时**不写回**共享注册表：注册表是插件声明的家，内核的每轮快照
// 写进去会让它无限增长（每个 agent 一份），且下一轮读到上一轮的陈旧块。

import (
	"sort"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/kstate"
	"github.com/JianFeeeee/HologramAgent/internal/promptsec"
)

// 内核段的 Order 区段。
//
// 区段划分与 SDK 的 PromptSection.Order 约定一致（见 sdk/systemprompt.go）：
//
//	0–99     内核框架（基础人格、人格卡、任务提示词）
//	100–199  工具与技能使用说明（插件段：skillmgr 等）
//	200–299  记忆与上下文说明
//	300–399  输出与通道规则
//	400–499  工具目录
//
// ★ 为什么把"记忆"排在"工具"之后："什么时候用哪个工具"要在模型已经知道
// 有哪些工具之后读才有效；反过来（先讲记忆工具、后列工具）会让前面那段
// 引用还不存在的工具名。这是既有顺序，此处只是把它显式化。
const (
	orderKernelBase      = 0   // 基础人格（systemPrompt 兜底句）
	orderKernelTask      = 10  // 驻留子的任务提示词
	orderKernelPersona   = 20  // 人格卡
	orderKernelMemoryCtx = 200 // 记忆上下文（由 memoryPass 产出）
	orderKernelMemory    = 210 // 【Memory】三层记忆说明
	orderKernelMemMaint  = 220 // 【Memory maintenance】
	orderKernelInterrupt = 230 // 【Interrupt】
	orderKernelToolOrder = 240 // 【Tool order】
	orderKernelOutput    = 300 // 【Output rules】
	orderKernelIndexer   = 320 // 索引器（工具提示）
	orderKernelFirstRun  = 400 // 首启人格门禁
	orderKernelToolCata  = 500 // 工具目录
)

// kernelSection 是内核当轮计算出来的一段。
type kernelSection struct {
	order   int
	section string // 仅用于同 Order 的稳定排序（按名字典序）
	content string
}

// kernelSections 按**当前实例状态**算出内核本轮要注入的段。
//
// 顺序由 order 决定，与函数体里的书写顺序无关 —— 这是本次统一的要点：
// 调整提示词结构变成调常量，而不是挪代码块。
func (a *Agent) kernelSections(memContext string) []kernelSection {
	base := expandPromptVars(a.systemPrompt)
	if base == "" {
		base = "You are HologramAgent, a household-manager AI assistant. Never use Unicode emoji; express affect with kaomoji only, and end sentences with a soft particle. The WebUI overview page shows your portrait."
	}
	out := []kernelSection{
		{order: orderKernelBase, section: "kernel.base", content: base},
	}

	// 驻留子：在固定提示词之上注入任务提示词（设计 §7「创建」）。
	if a.taskPrompt != "" {
		out = append(out, kernelSection{
			order: orderKernelTask, section: "kernel.task",
			content: "[Task] " + a.taskPrompt,
		})
	}

	// 人格卡：同样展开版本占位符（人格里写死版本号会随发版说谎 ——
	// 实测线上人格卡写 v1.0.3，内核早已 1.3.x）。
	if a.personality != nil {
		if pp := a.personality.InjectPrompt(); pp != "" {
			out = append(out, kernelSection{
				order: orderKernelPersona, section: "kernel.persona",
				content: expandPromptVars(pp),
			})
		}
	}

	if memContext != "" {
		out = append(out, kernelSection{
			order: orderKernelMemoryCtx, section: "kernel.memory_context",
			content: memContext,
		})
	}

	// 【三层记忆】只留"模型据此选择工具"的语义。
	//
	// 它讲的是内核真实执行语义（哪些内容在哪一层、何时进上下文、哪个工具
	// 会做什么），不是工具用法 —— 工具用法归各 ToolDef.Description。
	//
	// 为什么必须有：此前系统提示词从未解释 L1/L2/L3，agent 只能从工具名猜。
	// 生产日志实测：agent 几乎从不调 coldcontext_query，即使调了也只看返回值、
	// 不知道原文已被回填到对话时序。
	//
	// ★ 但有一件事必须说：coldcontext_query 会把历史事件**插回时序**，
	//   而不是返回摘要 —— 这与模型对"检索工具"的默认预期相反，不说就误用。
	out = append(out, kernelSection{
		order: orderKernelMemory, section: "kernel.memory",
		content: "[Memory] Recent turns are above. For older conversations use `coldcontext_query` - it **re-inserts them into your timeline at their original timestamps**, so read them there, not in the return value. " +
			"Facts and relations: `stablemem_recall`. Written knowledge: `knowledge_search`.",
	})

	// 记忆维护：具体选哪个工具、各有什么副作用都写在 memory_* 的工具描述里
	//（【记忆清理】前缀）。这里只保留那条**不看描述也会犯**的错：只描述计划不执行。
	out = append(out, kernelSection{
		order: orderKernelMemMaint, section: "kernel.memory_maintenance",
		content: "[Memory maintenance] To tidy or clear memory, call the memory_* tools - a described plan is not the work. Report the result in one sentence.",
	})

	// L2（冷上下文）预填充已并入 buildMemoryContext（输入路/工具路共用一次），
	// 这里不再重复注入，否则同一批摘要会出现两遍。

	out = append(out, kernelSection{
		order: orderKernelInterrupt, section: "kernel.interrupt",
		content: "[Interrupt] During long tasks a system message prefixed [Interrupt] may arrive (new message, terminal output, timer). It is not user speech, but handle it: pause, respond, or consciously continue.",
	})

	// 【工具执行顺序】只留**模型必须据此作决定**的三条。
	//
	// 必须在并行执行落地之后才加：反序会让本段对模型说谎 —— 说"并发"而
	// 内核仍串行，模型据此推断安全性并写出真正依赖顺序的调用。
	// ⚠️ 措辞必须与 batchRunnable 的**真实**判据一致。
	out = append(out, kernelSection{
		order: orderKernelToolOrder, section: "kernel.tool_order",
		content: "[Tool order] Calls in one reply run **in parallel** by default. **Never depend on order**: if one needs another's result, use two turns. " +
			"Outputs to the same channel **keep their order**, so \"acknowledge, then conclude\" fits in one turn.",
	})

	out = append(out, kernelSection{
		order: orderKernelOutput, section: "kernel.output_rules",
		content: kernelOutputRules(),
	})

	if a.indexer != nil {
		out = append(out, kernelSection{
			order: orderKernelIndexer, section: "kernel.indexer",
			content: a.indexer.BuildToolPrompt(),
		})
	}

	// seq 的触发条件**刻意不写在这里**（用户裁定：seq 知识应在 skill 中）。
	//
	// 一度加过【加速序列】块，理由是"skill 索引只给一行摘要，模型不知道何时该用"。
	// 那个理由站不住：插件声明的技能索引已明确要求"准备调用某个插件的工具时
	// 先看同名技能"，seq 的"什么时候用"写在 skill 正文里会被这条规则正确带到
	// 模型面前。再加一块提示词 = 同一件事记两处，且必然漂移。
	//
	// 副作用证据：加块后 TestSystemPromptStaysCompact 报 2022 > 2000。
	// 那个超限**本身就是信号** —— 该去掉的是内容，不是调高上限。

	// 首启人格门禁（跨通道唯一闸口）：人格未确认时要求模型主动询问用户。
	// 系统提示词每轮重建，因此 WebUI / QQ / CLI / ACP / 邮件等所有通道都会
	// 带上它；模型调用 persona_set（或用户在向导里选）落地后，标记置位，本段消失。
	if a.personaStore != nil && !a.personaStore.PersonaInitialized() {
		out = append(out, kernelSection{
			order: orderKernelFirstRun, section: "kernel.first_run",
			content: "[First-run persona setup] Your **persona is not confirmed**. Ask once this turn: default persona, or a custom one? Then **call persona_set**: " +
				"default -> mode=default; custom -> mode=custom with the text in content; later -> mode=later. Until answered, don't assume it is set and don't repeat the question.",
		})
	}

	out = append(out, kernelSection{
		order: orderKernelToolCata, section: "kernel.tool_catalog",
		content: a.buildToolCatalog(),
	})

	return out
}

// kernelOutputRules 是【Output rules】的正文。
//
// 单独成函数只为了让 kernelSections 保持可读；渲染结果与原先逐字一致。
func kernelOutputRules() string {
	var b strings.Builder
	// ★ 这一句是本段最重要的：用户明确要求「必须显式调用 outputch 才会输出给用户」。
	// 模型最常见的错误不是不会调工具，而是**以为返回文本就等于回复了** ——
	// 异步通道下那等于自言自语（用户永远收不到）。
	_, _ = b.WriteString("[Output rules]\n")
	_, _ = b.WriteString("**Content reaches the user only when you explicitly call `output_send__{channel}`.** " +
		"Returning plain text only speaks inside this reasoning step; the user cannot see it - except on the synchronous channels described below.\n")
	_, _ = b.WriteString("- Don't assume a fixed channel: one conversation may span devices and channels. If unsure, call output_list_channels.\n")
	_, _ = b.WriteString("- Sync channels (webui / cli / terminal): return plain text. **The only exception.**\n")
	_, _ = b.WriteString("- Async channels (qq / wechat / groups): call output_send__{channel} with the right user_id or group_id.\n")
	_, _ = b.WriteString("- One call may send several messages: pass an array as payload. Unclear how to send? output_send__{channel}_help.\n")
	// ❗这里**不得**限制"一轮只能发一次"。设计上输出是 agent 的**主动调用**：
	// 收到一次输入后，可以往**任意（已授权的）通道**发**任意多次**（分段播报、
	// 先回执后结论、同时通知多个通道都合法）。此前这里写着"每轮对话通常只需
	// 调用一次 output_send" —— 那是一条**凭空的限制**，会让模型自己收起合理
	// 的多次输出。真正需要提醒的只有两件事：单条长度上限（超长拆成完整段落）
	// 与"别反复重发完全相同的内容"（自律，不是判据）。
	_, _ = b.WriteString("- **You choose how many outputs and which channels.** **No \"one output per turn\" limit.** Keep messages whole; don't resend identical content.\n")
	_, _ = b.WriteString("- Multi-step work: acknowledge on the current channel **first**, then run the real tools. An acknowledgement is not completion - finish and report.")
	return b.String()
}

// renderSystemPrompt 把内核当轮段与插件声明段合并渲染成最终 system prompt。
//
// ★ 这是**唯一**的渲染入口：buildSystemPrompt 只负责算出 memContext 递进来，
// 不再自己拼块。这样"提示词长什么样"只有一处可看。
//
// 合并语义：
//   - 内核段与插件段按 order 统一排序（同 order 按段名字典序，保证稳定）
//   - 段间用空行分隔（与 appendPromptBlock 的语义一致，保证输出逐字不变）
//   - 空内容段跳过（插件可能声明了段但暂时没内容）
func (a *Agent) renderSystemPrompt(memContext string) string {
	all := make([]renderableSection, 0, 16)
	for _, k := range a.kernelSections(memContext) {
		if strings.TrimSpace(k.content) == "" {
			continue
		}
		all = append(all, renderableSection{order: k.order, name: k.section, content: k.content})
	}
	// 插件声明的段从**统一挂载点**读（2026-11-04）。
	//
	// ★ 为何不是 Agent 字段：字段方案要求每种 agent（rtchild / 驻留子 /
	// 工作式子）在构造时都记得传一遍，漏传不报错也不崩，只是那个 agent
	// 的提示词里一条插件说明都没有 —— 生产上就是这样让分诊助手不知道
	// "cmd 插件要先 plugin_start"，于是拿 terminal_create 当命令执行器。
	//
	// 挂在 kstate 上后，任何 agent（含未来新类型）读同一份，**零接线**。
	if ks := kstate.Get(); ks != nil && ks.PromptSections != nil {
		for _, s := range ks.PromptSections.Snapshot() {
			if strings.TrimSpace(s.Content) == "" {
				continue
			}
			all = append(all, renderableSection{order: s.Order, name: s.Section, content: s.Content})
		}
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].order != all[j].order {
			return all[i].order < all[j].order
		}
		return all[i].name < all[j].name
	})

	var b strings.Builder
	for i, s := range all {
		if i > 0 {
			_, _ = b.WriteString("\n\n")
		}
		_, _ = b.WriteString(strings.TrimSpace(s.content))
	}
	return b.String()
}

// renderableSection 是排序用的临时结构（内核段与插件段合并后）。
type renderableSection struct {
	order   int
	name    string
	content string
}

// 编译期确认：内核用的段类型与 registry 的段类型同源，避免两处各自演化。
var _ = promptsec.Section{}
