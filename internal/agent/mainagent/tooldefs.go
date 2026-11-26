package mainagent

import (
	"fmt"
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	"log"
	"strings"
	"sync"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/meta"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
	sdkmeta "github.com/JianFeeeee/hologramagentsdk/meta"
)

func (m *Module) buildMemoryContext(input string, maxTokens int, scenes []string) string {
	if m.st.indexer == nil {
		return ""
	}
	injected := m.st.indexer.BuildContextInScene(input, scenes)
	s := m.st.indexer.FormatContext(injected)

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
	if mc := m.mediaContextForInjectedEntities(injected); mc != "" {
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
	if m.st.coldcontextStore != nil {
		if md := m.CrossModalMarkdown(m.retrieveCrossModal(input, 3, m.st.fusionCfg)); md != "" {
			if s != "" {
				s += "\n"
			}
			s += md
		}
	}

	if maxTokens > 0 {
		s = kit.TruncateByTokens(s, maxTokens)
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
func (m *Module) BuildTaskMemoryContext(f *TaskFrame, input string, maxTokens int) string {
	scenes := m.SceneKeysFor(evtOf(f), "")
	if f == nil {
		return m.recallText(input, "input", maxTokens, scenes)
	}
	if !m.RecallDeclared(f.Evt) {
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
	turn := m.resolveTurnScenes(f, "")
	scenes = mergeSceneKeys(scenes, turn.Keys)
	return m.recallText(query, trigger, maxTokens, scenes)
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
func (m *Module) RecallTextFor(query, trigger string, scenes []string) string {
	memTokens := 0 // 0 = 不截断
	if m != nil && m.a != nil {
		memTokens = m.ComputeTokenBudget().MemoryTokens
	}
	return m.recallText(query, trigger, memTokens, scenes)
}

// recallText 是召回侧的共同实现：query → 记忆索引文本（空串表示无）。
//
// 输入侧的 buildTaskMemoryContext 与工具侧的 recallTextFor 都收敛到这里，
// 使“同一份 query、同一次预算、同一条审计日志”只写一遍。
func (m *Module) recallText(query, trigger string, maxTokens int, scenes []string) string {
	if m == nil || m.a == nil || query == "" || m.st.indexer == nil {
		return ""
	}
	text := m.buildMemoryContext(query, maxTokens, scenes)
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

// buildSystemPrompt 生成发给 LLM 的 system prompt。
//
// ★ 2026-11-04 起**不再自己拼块**：所有内容（内核的十几块 + 插件声明的段）
// 统一由 renderSystemPrompt 按 Order 排序渲染。本函数只负责把当轮上下文
// （memContext）递进去。
//
// 为什么要统一（用户裁定）：
//
//	「把现在内核所有 systemprompt 的相关注入都接入到这个统一的
//	  systemprompt 管理器上」
//
// 此前两套并存（内核硬拼 + 插件声明），后果是**顺序不可控**：插件段只能
// 整块插在某个固定位置；内核那十几块的相对顺序由函数体书写顺序决定，
// 改顺序得挪代码。现在顺序由 Order 常量决定，内核只是"另一个声明者"。
//
// userInput 参数保留：它是部分内核块（如记忆上下文的相关性）的输入，
// 由调用方在 buildMemoryContext 里使用后传入，渲染层不需要它。
func (m *Module) BuildSystemPrompt(memContext string, userInput string) string {
	_ = userInput
	return m.renderSystemPrompt(memContext)
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

func (m *Module) BuildToolCatalog() string {
	defs := m.BuildToolDefs()
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
		plg := m.ResolveToolPlugin(name)
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
	//   显式名单登记。原因：工具表是按需构造的（受 `if m.st.coldcontextStore != nil`
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
	//   那些工具都在 `if m.st.knowledge != nil` 之类的条件分支里。
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
	//   `if m.st.coldcontextStore != nil` 之类的条件分支控制），而 textForVector
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

// toolDefsFace 是本类型注册给基座的**工具清单只读面**。
//
// ★ 把"渲染结果"（map 形态，发给 LLM）转成"工具定义"（api 形态，给 device 读）：
//
//	两者的字段集不同，但**同一份来源**（BuildToolDefs），不会漂移。
func (m *Module) toolDefsFace() []agentAPI.ToolDefinition {
	raw := m.BuildToolDefs()
	out := make([]agentAPI.ToolDefinition, 0, len(raw))
	for _, item := range raw {
		mp, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := mp["function"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := fn["description"].(string)
		out = append(out, agentAPI.ToolDefinition{
			Name:         name,
			Description:  desc,
			ParallelSafe: m.ToolParallelSafe(name),
			Timeout:      builtinToolTimeout(name),
		})
	}
	return out
}

// collectPrivateNames 从渲染结果里挑出属于本类型私有族的名字。
func (m *Module) collectPrivateNames(raw []interface{}) []string {
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		mp, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := mp["function"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name != "" && IsPrivateToolName(name) {
			names = append(names, name)
		}
	}
	return names
}

// BuildToolDefs 渲染本 agent 的完整工具表（主 agent 的装配面）。
func (m *Module) BuildToolDefs() []interface{} {
	raw := m.buildToolDefs()
	// ★ 声明本轮真正渲染出来的私有工具（幂等）：分岔判据按 Owner 走，
	//   而前缀表写错不该让工具静默失效 —— 声明只认"真实存在的名字"。
	m.DeclarePrivateTools(m.collectPrivateNames(raw))
	return raw
}

// buildToolDefs 是渲染实体（BuildToolDefs 负责渲染后的归属声明）。
func (m *Module) buildToolDefs() []interface{} {
	var tools []interface{}

	// ★ **公有面由基座给**（用户裁定 2026-11-25）：
	//   「基座给类型提供的是直接过滤后的清单，类型需要自己再按 deny 等过滤」。
	//
	// 公有 = 内核族声明 + IO 设备工具 + 插件工具（stageHost 登记簿）+
	//        registry 表（按侧别渲染）。这四处**不再由本类型各取一遍** ——
	// 各取一遍的后果是四个类型看得到的公有面互不相同（resident 只有
	// registry，连 IO 设备工具都列不出来），而它们本该同构（docs §0.3）。
	tools = append(tools, m.a.PublicToolDefs()...)

	// ★ **槽内工具不在这里渲染**（基座在 `PublicToolDefs()` 的 registry 段里
	//   已经把它们渲染了 —— 槽工具与插件工具登记在**同一张表**）。
	//
	// 教训（2026-11-25 实测）：曾在这里按 `ToolsFor(SideAny)` 再拼一遍，
	// 于是每个工具名在发出的请求里出现两次 ⇒ 上游硬拒整轮请求：
	//
	//	Tool names must be unique.
	//
	// 现场：新内核连「你好」都答不出来，而旧内核正常。

	// ★ 记忆族（stablemem_recall / commit / introspect）：**声明在本类型**。
	//
	// 用户裁定（2026-11-19）：渲染归类型。它们读的是本类型的私有状态
	// （图记忆 + 索引器），执行体也在这个包里 —— 声明留在 internal/memory
	// 就是"schema 一个包、执行体另一个包"，门控与可用性会各自漂移。
	// 那一份现在只服务**插件只读展示**（webui/cli 的 /memory tools）。
	if m.st.indexer != nil {
		tools = append(tools, memoryToolDefs()...)
	}

	// 超长工具结果的按范围回读工具。
	//
	// ★ 只在**真的存在**落盘结果时才暴露：
	//   · 绝大多数会话没有超长结果，无条件暴露等于每轮多付一份 schema 开销
	//     （本仓已有"175 个工具占满上下文"的实测教训）；
	//   · 没有句柄时这个工具对模型毫无用处，给了只会诱导它乱猜 event_id。
	if m.HasStoredToolResults() {
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

	if m.st.personaStore != nil {
		tools = append(tools, toolDef("persona_set", "【首启人格】落地用户的人格选择并记录「已经问过」。仅在用户明确答复后调用：默认用 mode=default；自定义用 mode=custom 并把人格内容放进 content；用户说以后再说用 mode=later。", map[string]interface{}{
			"mode":    map[string]interface{}{"type": "string", "description": "default | custom | later"},
			"content": map[string]interface{}{"type": "string", "description": "Custom persona text (required when mode=custom)"},
		}, "mode"))
	}

	// 安全模块的变量库工具（文档 C8）。
	//
	// 仅当安全模块真的启用时才暴露：未启用时变量库不可用，
	// 把工具给模型只会让它反复撞到"安全模块未启用"（无谓往返）。
	if m.a.SecretStore() != nil {
		tools = append(tools, agent.SecretToolDefs()...)
	}

	if m.graphDB() != nil {
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

	if m.st.coldcontextStore != nil {
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

	if m.st.social != nil {
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

	// ★★ **注册表驱动的工具**（用户 2026-11-18 质问的修法）。
	//
	// 这里不再写任何 `if isChild` + 工具名字面量：
	// 每条工具的 schema/侧别/归属/执行体都在 registry 里登记过，
	// 基座只按"我的侧别"取表渲染。
	//
	// 于是加一类子 agent、或换一组管理动作，**不需要回来改这里**。
	//
	// ★ 通道类工具（output_list_channels / input_channels）也在注册表里
	//   （`DeclaredOnly` + `OutputGate` 标记）—— **不再**由
	//   `agent.OutputToolDefs()` 二次渲染。两处渲染就是两份事实来源，
	//   实测后果是同一个工具名出现两次、上游硬拒绝整轮请求
	//   （`Tool names must be unique`）。
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

	// ★ **不再过滤**（用户裁定 2026-11-25）：
	//   「基座给类型提供的是直接过滤后的清单」—— 类型拿到就是最终的，
	//   不对它再判一次 allow/deny（那已经是基座在 `PublicToolDefs()` 里做的）。
	//
	// 私有族天然只属于本类型（数据与执行体都在本包），父的 allow/deny 是
	// 对"这个 agent 可见的公具面"的收窄，不改变"类型自己有什么"。
	return tools
}

// builtinDefRegistry 汇总内置工具的声明项。
//
// 形态照 StageHost.NoMemoryToolNames：**一次聚合**，查询不再遍历工具表。
// 之前 builtinToolParallelSafe 每次 toolParallelSafe 调用都重跑一遍
// BuildToolDefs()，等于 O(工具数) 的重复劳动 —— 声明是静态的，没有理由每次重算。
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
// 那些工具都在 `if m.st.knowledge != nil` 之类的条件分支里，测试环境根本不
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

// schemaOfTool 把注册表的 Properties/Required 还原成 JSON Schema 形状。
//
// ★ 形状必须与 IO / stageHost 工具一致（`{"type":"object","properties":…,
//
//	"required":…}`）：模型侧看到的 schema 只有一种形状，不能因来源不同而分叉。
func schemaOfTool(t registry.Tool) map[string]interface{} {
	props := t.Properties
	if props == nil {
		props = map[string]interface{}{}
	}
	out := map[string]interface{}{
		"type":       "object",
		"properties": props,
	}
	if len(t.Required) > 0 {
		out["required"] = t.Required
	}
	return out
}
