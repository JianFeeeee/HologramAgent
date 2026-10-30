package core

import (
	"fmt"
	"log"
	"strings"
	"sync"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/meta"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	sdkmeta "github.com/JianFeeeee/hologramagentsdk/meta"
)

func (a *Agent) buildMemoryContext(input string, maxTokens int, scenes []string) string {
	if a.indexer == nil {
		return ""
	}
	injected := a.indexer.BuildContextInScene(input, scenes)
	s := a.indexer.FormatContext(injected)

	// 图库召回命中的实体若关联着带媒体的句子，把媒体说明一并注入。
	//
	// 不做这一步的后果：媒体描述进了 L3，agent 却拿不出来。图库句子里
	// 写着 [image/png a1b2c3d4e5f6] 这样的短标记，但没有任何东西告诉
	// 模型那份内容是否还在、能否重新查看——描述永存而 blob 可能已被
	// 删除，两者状态不同，必须显式告知。
	//
	// 注意不能直接用 injected.Relations：BuildContext 刻意把它置为 nil
	//（自动注入只给实体索引以省 token，细节留给 stablemem_recall）。
	// 因此这里用命中的实体名再查一次关系，只为拿到 sentence_id。
	if mc := a.mediaContextForInjectedEntities(injected); mc != "" {
		if s != "" {
			s += "\n"
		}
		s += "[Related media]\n" + mc
	}

	// ★ L2（冷上下文）相似度预填充：**必须在这里做**，不能只在上层 buildSystemPrompt。
	//
	// 设计里 prune 与 recall 是一对：工具声明 ContextPolicy=prune + RecallPolicy=auto
	// 时，工具输出既触发裁剪也触发预填充（memoryPass → recallTextFor → 本函数）。
	// 而此前 L2 那一路只写在 buildSystemPrompt 里、且 query 是**注入文本**
	// （QQ 是"来自「X」的私聊消息(message_id=…)，先用 qq_get_message 取正文"），
	// 跟用户真实说的话毫不相似 ⇒ 冷上下文预填充等于没有
	//（2026-10-27 现场：头天晚上讲的冷笑话第二天早上完全唤不回）。
	//
	// 放进本函数后：输入路用输入文本、工具路用工具正文，两条路都能召回 L2 摘要。
	if a.coldcontextStore != nil {
		if md := a.crossModalMarkdown(a.retrieveCrossModal(input, 3, a.fusionCfg)); md != "" {
			if s != "" {
				s += "\n"
			}
			s += md
		}
	}

	if maxTokens > 0 {
		s = TruncateByTokens(s, maxTokens)
	}
	return s
}

// buildTaskMemoryContext 按本任务声明的召回策略决定是否注入记忆索引。
//
// 默认 auto（保持“每条输入都召回”的既有行为）；输入/注入声明
// recall_policy=none 时返回空串，从而不注入记忆。策略与裁剪（ContextPolicy）正交。
//
// query 取**清洗后**的输入（通道 Cleaner 的输出），与裁剪侧同一套语义：
// 原始输入里的 ANSI/base64/JSON 包装会把相关性打分带偏。清洗为空时回退原文。
func (a *Agent) buildTaskMemoryContext(f *TaskFrame, input string, maxTokens int) string {
	scenes := a.sceneKeysFor(evtOf(f), "")
	if f == nil {
		return a.recallText(input, "input", maxTokens, scenes)
	}
	if !a.recallDeclared(f.Evt) {
		return ""
	}
	query := strings.TrimSpace(f.CleanInput)
	if query == "" {
		query = input
	}
	trigger := "input"
	if f.Evt != nil && f.Evt.Source != "" {
		trigger = "input:" + f.Evt.Source
	}
	// 场景集合 = 声明（主动）+ 涌现（被动）两条路的并集（去重）。
	turn := a.resolveTurnScenes(f, "")
	scenes = mergeSceneKeys(scenes, turn.Keys)
	return a.recallText(query, trigger, maxTokens, scenes)
}

// evtOf 安全取出 TaskFrame 的事件（f 为 nil 时返回 nil）。
func evtOf(f *TaskFrame) *agentIO.InputEvent {
	if f == nil {
		return nil
	}
	return f.Evt
}

// recallTextFor 以 query 触发一次记忆召回，按当前预算截断，返回可注入的文本。
//
// 这是“召回”侧的单一入口：与 Prune 共用同一份**清洗后**的 query，
// 使“取进来”（召回）与“踢出去”（裁剪）落在同一个相关性过程上。
// trigger 仅用于日志溯源（如 "tool:qq_get_message"）。
func (a *Agent) recallTextFor(query, trigger string, scenes []string) string {
	memTokens := 0 // 0 = 不截断
	if a != nil && a.provider != nil {
		memTokens = a.computeTokenBudget().MemoryTokens
	}
	return a.recallText(query, trigger, memTokens, scenes)
}

// recallText 是召回侧的共同实现：query → 记忆索引文本（空串表示无）。
//
// 输入侧的 buildTaskMemoryContext 与工具侧的 recallTextFor 都收敛到这里，
// 使“同一份 query、同一次预算、同一条审计日志”只写一遍。
func (a *Agent) recallText(query, trigger string, maxTokens int, scenes []string) string {
	if a == nil || query == "" || a.indexer == nil {
		return ""
	}
	text := a.buildMemoryContext(query, maxTokens, scenes)
	if text == "" {
		return ""
	}
	log.Printf("[agent] memory recall (%s): injected %d chars (scenes=%v)", trigger, len(text), scenes)
	return text
}

// expandPromptVars 展开自定义提示词（人格卡）里的版本占位符。
//
// 为什么需要：人格卡是**配置项**，一旦写死版本号就会随内核发版而说谎 ——
// 实测线上人格卡写着 "HΔ-Kernel v1.0.3 型号"，内核早已 1.3.x，agent 向用户
// 自报版本时就照抄 1.0.3。占位符让这类文本永远跟随真实构建：
//
//	{{kernel_version}}  → 内核版本（如 1.3.5）
//	{{kernel_commit}}   → 构建 commit
//	{{sdk_version}}     → 所兼容的 SDK 版本（如 1.3.0）
//
// 未知占位符**原样保留**：写错了要看得见，而不是被静默换成空串。
func expandPromptVars(s string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	return strings.NewReplacer(
		"{{kernel_version}}", meta.Version,
		"{{kernel_commit}}", meta.Commit,
		"{{sdk_version}}", sdkmeta.Version,
	).Replace(s)
}

// appendPromptBlock 把一个「整块」提示词接到已拼好的提示词后面。
//
// 为什么不能直接 `prompt += "\n\n" + block`：块自己常常以换行结尾
// （人格块是 "【人格设定】\n%s\n"、自定义 system_prompt 一般也有尾部换行），
// 直接拼就会得到 3~4 个连续换行 —— 实测一轮请求里出现 3 处：
//
//	…别包装成"稍等我看一下"。\n\n\n【人格设定】     ← 多一个空行
//	…/mascot.webp 直接能看。\n\n\n\n【场景记忆】   ← 多两个空行
//	…能否同轮发出。\n\n\n【输出规则】              ← 多一个空行
//
// 空行本身不致命，但会推高 token、也让「提示词到底长什么样」难以肉眼核对。
// 这里统一 trim 掉块两端空白再拼两个换行。
func appendPromptBlock(prompt, block string) string {
	block = strings.TrimSpace(block)
	if block == "" {
		return prompt
	}
	if prompt == "" {
		return block
	}
	return strings.TrimRight(prompt, "\n") + "\n\n" + block
}

func (a *Agent) buildSystemPrompt(memContext string, userInput string) string {
	prompt := expandPromptVars(a.systemPrompt)
	if prompt == "" {
		prompt = "You are HologramAgent, a household-manager AI assistant. Never use Unicode emoji; express affect with kaomoji only, and end sentences with a soft particle. The WebUI overview page shows your portrait."
	}

	// 驻留子：在**固定提示词之上**注入任务提示词（设计 §7「创建」）。
	if a.taskPrompt != "" {
		prompt = appendPromptBlock(prompt, "[Task] "+a.taskPrompt)
	}

	if a.personality != nil {
		if pp := a.personality.InjectPrompt(); pp != "" {
			// 人格卡同样展开版本占位符：人格里写死版本号会随发版说谎
			// （实测线上人格卡写 v1.0.3，内核早已 1.3.x）。
			prompt = appendPromptBlock(prompt, expandPromptVars(pp))
		}
	}

	if memContext != "" {
		prompt = appendPromptBlock(prompt, memContext)
	}

	// 【三层记忆】是**内核运行时块**（提示词分层规范：内核行为由内核生成）。
	//
	// 它讲的是内核的真实执行语义（哪些内容在哪一层、何时会进上下文、
	// 哪个工具会做什么），不是工具用法 —— 工具用法归各 ToolDef.Description。
	//
	// 为什么必须有这一段：此前系统提示词从未解释 L1/L2/L3 是什么，
	// agent 只能从工具名猜。生产日志实测：agent 几乎从不调用 coldcontext_query，
	// 即使调用了也只看返回值、不知道原文已被回填到对话时序。
	// 【三层记忆】只留**模型据此选择工具**的语义。
	//
	// 技术性内容（L1/L2/L3 编号、向量空间、消费式删除、缩略语）全部下沉到
	// 对应工具的 description 与 skill：模型只需要知道"哪里有原文、哪里有
	// 关系、用什么去取"，不需要知道内核怎么存。
	//
	// ★ 但有一件事必须说：coldcontext_query 会把历史事件**插回时序**，而不是返回
	//   一段摘要 —— 这与模型对"检索工具"的默认预期相反，不说就会误用。
	prompt = appendPromptBlock(prompt, "[Memory] Recent turns are above. For older conversations use `coldcontext_query` - it **re-inserts them into your timeline at their original timestamps**, so read them there, not in the return value. "+
		"Facts and relations: `stablemem_recall`. Written knowledge: `knowledge_search`.")

	// 记忆维护：具体选哪个工具、各有什么副作用，都写在 memory_* 的工具描述里
	//（【记忆清理】前缀）。这里只保留那条**不看描述也会犯**的错：只描述计划不执行。
	prompt = appendPromptBlock(prompt, "[Memory maintenance] To tidy or clear memory, call the memory_* tools - a described plan is not the work. Report the result in one sentence.")

	// L2（冷上下文）预填充已并入 buildMemoryContext（输入路/工具路共用一次），
	// 这里不再重复注入，否则同一批摘要会出现两遍。

	prompt = appendPromptBlock(prompt, "[Interrupt] During long tasks a system message prefixed [Interrupt] may arrive (new message, terminal output, timer). It is not user speech, but handle it: pause, respond, or consciously continue.")

	// 工具执行顺序（阶段 2.5）。必须在**并行执行落地之后**才加：
	// 反序会让本段对模型说谎——说"并发"而内核仍串行，模型据此推断
	// 安全性并写出真正依赖顺序的调用。宁可晚改，不可错改。
	//
	// ⚠️ 措辞必须与 batchRunnable 的**真实**判据一致（全批 ParallelSafe
	// 才并发 + 同通道保序），否则只是把"没说"换成"说错"。
	// 【工具执行顺序】只留**模型必须据此作决定**的三条。
	//
	// 技术性内容（ParallelSafe 声明、批内降级、内核如何判据）全部下沉：
	// 那是实现细节，模型既不需要也改不了 —— 讲它只会让回复带上说明书腔。
	// 判断依据仍是真实的 batchRunnable 语义（勿写反）。
	prompt = appendPromptBlock(prompt, "[Tool order] Calls in one reply run **in parallel** by default. **Never depend on order**: if one needs another's result, use two turns. "+
		"Outputs to the same channel **keep their order**, so \"acknowledge, then conclude\" fits in one turn.")

	prompt = appendPromptBlock(prompt, "[Output rules]\n")
	// ★ 这一句是本段最重要的：用户明确要求「必须显式调用 outputch 才会输出给用户」。
	// 模型最常见的错误不是不会调工具，而是**以为返回文本就等于回复了**——
	// 异步通道下那等于自言自语（用户永远收不到）。
	prompt += "**Content reaches the user only when you explicitly call `output_send__{channel}`.** " +
		"Returning plain text only speaks inside this reasoning step; the user cannot see it - except on the synchronous channels described below.\n"
	prompt += "- Don't assume a fixed channel: one conversation may span devices and channels. If unsure, call output_list_channels.\n"
	prompt += "- Sync channels (webui / cli / terminal): return plain text. **The only exception.**\n"
	prompt += "- Async channels (qq / wechat / groups): call output_send__{channel} with the right user_id or group_id.\n"
	prompt += "- One call may send several messages: pass an array as payload. Unclear how to send? output_send__{channel}_help.\n"
	// ❗这里**不得**限制"一轮只能发一次"。设计上输出是 agent 的**主动调用**：
	// 收到一次输入后，可以往**任意（已授权的）通道**发**任意多次**（分段播报、
	// 先回执后结论、同时通知多个通道都合法）。此前这里写着"每轮对话通常只需调用
	// 一次 output_send"——那是一条**凭空的限制**，会让模型自己收起合理的多次输出。
	// 真正需要提醒的只有两件事：单条长度上限（超长拆成完整段落）与"别反复重发
	// 完全相同的内容"（自律，不是判据）。
	prompt += "- **You choose how many outputs and which channels.** **No \"one output per turn\" limit.** Keep messages whole; don't resend identical content.\n"
	prompt += "- Multi-step work: acknowledge on the current channel **first**, then run the real tools. An acknowledgement is not completion - finish and report.\n"

	if a.indexer != nil {
		prompt += "\n\n" + a.indexer.BuildToolPrompt()
	}

	// seq 的触发条件**刻意不写在这里**（用户裁定：seq 知识应在 skill 中）。
	//
	// 一度加过【加速序列】块，理由是"skill 索引只给一行摘要，模型不知道何时该用"。
	// 那个理由站不住：紧邻下方的【可用技能】已明确要求"准备调用某个插件的工具时
	// 先看同名技能"，seq 的"什么时候用"写在 skill 正文里会被这条规则正确带到
	// 模型面前。再加一块提示词 = 同一件事记两处，且必然漂移。
	//
	// 副作用证据：加块后 TestSystemPromptStaysCompact 报 2022 > 2000。
	// 那个超限**本身就是信号**——该去掉的是内容，不是调高上限。

	// 技能索引（方案B）：轻量注入已加载技能列表，LLM 匹配到场景时
	// 主动 skill_info 拉取全文按文档执行
	if a.skillIndex != nil {
		if idx := a.skillIndex.SkillIndex(); idx != "" {
			// ★ 措辞从"用户请求匹配某技能时"改成"**调用插件工具之前**"。
			//
			// 旧措辞把人带偏成"只有用户点名要某个技能才去读"，而 skill 的真实
			// 用途是**工具的使用说明**：里面写着参数怎么填、有哪些已知坑
			// （例如 QQ 的"拉私聊历史只传 user_id，不要传 group_id"）、
			// 以及按需插件要先 plugin_start 拉起。
			//
			// 不看 skill 就直接调工具，是这个仓库反复吃过亏的一类错误。
			prompt = appendPromptBlock(prompt, "[Skills] Usage manuals for plugins. Before calling a plugin's tool (or when it fails), read the same-named skill with skill_info(\"skill-name\") and follow it. "+
				"Never guess from the name.\n\n"+idx)
		}
	}

	// 首启人格门禁（跨通道唯一闸口）：人格未确认时，要求模型主动询问用户。
	// 系统提示词每轮重建，因此 WebUI / QQ / CLI / ACP / 邮件等所有通道都会带上它；
	// 模型调用 persona_set（或用户在 WebUI 向导里选）落地后，标记置位，本段消失。
	if a.personaStore != nil && !a.personaStore.PersonaInitialized() {
		prompt = appendPromptBlock(prompt, "[First-run persona setup] Your **persona is not confirmed**. Ask once this turn: default persona, or a custom one? Then **call persona_set**: "+
			"default -> mode=default; custom -> mode=custom with the text in content; later -> mode=later. Until answered, don't assume it is set and don't repeat the question.")
	}

	prompt += a.buildToolCatalog()

	return prompt
}

func cleanParams(params map[string]interface{}) map[string]interface{} {
	if params == nil {
		return nil
	}
	cleaned := make(map[string]interface{}, len(params))
	for k, v := range params {
		cleaned[k] = v
	}
	if req, ok := cleaned["required"]; ok {
		switch v := req.(type) {
		case []interface{}:
			if len(v) == 0 {
				delete(cleaned, "required")
			}
		case []string:
			if len(v) == 0 {
				delete(cleaned, "required")
			}
		}
	}
	return cleaned
}

func (a *Agent) buildToolCatalog() string {
	defs := a.buildToolDefs()
	if len(defs) == 0 {
		return ""
	}
	// 仅注入插件/通道能力摘要，避免全量工具定义污染 system prompt。
	// 每个插件列：名称 + 能力描述 + 工具数。完整工具定义由 get_plugin_tools 按需拉取。
	byPlugin := map[string]int{}      // plugin -> 工具数
	pluginDesc := map[string]string{} // plugin -> 首个工具描述(作能力概览)
	var order []string
	for _, t := range defs {
		fn, ok := t.(map[string]interface{})["function"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		plg := a.resolveToolPlugin(name)
		if _, seen := byPlugin[plg]; !seen {
			order = append(order, plg)
		}
		byPlugin[plg]++
		if pluginDesc[plg] == "" {
			desc, _ := fn["description"].(string)
			// 按**字符**截断，不是字节：中文占 3 字节，字节切会落在多字节
			// 字符中间产生非法 UTF-8（同 skillmgr 的那个真实缺陷）。
			if r := []rune(desc); len(r) > 60 {
				desc = string(r[:60]) + "..."
			}
			pluginDesc[plg] = desc
		}
	}
	var sb strings.Builder
	sb.WriteString("\n\n[Available tool capabilities]\n")
	sb.WriteString("Grouped per plugin; call get_plugin_tools(\"{plugin}\") for full definitions.\n")
	// ★ 说明"按需插件"这回事（2026-10-22）。
	//
	// 插件有两种启动形态（SDK autostart.go）：
	//   常驻（auto_start 缺省/true）—— 工具一开始就在清单里
	//   按需（auto_start=false）  —— 初始不在清单里，要靠 plugin_start 拉起
	//
	// 不说清这件事的后果：模型看到"清单里没有某能力"就断定"没有这个能力"，
	// 于是绕道去用别的工具硬凑，或者干脆告诉用户做不到。
	sb.WriteString("**Not listed does not mean unavailable**: some plugins are **on-demand**; " +
		"their tools are absent until started. Call get_plugin_tools(\"plugin\") to see them, " +
		"then plugin_start(\"plugin\") to bring the plugin up. The matching skill states " +
		"whether a plugin is resident or on-demand, and how to use it once started.\n")
	for _, plg := range order {
		sb.WriteString(fmt.Sprintf("- %s (%d tools)", plg, byPlugin[plg]))
		if d := pluginDesc[plg]; d != "" {
			sb.WriteString(": " + d)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// toolDef 构造一个 OpenAI function-call 工具 schema。
//
// 存在的理由：这张工具表里每一条原本都是 4 层嵌套的
// map[string]interface{} 字面量（约 20 行/条）；本助手把它压成一次调用，
// 只消除重复、不改变 schema 形状——properties 原样保留（空表仍序列化为 {}），
// required 为空则整个键省略。
// toolDefOptions 是内置工具的**声明项**。
//
// ★ 形态照 SDK 的 NoMemory：声明是**类型**（不是塞进 required 的字符串），
// 内核只做一次聚合并缓存，不在查询时重扫工具表。
//
// 曾用错的两种做法（都是"把声明做成运行时猜谜"）：
//  1. 内核里一张 map[string]bool 硬编码名单 —— 声明从工具搬回内核，
//     工具改名不会跟着变；
//  2. 往 required 变参里塞字符串 "toolParallel" —— 拼错就静默失效，
//     编译器不报错，而"少一个工具能并发"正是最难察觉的那类问题。
//
// builtinToolTimeouts 是内置工具声明的执行超时（name → 如 "3m"）。
//
// 只在 init 阶段（buildToolDefs 首次调用）写入，之后只读 ⇒ 无需加锁的
// 前提是"写发生在任何并发读之前"。为稳妥起见用 RWMutex：buildToolDefs
// 可能被多次调用（每轮重建工具表），而 resolveToolTimeout 在工具执行时读。
var (
	builtinToolTimeoutMu sync.RWMutex
	builtinToolTimeouts  = map[string]string{}
)

func registerBuiltinToolTimeout(name, d string) {
	builtinToolTimeoutMu.Lock()
	builtinToolTimeouts[name] = d
	builtinToolTimeoutMu.Unlock()
}

// builtinToolTimeout 查内置工具声明的超时（"" = 未声明）。
func builtinToolTimeout(name string) string {
	builtinToolTimeoutMu.RLock()
	defer builtinToolTimeoutMu.RUnlock()
	return builtinToolTimeouts[name]
}

type toolDefOptions struct {
	// parallel 声明该工具可被并发执行（已核实只读、无共享写）。
	parallel bool
	// timeout 声明该工具单次执行的最长时间（如 "3m"）。
	//
	// ★ 为什么需要（2026-10-17 生产实测）：内核原本把所有工具一刀切 60s，
	// 而 output_send__* 是**投递到子 pipeline** 的长任务，天然可能超过 60s
	// （实测两次 60s 超时）。留空 = 用内核默认 60s，行为不变。
	timeout string

	// noMemory 声明该工具的**输出**不参与记忆计算（向量化/关键词/蒸馏），
	// 原文仍留在上下文可读。语义与 ToolDef.NoMemory 一致。
	//
	// ★ 注意：coldcontext_query 的 noMemory **不写在这里**，而是由 init() 里的
	//   显式名单登记。原因：工具表是按需构造的（受 `if a.coldcontextStore != nil`
	//   之类条件控制），而 textForVector 可能在构造之前被调用 ——
	//   靠构造时登记会让声明"时有时无"。声明是静态事实，init 期定死。
	//
	//   本字段保留给"确实想跟着工具构造走"的场景（工具不存在就不需要
	//   抑制它的输出），与 init 名单二选一即可。
	noMemory bool
}

// parallelOpts 是"可并发"的声明项。
func parallelOpts() toolDefOptions { return toolDefOptions{parallel: true} }

func toolDef(name, description string, properties map[string]interface{}, required ...string) map[string]interface{} {
	return toolDefWith(name, description, properties, required, toolDefOptions{})
}

// toolDefWith 是带声明项的 toolDef。
func toolDefWith(name, description string, properties map[string]interface{}, required []string, opts toolDefOptions) map[string]interface{} {
	params := map[string]interface{}{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		params["required"] = required
	}
	def := sdk.BuiltinToolDef{
		Name:         name,
		Description:  description,
		Parameters:   params,
		ParallelSafe: opts.parallel,
		Timeout:      opts.timeout,
	}
	// ★ 内置工具不走 StageHost.RegisterTool（它们由 executeToolCallInner
	// 的 switch 分派），所以 Timeout 声明若只留在结构体上就没人读得到。
	// 这里顺手登记进包级表，供 resolveToolTimeout 查询。
	//
	// 为什么不在 ToSchema 里带上 timeout：那份 map 是**发给模型的**
	// function schema。把内核的执行参数混进去，模型会以为需要自己传
	// timeout 参数，反而制造新的误解。
	if opts.timeout != "" {
		registerBuiltinToolTimeout(name, opts.timeout)
	}
	// 声明项也要落在**包级聚合表**上，供 textForVector 查询。
	//
	// ⚠️ 不在 declare*Tool 里做、而在这里做，是因为有先例的教训：
	//   让 toolDefWith 自己登记 parallel 曾导致"聚合表是空的"——
	//   那些工具都在 `if a.knowledge != nil` 之类的条件分支里。
	//   但 noMemory 的情况不同：它必须在**工具真被构造出来时**才成立
	//   （工具不在表里，也就没有输出要抑制）。两者语义一致：
	//   登记的是"这个工具的声明"，构造时登记最准确。
	if opts.noMemory {
		declareNoMemoryTool(name)
	}
	return def.ToSchema()
}

// init 登记**全部**声明为可并发的内置工具。
//
// 集中在这里而不是散落在各调用点，是为了可审计：一屏能看全"哪些内置工具
// 允许并发"，新增/改名时漏改会立刻被下面那条判据抓到。
//
// ⚠️ 这份名单是**已核实无共享写**的结论，不是分类标签。
// 任何内置工具只要引入写操作，就必须从这里移除。
// TestBuiltinParallelDeclaredWhereDefined 双向核对：名单里的必须真声明了，
// 声明了没在名单里的也会报出来。
func init() {
	for _, name := range []string{
		// 知识库：检索与列举
		"knowledge_search", "knowledge_list",
		// 人物图谱：查询与邻域读取
		"person_query", "person_network",
		// 通道：列举
		"input_channels", "output_list_channels",
		// 插件与来源：列举
		"get_plugin_tools", "llm_list_sources",
		// 文档：检索
		"coldcontext_query",
		// 安全模块：列举变量（只读，无共享写）
		"secret_list",
	} {
		declareParallelTool(name)
	}

	// 输出不进记忆计算的内置工具。
	//
	// ★ 必须在这里显式登记，不能只靠 toolDefWith 构造时登记：
	//   工具表是**按需构造**的（buildToolDefs 每轮重建，且受
	//   `if a.coldcontextStore != nil` 之类的条件分支控制），而 textForVector
	//   在工具被构造之前也可能被调用 ⇒ 声明会"时有时无"。
	//   声明是静态事实，就该在 init 期定死（与上面 parallel 同一理由）。
	//
	// 目前只有 coldcontext_query：它的返回值含被召回事件的正文片段，
	// 而那些正文已在记忆体系里 —— 再参与向量化/蒸馏就是重复注入。
	for _, name := range []string{
		"coldcontext_query",
	} {
		declareNoMemoryTool(name)
	}
}

func (a *Agent) buildToolDefs() []interface{} {
	var tools []interface{}

	if a.io != nil {
		for _, td := range a.io.GetAllTools() {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        td.Name,
					"description": td.Description,
					"parameters":  cleanParams(td.Parameters),
				},
			})
		}
	}

	if a.stageHost != nil {
		for _, td := range a.stageHost.GetToolDefs() {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        td.Name,
					"description": td.Description,
					"parameters":  cleanParams(td.Parameters),
				},
			})
		}
	}

	if a.indexer != nil {
		for _, td := range a.indexer.GetToolDefinitions() {
			tools = append(tools, td)
		}
	}

	// 超长工具结果的按范围回读工具。
	//
	// ★ 只在**真的存在**落盘结果时才暴露：
	//   · 绝大多数会话没有超长结果，无条件暴露等于每轮多付一份 schema 开销
	//     （本仓已有"175 个工具占满上下文"的实测教训）；
	//   · 没有句柄时这个工具对模型毫无用处，给了只会诱导它乱猜 event_id。
	if a.hasStoredToolResults() {
		tools = append(tools, toolDef("toolresult_read",
			"【工具结果回读】读取某个超大工具结果的指定字节区间。"+
				"当工具返回内容过大时，上下文里给你的只是预览，会附一个 event_id 句柄和总字节数。"+
				"用本工具按 offset/limit 取其余部分；单次最多 64KB。"+
				"结果不会自动过期，但会**随那条上下文事件淘汰**——事件被裁掉后再读会明确告知已淘汰。",
			map[string]interface{}{
				"event_id": map[string]interface{}{"type": "string", "description": "Handle shown in the preview (e.g. evt_1770000000000000000)"},
				"offset":   map[string]interface{}{"type": "number", "description": "Start byte offset (0-based); omit to read from the beginning"},
				"limit":    map[string]interface{}{"type": "number", "description": "Bytes to read; omit or <=0 for 64KB; should not exceed the remainder"},
			}, "event_id"))
	}

	if a.personaStore != nil {
		tools = append(tools, toolDef("persona_set", "【首启人格】落地用户的人格选择并记录「已经问过」。仅在用户明确答复后调用：默认用 mode=default；自定义用 mode=custom 并把人格内容放进 content；用户说以后再说用 mode=later。", map[string]interface{}{
			"mode":    map[string]interface{}{"type": "string", "description": "default | custom | later"},
			"content": map[string]interface{}{"type": "string", "description": "Custom persona text (required when mode=custom)"},
		}, "mode"))
	}

	// 安全模块的变量库工具（文档 C8）。
	//
	// 仅当安全模块真的启用时才暴露：未启用时变量库不可用，
	// 把工具给模型只会让它反复撞到"安全模块未启用"（无谓往返）。
	if security.Global() != nil {
		tools = append(tools, secretToolDefs()...)
	}

	if a.memory != nil {
		tools = append(tools, toolDef("stablemem_merge", "【记忆清理】合并两个同义实体。将所有关系从 source 重定向到 target，然后彻底删除 source。注意：实体删除后不可恢复，合并前请确认语义一致。", map[string]interface{}{
			"source": map[string]interface{}{"type": "string", "description": "被合并的实体名（合并后消失）"},
			"target": map[string]interface{}{"type": "string", "description": "保留的实体名"},
		}, "source", "target"))
		tools = append(tools, toolDef("stablemem_delete_entity", "【记忆清理】删除指定名称的记忆块及其所有关联关系。name 必须是**块文本**（端点名），不是关系文本；按精确匹配定位，不会连带删掉名字相近的其他块。要按子串批量清理改用 stablemem_purge（subject_contains）。用于清理无用的噪音块、distiller 自动产生的垃圾节点、确认无用的旧数据。此操作不可恢复。", map[string]interface{}{
			"name": map[string]interface{}{"type": "string", "description": "要删除的实体名称"},
		}, "name"))
		tools = append(tools, toolDef("stablemem_block_merge", "【记忆清理】标记两个实体在指定轮次内不尝试合并，用于阻止误判。当 LLM 判断两个实体虽然相似但不是同一事物时，使用此工具阻止后续心跳自动推送合并候选。每次心跳扫描双方计数各减一，归零后恢复候选资格。", map[string]interface{}{
			"entity_a": map[string]interface{}{"type": "string", "description": "第一个实体名"},
			"entity_b": map[string]interface{}{"type": "string", "description": "第二个实体名"},
			"rounds":   map[string]interface{}{"type": "integer", "description": "阻止轮次数（每次心跳各减一，归零后恢复）"},
		}, "entity_a", "entity_b", "rounds"))
		tools = append(tools, toolDef("stablemem_purge", "【记忆清理】删除记忆库中符合条件的垃圾关系和数据。当用户要求整理记忆时，用 stablemem_introspect 发现低质量实体后，用此工具批量删除。如 @merged 后缀的残留实体、mentionCount=0 的孤立实体、distiller 自动生成的噪音关系等。支持软删（soft）和物理删除（hard）。", map[string]interface{}{
			"subject_contains": map[string]interface{}{"type": "string", "description": "主体名包含的关键词，如 '@merged' 可清理已合并残留"},
			"relation_type":    map[string]interface{}{"type": "string", "description": "关系类型，如 '提及'、'回应'"},
			"target_contains":  map[string]interface{}{"type": "string", "description": "客体名包含的关键词"},
			"mode":             map[string]interface{}{"type": "string", "description": "soft（标记删除）/ hard（物理删除）", "default": "soft"},
		}))
		tools = append(tools, toolDef("stablemem_edit", "【记忆清理】编辑单条记忆关系：删除旧的 relation 并写入新的。用于修正错误的实体名或关系类型。", map[string]interface{}{
			"old_subject":  map[string]interface{}{"type": "string", "description": "旧主体名"},
			"old_relation": map[string]interface{}{"type": "string", "description": "旧关系类型"},
			"old_object":   map[string]interface{}{"type": "string", "description": "旧客体名"},
			"new_subject":  map[string]interface{}{"type": "string", "description": "新主体名（不填则不变）"},
			"new_relation": map[string]interface{}{"type": "string", "description": "新关系类型（不填则不变）"},
			"new_object":   map[string]interface{}{"type": "string", "description": "新客体名（不填则不变）"},
		}, "old_subject", "old_relation", "old_object"))
	}

	if a.knowledge != nil {
		tools = append(tools, toolDefWith("knowledge_search", "搜索知识库。输入查询关键词，返回相关知识内容。可用 category 把搜索限定在某个分类子树内。", map[string]interface{}{
			"query":    map[string]interface{}{"type": "string", "description": "查询关键词"},
			"top_k":    map[string]interface{}{"type": "integer", "description": "返回数量", "default": 5},
			"category": map[string]interface{}{"type": "string", "description": "可选：限定在某个分类内（前缀匹配子树，如 tech 会搜 tech/go、tech/rust）。留空则搜全库"},
		}, []string{"query"}, parallelOpts()))
		tools = append(tools, toolDefWith("knowledge_list", "列出知识库中所有知识分类。", map[string]interface{}{}, nil, parallelOpts()))
	}

	if a.knowledge != nil {
		tools = append(tools, toolDef("knowledge_create", "创建新知识。将知识写入知识库（knowledge/目录），自动向量化索引。可关联已入库媒体（附图/音视频）使该知识能被图本身检索到。", map[string]interface{}{
			"name":    map[string]interface{}{"type": "string", "description": "知识名称（用作目录名）"},
			"content": map[string]interface{}{"type": "string", "description": "知识内容，支持 Markdown"},
			"media_digests": map[string]interface{}{
				"type":        "array",
				"items":       map[string]interface{}{"type": "string"},
				"description": "可选：关联的媒体 digest（可传前缀）。媒体作为一等节点参与跨模态检索——知识能按图本身被搜到，而不依赖生成的描述文本",
			},
		}, "name", "content"))
		tools = append(tools, toolDef("knowledge_import_dir",
			"从目录批量导入知识（复制，不是引用）。递归扫描该目录，把文档复制进知识库并自动完成索引与向量化，之后即可被 knowledge_search 检索到。\n"+
				"目录约定（自动适配，无需改造资料）：含 content.md 的目录整体作为一条知识；否则目录里的 .md/.txt 等文件各成一条，**目录路径即分类**。\n"+
				"注意：dir 必须是绝对路径（你的工作目录不等于进程 cwd）；同名条目会跳过而不覆盖。",
			map[string]interface{}{
				"dir":      map[string]interface{}{"type": "string", "description": "源目录的绝对路径，如 /home/qq/notes。不接受相对路径"},
				"category": map[string]interface{}{"type": "string", "description": "可选：分类前缀，叠加在源目录结构之上。导入 ~/docs/go/x.md 配 category=tech 得到 tech/go/x。留空则沿用源目录自身的层级"},
				"include_media": map[string]interface{}{
					"type":        "boolean",
					"description": "可选：是否把目录里的图片/音视频也导入（复制进媒体库，使该知识能按图本身被搜到）。默认 false",
					"default":     false,
				},
				"dry_run": map[string]interface{}{
					"type":        "boolean",
					"description": "可选：只报告将导入什么，不实际写入。试探性导入时建议先跑一次",
					"default":     true,
				},
				"max_items": map[string]interface{}{"type": "integer", "description": "可选：本次最多导入多少条（默认 500，防误传大目录）"},
			}, "dir"))
		tools = append(tools, toolDef("knowledge_delete", "删除知识库中的指定知识条目。", map[string]interface{}{
			"name": map[string]interface{}{"type": "string", "description": "要删除的知识名称"},
		}, "name"))
	}

	if a.coldcontextStore != nil {
		// The public tool name remains coldcontext_query for compatibility. Its description
		// uses "cold context" so the model does not mistake replayable conversation
		// history for a user-authored document.
		tools = append(tools, toolDefWith("coldcontext_query",
			"Retrieve earlier conversation turns that are no longer in the current context. Use this when the user refers to a past episode, asks what was said or done, or you remember an event but need its exact details. For current facts or entity relations, use `stablemem_recall` instead.\n\n"+
				"The full matching turns are restored into your conversation timeline at their original timestamps (they are inserted back into the timeline, not returned as a summary); read them there. This is consuming: retrieved material leaves cold context.",
			map[string]interface{}{
				"query": map[string]interface{}{"type": "string", "description": "查询内容，用当时可能出现的词描述"},
				"top_k": map[string]interface{}{"type": "integer", "description": "召回多少篇（默认 3）", "default": 3},
			}, []string{"query"}, parallelOpts()))

		tools = append(tools, toolDef("coldcontext_commit",
			"Write a durable episode or conclusion into cold context (older conversation history). Use this for a complete process, investigation trail, or decision rationale. For a single fact or relation use `stablemem_commit`; for reusable written knowledge use `knowledge_create`.",

			map[string]interface{}{
				"content": map[string]interface{}{"type": "string", "description": "文档内容（完整正文，不要自己截断）"},
				"summary": map[string]interface{}{"type": "string", "description": "一句话摘要（可选，留空则由正文首行生成）"},
				"media_digests": map[string]interface{}{
					"type":        "array",
					"description": "可选：这篇文档关联的媒体 digest（对话或 stablemem_recall 的「关联媒体」里显示的十六进制串，短的即可）。填了以后检索到这篇文档就能看到并取回原图/音频。",
					"items":       map[string]interface{}{"type": "string"},
				},
			}, "content"))
	}

	if a.social != nil {
		tools = append(tools, toolDefWith("person_query", "查询指定人物的完整档案（特质+社交关系）。用于了解一个人的性格、喜好、背景和社交圈。", map[string]interface{}{
			"name": map[string]interface{}{"type": "string", "description": "人物名称"},
		}, []string{"name"}, parallelOpts()))
		tools = append(tools, toolDef("person_set_trait", "记录/更新一个人的特质（性格、喜好、习惯等）。例如：person_set_trait(name=\"张三\", trait=\"喜欢\", value=\"红色\")。如果该特质已存在则覆盖。", map[string]interface{}{
			"name":  map[string]interface{}{"type": "string", "description": "人物名称"},
			"trait": map[string]interface{}{"type": "string", "description": "特质名称，如：喜欢、性格、职业、年龄"},
			"value": map[string]interface{}{"type": "string", "description": "特质值，如：红色、开朗、工程师、25岁"},
		}, "name", "trait", "value"))
		tools = append(tools, toolDef("person_relate", "记录两个人之间的社交关系。例如：person_relate(person_a=\"张三\", relation=\"朋友\", person_b=\"李四\")。关系是双向的。", map[string]interface{}{
			"person_a": map[string]interface{}{"type": "string", "description": "人物A"},
			"relation": map[string]interface{}{"type": "string", "description": "关系类型，如：朋友、家人、同事、邻居、同学"},
			"person_b": map[string]interface{}{"type": "string", "description": "人物B"},
		}, "person_a", "relation", "person_b"))
		tools = append(tools, toolDefWith("person_network", "查询某人的社交网络（多度关系）。显示该人物周围的相关人物及其关系和特质。", map[string]interface{}{
			"name":  map[string]interface{}{"type": "string", "description": "人物名称"},
			"depth": map[string]interface{}{"type": "integer", "description": "关系深度（默认2）", "default": 2},
		}, []string{"name"}, parallelOpts()))
	}

	if a.pluginReg != nil && a.pluginDir != "" {
		tools = append(tools, toolDef("plgreload", "重载 plugins/ 目录的所有插件。扫描目录变更，原子化替换 IO 设备。", map[string]interface{}{}))
	}

	// 按插件动态拉取工具定义(避免全量注入提示词污染)
	tools = append(tools, toolDefWith("get_plugin_tools", "Get a plugin's full tool definitions (name / parameters / purpose). Pass the plugin name as plugin_name (see the [Available tool capabilities] list in the system prompt). Omit it to get a summary of tools for every plugin.", map[string]interface{}{
		"plugin_name": map[string]interface{}{"type": "string", "description": "插件名，如 qq / remotedevice / weather", "default": ""},
	}, nil, parallelOpts()))

	tools = append(tools, toolDef("spawn_child", "启动一个异步子 Agent 执行独立任务。子 Agent 后台运行，不阻塞当前对话。完成后系统会自动通知你，届时请调用 child_result 工具查看输出。\n使用时机：多个互不依赖的子任务（如同时查三个网站、分别处理多个文件）可以在**同一轮**里一次 spawn 多个子 Agent——同轮调用默认并行，子 Agent 会各自后台启动（是否真正并发取决于工具的并发安全声明）。长耗时任务（批量处理、多轮搜索）也应交给子 Agent，避免阻塞对话。注意：一次 spawn 只是一个启动动作；要立刻拿到结果仍需另一次 `child_result` 调用。", map[string]interface{}{
		"task": map[string]interface{}{
			"type":        "string",
			"description": "要子 Agent 完成的任务描述。请描述清晰、完整，包含所有必要背景。",
		},
		"max_turns": map[string]interface{}{
			"type":        "integer",
			"description": "子 Agent 最大工具轮数（默认 5，范围 1-30）。复杂任务可调高。",
		},
	}, "task"))
	tools = append(tools, toolDef("child_result", "查询异步子 Agent 的执行结果。当收到'子任务已完成'的通知后，调用此工具获取输出。", map[string]interface{}{
		"task_id": map[string]interface{}{
			"type":        "string",
			"description": "spawn_child 返回的任务 ID，如 child_1",
		},
	}, "task_id"))

	if a.providerManager != nil {
		tools = append(tools, toolDefWith("llm_list_sources", "列出所有可用的 LLM 源（如 deepseek、openai、ollama），每个源有对应的 Lua 适配器和配置。如需切换 LLM 源，请使用 llm_set_source。", map[string]interface{}{}, nil, parallelOpts()))
		tools = append(tools, toolDef("llm_set_source", "切换当前 LLM 源到指定名称。变更立即生效，后续对话将使用新的 LLM 源。源名称可通过 llm_list_sources 查看。", map[string]interface{}{
			"name": map[string]interface{}{
				"type":        "string",
				"description": "LLM 源名称（如 deepseek、openai、ollama）",
			},
		}, "name"))
	}

	// ⚠️ 这里必须判 nil：本函数开头对 a.io 做了 nil 保护（io 工具那段），
	// 末尾却没有，前后不一致。任何没有 IO 的 Agent（单测、ToolAPI 校验）
	// 调 buildToolDefs 都会 panic —— Go 允许对 nil 指针调方法，
	// panic 发生在 ListChannels 内部解引用字段时，症状出现在 io 包里，
	// 根因却在这里。
	var channels []agentIO.ChannelInfo
	if a.io != nil {
		channels = a.io.ListChannels()
	}
	for _, ch := range channels {
		if ch.Type != agentIO.DeviceOutput && ch.Type != agentIO.DeviceIO {
			continue
		}
		// 输出通道授权（设计 §4.4/R2）：默认完整授权；父可用白名单收窄子的输出能力。
		// 未授权就不生成 output_send__X —— 模型看不到它，自然不会调。
		if !a.IsOutputAllowed(ch.Name) {
			continue
		}
		capStr := a.io.GetChannelCapabilities(ch.Name).String()
		desc := ch.Description
		if desc == "" {
			desc = ch.Name + " 输出通道"
		}

		// ★ 一次可发多条、多种类型（2026-10-22）。
		//
		// payload / type / meta 同时接受字符串与数组 —— 单值时行为与旧版逐字相同
		// （向后兼容）。描述里**不写** type 的具体枚举：那是插件自定义字段，
		// 内核不该承载它的语义文档（用户明确要求技术性内容收敛到 skill 与 _help）。
		tools = append(tools, toolDef("output_send__"+ch.Name,
			desc+"。能力: "+capStr+"。\n"+
				"payload 是消息载荷，可以是**一条字符串**，也可以是**数组**（一次发多条）。\n"+
				"type 是载荷类型，meta 是发送元数据 —— 两者的格式与取值由该通道定义，"+
				"用 output_send__"+ch.Name+"_help 或对应 skill 查看。\n"+
				"type/meta 只给一个值时，会应用到 payload 的每一条。",
			map[string]interface{}{
				"payload": map[string]interface{}{
					"description": "消息载荷。字符串=发一条；数组=一次发多条（按数组顺序送达）",
					"oneOf": []interface{}{
						map[string]interface{}{"type": "string"},
						map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
				},
				"type": map[string]interface{}{
					"description": "载荷类型。字符串=全部同类型；数组=与 payload 一一对应（数量必须相等）。缺省按通道默认",
					"oneOf": []interface{}{
						map[string]interface{}{"type": "string"},
						map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
				},
				"meta": map[string]interface{}{
					"description": "发送元数据（JSON 字符串）。只给一个值时广播给所有条；数组则一一对应。格式见 _help 或对应 skill",
					"oneOf": []interface{}{
						map[string]interface{}{"type": "string"},
						map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
				},
			}, "payload"))

		tools = append(tools, toolDef("output_send__"+ch.Name+"_help", "查看 "+ch.Name+" 输出通道的 meta 格式说明和 type 枚举", map[string]interface{}{}))
	}

	tools = append(tools, toolDefWith("output_list_channels", "列出所有可用输出通道及其能力（如 text/file/image/audio）和对应的输出门工具名称。", map[string]interface{}{}, nil, parallelOpts()))

	// 父侧：rtchild 控制面（实时通话子 agent，plan.md 十一）。
	if a.parentID == "" {
		tools = append(tools, toolDefWith("rt_children",
			"管理实时子 agent（rtchild，用于通话/视频通话）：list 列出 / "+
				"create 创建（**必须成对**给 inputch + outputch）/ "+
				"send 注入一条新输入（**会立即打断它当前轮**，不排队）/ "+
				"stop 回收（先交给你全部上下文，是否保留由你决定，用 rt_transcript 取回）。"+
				"注意：rt 通道必须由插件声明（ChannelDef.Realtime=true）；"+
				"没有 rt 能力的 agent 不能持有 rt 通道。", map[string]interface{}{
				"action": map[string]interface{}{
					"type": "string",
					"enum": []string{"list", "create", "send", "stop"},
				},
				"id":            map[string]interface{}{"type": "string", "description": "rtchild 的 id"},
				"inputch":       map[string]interface{}{"type": "string", "description": "create：rt inputch 名（采集侧灌入；成对的一半）"},
				"outputch":      map[string]interface{}{"type": "string", "description": "create：rt outputch 名（内核把文本写进它；成对的另一半）"},
				"system_prompt": map[string]interface{}{"type": "string", "description": "create：系统提示词（留空用内置的实时通话提示词）"},
				"max_tools":     map[string]interface{}{"type": "number", "description": "create：单轮最多几批工具调用（0=不限，rt 默认不限）"},
				"text":          map[string]interface{}{"type": "string", "description": "send：要注入的输入（会打断当前轮）"},
			},
			[]string{"action"}, parallelOpts()))
	}

	// 父侧：rtchild 上下文交接（用户裁定 2026-10-30：走 contextfull 式中断，
	// 主 agent **自行选择**保留哪些内容 —— 所以给一个按需拉取的读口）。
	if a.parentID == "" {
		tools = append(tools, toolDefWith("rt_transcript",
			"取回实时子 agent（rtchild）的通话上下文原文。回收 rtchild 时你会收到一条中断，"+
				"告知有多少条消息；**是否保留、保留哪些由你决定** —— 用本工具分段读取后再取舍，"+
				"不要把整段塞进自己的上下文（一条通话可能有几百条消息）。",
			map[string]interface{}{
				"id":     map[string]interface{}{"type": "string", "description": "rtchild 的 id"},
				"offset": map[string]interface{}{"type": "number", "description": "从第几条消息开始（0 = 从头）"},
				"limit":  map[string]interface{}{"type": "number", "description": fmt.Sprintf("最多返回多少条（默认 %d）", rtTranscriptDefaultLimit)},
				"format": map[string]interface{}{"type": "string", "enum": []string{"text", "raw"}, "description": "text=可读渲染（默认）/ raw=原始消息"},
			},
			[]string{"id"}, parallelOpts()))
	}

	// 父侧：驻留子控制面（单工具多动作，见设计 §7）。
	if a.parentID == "" {
		tools = append(tools, toolDef("resident_agents", "管理驻留子 agent（长期派驻的下属）：list 列出 / create 创建（划入 inputch + "+
			"授权输出通道 + 注入任务提示词）/ send 发送消息（对子而言是 L4 中断，取消其当前状态并插入新消息）"+
			"/ inspect 查看其 inputch 处理表（不打断它）/ compress 压缩其上下文（保留语义，子继续存在）"+
			"/ reclaim 回收（父选哪些纳入主记忆，然后取消该子）/ destroy 立刻销毁并移除。"+
			"reclaim/destroy 时它手头**尚未处理的消息**（残余任务）由你用 residual 决定："+
			"keep=转回你自己的队列（默认）/ drop=明确丢弃，两者都会逐条记日志。", map[string]interface{}{
			"action": map[string]interface{}{
				"type": "string",
				"enum": []string{"list", "create", "send", "inspect", "compress", "reclaim", "destroy"},
			},
			"id":              map[string]interface{}{"type": "string", "description": "驻留子 id"},
			"task_prompt":     map[string]interface{}{"type": "string", "description": "create：在固定提示词之上注入的任务提示词"},
			"input_chs":       map[string]interface{}{"type": "string", "description": "create：划入的 inputch（逗号分隔）"},
			"allowed_outputs": map[string]interface{}{"type": "string", "description": "create：授权的输出通道（逗号分隔；留空=完整授权）"},
			"capacity":        map[string]interface{}{"type": "number", "description": "create：划入 inputch 的队列容量"},
			"temp_path":       map[string]interface{}{"type": "string", "description": "create：temp 图记忆路径（留空则用 data_dir/residents/<id>/graph.db）"},
			"text":            map[string]interface{}{"type": "string", "description": "send：要发给子 agent 的消息"},
			"residual": map[string]interface{}{
				"type": "string",
				"description": "reclaim/destroy：该子手头**未处理消息**的处置。" +
					"keep=转回你自己的队列（默认）/ takeover=同 keep 但语义明确为「我接手」/" +
					"stash=暂存到它各自的原通道，等该通道被接回时再放出（你暂时忙但活不能丢时用）/" +
					"drop=丢弃（已确认不要）。" +
					"四者都会逐条记日志；异步通道（如 qq）被 drop 时用户收不到任何回复，请慎重。",
				"enum": []string{"keep", "takeover", "stash", "drop"},
			},
			"channels": map[string]interface{}{
				"type": "string",
				"description": "reclaim/destroy：该子**持有的通道**的处置。" +
					"release=归还池子变「未分配」，可被任意 agent 再分配（默认）/ " +
					"suspend=挂起：归属清空但不参与自动分配，通道上的暂存任务保留，等你接回/" +
					"takeover=接回你自己：通道归你，并立即释放该通道上的暂存任务到你的队列。",
				"enum": []string{"release", "suspend", "takeover"},
			},
		}, "action"))
	}

	// 子侧（驻留子）：主动汇报（L3）与主动写处理表。
	if a.parentID != "" {
		tools = append(tools, toolDef("notify_parent", "向主 agent 汇报（以 L3 中断投给它）。用于主动报告进展/结论，而不是等它来问。", map[string]interface{}{"text": map[string]interface{}{"type": "string", "description": "汇报内容"}}, "text"))
		tools = append(tools, toolDef("inputch_note", "为**本轮** inputch 主动写入处理信息（主 agent 会查这张表判断你的进度）。"+
			"写了就不会再被系统自动记录；不写则本轮结束时系统自动写。", map[string]interface{}{"text": map[string]interface{}{"type": "string", "description": "本轮处理信息摘要"}}, "text"))
	}

	tools = append(tools, toolDefWith("input_channels", "查看 inputch（最基本的输入路由单位）：哪些已注册、谁注册的、"+
		"各自划给了哪个 agent、容量与记忆策略。单工具多视图。", map[string]interface{}{
		"view": map[string]interface{}{
			"type": "string",
			"description": "all=全部已注册（默认）| mine=划给本 agent 的 | " +
				"unassigned=尚未划出的 | by_agent=按归属分组的划分总览 | detail=单个详情",
			"enum": []string{"all", "mine", "unassigned", "by_agent", "detail"},
		},
		"name": map[string]interface{}{
			"type":        "string",
			"description": "view=detail 时必填：inputch 名",
		},
	}, nil, parallelOpts()))

	// ★★ 2026-10-06：多模态工具已**移出内核**，改为内置插件 multimodal。
	//
	// 移出理由（README 第一原则）：内核职责限定 LLM 编排/记忆/知识检索，
	// 而「读本地文件」是 IO —— 三个工具读文件却住在内核进程内，
	// 无沙箱、无能力面、无审计，正是 capability.go 那套权限梯度要防的事。
	//
	// 留在这里的是**待退场**的部分：下面这一大块 pendingMedia 只服务于
	// 那三个工具的工具面。input 通路（processInput 直接把 media block
	// 拼进本轮请求）**不依赖它**，所以可安全移除。
	// 退场顺序见 docs 或本次提交的说明；先确保插件侧能力等价。
	//
	// ⇒ describe_image / transcribe_audio / ocr_image 现在由插件注册，
	//   不再出现在这里。pendingMedia 的清理在本次一并处理。

	return tools
}

// builtinDefRegistry 汇总内置工具的声明项。
//
// 形态照 StageHost.NoMemoryToolNames：**一次聚合**，查询不再遍历工具表。
// 之前 builtinToolParallelSafe 每次 toolParallelSafe 调用都重跑一遍
// buildToolDefs()，等于 O(工具数) 的重复劳动 —— 声明是静态的，没有理由每次重算。
type builtinDefRegistry struct {
	mu     sync.RWMutex
	byName map[string]sdk.BuiltinToolDef
	built  bool
}

var builtinDefs = &builtinDefRegistry{byName: map[string]sdk.BuiltinToolDef{}}

// declareParallelTool 在**工具定义处**登记"可并发"声明。
//
// 由每个 toolDefWith(..., parallelOpts()) 调用点在 init 里调用 ——
// 不依赖运行时路径。
//
// ⚠️ 曾经让 toolDefWith 在**被调用时**顺带登记，结果聚合表是空的：
// 那些工具都在 `if a.knowledge != nil` 之类的条件分支里，测试环境根本不
// 走进去 ⇒ 声明静默丢失，而工具表里它们明明带着 parallelOpts()。
// 症状是"判据全绿但并发能力为零"—— 判据查的是同一张空表，自证。
// declareNoMemoryTool 在**工具定义处**登记"输出不进记忆计算"声明。
//
// 与 declareParallelTool 同构：声明是静态的，在 init 期聚合一次，
// 运行期只查表。落在工具自己的结构体上，内核里不出现任何工具名硬编码。
func declareNoMemoryTool(name string) {
	builtinDefs.mu.Lock()
	d := builtinDefs.byName[name]
	d.Name = name
	d.NoMemory = true
	builtinDefs.byName[name] = d
	builtinDefs.mu.Unlock()
}

// noMemoryOf 查询内置工具的输出是否不进记忆计算。
// 未登记 ⇒ 参与记忆（保守：与 SDK 零值语义一致）。
func noMemoryOf(name string) bool {
	builtinDefs.mu.RLock()
	defer builtinDefs.mu.RUnlock()
	return builtinDefs.byName[name].NoMemory
}

func declareParallelTool(name string) {
	builtinDefs.mu.Lock()
	// ★ 按字段合并而非整体覆盖：同一工具可能同时声明 noMemory 与 parallel，
	//   覆盖会让后登记的那项静默丢失（声明丢一个是最难察觉的一类问题）。
	d := builtinDefs.byName[name]
	d.Name = name
	d.ParallelSafe = true
	builtinDefs.byName[name] = d
	builtinDefs.mu.Unlock()
}

// concurrencySafeOf 查询内置工具的并发声明。
// 未登记 ⇒ 不可并发（保守，与 SDK 的零值语义一致）。
func concurrencySafeOf(name string) bool {
	builtinDefs.mu.RLock()
	defer builtinDefs.mu.RUnlock()
	def, ok := builtinDefs.byName[name]
	if !ok {
		return false
	}
	return def.ConcurrencySafe()
}
