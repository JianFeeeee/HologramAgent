package core

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// childTaskState 是一个子任务的生命周期状态。
//
// delivered 代替了早期的“读到即删”：完成通知会写进持久上下文
// （formatMergedTimeline 每轮重新注入），模型之后还会再查。读一次就删的
// 话，第二次查询返回“不存在或已过期”——那是一个**永远不会成功的可操作
// 信号**，模型只能一遍遍地重试/汇报，循环永不结束。
type childTaskState struct {
	running   bool
	result    string
	delivered bool  // 结果是否已交付过（用于幂等应答）
	seq       int64 // 完成顺序，用于有界淘汰
}

// maxRetainedChildTasks 是保留的已完成子任务上限（防结果无限占用内存）。
const maxRetainedChildTasks = 20

// evictChildTasksLocked 淘汰最旧的已完成子任务。调用方必须持有 childMu。
func (a *Agent) evictChildTasksLocked() {
	for len(a.childTasks) > maxRetainedChildTasks {
		oldestID := ""
		var oldestSeq int64
		for id, st := range a.childTasks {
			if st.running {
				continue
			}
			if oldestID == "" || st.seq < oldestSeq {
				oldestID, oldestSeq = id, st.seq
			}
		}
		if oldestID == "" {
			return // 剩下全是运行中的，不淘汰
		}
		delete(a.childTasks, oldestID)
	}
}

func (a *Agent) executeSpawnChild(tc agentAPI.ToolCall, parentChannel string) string {
	task, _ := tc.Arguments["task"].(string)
	if task == "" {
		if b, _ := json.Marshal(tc.Arguments); len(b) > 2 {
			log.Printf("[spawn] task empty but arguments present: %s", truncateStr(string(b), 300))
		}
		return "请提供 task 参数"
	}
	maxTurns := 0
	if v, ok := tc.Arguments["max_turns"].(float64); ok {
		maxTurns = int(v)
	}
	if maxTurns < 1 {
		maxTurns = defaultChildMaxTurns
	}
	if maxTurns > 30 {
		maxTurns = 30
	}

	a.childMu.Lock()
	a.childNextID++
	taskID := fmt.Sprintf("child_%d", a.childNextID)
	a.childMu.Unlock()

	// parentChannel 由调用方（任务帧）传入：子任务完成通知要回到**发起这次
	// spawn 的那个任务**的通道，而不是"内核当前通道"（那个概念已删除）。
	if parentChannel == "" || parentChannel == channelConsolidation {
		parentChannel = "cli"
	}

	a.childMu.Lock()
	a.childTasks[taskID] = &childTaskState{running: true}
	a.childMu.Unlock()
	go a.runChildTask(taskID, task, parentChannel, maxTurns)

	return fmt.Sprintf("子任务已启动（ID: %s，最多 %d 轮）。完成后会自动通知你，届时用 child_result 查看输出即可（**只需查询一次**）", taskID, maxTurns)
}

// defaultChildMaxTurns 子 Agent 默认工具轮数（可被 spawn_child 的 max_turns 参数覆盖）。
const defaultChildMaxTurns = 5

// childSystemPromptTemplate 是非驻留子 Agent 的系统提示词（%s = 任务描述）。
//
// ★ 为什么必须显式写「执行纪律」与「工具执行顺序」两节：
//
// 此前这两节缺失，子只能拿到「你是子任务助手，请完成任务」这一句。实测（2026-10-08
// 生产日志）子把「宣告计划」当成了任务完成 —— 交回父的结果大量是将来时：
//
//	child_1 done: I'll start by searching for each school's admissions catalog information.
//	child_3 done: 我将逐校用官方渠道核实。先并行检索四校的招生专业目录信息。
//	child_5 done: I'll start by reading the existing artifacts.
//
// 根因是退出判据只看「模型有没有返回 tool_calls」（spawn.go 的 `len(resp.ToolCalls) == 0`）：
// 模型一输出「I'll start by...」就再无 tool_calls，循环立即判定完成。**该判据未改动**
// （它是对的：模型确实不再要求工具了），所以要靠提示词让模型**不要**在拿到结果前
// 停止调用工具。
//
// 父的提示词里早有同类先例（buildSystemPrompt 的【记忆清理指令】：「必须实际调用
// memory_ 工具执行操作，不能只回复文本」「不要只描述计划而不执行」），子这里缺的正是它。
//
// 第二缺失是【工具执行顺序】：同轮多个 tool_call **默认并行**，而子不知道
// 「依赖前一步结果的调用必须分轮」。多步复杂任务（搜索 → 按结果抓页 → 解析）
// 因此被塞进同一轮，并行跑出无意义的组合。
//
// 不把父的【输出规则】/【中断消息】/【记忆清理】/【可用技能】/人格那几节一并搬过来：
// 子被显式禁用了 output_send__*（结果回父而非直接回用户），也无需保留记忆或响应中断；
// 搬来只会白占提示词预算。
const childSystemPromptTemplate = `你是 HologramAgent 的子任务助手。
请完成以下任务。完成即可，无需保留记忆或查询历史。

【执行纪律】这是硬性要求，不是建议：
- **必须实际调用工具去执行**，不要只回复「我将要做什么」。宣告计划不等于完成任务；
  只要你还没把结果拿到手，就必须继续调用工具。
- 只有当你**已经取得实际结果**（或确认无法取得）时，才用纯文本汇报结论；
  汇报内容必须是**已发生的事实**，而不是打算做的事。
- 任务确实无法完成时，如实说明卡在哪一步、已取得了什么，不要谎报完成。

【工具执行顺序】同一条回复里的多个工具调用**默认并行执行**，不是依次执行：
- 若某个调用的参数**需要另一个调用的结果**，必须**分两轮** —— 先发前一个，
  看到结果后再发下一个。写在同一轮就等于假设了不存在的先后。
- 互相独立的调用可以放在同一轮并行发出（这是被鼓励的，更快）。

任务: %s`

func (a *Agent) runChildTask(taskID, task string, parentChannel string, maxTurns int) {
	if a.provider == nil {
		log.Printf("[child] %s failed: no LLM provider configured", taskID)
		return
	}
	log.Printf("[child] %s started: %s", taskID, truncateStr(task, 80))

	sysPrompt := fmt.Sprintf(childSystemPromptTemplate, task)

	msgs := []agentAPI.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: task},
	}

	allTools := a.buildToolDefs()
	childTools := make([]interface{}, 0, len(allTools))
	for _, t := range allTools {
		toolMap, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := toolMap["function"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if strings.HasPrefix(name, "output_send__") || name == "output_list_channels" || name == "spawn_child" || name == "plgreload" {
			continue
		}
		childTools = append(childTools, t)
	}

	// 循环因何结束，供收尾文案如实描述（不要把「跑满预算」说成别的数字）。
	//
	// 为什么需要它：此前收尾文案硬编码「超过 5 轮」，而 max_turns 是**模型可传**
	// 的参数（1-30），于是 max_turns=10 的子 Agent 明明跑满 10 轮，报告却说
	// 「超过 5 轮」——日志在说谎，排查时会被这个假数字带偏。
	//
	// completed 与会空内容分开记：模型返回了空文本也是一次「回答过了」，
	// 不能与「循环中途 break」归为同一类（那会把可解释的空回答报成故障）。
	var finalResult string
	completed := false
	reachedLimit := false
	for turn := 0; turn < maxTurns; turn++ {
		req := &agentAPI.CompletionRequest{
			Messages:        msgs,
			MaxTokens:       4096,
			Tools:           childTools,
			ToolChoice:      "auto",
			DisableThinking: !a.thinkingEnabled,
		}

		resp, err := a.provider.Chat(a.ctx, req)
		if err != nil {
			finalResult = fmt.Sprintf("子 Agent 执行失败: %v", err)
			break
		}

		if len(resp.ToolCalls) == 0 {
			finalResult = resp.Content
			completed = true
			break
		}

		if turn == maxTurns-1 {
			// 本轮是最后一轮：它仍返回了工具调用，说明预算在此耗尽。
			reachedLimit = true
		}

		for _, ct := range resp.ToolCalls {
			var result string
			switch {
			case strings.HasPrefix(ct.Name, "output_send__") || ct.Name == "output_list_channels":
				result = fmt.Sprintf("子 Agent 不允许调用输出工具: %s", ct.Name)
			case ct.Name == "spawn_child" || ct.Name == "plgreload":
				result = fmt.Sprintf("子 Agent 不允许调用系统工具: %s", ct.Name)
			default:
				result = a.executeToolCall(ct, parentChannel)
			}
			// 子 Agent 的工具调用必须自记一行。此前子侧全程静默：父内核日志里
			// 只有「started / done」两行，子任务失败时无法判断倒在哪一步（是搜不到、
			// 解析失败，还是被轮上限截断）——只能看到一句「超时」。
			log.Printf("[child] %s turn %d/%d: %s → %d 字节", taskID, turn+1, maxTurns, ct.Name, len(result))
			msgs = append(msgs, agentAPI.Message{Role: "assistant", Content: resp.Content, ToolCalls: []agentAPI.ToolCall{ct}})
			msgs = append(msgs, agentAPI.Message{Role: "tool", ToolCallID: ct.ID, Content: result})
		}
	}

	if finalResult == "" {
		switch {
		case completed:
			// 模型确实回答完了，但内容是空的。如实说，不当作故障。
			finalResult = "子 Agent 已结束（模型未给出文本内容）"
		case reachedLimit:
			// 跑满预算 ≠ 白干。子 Agent 在前几轮已经拿到了一批工具结果
			// （实测 child_5 已跑完一串页面解析），直接丢弃它们会让父 Agent
			// 只能重 spawn（日志里 child_5→6→7 就是同一任务连试三次，每次都
			// 重烧一整个上下文）。所以先做一次「不带工具的收尾调用」，把已有
			// 成果落成文本。仅在失败路径上多花一次 LLM 调用。
			log.Printf("[child] %s 跑满预算 maxTurns=%d，尝试收尾提炼已有成果", taskID, maxTurns)
			if summary := a.salvageChildSummary(msgs, maxTurns); summary != "" {
				finalResult = summary
			} else {
				finalResult = fmt.Sprintf("子 Agent 已达工具轮上限（%d 轮）仍未产出最终答复，任务未完成", maxTurns)
			}
		default:
			// 既没拿到答复、也不是跑满预算：只可能是 Chat 出错后 break。
			finalResult = "子 Agent 未产出结果（执行中断）"
		}
	}

	a.childMu.Lock()
	if st := a.childTasks[taskID]; st != nil {
		st.running = false
		st.result = finalResult
		a.childSeq++
		st.seq = a.childSeq
	}
	a.evictChildTasksLocked()
	a.childMu.Unlock()

	log.Printf("[child] %s done: %s", taskID, truncateStr(finalResult, 100))

	notification := fmt.Sprintf("子任务 %s 已完成。请用 child_result 工具查看输出（只需查询一次；重复查询不会返回失败）。", taskID)
	a.injectSelfChannel(selfInputMsg{
		text:    notification,
		channel: parentChannel, // 回到父对话通道，正常处理（写入上下文 + emit 响应）
	})
}

// executeChildResultTool 取回子任务结果。
//
// **幂等**：结果不会被“读到即删”，重复查询返回同一结果或一条明确提示。
// 这一点至关重要——完成通知会长期留在持久上下文里（formatMergedTimeline
// 每轮重新注入），如果重复查询返回“不存在”这种失败信号，模型会认定任务
// 未完成而无限重试（实测单轮 35 次工具调用、持续 514 秒）。
func (a *Agent) executeChildResultTool(tc agentAPI.ToolCall) string {
	taskID, _ := tc.Arguments["task_id"].(string)
	if taskID == "" {
		return "请提供 task_id 参数"
	}

	a.childMu.Lock()
	st, ok := a.childTasks[taskID]
	if !ok {
		a.childMu.Unlock()
		return fmt.Sprintf("子任务 %s 不存在：从未创建该 ID（请核对 spawn_child 返回的 ID 拼写）", taskID)
	}
	if st.running {
		a.childMu.Unlock()
		return fmt.Sprintf("子任务 %s 仍在运行中，尚未完成。请等待完成通知后再查询。", taskID)
	}
	first := !st.delivered
	st.delivered = true
	result := st.result
	a.childMu.Unlock()

	if first {
		return fmt.Sprintf("【子任务 %s 结果】\n%s", taskID, result)
	}
	// 重复查询不是失败：明确告诉模型“任务已完成、结果已给过”，让它停止重试。
	return fmt.Sprintf("【子任务 %s 已完成】结果已在上文提供（见先前的 child_result 工具结果），无需重复查询；请直接基于上文结果继续。", taskID)
}

// childSalvagePrompt 是跑满预算后的收尾指令。
//
// 强调「不得再请求调用工具」而不是仅靠不带 Tools 参数：provider 形态各异，
// 有的对“无工具但正文写了工具调用”仍会编造出 tool_calls，而此处已经不再执行
// 任何工具，会直接落成空结果。
const childSalvagePrompt = `你已用完工具调用轮次预算，不能再调用任何工具。
请只根据上面已经取得的工具结果，直接给出**目前已知的结论与已收集到的数据**；
如有未完成的部分，明确列出“未查到/未完成”的项。不要请求调用工具，不要说“我将要去查”。`

// salvageChildSummary 在子 Agent 跑满轮预算后，用一次**不带工具**的调用把
// 已取得的中间成果落成文本。
//
// 为什么值得多花一次调用：此前的行为是直接丢弃全部中间结果、只回一句“超时”，
// 父 Agent 拿到的是一个不可操作的信号，只能重 spawn——那会重烧一整个上下文，
// 比这多出来的**一次**调用贵得多。
//
// 失败不抛错（外层只在失败路径上调用它）：返回空串即回退到诚实的事实陈述。
func (a *Agent) salvageChildSummary(msgs []agentAPI.Message, maxTurns int) string {
	if a.provider == nil {
		return ""
	}
	salvageMsgs := make([]agentAPI.Message, 0, len(msgs)+1)
	salvageMsgs = append(salvageMsgs, msgs...)
	salvageMsgs = append(salvageMsgs, agentAPI.Message{Role: "user", Content: childSalvagePrompt})

	resp, err := a.provider.Chat(a.ctx, &agentAPI.CompletionRequest{
		Messages:        salvageMsgs,
		MaxTokens:       4096,
		ToolChoice:      "none", // 不给工具，强制走文本产出
		DisableThinking: !a.thinkingEnabled,
	})
	if err != nil {
		log.Printf("[child] 收尾提炼失败（回退为事实陈述）: %v", err)
		return ""
	}
	content := strings.TrimSpace(resp.Content)
	if content == "" {
		return ""
	}
	return fmt.Sprintf("（注：已达 %d 轮工具上限，以下为已取得成果的收尾提炼，可能不完整）\n%s", maxTurns, content)
}

func (a *Agent) executeLLMTool(tc agentAPI.ToolCall) string {
	if a.providerManager == nil {
		return "LLM 源管理器不可用"
	}
	switch tc.Name {
	case "llm_list_sources":
		sources := a.providerManager.List()
		if len(sources) == 0 {
			return "没有可用的 LLM 源"
		}
		parts := []string{"可用 LLM 源:"}
		for _, name := range sources {
			mark := " "
			if p := a.providerManager.Get(""); p != nil && p.Name() == name {
				mark = "→"
			}
			parts = append(parts, fmt.Sprintf("  %s %s", mark, name))
		}
		return strings.Join(parts, "\n")

	case "llm_set_source":
		name, _ := tc.Arguments["name"].(string)
		if name == "" {
			return "请提供源名称"
		}
		if err := a.providerManager.SetDefault(name); err != nil {
			return fmt.Sprintf("切换失败: %v", err)
		}
		a.provider = a.providerManager.Get(name)
		return fmt.Sprintf("已切换到 LLM 源: %s", name)

	default:
		return fmt.Sprintf("未知的 LLM 工具: %s", tc.Name)
	}
}
