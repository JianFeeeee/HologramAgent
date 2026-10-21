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
	sdkmeta "github.com/JianFeeeee/homeagentsdk/meta"
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
	//（自动注入只给实体索引以省 token，细节留给 memory_recall）。
	// 因此这里用命中的实体名再查一次关系，只为拿到 sentence_id。
	if mc := a.mediaContextForInjectedEntities(injected); mc != "" {
		if s != "" {
			s += "\n"
		}
		s += "【关联媒体】\n" + mc
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
		prompt = "你是小宅，HomeAgent 的看板娘，一个家政型 AI 管家助手。绝不用 Unicode emoji，只用颜文字表达情感，句尾带语气词。WebUI 概览页展示你的立绘。"
	}

	// 驻留子：在**固定提示词之上**注入任务提示词（设计 §7「创建」）。
	if a.taskPrompt != "" {
		prompt = appendPromptBlock(prompt, "【任务】"+a.taskPrompt)
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

	// 记忆维护：具体选哪个工具、各有什么副作用，都写在 memory_* 的工具描述里
	//（【记忆清理】前缀）。这里只保留那条**不看描述也会犯**的错：只描述计划不执行。
	prompt = appendPromptBlock(prompt, "【记忆维护】用户要求整理或清理记忆时，必须真的调用 memory_* 工具去做，"+
		"不能只描述计划；选哪个工具、各有什么副作用见对应工具描述。做完用一句人话说结果。")

	// 跨模态召回：文本路（fastText/TF-IDF 文档层，媒体描述文本已随记忆进入）
	// + 视觉路（多模态文本编码 → 媒体库坐标）两路归一化融合。
	// 未配置多模态空间时视觉路为空，等价旧的 docStore.Query。
	if a.docStore != nil {
		hits := a.retrieveCrossModal(userInput, 3, a.fusionCfg)
		if md := a.crossModalMarkdown(hits); md != "" {
			prompt = appendPromptBlock(prompt, md)
		}
	}

	prompt = appendPromptBlock(prompt, "【中断消息】长任务执行期间，工具/插件/定时器等会通过中断机制向你发送提醒（如 QQ 新消息、终端输出到达、定时器到点等）。中断消息以 system 角色注入，内容带 [中断消息] 前缀，**不是用户发言，但也必须认真处理**：优先停下当前长任务，针对中断内容作出响应或决定继续执行。不要忽略带 [中断消息] 前缀的 system 消息。")

	// 工具执行顺序（阶段 2.5）。必须在**并行执行落地之后**才加：
	// 反序会让本段对模型说谎——说"并发"而内核仍串行，模型据此推断
	// 安全性并写出真正依赖顺序的调用。宁可晚改，不可错改。
	//
	// ⚠️ 措辞必须与 batchRunnable 的**真实**判据一致（全批 ParallelSafe
	// 才并发 + 同通道保序），否则只是把"没说"换成"说错"。
	prompt = appendPromptBlock(prompt, "【工具执行顺序】同一条回复里给出多个工具调用时，内核**默认并行执行**（同时跑），不是依次执行。这带来三条你必须知道的规则：\n")
	prompt += "- **不要依赖执行顺序**：若某个调用的参数需要另一个调用的结果，就**分两轮**——先发前一个，看到结果后再发下一个。写在同一轮里就等于假设了不存在的先后。\n"
	prompt += "- **例外一：同一通道的输出发送会保序**。对**同一个**通道连续调用多次 `output_send__`，内核会**按你发出的顺序依次执行**（用户可见消息顺序敏感），不会并发打乱。所以「先发回执、再发结论」这类有序发送可以放在同一轮；但若后续内容依赖前一条的用户反应，仍应分轮。\n"
	prompt += "- **例外二：不并发安全的工具不会并发**。写类工具（记忆/知识/文档写入等）与未声明并发安全的工具，**整批会退回依次执行**——同一批里只要有一个这样的工具，这一批就全部串行。这对你是透明的：你不需要判断，只需知道「同轮并发」不是无条件保证的。\n"
	prompt += "- 工具是否并发安全由**工具自己声明**（`ParallelSafe`），不由你决定；你只需按上面第 1 条判断能否同轮发出。\n"

	prompt = appendPromptBlock(prompt, "【输出规则】消息不会自动发送到对话来源通道，你必须自己决定如何回复：\n")
	prompt += "- **不要假设当前通道是某个固定值**：同一会话里可能同时有多个来源（多设备、多通道、子任务）。\n"
	prompt += "  先看这条消息本身与上下文里的来源信息，再决定往哪里回；不确定有哪些通道时先调 output_list_channels。\n"
	prompt += "- 同步通道（webui / cli / 终端）：直接返回纯文本，内核会把文本交给等待方显示，无需调用工具。\n"
	prompt += "- 异步通道（qq / wechat / 群聊等）：返回纯文本**【不会】**自动送达用户，必须调用 output_send__{通道名} 工具（注意 meta 里带上正确的 user_id 或 group_id）才能真正把消息发出去。\n"
	prompt += "- 不确定当前通道的发送方式时，先用 output_send__{通道名}_help 查看该通道的 meta 格式和 type 枚举，再决定。\n"
	// ❗这里**不得**限制"一轮只能发一次"。设计上输出是 agent 的**主动调用**：
	// 收到一次输入后，可以往**任意（已授权的）通道**发**任意多次**（分段播报、
	// 先回执后结论、同时通知多个通道都合法）。此前这里写着"每轮对话通常只需调用
	// 一次 output_send"——那是一条**凭空的限制**，会让模型自己收起合理的多次输出。
	// 真正需要提醒的只有两件事：单条长度上限（超长拆成完整段落）与"别反复重发
	// 完全相同的内容"（自律，不是判据）。
	prompt += "- **输出次数与目标通道由你自己决定**：一次输入可以对同一通道发多条（先回执后结论、分步播报、分段长文），也可以同时发到多个通道（例如同时通知 webui 与 qq）。**没有任何「一轮只能发一次」的限制。**\n"
	prompt += "- 输出时只需注意两点：单条消息的长度上限（超长就拆成完整段落，不要碎片化）；别反复重发**完全相同**的内容（那是浪费，不是限制）。\n"
	prompt += "- 需要多步执行的长任务：**必须先**向当前对话通道发一条确认消息告诉用户已收到（异步通道用输出门工具，同步通道直接返回文本），**然后再**执行具体排查工具。确认消息不代表任务完成，发出后仍需继续执行实际工具并最终汇报结果。\n"
	prompt += "- 用户从其他渠道发来「在哪里/怎么样了」这类追问时，先回忆上次任务的通道与上下文，再回同一通道。"

	if a.indexer != nil {
		prompt += "\n\n" + a.indexer.BuildToolPrompt()
	}

	// 技能索引（方案B）：轻量注入已加载技能列表，LLM 匹配到场景时
	// 主动 skill_info 拉取全文按文档执行
	if a.skillIndex != nil {
		if idx := a.skillIndex.SkillIndex(); idx != "" {
			prompt = appendPromptBlock(prompt, "【可用技能】以下是已安装的原生技能。当用户请求与某技能描述匹配时，\n先用 skill_info(\"技能名\") 拉取全文，再严格按文档步骤执行：\n"+idx)
		}
	}

	// 首启人格门禁（跨通道唯一闸口）：人格未确认时，要求模型主动询问用户。
	// 系统提示词每轮重建，因此 WebUI / QQ / CLI / ACP / 邮件等所有通道都会带上它；
	// 模型调用 persona_set（或用户在 WebUI 向导里选）落地后，标记置位，本段消失。
	if a.personaStore != nil && !a.personaStore.PersonaInitialized() {
		prompt = appendPromptBlock(prompt, "【首启人格设定】你的**人格设定尚未确认**。请在本轮回复里先问用户一句："+
			"要用默认人格，还是自定义一个？拿到明确答复后**必须调用 persona_set 工具**落库："+
			"用户选默认 → mode=default；自定义 → mode=custom 且把内容写进 content；"+
			"用户说以后再说 → mode=later。用户答复前不要假设已设置，也不要反复追问同一件事。")
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
			if len(desc) > 60 {
				desc = desc[:60] + "..."
			}
			pluginDesc[plg] = desc
		}
	}
	var sb strings.Builder
	sb.WriteString("\n\n【可用工具能力】\n")
	sb.WriteString("工具按插件分组注册。需要某个插件的具体工具时，调用 get_plugin_tools(\"{插件名}\") 获取该插件的完整工具定义（名称/参数/用途）。\n")
	for _, plg := range order {
		sb.WriteString(fmt.Sprintf("- %s (%d 个工具)", plg, byPlugin[plg]))
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
		"doc_query",
		// 安全模块：列举变量（只读，无共享写）
		"secret_list",
	} {
		declareParallelTool(name)
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

	if a.personaStore != nil {
		tools = append(tools, toolDef("persona_set", "【首启人格】落地用户的人格选择并记录「已经问过」。仅在用户明确答复后调用：默认用 mode=default；自定义用 mode=custom 并把人格内容放进 content；用户说以后再说用 mode=later。", map[string]interface{}{
			"mode":    map[string]interface{}{"type": "string", "description": "default | custom | later"},
			"content": map[string]interface{}{"type": "string", "description": "自定义人格内容（mode=custom 时必填）"},
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
		tools = append(tools, toolDef("memory_merge", "【记忆清理】合并两个同义实体。将所有关系从 source 重定向到 target，然后彻底删除 source。注意：实体删除后不可恢复，合并前请确认语义一致。", map[string]interface{}{
			"source": map[string]interface{}{"type": "string", "description": "被合并的实体名（合并后消失）"},
			"target": map[string]interface{}{"type": "string", "description": "保留的实体名"},
		}, "source", "target"))
		tools = append(tools, toolDef("memory_delete_entity", "【记忆清理】删除指定名称的记忆块及其所有关联关系。name 必须是**块文本**（端点名），不是关系文本；按精确匹配定位，不会连带删掉名字相近的其他块。要按子串批量清理改用 memory_purge（subject_contains）。用于清理无用的噪音块、distiller 自动产生的垃圾节点、确认无用的旧数据。此操作不可恢复。", map[string]interface{}{
			"name": map[string]interface{}{"type": "string", "description": "要删除的实体名称"},
		}, "name"))
		tools = append(tools, toolDef("memory_block_merge", "【记忆清理】标记两个实体在指定轮次内不尝试合并，用于阻止误判。当 LLM 判断两个实体虽然相似但不是同一事物时，使用此工具阻止后续心跳自动推送合并候选。每次心跳扫描双方计数各减一，归零后恢复候选资格。", map[string]interface{}{
			"entity_a": map[string]interface{}{"type": "string", "description": "第一个实体名"},
			"entity_b": map[string]interface{}{"type": "string", "description": "第二个实体名"},
			"rounds":   map[string]interface{}{"type": "integer", "description": "阻止轮次数（每次心跳各减一，归零后恢复）"},
		}, "entity_a", "entity_b", "rounds"))
		tools = append(tools, toolDef("memory_purge", "【记忆清理】删除记忆库中符合条件的垃圾关系和数据。当用户要求整理记忆时，用 memory_introspect 发现低质量实体后，用此工具批量删除。如 @merged 后缀的残留实体、mentionCount=0 的孤立实体、distiller 自动生成的噪音关系等。支持软删（soft）和物理删除（hard）。", map[string]interface{}{
			"subject_contains": map[string]interface{}{"type": "string", "description": "主体名包含的关键词，如 '@merged' 可清理已合并残留"},
			"relation_type":    map[string]interface{}{"type": "string", "description": "关系类型，如 '提及'、'回应'"},
			"target_contains":  map[string]interface{}{"type": "string", "description": "客体名包含的关键词"},
			"mode":             map[string]interface{}{"type": "string", "description": "soft（标记删除）/ hard（物理删除）", "default": "soft"},
		}))
		tools = append(tools, toolDef("memory_edit", "【记忆清理】编辑单条记忆关系：删除旧的 relation 并写入新的。用于修正错误的实体名或关系类型。", map[string]interface{}{
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

	if a.docStore != nil {
		tools = append(tools, toolDefWith("doc_query", "查询文档记忆。输入查询内容，返回相关文档摘要。", map[string]interface{}{
			"query": map[string]interface{}{"type": "string", "description": "查询内容"},
			"top_k": map[string]interface{}{"type": "integer", "description": "返回数量", "default": 3},
		}, []string{"query"}, parallelOpts()))
		tools = append(tools, toolDef("doc_commit", "提交一条文档记忆。将重要信息显式写入文档记忆层。", map[string]interface{}{
			"content": map[string]interface{}{"type": "string", "description": "文档内容"},
			"summary": map[string]interface{}{"type": "string", "description": "摘要（可选）"},
			"tags": map[string]interface{}{
				"type":        "array",
				"description": "标签列表",
				"items":       map[string]interface{}{"type": "string"},
			},
			"media_digests": map[string]interface{}{
				"type":        "array",
				"description": "可选：这篇文档关联的媒体 digest（对话或 memory_recall 的「关联媒体」里显示的十六进制串，短的即可）。填了以后检索到这篇文档就能看到并取回原图/音频。",
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
	tools = append(tools, toolDefWith("get_plugin_tools", "获取指定插件的完整工具定义(名称/参数/用途)。参数 plugin_name 传插件名(见系统提示的【可用工具能力】列表)。省略时返回全部插件的工具摘要。", map[string]interface{}{
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

		tools = append(tools, toolDef("output_send__"+ch.Name, desc+"。能力: "+capStr+"。payload 为消息载荷（type 默认 text，可省略），meta 为 JSON 发送元数据。用 _help 查看 meta 格式与 type 枚举。", map[string]interface{}{
			"payload": map[string]interface{}{
				"type":        "string",
				"description": "消息载荷。type=text 时填文字，type=file/image 时填 URL 或路径",
			},
			"meta": map[string]interface{}{
				"type":        "string",
				"description": "JSON 对象，包含发送所需的元数据。用 output_send__" + ch.Name + "_help 查看 meta 格式",
			},
			"type": map[string]interface{}{
				"type":        "string",
				"description": "载荷类型，默认 text；其它枚举用 channel._help 查看",
			},
		}, "payload"))

		tools = append(tools, toolDef("output_send__"+ch.Name+"_help", "查看 "+ch.Name+" 输出通道的 meta 格式说明和 type 枚举", map[string]interface{}{}))
	}

	tools = append(tools, toolDefWith("output_list_channels", "列出所有可用输出通道及其能力（如 text/file/image/audio）和对应的输出门工具名称。", map[string]interface{}{}, nil, parallelOpts()))

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
				"description": "reclaim/destroy：该子手头未处理消息的处置。" +
					"keep=转回主 agent 队列（默认，宁可多做一件不可默默丢一条）/ drop=丢弃（已确认不要）。" +
					"无论哪种都会逐条记日志；异步通道（如 qq）被丢时用户收不到任何回复，请慎重选 drop。",
				"enum": []string{"keep", "drop"},
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
	// 留在这里的是**待��退场**的部分：下面这一大块 pendingMedia 只服务于
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
func declareParallelTool(name string) {
	builtinDefs.mu.Lock()
	builtinDefs.byName[name] = sdk.BuiltinToolDef{Name: name, ParallelSafe: true}
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
