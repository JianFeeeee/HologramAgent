package core

import (
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"time"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	"github.com/JianFeeeee/HomeAgent/internal/knowledge"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/memory/document"
	"github.com/JianFeeeee/HomeAgent/internal/memory/text"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	"github.com/JianFeeeee/HomeAgent/internal/security"
)

// 工具执行超时的默认值与硬上限。
const (
	// defaultToolTimeout 是**未声明** Timeout 的工具沿用的值。
	// 与改动前逐字一致（60s）：存量插件不改一行，行为不变。
	defaultToolTimeout = 60 * time.Second
	// maxToolTimeout 是硬上限，防止插件声明成 0（无限）或荒唐的大值
	// 把任务队列堵死。10 分钟足够覆盖已知的长任务：
	//   cmd_run 长命令 / healthcheck（120s）/ output_send 到子 pipeline。
	maxToolTimeout = 10 * time.Minute
)

// resolveToolTimeout 取某个工具应使用的超时。
//
// 顺序：ToolDef.Timeout（插件声明）→ defaultToolTimeout。
// 声明值非法（解析失败 / ≤0 / 超上限）时**回退默认并记日志**，
// 不静默接受：写错单位（如 "130" 少了 s）是最容易犯的错，
// 让它默默变成 60s 会让人以为"声明没生效"却查不出原因。
func (a *Agent) resolveToolTimeout(toolName string, args map[string]interface{}) time.Duration {
	if a.stageHost == nil {
		return defaultToolTimeout
	}
	def := a.stageHost.ToolDef(toolName)
	if def == nil {
		// 内置工具（output_send__* 等）不在 StageHost 里 —— 走内置表。
		def = &sdk.ToolDef{}
	}
	// ★ 第二条通道：调用参数里的 timeout。
	//
	// 有些工具的时长**不是固定的**，而是由本次调用参数决定 —— 典型是
	// cmd_run（模型自己传 timeout）。只做静态声明修不好它：
	//   声明 130s：模型传 timeout=10s 时白等（浪费配额）
	//   声明 30s ：模型传 timeout=2m  时仍被砍（就是原 bug）
	// 所以这里把参数值也算进来，取**较大者**。
	//
	// 为什么取较大而不是直接用参数值：参数是模型给的，可能写成
	// "1" 这种缺单位的（生产日志实测过）或是恶意的超大值。
	// 用 max(声明值, 参数值) 并夹到硬上限，两边都兜住。
	base := defaultToolTimeout
	// 声明来源有两处：StageHost（插件工具 + 经 RegisterTool 注册的）
	// 与内置工具表（output_send__* 等由 switch 分派的）。
	// def.Timeout 为空时再查内置表。
	declared := strings.TrimSpace(def.Timeout)
	if declared == "" {
		declared = builtinToolTimeout(toolName)
	}
	if declared != "" {
		if d, err := time.ParseDuration(declared); err == nil && d > 0 {
			base = d
		} else if err != nil {
			log.Printf("[agent] tool %s 声明的 Timeout=%q 解析失败（%v），用默认 %v",
				toolName, declared, err, defaultToolTimeout)
		}
	}
	if v, ok := args["timeout"].(string); ok && strings.TrimSpace(v) != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil && d > base {
			base = d
		}
	}
	if base > maxToolTimeout {
		log.Printf("[agent] tool %s 超时 %v 超过硬上限，夹到 %v", toolName, base, maxToolTimeout)
		return maxToolTimeout
	}
	return base
}

// resolveToolTimeoutDeclared 只取静态声明（保留给需要区分来源的调用方）。
func (a *Agent) resolveToolTimeoutDeclared(toolName string) time.Duration {
	if a.stageHost == nil {
		return defaultToolTimeout
	}
	def := a.stageHost.ToolDef(toolName)
	if def == nil || strings.TrimSpace(def.Timeout) == "" {
		return defaultToolTimeout
	}
	d, err := time.ParseDuration(strings.TrimSpace(def.Timeout))
	if err != nil {
		log.Printf("[agent] tool %s 声明的 Timeout=%q 解析失败（%v），回退默认 %v",
			toolName, def.Timeout, err, defaultToolTimeout)
		return defaultToolTimeout
	}
	if d <= 0 {
		log.Printf("[agent] tool %s 声明的 Timeout=%q 非正数，回退默认 %v",
			toolName, def.Timeout, defaultToolTimeout)
		return defaultToolTimeout
	}
	if d > maxToolTimeout {
		log.Printf("[agent] tool %s 声明的 Timeout=%v 超过硬上限，夹到 %v",
			toolName, d, maxToolTimeout)
		return maxToolTimeout
	}
	return d
}

// fmtDuration 把时长写成给模型看的中文形式（秒/分钟）。
//
// 不用 time.Duration.String()：它给出 "1m0s"、"1h30m0s" 这类紧凑写法，
// 模型读得懂，但用户看错误提示时"1m0s"不如"1 分钟"直观。
func fmtDuration(d time.Duration) string {
	if d%time.Minute == 0 && d >= time.Minute {
		return fmt.Sprintf("%d 分钟", int(d/time.Minute))
	}
	return fmt.Sprintf("%d 秒", int(d/time.Second))
}

// toolOutcome 是一次工具执行的完整结果：**文本**（给模型）与
// **原值**（给契约判断）分开携带。
//
// 为何必须分开：executeToolCall 历来只返回 string，结构化信息在这一步
// 被抹平，导致（a）ToolResult.Success 无法诚实化、（b）after_toolcall
// 阶段插件对结构化结果的改写因类型断言失败而静默失效。
type toolOutcome struct {
	Text string
	Raw  interface{}
}

// executeToolCall 保留原签名（spawn.go 与既有测试依赖），只取文本。
func (a *Agent) executeToolCall(tc agentAPI.ToolCall, channel string, turnScenes ...string) string {
	return a.executeToolCallOutcome(tc, channel, turnScenes...).Text
}

// executeToolCallOutcome 是完整形态：崩溃/超时同样以 ToolError 表达，
// 使「工具故障」与「工具报告的业务失败」在上层可区分。
func (a *Agent) executeToolCallOutcome(tc agentAPI.ToolCall, channel string, turnScenes ...string) (out toolOutcome) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			log.Printf("[agent] tool %s panic: %v\n%s", tc.Name, r, stack)

			if pluginName := a.resolveToolPlugin(tc.Name); pluginName != "" {
				if a.pluginHealth.recordCrash(pluginName) {
					log.Printf("[agent] plugin %s exceeded crash threshold, scheduling reload", pluginName)
				}
			}

			te := newToolError("panic", "", fmt.Sprintf("工具 %s 执行崩溃: %v", tc.Name, r),
				"这是工具自身故障（不是你的参数问题），请勿原样重试；可换用其他工具或告知用户。")
			out = toolOutcome{Text: te.Error(), Raw: te}
		}
		// ★★ 出向脱敏（2026-10-18 补）：工具返回值在交给模型**之前**必须清洗。
		//
		// ## 原缺陷
		//
		// 安全模块只做了**入向**回填（StageHost.ExecuteTool 里 FillMap，
		// 把别名换成真凭据给插件用），**从不处理出向**。于是：
		//
		//	模型调 enc_shell 跑 curl → 服务端把 key 回显在响应体里
		//	→ 工具原样返回 → f.CurResult → f.Msgs（role=tool）
		//	→ **明文 key 直接进了模型上下文**
		//
		// 实测证据（2026-10-18 08:48，/home/newqqagent/log）：
		//   `[security] 工具 enc_shell 参数回填 1 处（别名 [sec_...]）`  ← 入向有
		//   紧随其后 `tool enc_shell result: {...}`                        ← 出向无
		//
		// ## 为什么 scrubbing 已有却不管用
		//
		// `scrubEventSecrets` 只在 `RelevanceContext.Append`（写上下文/记忆）
		// 时运行 —— 那是**工具结果已经发出去之后**。同一轮里模型看到的是
		// 未清洗的 f.Msgs。记忆侧干净、当轮泄漏，是最坏的组合：事后查不到。
		//
		// ## 为什么放在这个 defer 里
		//
		// executeToolCallOutcome 有多个 return 出口（插件工具、io 工具、
		// 设备授权拒绝、不存在、panic），逐个加必然漏。defer 是唯一
		// 能覆盖全部分支的位置，且 panic 路径也一起覆盖。
		if out.Text != "" {
			if st := security.Global(); st != nil {
				if clean, evts := st.Replace(out.Text, "tool_out:"+tc.Name, security.Patterns); len(evts) > 0 {
					out.Text = clean
					for _, e := range evts {
						log.Printf("[security] 工具输出 %s 拦截 %s（模式 %s，%d 字节）",
							tc.Name, e.Alias, e.Kind, e.Len)
					}
				}
			}
		}
	}()

	done := make(chan toolOutcome, 1)
	go func() {
		done <- a.executeToolCallInner(tc, channel, turnScenes)
	}()

	// ★ 超时：优先用工具**自己声明**的时长，而不是一刀切 60s。
	//
	// 2026-10-17 生产实测的缺陷：这里原本写死 60s，与工具自己的超时配置
	// **互相矛盾**且让内层形同虚设：
	//
	//	23:13:38  执行 cmd_run（agent 传了 130s 级 timeout 参数）
	//	23:14:38  ★ 内核 60s 硬超时掐断 → 「执行超时（60秒）」
	//
	// 而 cmd_run 内部明明有自己的 ctx（按 timeout 参数）；healthcheck
	// 更是声明默认 120s —— 直接砍掉一半。output_send__*（向开发线投递，
	// 子 pipeline 执行）天然就是长任务，也一起被误杀。
	//
	// 现在：ToolDef.Timeout 声明多久就给多久，但仍有**硬上限**
	// （maxToolTimeout = 10m）兜住"插件声明成 0/无限"这种把队列堵死的写法。
	toolTimeout := a.resolveToolTimeout(tc.Name, tc.Arguments)
	select {
	case result := <-done:
		return result
	case <-time.After(toolTimeout):
		log.Printf("[agent] tool %s timed out after %v", tc.Name, toolTimeout)
		te := newToolError(ErrReasonTimeout, "", fmt.Sprintf("工具 %s 执行超时（%s）", tc.Name, fmtDuration(toolTimeout)),
			"该工具本次未在时限内返回。可改用更小的任务，或换用其他工具。")
		return toolOutcome{Text: te.Error(), Raw: te}
	}
}

func (a *Agent) executeToolCallInner(tc agentAPI.ToolCall, channel string, turnScenes []string) toolOutcome {
	// 参数没法用（被 max_tokens 截断，或 JSON 写坏了）：**不要**拿着空/残缺参数去调工具。
	// 否则工具会报 “path is required”“command is required” 这类与真因无关的错，
	// 模型看不出真因、只能原样重试（实测 cmd_run 失败率高达 34%~48%）。
	// __arg_error 里带的已经是分因写好的可执行指引，直接交回模型。
	if msg, ok := tc.Arguments["__arg_error"].(string); ok && msg != "" {
		log.Printf("[agent] tool %s skipped: arguments unusable (truncated or malformed)", tc.Name)
		return toolOutcome{Text: msg}
	}

	// 按 schema 预校验（阶段 1c）。放在分派**之前**：坏参数不该进到工具内部
	// 再报一句与真因无关的 "path is required"——模型据此只会原样重试。
	// ⚠️ 校验器刻意宽松（见 argvalidate.go）：只拦真正无法解析的形态，
	// 对 "true"/20/"20s" 这类宽松等价形态一律放行，避免制造新失败。
	if ve := a.validateArgsAgainstSchema(tc); ve != nil {
		log.Printf("[agent] tool %s rejected by schema validation: field=%s reason=%s", tc.Name, ve.Field, ve.Reason)
		return toolOutcome{Text: renderToolError(tc.Name, ve), Raw: ve}
	}

	switch {
	case tc.Name == "persona_set":
		return toolOutcome{Text: a.executePersonaTool(tc)}
	case strings.HasPrefix(tc.Name, "memory_"):
		return toolOutcome{Text: a.executeMemoryTool(tc, turnScenes)}
	case strings.HasPrefix(tc.Name, "social_"):
		return toolOutcome{Text: a.executeSocialTool(tc)}
	case strings.HasPrefix(tc.Name, "knowledge_"):
		return toolOutcome{Text: a.executeKnowledgeTool(tc)}
	case strings.HasPrefix(tc.Name, "doc_"):
		return toolOutcome{Text: a.executeDocTool(tc)}
	case strings.HasPrefix(tc.Name, "output_send__") && strings.HasSuffix(tc.Name, "_help"):
		return toolOutcome{Text: a.executeOutputSendHelp(tc)}
	case strings.HasPrefix(tc.Name, "output_send__"):
		return toolOutcome{Text: a.executeOutputSendTool(tc)}
	case tc.Name == "output_list_channels":
		return toolOutcome{Text: a.executeOutputListChannels()}
	case tc.Name == "input_channels":
		return toolOutcome{Text: a.executeInputChannels(tc)}
	case tc.Name == "resident_agents":
		return toolOutcome{Text: a.executeResidentAgents(tc)}
	case tc.Name == "notify_parent":
		return toolOutcome{Text: a.executeNotifyParent(tc)}
	case tc.Name == "inputch_note":
		return toolOutcome{Text: a.executeInputchNote(tc)}
	case tc.Name == "plgreload":
		return toolOutcome{Text: a.executePluginReload()}
	case tc.Name == "get_plugin_tools":
		pluginName, _ := tc.Arguments["plugin_name"].(string)
		return toolOutcome{Text: a.executeGetPluginTools(pluginName)}
	case tc.Name == "spawn_child":
		return toolOutcome{Text: a.executeSpawnChild(tc, channel)}
	case tc.Name == "child_result":
		return toolOutcome{Text: a.executeChildResultTool(tc)}
	case strings.HasPrefix(tc.Name, "llm_"):
		return toolOutcome{Text: a.executeLLMTool(tc)}
	case strings.HasPrefix(tc.Name, "secret_"):
		return toolOutcome{Text: a.executeSecretTool(tc)}
		// ★★ 2026-10-06：describe_image / transcribe_audio / ocr_image 已移出内核，
		//   改为内置插件 multimodal（见 internal/plugins/multimodal）。
		//   理由：它们要读本地文件（IO），而内核承诺零 IO。
		//   这三个名字现在由 StageHost 路由到插件 —— 上面 if 走不到，
		//   会落到 stageHost.ExecuteTool，正是插件工具的正常路径。
	}

	if a.stageHost != nil {
		if result, err := a.stageHost.ExecuteTool(tc.Name, tc.Arguments); err == nil {
			// Raw 必须带上：否则结构化失败（{"error":…} / ToolError）在这一步被抹平成文本，
			// Success 又会退回恒真——正是阶段 1b 要修的那个洞。
			return toolOutcome{Text: renderToolResult(tc.Name, result), Raw: result}
		} else if !agentIO.IsToolNotFound(err) {
			// 非「不存在」= 真的执行失败，如实上报（可被 on_error/retry 处置）。
			return toolOutcome{Text: fmt.Sprintf("工具 %s 执行失败: %v", tc.Name, err)}
		}
		// 是「不存在」：继续往下走 io / 设备路径，两处都没有才报缺工具。
	}

	// 设备类工具的**授权闸**（最小授权的缺口在这里）。
	//
	// 设备指令类工具（device_ctl_cmdrun/screensee/computeruse/...）走的是工具面，
	// 而 AllowedOutputs 只作用于 output_send__<通道> —— 于是"授权"对指令类工具完全无效：
	// 驻留子只要拿到 device_ctl_cmdrun 就能指挥**任意**设备。
	// 这里按目标设备的通道名 device/<id> 查同一道闸：父授权了哪台设备，才允许指挥哪台。
	if _, isDeviceTool := a.io.DeviceOfTool(tc.Name); isDeviceTool {
		if id, _ := tc.Arguments["device_id"].(string); id != "" && !a.IsOutputAllowed("device/"+id) {
			return toolOutcome{Text: fmt.Sprintf("设备 [%s] 未授权给本 agent（可用设备见 output_list_channels 的 device/<id> 通道，或 devicedetect）", id)}
		}
	}

	if a.tracker != nil {
		a.tracker.PreAction(tc.Name)
	}
	result, err := a.io.ExecuteTool(tc.Name, tc.Arguments)
	if a.tracker != nil {
		if cs := a.tracker.PostAction(tc.Name); cs != nil && len(cs.Files) > 0 {
			log.Printf("[agent] tool %s changed %d files (changeset: %s)", tc.Name, len(cs.Files), cs.ID)
		}
	}
	if err != nil {
		if agentIO.IsToolNotFound(err) {
			// 工具是动态注册的，"不存在"是常态而非异常（插件未加载/已卸载/崩溃）。
			// 文案必须让模型知道该做什么，而不是含糊的"执行失败"——
			// 后者会让模型反复重试同一个不存在的名字。
			return toolOutcome{Text: fmt.Sprintf("工具 %s 不存在或未注册：它可能属于未加载/已崩溃的插件。"+
				"先调 get_plugin_tools(\"\") 看当前可用工具，或 output_list_channels 看通道；"+
				"确认名称无误后再调用", tc.Name)}
		}
		return toolOutcome{Text: fmt.Sprintf("工具 %s 执行失败: %v", tc.Name, err)}
	}
	// Raw 必须带上：否则结构化失败（{"error":…} / ToolError）在这一步被抹平成文本，
	// Success 又会退回恒真——正是阶段 1b 要修的那个洞。
	return toolOutcome{Text: renderToolResult(tc.Name, result), Raw: result}
}

// toolNotFound / isToolNotFound 是 agentIO 哨兵在 core 侧的薄封装，
// 便于 core 内部与测试直接使用（core 依赖 io，不反向）。
func toolNotFound(name string) error { return agentIO.ToolNotFound(name) }

func isToolNotFound(err error) bool { return agentIO.IsToolNotFound(err) }

func (a *Agent) executeMemoryTool(tc agentAPI.ToolCall, turnScenes []string) string {
	g := a.graphMem()
	if g == nil {
		if tc.Name == "memory_document_query" {
			return a.executeDocTool(tc)
		}
		return "图记忆系统不可用"
	}
	// 整理类工具需要**完整内核**的记忆整理面（块/媒体/结构操作）。
	// 轻量内核（驻留子）只有图记忆共同面 ⇒ 这些操作明确不可用，不静默降级。
	requireFull := func() string {
		if a.memory == nil {
			return "本 agent 是轻量内核：只能读写图记忆，记忆整理（合并/删除/清理/编辑/统计）不可用"
		}
		return ""
	}
	switch tc.Name {
	case "memory_recall":
		query, _ := tc.Arguments["query_intent"].(string)
		depth, _ := tc.Arguments["depth"].(float64)
		if depth <= 0 {
			depth = 2
		}
		if query == "" {
			return "请输入查询关键词"
		}
		// 关键词提取：支持逗号分隔和自然语言
		keywords := strings.Split(query, ",")
		if len(keywords) == 1 {
			keywords = memory.ExtractKeywords(query)
		}
		// 排序模式：默认相关性；显式要"最近/最新"时用 recent。
		//
		// ★ 为什么要分两种（实测 v4 跑分）：overwrite 组考的是"新值覆盖旧值"，
		// 相关性排序下新旧同名实体的命中层级完全相同，只能靠时间分胜负；
		// 而 casual 组问"某个服务端口是多少"，要的是实词精确命中。
		// 用一个排序同时服务这两类问题，必然有一边错。
		sortMode := memory.ParseSortMode(fmt.Sprint(tc.Arguments["sort"]))

		// 块向量召回优先。
		//
		// ★ 为什么要有它（这是这条路径缺失的直接后果）：memory_blocks 是
		// 带 vector+fingerprint 的图节点载体，但此前**没有任何召回读它** ——
		// BlocksForNode 只能按端点反查，得先知道 nodeID。于是「问一个具体
		// 问题」只能退到 entities 的 jieba+LIKE，实测跨维度定位 0/5
		//（问「第181批的值班手册是第几版」完全答不出）。
		//
		// 混合而非替换：端口号（8328）、分机号（4324）这类纯数字串向量天然弱，
		// 而它们恰是本项目最常问的。两条路都跑，块向量在前，符号路兜底。
		if blockOut := a.recallByBlocks(query); blockOut != "" {
			return blockOut
		}

		// ★ fingerprint 必须传：这是 recallByBlocks 失败后的兜底路，
		// 不传就等于让旧向量空间的块混进结果
		// （判据 TestMemoryRecall_指纹不匹配的块被跳过）。
		result, err := g.RecallSorted(keywords, nil, int(depth), "", a.currentFingerprint(), sortMode)
		if err != nil {
			return fmt.Sprintf("记忆检索失败: %v", err)
		}
		if len(result.Entities) == 0 && len(result.Relations) == 0 {
			return "未找到相关记忆"
		}
		if a.indexer != nil {
			names := make([]string, len(result.Entities))
			for i, e := range result.Entities {
				names[i] = e.Name
			}
			a.indexer.MarkRecalled(names...)
		}
		var parts []string
		parts = append(parts, fmt.Sprintf("找到 %d 个相关实体（按%s排序）:", len(result.Entities), sortLabel(sortMode)))
		for _, e := range result.Entities {
			// ★ 带出匹配层级：此前输出是同格式平铺，模型无从判断该信哪条。
			//
			// 实测 v4：193 个实体平铺（13744 tokens）后，模型放弃向量检索、
			// 转去 grep 知识库文件，还把"没检索到"说成"库里不存在"。
			// 层级标记让最相关的几条一眼可辨。
			tag := ""
			switch {
			case e.MatchRank == 0:
				tag = ", 精确匹配"
			case e.MatchRank == 1:
				tag = ", 前缀匹配"
			}
			parts = append(parts, fmt.Sprintf("- %s (提及%d次, 类型:%s%s)", e.Name, e.MentionCount, e.Type, tag))
		}
		parts = append(parts, fmt.Sprintf("找到 %d 条关系:", len(result.Relations)))
		parts = append(parts, formatRecallRelations(result.Relations, 10)...)
		// 命中的关系若挂着媒体块，把媒体说明附在结果末尾。
		//
		// 关系行只有实体名和关系类型，看不出"这条记忆当时还带了一张图"。
		// 媒体块以结构边与句子相连，需经关系→句子反查。
		// 不附上的后果：agent 显式查了图记忆，却仍然不知道有图。
		if mc := a.mediaContextForRelations(result.Relations); mc != "" {
			parts = append(parts, "", "关联媒体:", mc)
		}
		return strings.Join(parts, "\n")

	case "memory_block_merge":
		if msg := requireFull(); msg != "" {
			return msg
		}
		entityA, _ := tc.Arguments["entity_a"].(string)
		entityB, _ := tc.Arguments["entity_b"].(string)
		rounds, _ := tc.Arguments["rounds"].(float64)
		if entityA == "" || entityB == "" || rounds <= 0 {
			return "entity_a、entity_b 和 rounds 不能为空"
		}
		if entityA > entityB {
			entityA, entityB = entityB, entityA
		}
		key := entityA + "||" + entityB
		a.noMergeMu.Lock()
		a.noMergeMarkers[key] = int(rounds)
		a.noMergeMu.Unlock()
		return fmt.Sprintf("已标记「%s」与「%s」在 %d 轮内不合并", entityA, entityB, int(rounds))

	case "memory_commit":
		triplesData, ok := tc.Arguments["triples"].([]interface{})
		if !ok {
			return "参数格式错误，需要 triples 数组"
		}
		// 场景键：模型可以在三元组里逐条给（scene 字段），也可以在工具参数
		// 顶层给一次（scene 参数），后者作为本批次的默认场景。
		// 两条路都为空则这条记忆不参与场景召回——不做猜测：猜错的场景会把
		// 无关记忆钉死，之后每次进入该场面都会被注入，比漏标更难发现。
		batchScene := getString(tc.Arguments, "scene")
		// 写侧的场景是**两条路都挂**：
		//   显式声明（模型在参数里点名）优先；
		//   否则挂本轮解析出的场景集合——主动声明的 + 被动涌现的。
		// 只挂一条会丢东西：只挂声明则细粒度唤起丢失，只挂涌现则首次交互
		// （场景还没长出来）没有兜底。
		var batchScenes []string
		if batchScene == "" {
			batchScenes = turnScenes
		}
		var triples []memory.Triple
		for _, td := range triplesData {
			if m, ok := td.(map[string]interface{}); ok {
				t := memory.Triple{
					Subject:      getString(m, "subject"),
					Relation:     getString(m, "relation"),
					Object:       getString(m, "object"),
					SentenceText: getString(m, "sentence_text"),
					Scene:        getString(m, "scene"),
				}
				if t.Scene == "" {
					t.Scene = batchScene
				}
				if len(t.Scenes) == 0 {
					t.Scenes = batchScenes
				}
				// 模型显式关联的媒体：结构化字段随三元组一起提交，
				// 由 commitTriplesWithMedia 变成 L3 一等块并与句子建边——
				// 不再把 marker 写进句子文本。值是块 ID（或其唯一前缀）。
				if refs := getStringSlice(m, "media_digests"); len(refs) > 0 {
					t.MediaDigests = a.resolveMediaRefs(refs)
					// 块边需要句子作端点。模型没给原句时用三元组本身拼一句
					// 自然语言——不能造一段 marker 文本，那正是被废弃的东西。
					if t.SentenceText == "" && len(t.MediaDigests) > 0 {
						t.SentenceText = fmt.Sprintf("%s%s%s。", t.Subject, t.Relation, t.Object)
					}
				}
				if t.Subject != "" && t.Relation != "" && t.Object != "" {
					triples = append(triples, t)
				}
			}
		}
		if len(triples) == 0 {
			return "没有有效的三元组"
		}
		// remember 工具是用户/模型显式写入，不涉及归档删除，
		// 因此不需要 mediaBound——没有旧引用要释放。
		// ★★ 写入前取块数基线（2026-10-04）
		//
		// 旧表双写已停 ⇒ Commit 的 ec/rc 恒为 0，
		// 拿它们判断「有没有写进去」会**永远判成没写进去**。
		blocksBefore, err := a.memory.MemoryBlockCount()
		if err != nil {
			return fmt.Sprintf("记忆写入失败: %v", err)
		}
		edgesBefore, err := a.memory.MemoryEdgeCount()
		if err != nil {
			return fmt.Sprintf("记忆写入失败: %v", err)
		}

		// ★ ec/rc 丢弃（2026-10-05）：它们恒为 0（graph.go 里
		// relationsCreated = 0 写死），拿它们当「写了几条」会**永远说 0**。
		// 保留位置只是因为 commitTriplesWithMedia 的签名未变。
		//   唯一可信信号是下面实测的 newBlocks / newEdges。
		_, _, mb, err := a.commitTriplesWithMedia(triples, string(a.id), 0, nil)
		if err != nil {
			return fmt.Sprintf("记忆写入失败: %v", err)
		}
		blocksAfter, err := a.memory.MemoryBlockCount()
		if err != nil {
			return fmt.Sprintf("记忆写入失败: %v", err)
		}
		edgesAfter, err := a.memory.MemoryEdgeCount()
		if err != nil {
			return fmt.Sprintf("记忆写入失败: %v", err)
		}
		newBlocks := blocksAfter - blocksBefore
		newEdges := edgesAfter - edgesBefore
		// ★ 0 写入必须显式报告：提交了 N 条但一条都没落库（如实体名校验被拒）
		// 却回「已写入 0 个」，模型会当成成功而永不重试 —— 实测（2026-10-01
		// 跑分）：metrics 端口/分机号更新全部因此静默丢失。
		// ★ 判据用**新增块数**，不是 ec/rc（2026-10-04）
		//
		// ★★ 这条提示曾经会让模型做错事：
		//
		//	旧表停写后 ec/rc 恒为 0 ⇒ 每次提交都被告知
		//	「全部被拒，请检查实体名写法」——
		//	而写入其实**成功了**。
		//
		//	模型据此去改不该改的东西（把正常实体名改短、
		//	加字母数字），把记忆内容改坏。
		//
		//	⇒ 「报假失败」比「报假成功」危险：前者会诱发破坏性动作。
		if newBlocks == 0 && mb == 0 {
			return fmt.Sprintf("提交了 %d 条三元组但全部被拒（未写入）。常见原因：实体名为空、过长（>50 字）、或不含字母/汉字/数字。请检查主语/宾语的写法后重试。", len(triples))
		}
		// ★★★ 成功分支也**不能用 ec/rc**（2026-10-05）
		//
		//	旧表停双写后 ec/rc 恒为 0，所以这里原本回
		//	「已写入 0 个实体和 0 条关系」——**而实际写进去了**。
		//
		//	实测（隔离实例）：一次对话写入 7 块 9 边，工具却回
		//	「已写入 0 个实体和 0 条关系」与「全部被拒（未写入）」。
		//	模型据此认为记忆系统坏了，回复里明说
		//	「the write is being rejected」并放弃重试。
		//
		//	★ 与上面那个「报假失败」是同一个病的两种表现：
		//	  判据用 newBlocks（对）但文案用 ec/rc（错），
		//	  于是同一份代码里两套口径打架。
		//
		//	唯一可信的信号是 newBlocks（块总量差值）——
		//	它直接量「库里多了几个块」，不依赖任何旧表口径。
		if mb > 0 {
			return fmt.Sprintf("已写入 %d 个记忆块、%d 条关系边，关联 %d 份媒体",
				newBlocks, newEdges, mb)
		}
		return fmt.Sprintf("已写入 %d 个记忆块、%d 条关系边", newBlocks, newEdges)

	case "memory_introspect":
		if msg := requireFull(); msg != "" {
			return msg
		}
		stats, err := a.memory.Introspect()
		if err != nil {
			return fmt.Sprintf("查询失败: %v", err)
		}
		return fmt.Sprintf("记忆统计: %v", stats)

	case "memory_document_query":
		return a.executeDocTool(tc)

	case "memory_merge":
		if msg := requireFull(); msg != "" {
			return msg
		}
		source, _ := tc.Arguments["source"].(string)
		target, _ := tc.Arguments["target"].(string)
		if source == "" || target == "" {
			return "source 和 target 不能为空"
		}
		count, err := a.memory.MergeEntities(source, target)
		if err != nil {
			return fmt.Sprintf("合并失败: %v", err)
		}
		return fmt.Sprintf("已将「%s」合并到「%s」，source 已彻底删除，%d 条关系已重定向", source, target, count)

	case "memory_delete_entity":
		if msg := requireFull(); msg != "" {
			return msg
		}
		name, _ := tc.Arguments["name"].(string)
		if name == "" {
			return "name 不能为空"
		}
		res, err := a.memory.DeleteEntity(name)
		if err != nil {
			return fmt.Sprintf("删除失败: %v", err)
		}
		// ★ 如实报告实际删掉多少，不说「已彻底删除…及其所有关联关系」。
		//
		//   旧文案是谎报：底层只碰旧表、活图谱 Δ0，却回「彻底删除」。
		//   模型据此认为内容已消失（不再提及或重新写入），
		//   而关联边还在、召回继续命中 —— 谎报会让模型的行为跟着错。
		if res.Blocks == 0 {
			return fmt.Sprintf("未找到名为「%s」的块，未删除任何内容", name)
		}
		return fmt.Sprintf("已删除块「%s」及其 %d 条关联关系（块 %d 个）",
			name, res.Edges, res.Blocks)

	case "memory_purge":
		if msg := requireFull(); msg != "" {
			return msg
		}
		criteria := make(map[string]string)
		if v, ok := tc.Arguments["subject_contains"].(string); ok && v != "" {
			criteria["subject_contains"] = v
		}
		if v, ok := tc.Arguments["relation_type"].(string); ok && v != "" {
			criteria["relation_type"] = v
		}
		if v, ok := tc.Arguments["target_contains"].(string); ok && v != "" {
			criteria["target_contains"] = v
		}
		mode, _ := tc.Arguments["mode"].(string)
		if mode == "" {
			mode = "soft"
		}
		n, err := a.memory.Purge(criteria, mode)
		if err != nil {
			return fmt.Sprintf("删除图记忆失败: %v", err)
		}

		textRemoved := 0
		if a.textMem != nil {
			if subj, ok := criteria["subject_contains"]; ok && subj != "" {
				textRemoved, _ = a.textMem.PurgeByFilter(func(evt text.Event) bool {
					return strings.Contains(evt.Source, subj) || strings.Contains(evt.Input, subj) || strings.Contains(evt.Response, subj)
				})
			}
		}
		parts := []string{fmt.Sprintf("已%s删除 %d 条图记忆关系", mode, n)}
		if textRemoved > 0 {
			parts = append(parts, fmt.Sprintf("清理 %d 条文本记忆日志", textRemoved))
		}
		return strings.Join(parts, "，")

	case "memory_edit":
		if msg := requireFull(); msg != "" {
			return msg
		}
		oldSubject, _ := tc.Arguments["old_subject"].(string)
		oldRelation, _ := tc.Arguments["old_relation"].(string)
		oldObject, _ := tc.Arguments["old_object"].(string)
		if oldSubject == "" || oldRelation == "" || oldObject == "" {
			return "old_subject、old_relation、old_object 不能为空"
		}
		newSubject, _ := tc.Arguments["new_subject"].(string)
		newRelation, _ := tc.Arguments["new_relation"].(string)
		newObject, _ := tc.Arguments["new_object"].(string)
		if newSubject == "" && newRelation == "" && newObject == "" {
			return "至少提供一个新值（new_subject / new_relation / new_object）"
		}
		if newSubject == "" {
			newSubject = oldSubject
		}
		if newRelation == "" {
			newRelation = oldRelation
		}
		if newObject == "" {
			newObject = oldObject
		}
		// 编辑前先精确取回旧关系：Purge 是「删旧写新」，中间那一步会把
		// 置信度、场景引用、原句一起丢掉。复审心跳（reviewLoop）正是走这条路，
		// 于是每次复审都把置信度重置成默认 1.0、把场景钉死的记忆打散成无场景，
		// 而且没有任何日志——这类「静默降级」比报错难查得多。
		var carriedConf float64
		var carriedSentence, carriedScene string
		if olds, ferr := a.memory.FindRelations(oldSubject, oldRelation, oldObject); ferr == nil && len(olds) > 0 {
			carriedConf = olds[0].Confidence
			carriedSentence = olds[0].SentenceText
			if keys, serr := a.memory.ScenesOfRelation(olds[0].ID); serr == nil && len(keys) > 0 {
				carriedScene = keys[0]
			}
		}

		n, err := a.memory.Purge(map[string]string{
			"subject_contains": oldSubject,
			"relation_type":    oldRelation,
			"target_contains":  oldObject,
		}, "hard")
		if err != nil {
			return fmt.Sprintf("编辑图记忆失败（删除旧记录）: %v", err)
		}
		triples := []memory.Triple{{
			Subject:      newSubject,
			Relation:     newRelation,
			Object:       newObject,
			Confidence:   carriedConf,
			SentenceText: carriedSentence,
			Scene:        carriedScene,
		}}
		ec, rc, err := a.memory.Commit(triples, string(a.id), 0)
		if err != nil {
			return fmt.Sprintf("编辑图记忆失败（写入新记录）: %v", err)
		}

		textReplaced := 0
		if a.textMem != nil && oldSubject != "" {
			textReplaced, _ = a.textMem.ReplaceByFilter(
				func(evt text.Event) bool {
					return strings.Contains(evt.Input, oldSubject) || strings.Contains(evt.Response, oldSubject)
				},
				func(evt text.Event) text.Event {
					evt.Input = strings.ReplaceAll(evt.Input, oldSubject, newSubject)
					evt.Response = strings.ReplaceAll(evt.Response, oldSubject, newSubject)
					return evt
				},
			)
		}
		result := fmt.Sprintf("已编辑记忆：删除 %d 条旧关系，写入 %d 个实体 + %d 条新关系", n, ec, rc)
		if textReplaced > 0 {
			result += fmt.Sprintf("，更新 %d 条文本记忆日志", textReplaced)
		}
		return result

	default:
		return fmt.Sprintf("未知的记忆工具: %s", tc.Name)
	}
}

func (a *Agent) executeSocialTool(tc agentAPI.ToolCall) string {
	if a.social == nil {
		return "人物关系网不可用（social store 未初始化）"
	}
	switch tc.Name {
	case "person_query":
		name, _ := tc.Arguments["name"].(string)
		if name == "" {
			return "请输入人物名称"
		}
		profile, err := a.social.GetPerson(name)
		if err != nil {
			return fmt.Sprintf("查询人物失败: %v", err)
		}
		var parts []string
		parts = append(parts, fmt.Sprintf("▎%s 的档案", name))
		if len(profile.Traits) > 0 {
			parts = append(parts, "【特质】")
			for k, v := range profile.Traits {
				parts = append(parts, fmt.Sprintf("  %s: %s", k, v))
			}
		}
		if len(profile.Relations) > 0 {
			parts = append(parts, "【社交关系】")
			for _, r := range profile.Relations {
				parts = append(parts, fmt.Sprintf("  %s —(%s)—→ %s", name, r.Relation, r.Person))
			}
		}
		if len(profile.Traits) == 0 && len(profile.Relations) == 0 {
			parts = append(parts, "  （尚无记录）")
		}
		return strings.Join(parts, "\n")

	case "person_set_trait":
		name, _ := tc.Arguments["name"].(string)
		trait, _ := tc.Arguments["trait"].(string)
		value, _ := tc.Arguments["value"].(string)
		if name == "" || trait == "" || value == "" {
			return "name、trait、value 都不能为空"
		}
		if err := a.social.SetTrait(name, trait, value); err != nil {
			return fmt.Sprintf("设置特质失败: %v", err)
		}
		return fmt.Sprintf("已记录：%s 的 %s = %s", name, trait, value)

	case "person_relate":
		personA, _ := tc.Arguments["person_a"].(string)
		relation, _ := tc.Arguments["relation"].(string)
		personB, _ := tc.Arguments["person_b"].(string)
		if personA == "" || relation == "" || personB == "" {
			return "person_a、relation、person_b 都不能为空"
		}
		if err := a.social.AddRelation(personA, relation, personB); err != nil {
			return fmt.Sprintf("建立关系失败: %v", err)
		}
		return fmt.Sprintf("已记录：%s —(%s)—→ %s", personA, relation, personB)

	case "person_network":
		name, _ := tc.Arguments["name"].(string)
		depth := int(getFloat(tc.Arguments, "depth"))
		if depth <= 0 {
			depth = 2
		}
		if name == "" {
			return "请输入人物名称"
		}
		profiles, err := a.social.GetNetwork(name, depth)
		if err != nil {
			return fmt.Sprintf("查询社交网络失败: %v", err)
		}
		if len(profiles) == 0 {
			return fmt.Sprintf("未找到 %s 的社交网络", name)
		}
		var parts []string
		parts = append(parts, fmt.Sprintf("▎%s 的社交网络（%d 度）", name, depth))
		for _, p := range profiles {
			if p.Name == name {
				continue
			}
			parts = append(parts, fmt.Sprintf("  · %s", p.Name))
			for k, v := range p.Traits {
				parts = append(parts, fmt.Sprintf("    %s: %s", k, v))
			}
			for _, r := range p.Relations {
				if r.Person != name {
					parts = append(parts, fmt.Sprintf("    —(%s)—→ %s", r.Relation, r.Person))
				}
			}
		}
		return strings.Join(parts, "\n")

	default:
		return fmt.Sprintf("未知的人物工具: %s", tc.Name)
	}
}

// knowledgeMediaRefs 把模型给的块引用（ID 或其唯一前缀）解析成知识条目的媒体引用。
//
// 复用 doc_commit 的既有约定：引用可传前缀，解析不了的跳过而不是报错——
// 模型偶尔会把 ID 记错，不该让整次写入失败。MIME 从块回读，嵌入时需要
// （EmbedImageDense 靠它判定模态）。字节随条目落盘（writeMediaFiles）。
func (a *Agent) knowledgeMediaRefs(refs []string) []knowledge.KnowledgeMediaRef {
	if a.memory == nil || len(refs) == 0 {
		return nil
	}
	var out []knowledge.KnowledgeMediaRef
	for _, r := range refs {
		b, ok, err := a.memory.BlockByIDPrefix(r)
		if err != nil || !ok {
			log.Printf("[knowledge] 媒体块引用 %s 无法唯一解析，跳过", r)
			continue
		}
		out = append(out, knowledge.KnowledgeMediaRef{
			Digest: b.ID,
			MIME:   b.MIME,
			Kind:   string(b.Modality),
			Data:   b.Data,
		})
	}
	return out
}

func (a *Agent) executeKnowledgeTool(tc agentAPI.ToolCall) string {
	if a.knowledge == nil {
		return "知识库不可用"
	}
	switch tc.Name {
	case "knowledge_search":
		query, _ := tc.Arguments["query"].(string)
		topK := int(getFloat(tc.Arguments, "top_k"))
		if topK <= 0 {
			topK = 5
		}
		if query == "" {
			return "请输入查询关键词"
		}
		// 可选分类限定：把召回限制在某棵分类子树内（前缀匹配，见
		// knowledge.Store.SearchIn）。不传 = 全库。
		category, _ := tc.Arguments["category"].(string)
		results := a.knowledge.SearchIn(query, category, topK)
		if len(results) == 0 {
			return "未找到相关知识"
		}
		var parts []string
		for i, k := range results {
			if i >= topK {
				break
			}
			// Name 已是含分类的规范名（"tech/go/并发"），分类前缀就在里面。
			// 曾经这里再拼一次 Category，输出成 "tech/go/tech/go/并发"（实测）。
			parts = append(parts, fmt.Sprintf("[%s]\n%s", k.Name, truncateStr(k.Content, 200)))
		}
		return strings.Join(parts, "\n---\n")

	case "knowledge_create":
		name, _ := tc.Arguments["name"].(string)
		content, _ := tc.Arguments["content"].(string)
		if name == "" || content == "" {
			return "name 和 content 不能为空"
		}
		// 模型可显式关联已入库的媒体（与 doc_commit 的 media_digests 同形）。
		// 这些媒体成为知识条目的一等节点：其向量会与正文向量融合，
		// 使该条目能按图本身被召回，而不依赖任何生成的描述文本。
		media := a.knowledgeMediaRefs(getStringSlice(tc.Arguments, "media_digests"))
		if err := a.knowledge.AddWithMedia(name, content, media); err != nil {
			return fmt.Sprintf("知识创建失败: %v", err)
		}
		if len(media) > 0 {
			return fmt.Sprintf("知识「%s」已创建并向量化索引（%d 字符，%d 个媒体参与跨模态召回）", name, len(content), len(media))
		}
		return fmt.Sprintf("知识「%s」已创建并向量化索引（%d 字符）", name, len(content))

	case "knowledge_list":
		tree := a.knowledge.BuildTree()
		return formatTree(tree, 0)

	case "knowledge_import_dir":
		dir, _ := tc.Arguments["dir"].(string)
		category, _ := tc.Arguments["category"].(string)
		// dry_run 默认 true：导入是批量写，agent 第一次试某个目录时
		// 应该先看清会写什么。默认直接写等于让它盲写一批数据。
		dryRun := true
		if b, ok := getBool(tc.Arguments, "dry_run"); ok {
			dryRun = b
		}
		includeMedia := false
		if b, ok := getBool(tc.Arguments, "include_media"); ok {
			includeMedia = b
		}
		st, err := a.knowledge.ImportDir(knowledge.ImportOptions{
			Dir:          dir,
			Category:     category,
			DryRun:       dryRun,
			IncludeMedia: includeMedia,
			MaxItems:     int(getFloat(tc.Arguments, "max_items")),
		})
		if err != nil {
			return fmt.Sprintf("知识导入失败: %v", err)
		}
		var b strings.Builder
		verb := "已导入"
		if dryRun {
			verb = "将导入（dry_run，未实际写入）"
		}
		fmt.Fprintf(&b, "%s %d 条", verb, st.Imported)
		if category != "" {
			fmt.Fprintf(&b, "（分类前缀 %s）", category)
		}
		if st.Media > 0 {
			fmt.Fprintf(&b, "，含 %d 个媒体", st.Media)
		}
		if st.Skipped > 0 {
			fmt.Fprintf(&b, "；跳过 %d", st.Skipped)
		}
		if st.Failed > 0 {
			fmt.Fprintf(&b, "；失败 %d", st.Failed)
		}
		if st.Truncated {
			fmt.Fprintf(&b, "；★ 超出 max_items 被截断，未导完（可调大 max_items 或分批）")
		}
		if len(st.Names) > 0 {
			show := st.Names
			if len(show) > 10 {
				show = show[:10]
			}
			fmt.Fprintf(&b, "。知识名：%s", strings.Join(show, "、"))
			if len(st.Names) > 10 {
				fmt.Fprintf(&b, " …共 %d 个", len(st.Names))
			}
		}
		// 失败原因要报给 agent：否则它只知道"失败 37 条"却无从下手。
		for _, e := range st.Errors {
			fmt.Fprintf(&b, "\n- %s", e)
		}
		return b.String()

	case "knowledge_delete":
		name, _ := tc.Arguments["name"].(string)
		if name == "" {
			return "name 不能为空"
		}
		if err := a.knowledge.Remove(name); err != nil {
			return fmt.Sprintf("知识删除失败: %v", err)
		}
		return fmt.Sprintf("知识「%s」已删除", name)

	default:
		return fmt.Sprintf("未知的知识工具: %s", tc.Name)
	}
}

func (a *Agent) executeDocTool(tc agentAPI.ToolCall) string {
	if a.docStore == nil {
		return "文档记忆不可用"
	}
	switch tc.Name {
	case "doc_query":
		query, _ := tc.Arguments["query"].(string)
		topK := int(getFloat(tc.Arguments, "top_k"))
		if topK <= 0 {
			topK = 3
		}
		if query == "" {
			return "请输入查询内容"
		}
		docs := a.docStore.Consume(query, topK)
		if len(docs) == 0 {
			return "未找到相关文档记忆"
		}
		var parts []string
		var refs []string
		for i, d := range docs {
			parts = append(parts, fmt.Sprintf("[%d] %s (来源: %s)", i+1, d.Summary, d.Source))
			if len(d.Tags) > 0 {
				parts = append(parts, "  标签: "+strings.Join(d.Tags, ", "))
			}
			content := d.Content
			if len(content) > 2000 {
				content = content[:2000] + "..."
			}
			// 媒体块标签单独一行进冷存事件：正文可能被上面的 2000 字截断，
			// 截掉之后模型就不知道这篇文档带过图。
			if labels := a.blockLabelsForDoc(d); labels != "" {
				content = content + "\n关联媒体: " + labels
			}
			a.context.InsertByTimestamp(ContextEvent{
				Timestamp: d.CreatedAt,
				Source:    "cold_storage",
				Input:     fmt.Sprintf("加载文档记忆: %s", query),
				Response:  content,
			})
			refs = append(refs, fmt.Sprintf("#%d(%s)", i+1, d.Summary))
		}
		return fmt.Sprintf("已加载 %d 篇文档记忆: %s\n(完整内容参见对话时序中 cold_storage 事件)",
			len(docs), strings.Join(refs, ", "))

	case "doc_commit":
		content, _ := tc.Arguments["content"].(string)
		summary, _ := tc.Arguments["summary"].(string)
		if content == "" {
			return "content 不能为空"
		}
		if summary == "" {
			summary = truncateStr(content, 100)
		}

		tagsRaw, _ := tc.Arguments["tags"].([]interface{})
		var tags []string
		for _, t := range tagsRaw {
			if s, ok := t.(string); ok {
				tags = append(tags, s)
			}
		}

		doc := &document.Doc{
			Summary: summary,
			Content: content,
			Tags:    tags,
			Source:  "manual",
		}

		// 模型显式关联的媒体：直接变成文档持有的一等块。
		// 不再往正文写 marker——文档向量会融合这些块的媒体向量，
		// 图片按自己的向量被检索。值是块 ID（或其唯一前缀）。
		for _, ref := range a.resolveMediaRefs(getStringSlice(tc.Arguments, "media_digests")) {
			if b, ok, err := a.memory.BlockByIDPrefix(ref); err == nil && ok {
				doc.Blocks = append(doc.Blocks, b)
			}
		}

		if err := a.docStore.Insert(doc); err != nil {
			return fmt.Sprintf("文档写入失败: %v", err)
		}
		if n := len(doc.Blocks); n > 0 {
			return fmt.Sprintf("文档已提交 (id: %s, 摘要: %s, 关联 %d 份媒体)", doc.ID, summary, n)
		}
		return fmt.Sprintf("文档已提交 (id: %s, 摘要: %s)", doc.ID, summary)

	default:
		return fmt.Sprintf("未知的文档工具: %s", tc.Name)
	}
}

// sortLabel 把排序模式翻成给模型看的中文标签。
func sortLabel(m memory.SortMode) string {
	if m == memory.SortRecent {
		return "时间倒序，最新在前"
	}
	return "相关性"
}

// recallByBlocks 用稠密向量召回块节点，返回格式化文本；不可用/无命中时返回空串。
//
// 返回空串让调用方无缝退回符号路 —— 这保证了「向量侧没配好」不会让
// memory_recall 整体失败（那会让模型完全失去记忆，比召回差得多）。
func (a *Agent) recallByBlocks(query string) string {
	if a == nil || a.memory == nil || a.multimodalSpace == nil {
		return ""
	}
	if !a.multimodalSpace.Loaded() {
		return ""
	}
	vec, err := a.multimodalSpace.VectorizeDense(query)
	if err != nil || len(vec) == 0 {
		// 向量化失败不报错到工具层：符号路仍可用，而这条失败通常意味着
		// 模型未加载/超时，报给模型只会让它以为"记忆不存在"。
		return ""
	}
	// ★ 走**融合**入口（向量 + 符号），而不是纯向量或仲裁-only
	//
	// 三层根因（生产快照 1391 块实测）：
	//
	//	1. 各向异性    已修（中心化，补上了生产调用者）
	//	2. 精确串被稀释  向量模型固有限制：「13010/13011 而非 12011」
	//	              含精确串却召不回，而「本地网关8081」召回了
	//	3. 符号路无补位  本函数修的就是这个
	//
	// 旧实现是「块向量在前，符号路兜底」—— 块一旦有命中就直接 return，
	// 符号路的 RecallSorted **根本没被调用**。设计注释写的
	//「端口号、分机号这类纯数字串向量天然弱」是对的，但没有真正生效。
	//
	// 而仲裁仍必须在 topK 截断**之前**（RecallBlocksFused 内部
	// 已按「放大 TopK → 符号融合 → 仲裁 → 截断」实现）：
	// 实测旧号 4379 以 0.8127 排 top1 而新号进不了 top8，
	// 事后仲裁无从挽回，因为被判取代的旧值和新值都不在候选里。
	//
	// ★ MinScore 传 0 而不是 blockRecallMinScore(0.5)：
	//   融合分数的**量纲变了** —— 精确串命中是 1.0（布尔置顶），
	//   而向量加权只有 0.7×余弦。用 0.5 筛会把「精确串命中 1.0」
	//   留下，却把所有纯向量候选（0.35~0.45）全筛掉 ——
	//   反而丢掉向量侧的有效召回。筛选改由融合内部按路处理。
	// ★ 走**带拒答的**入口 RecallBlocksGuarded。
	//
	// 拒答在图库层而不是这一层，是有教训的：第一版把 AbstainCheck
	// 写在这里（core 层），而探针在 memory 包内直连图库，
	// 于是**探针完全绕过了拒答** —— 端到端跑出 abstention 0/3，
	// 但生产路径其实是有拒答的，却没人能证明它。
	// 「判据测不到被测路径 ⇒ 判据等于不存在」。
	//
	// 拒答为什么必须在召回之前：编造的成因正是「查询符号在库里零出现」
	// ⇒ 向量却仍给 0.84+ 的高分（grafana/kafka/容灾演练 三类都是）。
	// 先召回再判断的话，看到的是一堆高分块 ——
	// 而「分数高」本身不能证明相关。
	hits, abstain, arb, err := a.memory.RecallBlocksGuarded(
		memory.BlockRecallQuery{
			Vector:      vec,
			Fingerprint: a.multimodalSpace.Fingerprint(),
			TopK:        blockRecallTopK,
			// ★ MinScore 保留：融合内部按路应用（只约束纯向量那一路，
			//   精确串/纯符号命中不受它约束）。曾误传 0 让噪声过滤失效，
			//   toolcall_block_test.go 的两条判据立刻红了。
			MinScore: blockRecallMinScore,
		}, query)
	if abstain != nil {
		log.Printf("[memory] abstain: 符号零命中，拒答「%s」", query)
		return abstain.Notice
	}
	if err != nil || len(hits) == 0 {
		return ""
	}
	if n := len(arb.Superseded); n > 0 {
		// 被取代的块不返回给模型，但**记一笔**：旧值被显式作废这件事
		// 本身有信息（「旧号 4379 停用」解释了为什么现在打不通），
		// 而完全静默会让「记忆里为什么没有旧号」变成无解之谜。
		log.Printf("[memory] recall blocks: %d 条被时序仲裁剔除（被更新的值取代）", n)
	}
	// 归因统计：融合到底救回了多少条 —— 这是判断它在起作用的关键读数
	var bySymbol, byExact int
	for _, h := range hits {
		if h.ExactHit {
			byExact++
		} else if h.VectorHit == 0 && h.SymbolHit > 0 {
			bySymbol++
		}
	}
	if byExact+bySymbol > 0 {
		log.Printf("[memory] recall blocks: 符号路补位 %d 条（精确串 %d，词法 %d）",
			byExact+bySymbol, byExact, bySymbol)
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("找到 %d 条相关记忆片段:", len(hits)))
	for _, h := range hits {
		text := strings.TrimSpace(h.Text)
		if text == "" {
			continue
		}
		// 分数照旧给模型看（它用这个判断相关性），
		// 但精确串命中统一显示 1.00 —— 它是布尔信号不是强度信号。
		score := h.Score
		if h.ExactHit {
			score = 1.0
		}
		tag := "text"
		if h.ExactHit {
			tag = "exact"
		} else if h.VectorHit == 0 && h.SymbolHit > 0 {
			tag = "symbol"
		}
		parts = append(parts, fmt.Sprintf("- [%s %.2f] %s",
			tag, score, truncateStr(text, 160)))
	}
	return strings.Join(parts, "\n")
}

const (
	// blockRecallTopK 是块召回的条数上限。与实体路的 20 条同量级：
	// 实测 193 条平铺会把模型淹没（13744 tokens / 预算 436%）。
	blockRecallTopK = 8
	// blockRecallMinScore 是入选下限。低于它的候选分数已无区分意义
	// （生成模型主干的余弦普遍偏高，实测无关句也能到 0.83）。
	blockRecallMinScore = 0.5
)

// currentFingerprint 返回**当前向量空间**的指纹，用于给兜底召回路径做隔离。
//
// ★ 为什么需要它
//
//	memory_recall 的主路是 recallByBlocks（带向量空间检查），
//	失败时退到 RecallSorted —— 而 RecallSorted 是纯词法的，不查 fingerprint
//	⇒ 换向量空间后旧块会从兜底路混回结果。
//	（判据 TestMemoryRecall_指纹不匹配的块被跳过 就是抓这个的）
//
// ★★ 为什么用「当前空间的指纹」而不是「库里多数取值」
//
//	我先写的版本是查库（DominantBlockFingerprint）—— **错的**：
//	库里只有旧空间的块时，多数取值就是旧指纹，
//	拿它当过滤条件等于「不��滤」，旧块原样通过。
//	（判据当场抓住：测试库里唯一的块是 old-fp，而当前空间是 fp1。）
//
//	隔离的语义是「只信任当前空间写入的块」，
//	判据必须来自**空间对象**而不是库 —— 库只能说明过去，不能说明现在。
func (a *Agent) currentFingerprint() string {
	if a == nil || a.multimodalSpace == nil {
		return ""
	}
	if !a.multimodalSpace.Loaded() {
		return ""
	}
	return a.multimodalSpace.Fingerprint()
}
