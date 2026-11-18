package agent

// 任务状态机（M1：行为等价的纯重构）。
//
// 背景与设计见 docs/zh/input-scheduler-design.md。
//
// M1 只做一件事：把原先「一个 425 行的 process() 大函数」拆成
// **显式 step 游标 + TaskFrame**。目的不是加能力，而是让「现场」变成数据——
// 之后 M3 才能把帧存进 suspendStack 并在安全点恢复。
//
// 行为等价的判据：既有全部 agent 测试通过，且 R3/X3（见设计文档 §11）通过。
//
// 本文件**不引入**优先级、抢占、队列与并发；那些在 M2 起逐层加上。
//
// Step 与安全点（设计文档 §4.2）：
//   step 与 step 之间是安全点；StepToolExec（工具执行）与 StepPrepare 中的
//   ONNX/落盘片段是**临界区**，执行中不可抢占。
//
// 与设计文档的差异：文档里的 StepBeforeOutput / StepAfterOutput / StepCommit /
// StepFinish 属于 emitResponse 与 processInput 层，M1 不动它们（M6 再迁入帧）。

import (
	"context"
	"errors"
	"fmt"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// Step 是任务状态机的游标（声明点在 internal/agent/agenttypes）。
type Step = agenttypes.Step

const (
	// StepPrepare 构建消息与工具表、应用中断标记、跑 pre_action 阶段。
	// ★ Step 的**声明点**在 internal/agent/agenttypes（拆包后各类型模块
	//   都要命名它）；这里做别名，调用点逐字不变。
	StepPrepare   = agenttypes.StepPrepare
	StepLLM       = agenttypes.StepLLM
	StepToolBatch = agenttypes.StepToolBatch
	StepToolBegin = agenttypes.StepToolBegin
	StepToolExec  = agenttypes.StepToolExec
	StepToolAfter = agenttypes.StepToolAfter
	StepTurnEnd   = agenttypes.StepTurnEnd
)

// stepOutcome 是一次 step 执行的结果。
type stepOutcome = agenttypes.Outcome

const (
	// 终态的**声明点**在 agenttypes（同上）。
	outcomeContinue  = agenttypes.OutcomeContinue
	outcomeDone      = agenttypes.OutcomeDone
	outcomeFailed    = agenttypes.OutcomeFailed
	outcomeSuspended = agenttypes.OutcomeSuspended
)

// taskTerminal 是任务的终态种类（设计文档 §7：每个任务恰有一个终态）。
type taskTerminal = agenttypes.Terminal

const (
	// 终态的声明点在 agenttypes（拆包后各类型模块要用）。
	terminalNone              = agenttypes.TerminalNone
	terminalOK                = agenttypes.TerminalOK
	terminalError             = agenttypes.TerminalError
	terminalStageShortCircuit = agenttypes.TerminalStageShortCircuit
	terminalSkipped           = agenttypes.TerminalSkipped
	terminalConsolidation     = agenttypes.TerminalConsolidation
	terminalSuspended         = agenttypes.TerminalSuspended
)

// TaskFrame 承载一个任务在安全点之间必须存活的所有状态。
//
// 不变量（设计文档 §8.1 I3）：帧是**纯数据**；不得持有任何锁或资源跨越安全点。
// TaskFrame 是 agenttypes.Frame 的别名（声明点在值类型层，拆包后各类型模块要用）。
type TaskFrame = agenttypes.Frame

// outputChannelOf 从**输入事件**推导本次输出应走的通道。
//
// 内核不持有"当前通道"可变状态：那类字段会被后来的任务（中断任务）覆盖，
// 使被打断任务恢复后的提示词/事件标签串台。通道只跟着事件与帧走。
func outputChannelOf(evt *agentIO.InputEvent) string {
	if evt == nil {
		return ""
	}
	if evt.OutputChannel != "" {
		return evt.OutputChannel
	}
	return evt.Source
}

// isCriticalChannel 报告某个通道是否是**整任务不可抢占**的临界区。
//
// 目前只有 `_consolidation_`（记忆整理直接改图库）。工具执行/ONNX/CAS 属于
// **单步**临界区，由"只在 step 之间检查让位"天然保护，不在这里列。
func isCriticalChannel(channel string) bool {
	return channel == channelConsolidation
}

func (a *Agent) newTaskFrame(input string, stageCtx *sdk.StageContext) *TaskFrame {
	return &TaskFrame{Input: input, StageCtx: stageCtx, Step: StepPrepare}
}

// runTaskSteps 把一帧交给**类型注册的 loop**（基座不假设任何 loop 样式）。
//
// ## 用户裁定（2026-11-17）
//
// > 「基座标准骨架？基座不假设任何 agentloop 样式」
//
// 所以基座这里**没有任何**内置状态机：它只做一次转发。loop 由类型模块在
// 装配点注册（`Hooks.Loop`），未注册即装配错误（`Hooks.Validate` 拦住）。
//
// 下面这段 `LoopStandard` 是**标准实现**（供主 agent / 驻留子注册）：
// 它现在与基座同包只是因为拆包（→ `internal/agent/standard`）还没做完；
// 但它**不会被基座自动启用** —— 基座不认识它，也不调用它。
func (a *Agent) RunTaskSteps(f *TaskFrame) stepOutcome {
	if a == nil || a.hooks.Loop == nil {
		// 装配错误：Validate 应该已经在构造期拦住。这里返回失败而不是
		// 退回某个"默认骨架" —— 基座不假设任何 loop 样式。
		f.Err = fmt.Errorf("agent %q: no agent loop registered (Hooks.Loop is nil) — "+
			"the assembly point must register this type's loop (kit.Loop for main/Resident, async for rt)", a.id)
		return outcomeFailed
	}
	return a.hooks.Loop(a, f)
}

// LoopStandard 是**标准同步 loop**（主 agent / 驻留子的实现）。
//
// 它在装配点显式注册：
//
//	base.SetHooks(agent.Hooks{Loop: agent.LoopStandard, …})
//
// ★ 为什么不作为基座默认：那会让"这个 agent 用哪种 loop"变成隐式事实
//
//	（rt 的异步 loop 就只能靠覆盖默认值来表达），而用户要求基座**不假设**
//	任何样式。注册是显式的、可审计的。
func LoopStandard(a *Agent, f *TaskFrame) stepOutcome {
	// 步数上限只是防"转移缺失导致死循环"的护栏；正常任务远达不到。
	const maxSteps = 1 << 20
	for i := 0; i < maxSteps; i++ {
		// 安全点：只在 step 之间检查让位。临界区（StepToolExec）不在此列，
		// 因为让位信号由 interruptLoop 置位、而本循环是唯一读帧者。
		//
		// 先「重新求值」再判让位：临界区（或抢占冷却期）内被拦成入队的中断，
		// 必须在这里重新武装——否则它只能等当前任务自然结束（设计 §4.3/§5.2）。
		a.sched.rearmPending()
		if !isCriticalChannel(f.OutputChannel) && a.sched.preemptGrantedFor() && a.sched.canSuspend() {
			return outcomeSuspended
		}
		// 工具轮次硬上限（设计文档 D6）：在发起下一轮 LLM 前收尾。
		// f.Turn 只在 stepTurnEnd 递增，所以它等于「已完成的工具批数」；
		// 因此这里允许 maxToolTurns 批，而不会多跑第 maxToolTurns+1 轮。
		if f.Step == StepLLM && a.maxToolTurns > 0 && f.Turn >= a.maxToolTurns {
			a.Logf("reached max tool turns %d (turn=%d), forcing wrap-up", a.maxToolTurns, f.Turn)
			if f.Resp != nil && strings.TrimSpace(f.Resp.Content) != "" {
				f.Response = f.Resp.Content
			} else {
				f.Response = fmt.Sprintf("[系统] 已达到最大工具轮次 %d，任务中止。", a.maxToolTurns)
			}
			return outcomeDone
		}
		switch a.step(f) {
		case outcomeDone:
			return outcomeDone
		case outcomeFailed:
			return outcomeFailed
		case outcomeSuspended:
			return outcomeSuspended
		}
	}
	f.Err = fmt.Errorf("agent: task step budget exhausted（状态机未收敛，疑似转移缺失）")
	return outcomeFailed
}

// process 是保留给 processConsolidation 与测试的薄壳，返回与原实现相同的四元组。
//
// 注意：M3a 起**不再持 a.mu**——调度器是唯一执行者，而挂起不能持锁。
func (a *Agent) process(input string, stageCtx *sdk.StageContext) (response string, toolsUsed []string, toolResults []ToolResultItem, err error) {
	if a.provider == nil {
		return "", nil, nil, fmt.Errorf("agent: no LLM provider configured")
	}
	f := a.newTaskFrame(input, stageCtx)
	switch a.RunTaskSteps(f) {
	case outcomeDone:
		return f.Response, f.ToolsUsed, f.ToolResults, nil
	case outcomeFailed:
		return "", f.ToolsUsed, f.ToolResults, f.Err
	default:
		// 不该发生：process() 不参与挂起（只有 runInputTask 会）。
		return "", f.ToolsUsed, f.ToolResults,
			fmt.Errorf("agent: task suspended outside scheduler")
	}
}

// runInputTask 是一个输入任务的完整生命周期：prepare → run → finish。
//
// 它是原 processInput 的全部职责，被拆成三段而不是一个大函数，目的只有一个：
// 让帧可以跨安全点被挂起——挂起后由调度器保存，恢复时接着 run 段继续，
// 而 finish 段（上下文提交与回执）只在任务真正结束时执行一次。
//
// M3a 还没有抢占，因此 outcomeSuspended 只会由 M3b 的抢占检查产生。
func (a *Agent) RunInputTask(evt *agentIO.InputEvent) (*TaskFrame, stepOutcome) {
	// 临界区标记由调度器 goroutine 维护，任务结束（含挂起）即清。
	// interceptLoop 读它来决定“能不能取消”，因此必须是原子的。
	defer a.sched.setCritical(false)

	// ★ 安全轮次边界（2026-10-31 收敛）。
	//
	// 一条输入 = 一个安全轮次。这里把**原始事实**交给 security，
	// 由它自己解析档位、自己判定主体类别；本 agent 不参与任何策略判断。
	//
	// ★ defer End() 而非在末尾调：一条输入可能经过多个 return 出口
	// （跳过 / 短路 / 挂起 / 正常），逐个加必然漏一个 —— 与
	// `executeToolCallOutcome` 的入向脱敏用 defer 同一理由。
	round := a.BeginSecurityRound(evt.Payload, evt.Source)
	// ★ nil 安全（未装配闸门的单测/独立工具）：接口是**非 nil 接口值**才可调，
	// 所以必须显式判 nil —— typed-nil 陷阱的同类（见 Hooks 的注释）。
	if round != nil {
		defer round.End()
	}

	// ★ 内容审查（敏感词 / 注入迹象）接线（2026-11-02，用户裁定 B 档）。
	//
	// 必须在 beginSecurityRound **之后**：敏感词来自插件声明的档位
	// （SecurityProfile.SensitiveWords），而档位是 BeginRound 按 payload
	// 里的 security_profile 激活的。先扫后激活 = 扫的是空的词表。
	//
	// 命中不阻断（B 档），只在正文前追加警示语。详见 ReviewInputContent。
	if txt, _ := evt.Payload["content"].(string); txt != "" {
		ReviewInputContent(round, evt.Payload, evt.Source, txt)
	}

	f, term := a.prepareInputTask(evt)
	switch term {
	case terminalSkipped, terminalStageShortCircuit, terminalConsolidation:
		return nil, outcomeDone
	}

	out := a.RunTaskSteps(f)
	if out == outcomeSuspended {
		f.Terminal = terminalSuspended
		return f, outcomeSuspended
	}
	a.FinishInputTask(f, out)
	return f, out
}

// rebaseFramePrefix 把被挂起任务的上下文现场「加载回中断任务之上」。
//
// 语义（用户明确）：
//   - 中断打断时，被挂起任务自到达以来累积的全部现场（含 toolcall）被保护；
//   - 中断在上一个任务之前的**完整状态**上开始运行（所以中断看不到本任务的部分进展）；
//   - 中断结束后，把被挂起任务与其现场加载回中断任务**之上**再继续——
//     即中断已提交的那段上下文留在下面（前缀），本任务自己的现场落回其上。
//
// 实现：重建基础前缀（system + timeline + 用户输入）；由于中断结束时已把它的
// 输入/输出提交进 a.context，重建出的 timeline 已含中断的效果；再把本任务
// 自己的尾部（Stage 上下文 + 工具轮产物 + 占位）原样接回。
func (a *Agent) RebaseFramePrefix(f *TaskFrame) {
	if f == nil || f.PrefixLen <= 0 || f.PrefixLen > len(f.Msgs) {
		return
	}
	tail := append([]agentAPI.Message(nil), f.Msgs[f.PrefixLen:]...)

	budget := a.ComputeTokenBudget()
	memContext := a.BuildTaskMemoryContext(f, f.Input, budget.MemoryTokens)
	sysPrompt := a.BuildSystemPrompt(memContext, f.Input)
	prefix := a.BuildMessages(sysPrompt, f.Input, a.ContextTokenBudget(budget))

	// 重建会丢掉 prepare 段对尾部消息的两处改写，这里等价地补回。
	if f.IsInterrupt && len(prefix) > 0 {
		last := prefix[len(prefix)-1]
		last.Role = "system"
		last.Content = "[Interrupt] " + last.Content
		prefix[len(prefix)-1] = last
	}
	if len(f.InputBlocks) > 0 && len(prefix) > 0 {
		prefix[len(prefix)-1].Blocks = f.InputBlocks
	}

	f.Msgs = append(prefix, tail...)
	f.PrefixLen = len(prefix)

	// ★ 恢复也是**新的上下文装配点**：中断任务跑完已把它的输入/输出提交进
	// a.context（见上面注释），于是重建出的前缀可能比被挂起时更大。
	// 不在这里裁，这份"更大的前缀"会被原样发出去，下一轮再靠 stepLLM 的
	// 闸门补救——而那时已经付过一次超限请求的代价。
	//
	// 用的是**恢复专用**入口：相关性按本帧最新上下文算、保护范围是本帧自己的
	// 现场（f.Msgs[PrefixLen:]），而不是全局末尾 N 条。
	//
	// ★ Query 由基座**推导**（"本帧最新一条有内容的消息"是帧的事实，不是策略），
	//   但"恢复时怎么处置"由类型注册的 handler 决定。
	if h := a.hooks.OverflowHandler; h != nil {
		if handled, _ := h(a, f, agenttypes.OverflowRequest{
			Kind: agenttypes.OverflowResume, Query: resumeQueryFrom(f),
		}); handled && f.Terminal == agenttypes.TerminalError {
			return // 预算耗尽：f.Err 已写明，调用方据此终止
		}
	}
}

// resumeQueryFrom 取任务帧的**最新有效上下文**作为相关性查询。
//
// 从尾部往回找第一条有实质内容的消息：恢复时 f.Msgs 的最后一条
// 可能是 tool 结果、continuation 占位或空的 assistant 消息，
// 直接拿它算相关性会退化成无意义查询。
//
// ★ 它住基座而不是类型：这是**帧的事实推导**（只读 f.Msgs），与
//
//	"用哪种 context 实现"无关，也不摸任何实现句柄。
func resumeQueryFrom(f *TaskFrame) string {
	if f == nil {
		return ""
	}
	for i := len(f.Msgs) - 1; i >= 0; i-- {
		if c := strings.TrimSpace(f.Msgs[i].Content); c != "" {
			return c
		}
	}
	return f.Input
}

// prepareInputTask 执行 processInput 的前半段（去重、通道解析、阶段、裁剪、
// 输入事件落上下文）。返回终态不为 terminalNone 时调用方不得进入 run 段。
func (a *Agent) prepareInputTask(evt *agentIO.InputEvent) (*TaskFrame, taskTerminal) {
	start := time.Now()

	in, ok := a.ResolveInput(evt)
	if !ok {
		// 空输入（文本与媒体都空）：没有可处理内容，但同步调用方仍在等回执。
		a.EmitSkippedReply(evt, "empty_input")
		return nil, terminalSkipped
	}

	// 去重按文本做：webui/GUI 断线重连会重放未确认消息。
	// 带媒体时跳过——媒体输入的 alt 文案（"[从 qq 收到了 image]"）对不同图片
	// 是同一句，拿它去重会把连发的两张图误判成重复。
	if len(in.blocks) == 0 && a.isDuplicateInput(evt.Source, in.text) {
		log.Printf("[agent] dropped duplicate input from %s: %s", evt.Source, truncateStr(in.text, 60))
		// 去重是「不处理」而不是「不回」，否则同步调用方（cli/clawhub 无超时）
		// 会永久挂起（设计文档 §7 不变量 I5、§11.3 X2/X4）。
		a.EmitSkippedReply(evt, "duplicate")
		return nil, terminalSkipped
	}

	// 通道只从**输入事件**推导，内核不持有"当前通道"可变状态
	// （见 outputChannelOf；这消除了中断任务覆盖它导致被打断任务串台的整类问题）。
	// 进入本任务的临界区属性（记忆整理整任务不可抢占）。
	// 必须在 processConsolidation 之前设置——它就在下面同步执行。
	a.sched.setCritical(isCriticalChannel(outputChannelOf(evt)))

	if in.selfKind != "" {
		// ★ 标签**优先于通道**判定：带标签的自输入不是"整理任务"，
		//   也不是普通对话 —— 它走类型注册的 handler。
		//
		// ★ 自循环输入的标签由**类型**解释：基座只把它交给该类型注册的
		//   OverflowHandler（未注册 ⇒ 什么都不做，那是明确的策略）。
		//
		// 此前这里写着"按最后一条用户输入裁剪"—— 那是主 agent 的策略，
		// 而基座不认识主 agent。现在：标签 → handler → 类型自己决定裁不裁、
		// 用哪个 query 裁。
		if h := a.hooks.OverflowHandler; h != nil {
			// Query 留空：相关性基准是**策略的知识**（主 agent 取"最后一条
			// 用户输入"，别的类型可能不同），基座不替它推导。
			h(a, nil, agenttypes.OverflowRequest{Kind: agenttypes.OverflowAir})
		}
		return nil, terminalSkipped
	}

	if evt.OutputChannel == channelConsolidation {
		a.processConsolidation(evt, in.text)
		return nil, terminalConsolidation
	}

	// pendingMedia 让 describe_image / transcribe_audio / ocr_image 拿到本轮媒体的
	// 原始 data/url，也是这三个工具是否出现在工具表里的开关。仅对用户直接上传成立
	//（payload 里才有 data/url）；插件注入的是成品 block，取不到原始数据。
	if evt.Type == "image" || evt.Type == "audio" {
		a.MainState().pendingMedia = evt.Payload
	}

	// 媒体先变成一等块（字节进块的 Data）。不存的后果是 ContextEvent.Input
	// 只剩一句 alt 文本，base64 随 message 数组发给模型后就丢了。
	if len(in.blocks) > 0 {
		a.stageMediaBlocks(a.CaptureBlockMedia(in.blocks, in.captureTool)...)
	}

	noMemory := false
	if v, ok := evt.Payload["no_memory"].(bool); ok {
		noMemory = v
	}
	if !noMemory && a.io != nil {
		if chDef, ok := a.io.GetInputChannelDef(evt.Source); ok && chDef.NoMemory {
			noMemory = true
		}
	}

	// ★★ 空中断（kernel/overflow）：不做任何对话处理，只做语义相关性裁剪。
	//
	// 这是 contextoverflow 的**原始设计**（2026-10-25 按用户口径修正）：
	// L4 中断本身不携带消息，唯一用途是把当前任务抢占下来，好在任务边界
	// 做一次语义相关性裁剪。此前实现把它反过来了（先裁剪、再发一条
	// 「已裁剪 N 条…」的通知），那等于把调度信号变成了回执。
	//
	// 因此这里：裁剪 → 结束本轮，**不进 LLM、不写上下文、不发回复**。
	// 用 terminalSkipped（不是 terminalError）：这是正常完成，不是故障。
	// 工具提醒/中断（terminal_watch、timer 等）不是用户发言：
	// 以 system 角色注入 LLM，且不写入用户对话履历。
	isInterrupt, _ := evt.Payload["interrupt"].(bool)
	a.interruptInput = isInterrupt
	if isInterrupt {
		noMemory = true
	}

	stageCtx := a.stageCtxFromInput(in.text, evt.Source, "")
	stageCtx.Extra["input_source"] = evt.Source
	stageCtx.Extra["output_channel"] = evt.OutputChannel
	if len(in.blocks) > 0 {
		stageCtx.Extra["media_blocks"] = in.blocks
		stageCtx.Extra["media_type"] = in.mediaType
	}
	if noMemory {
		stageCtx.NoMemory = true
	}
	a.InjectSourceContext(stageCtx, evt)

	if a.RunStage(sdk.StageOnInput, stageCtx) {
		// 插件在 OnInput 短路：本轮一个 LLM 都没跑，用量为零是如实的。
		a.EmitResponse(evt, *stageCtx.Response, agentAPI.TokenUsage{})
		return nil, terminalStageShortCircuit
	}

	input := stageCtx.RawMessage

	// 计算层用的清洗文本（不改原文）：通道 Cleaner 提取语义内容后用于向量化/提关键词
	cleanInput := input
	if a.io != nil {
		if chDef, ok := a.io.GetInputChannelDef(evt.Source); ok && chDef.Cleaner != nil {
			cleanInput = chDef.Cleaner(input)
		}
	}

	// upload_* 字段一并转发：webui 的 EventRawInput 订阅方靠它们还原附件卡片。
	rawPayload := map[string]interface{}{"content": input, "source": evt.Source}
	for _, k := range []string{"upload_url", "upload_type", "upload_size", "upload_name"} {
		if v, ok := evt.Payload[k]; ok {
			rawPayload[k] = v
		}
	}
	a.PublishEvent(events.EventRawInput, rawPayload)

	// 裁剪与审计统一在 memoryPass 内（日志已带 trigger）。
	a.PruneOnInput(evt, cleanInput)

	// 本轮 inputch（处理表按它记账）+ contextfull 检测（只有驻留子设了钩子）。
	if r := a.Resident(); r != nil {
		r.tableMu.Lock()
		r.currentInputch = outputChannelOf(evt)
		r.tableMu.Unlock()
	}

	if !isInterrupt {
		a.context.Append(ContextEvent{
			Timestamp: start,
			Source:    evt.Source,
			Input:     input,
		})
	}

	f := a.newTaskFrame(input, stageCtx)
	f.Evt = evt
	f.CleanInput = cleanInput
	f.IsInterrupt = isInterrupt
	f.StartedAt = start
	// 通道记进帧：恢复时用它把 agent 级字段改回来（见 TaskFrame.OutputChannel）。
	f.OutputChannel = outputChannelOf(evt)
	return f, terminalNone
}

// finishInputTask 执行 processInput 的后半段（日志、上下文提交、回执、记忆候选）。
//
// 只在任务真正结束时调用一次——这正是不变量 I5（每任务恰一次终态）的落点。
func (a *Agent) FinishInputTask(f *TaskFrame, out stepOutcome) {
	evt := f.Evt

	// pendingMedia 是「本轮」语义：任务结束即清（挂起时保留，见 runInputTask）。
	if evt != nil && (evt.Type == "image" || evt.Type == "audio") {
		a.MainState().pendingMedia = nil
	}

	if out == outcomeFailed {
		log.Printf("[agent] process %s error: %v", evt.Type, f.Err)
		resp := fmt.Sprintf("处理错误: %v", f.Err)
		a.EmitResponse(evt, resp, f.TurnUsage)
		a.context.Append(ContextEvent{Timestamp: time.Now(), Source: "agent", Input: f.Input, Response: resp})
		f.Terminal = terminalError
		return
	}

	// inputch 处理表：本轮**未主动写入**时由系统自动写（保证每轮必有记录）。
	// 只有驻留子会用到（根 agent 的 children 为 0 时这只是几个空操作）。
	a.AutoRecordInputch(f)

	elapsed := time.Since(f.StartedAt)
	log.Printf("[agent] %s from %s → response (%dms, tools=%v)",
		evt.Type, evt.Source, elapsed.Milliseconds(), f.ToolsUsed)

	// 本轮捕获的媒体一起挂到这条事件上：用户上传的、插件注入的，以及模型调
	// multimodal_see_picture / see_video 时经 SetToolBlocks 注入的。
	// ★ 把插件声明落到事件上（2026-10-27）：`NoMemory` 只影响**计算层**
	//（不向量化 / 不提关键词 / 不蒸馏）与**记忆视图**，而**归档照常** ——
	// 这是 ContextEvent.NoMemory 的既有契约（见 context.go 的字段注释：
	// coldcontext_query 的 Consume 已从 L2 删掉原文，回填事件必须能落回 L2，
	// 否则三层都不在，内容被永久丢弃）。
	//
	// 此前这个位从来没被赋值：插件声明 InjectOptions.NoMemory=true（QQ 那句
	// "来自「X」的私聊消息(message_id=…) 先用 qq_get_message 取正文…"），
	// 经 payload.no_memory 合进 stageCtx.NoMemory 后，却只有
	// emitMemoryCandidate 一处消费它 —— 于是这句样板每轮都进计算层。
	//
	// 声明来源：payload["no_memory"]（插件注入）或 ChannelDef.NoMemory（通道）。
	turnEvt := ContextEvent{
		Timestamp: time.Now(),
		Source:    "agent",
		NoMemory:  f.StageCtx.NoMemory,
		Input:     f.CleanInput,
		Response:  f.Response,
		// ★ 思维链必须进 L1：下一轮 renderL1Messages 要把它原样回传给上游。
		// DeepSeek thinking 模式在**带 tools 的请求**里强制要求历史 reasoning_content，
		// 而内核每轮都带工具定义 ⇒ 不存就永久 400。详见字段注释。
		ReasoningContent: f.TurnReasoning,
		ToolsUsed:        f.ToolsUsed,
		ToolResults:      f.ToolResults,
		ToolCalls:        f.ToolCalls,
	}
	a.BindEventMedia(&turnEvt, a.drainMediaBlocks())
	// ★ 明文拦截发生在 a.context.Append **内部**（见 context.go 的 Append），
	// 不是在每个调用点 —— 写 context 的地方有 5 处，逐个补必漏。
	a.context.Append(turnEvt)

	a.EmitResponse(evt, f.Response, f.TurnUsage)

	if !f.StageCtx.NoMemory {
		a.emitMemoryCandidate(evt.Source, f.CleanInput, f.Response, f.TurnReasoning, f.ToolResults, f.ToolsUsed)
	}
	f.Terminal = terminalOK
}

// step 执行恰好一个 step。
func (a *Agent) step(f *TaskFrame) stepOutcome {
	switch f.Step {
	case StepPrepare:
		return a.stepPrepare(f)
	case StepLLM:
		return a.stepLLM(f)
	case StepToolBatch:
		return a.stepToolBatch(f)
	case StepToolBegin:
		return a.stepToolBegin(f)
	case StepToolExec:
		return a.stepToolExec(f)
	case StepToolAfter:
		return a.stepToolAfter(f)
	case StepTurnEnd:
		return a.stepTurnEnd(f)
	default:
		f.Err = fmt.Errorf("agent: unknown task step %d", f.Step)
		return outcomeFailed
	}
}

// stepPrepare 构建本轮任务的初始帧。
func (a *Agent) stepPrepare(f *TaskFrame) stepOutcome {
	budget := a.ComputeTokenBudget()

	memContext := a.BuildTaskMemoryContext(f, f.Input, budget.MemoryTokens)
	sysPrompt := a.BuildSystemPrompt(memContext, f.Input)
	f.Tools = a.BuildToolDefs()

	f.Msgs = a.BuildMessages(sysPrompt, f.Input, a.ContextTokenBudget(budget))
	// ★ 插件声明的**本轮提示**（SDK InjectOptions.TurnHint）。
	//
	// 以**独立 system 消息**注入，绝不拼进用户消息正文：
	//   · 用户说的话与"给模型的操作说明"必须可区分（也让审计能分辨来源）
	//   · 拼进正文时模型常把说明当成人话的一部分，回复里会复述"我先调 xxx"
	if hint := stageHint(f.StageCtx, "turn_hint"); hint != "" {
		f.Msgs = append(f.Msgs, agentAPI.Message{
			Role:    "system",
			Content: "[Turn hints] " + hint,
		})
	}
	// 工具提醒（interrupt）：以 system 角色注入，不让模型误认为用户发言
	if a.interruptInput {
		last := f.Msgs[len(f.Msgs)-1]
		last.Role = "system"
		last.Content = "[Interrupt] " + last.Content
		f.Msgs[len(f.Msgs)-1] = last
		a.interruptInput = false
	}
	if blocks, ok := f.StageCtx.Extra["media_blocks"].([]agentAPI.ContentBlock); ok && len(blocks) > 0 {
		if len(f.Msgs) > 0 {
			f.Msgs[len(f.Msgs)-1].Blocks = blocks
		}
		f.InputBlocks = blocks
	}
	// 基础前缀到此为止（system + timeline + 用户输入）；其后的 Stage 上下文
	// 与工具轮产物都属于“本任务自己的现场”，恢复时要接回重建后的前缀之上。
	f.PrefixLen = len(f.Msgs)

	a.Logf("tool call loop start, max_ctx=%d target=%d fixed=%d mem=%d ctx=%d %d tools, %d events, personality=%t, docs=%d",
		budget.MaxContext, budget.TargetUsage, budget.FixedTokens, budget.MemoryTokens, budget.ContextTokens,
		len(f.Tools), a.context.Len(),
		a.MainState().personality != nil && a.MainState().personality.Content != "",
		a.coldcontextStoreSize())

	if a.RunStage(sdk.StagePreAction, f.StageCtx) {
		f.Response = *f.StageCtx.Response
		return outcomeDone
	}
	// 停止的第二个半边（用户明确的设计）：对**停止那一刻已排队**的消息，
	// 依次在这里短路——不进 LLM、不执行工具，直接以一条说明收尾。
	// 配额是停止时的快照，消费完即止；停止之后新到的输入不受影响。
	if a.sched.consumeCancel() {
		log.Printf("[agent] cancelled queued task at pre-action (input=%q)", truncateStr(f.Input, 40))
		f.Response = "已取消（用户停止）。"
		return outcomeDone
	}
	if len(f.StageCtx.ContextMsgs) > 0 {
		for _, m := range f.StageCtx.ContextMsgs {
			role, _ := m["role"].(string)
			content, _ := m["content"].(string)
			if role != "" {
				f.Msgs = append(f.Msgs, agentAPI.Message{Role: role, Content: content})
			}
		}
	}

	// 上下文占满检测：**上报**给上级（驻留子），不在此处裁剪。
	// 只有驻留子设了 onContextFull ⇒ 对根 agent 是 no-op（根自己走
	// enforceContextBudget 就地裁，没有上级可报）。
	a.CheckContextFull()

	f.Step = StepLLM
	return outcomeContinue
}

// stepLLM 是轮次顶部与 LLM 调用。
//
// 取消（context.Canceled 且 agent 未退出）时**留在本 step 并 Turn++**——等价于
// 原实现的 `continue`：重新排空中断、补占位、重新请求。抢占挂起将在 M3 从这里接管。
func (a *Agent) stepLLM(f *TaskFrame) stepOutcome {
	// ★ 类型自己的**策略巡检**：每次发 LLM 请求**之前**问一次。
	//
	//   · 这是"我要在付代价之前处置我的上下文"——只有类型知道该在哪些时刻问
	//     （拼消息后、工具回灌后、恢复重建后），所以它留在这里；
	//   · 内核那两次触发（把请求交给 provider 之前 / 收到 ErrContextFull 之后）
	//     是另一条来源，内核调的是同一个 handler。
	//
	// 未注册 handler ⇒ 什么都不做（那是明确的策略：整窗类型靠上报父）。
	if h := a.hooks.OverflowHandler; h != nil {
		if handled, _ := h(a, f, agenttypes.OverflowRequest{Kind: agenttypes.OverflowLocal}); handled {
			if f.Terminal == terminalError {
				return outcomeFailed // 恢复预算耗尽，f.Err 已写明
			}
			// 已处置：f.Msgs 必须在 stepPrepare 重建，这里那份就是超限那份。
			f.Step = StepPrepare
			return outcomeContinue
		}
	}

	// zen 兼容网关要求请求的最后一条消息必须是 user(thinking 续写模式校验),
	// 工具轮产出的 tool/assistant 消息作结尾会被 400 拒绝,故补一条 user 占位。
	f.Msgs = dropContinuationPlaceholders(f.Msgs)
	if last := f.Msgs[len(f.Msgs)-1]; last.Role == "assistant" || last.Role == "tool" {
		f.Msgs = append(f.Msgs, agentAPI.Message{
			Role:    "user",
			Content: continuationFor(f.LastBatchReplyOnly),
		})
	}

	// ★ 发送前自检：assistant(tool_calls) 必须紧跟它全部的 tool 结果。
	//
	// 为什么必须在**这里**（唯一一次组装请求的位置，task.go:750）：
	// 上游对这个约束是硬拒绝（HTTP 400），一句错误信息带不出任何定位信息，
	// 而 f.Msgs 是**多条路径**拼出来的（prepare 重建、rebase 中断恢复、
	// 批末 flush、占位补全），任何一条漏掉配对都表现为同一个 400。
	//
	// 实测（2026-11-04）：历史 context.json 已经干净（0 个 result 多于 call
	// 的帧），但 A2A 新会话仍报 400 ⇒ 问题不在 L1 历史渲染，在 f.Msgs。
	//
	// 这里做的是**止损 + 定位**：把没有配对结果的 tool_calls 剔掉（连同
	// 该 assistant 消息里其余仍有效的 call 一起重排），并记录被剔的是谁。
	// 不静默——日志里能看出是哪一轮、哪个工具。
	f.Msgs = kit.DropUnpairedToolCalls(f.Msgs)

	req := &agentAPI.CompletionRequest{
		Messages:        f.Msgs,
		MaxTokens:       4096,
		Tools:           f.Tools,
		ToolChoice:      "auto",
		DisableThinking: !a.MainState().thinkingEnabled,
	}

	resp, llmErr := a.completeLLM(a.ctx, req, f.OutputChannel)

	if llmErr != nil {
		// ★ 上下文超页（上游 ErrContextFull）必须在 provider fallback **之前**处理：
		//   fallback 会换 provider 重发**同一个**超限请求，换谁都一样超。
		//
		// 这里**不另立路径**：上游报错只是「判定已被短路」的一种（force=true），
		// 之后的恢复预算 / 裁剪 / raise L4 与本地预判完全相同，共用同一个函数。
		// 区别仅在于：本地预判在发请求**之前**判，这里在**收到错误之后**判。
		if errors.Is(kernelapi.ClassifyLLMError(llmErr), kernelapi.ErrLLMContextFull) {
			var handled bool
			if h := a.hooks.OverflowHandler; h != nil {
				handled, _ = h(a, f, agenttypes.OverflowRequest{Kind: agenttypes.OverflowLocal, Force: true})
			}
			if handled {
				if f.Terminal == terminalError {
					return outcomeFailed // 恢复预算耗尽，f.Err 已写明
				}
				// ★ 已发出 L4 空中断，**裁剪尚未发生**（它在空中断任务被调度
				// 执行时才做）。所以这里不能让本帧原地重跑：
				//
				//   · 原地重跑 = 拿同一份超限内容再发一次，必然再次超限
				//     （这正是 recover budget 会被快速烧光的原因）；
				//   · 真正的裁剪发生在任务边界，也就是**本任务让位之后**。
				//
				// 语义上本帧的职责到此为止：把现场交出去（outcomeSuspended），
				// 由空中断任务抢占执行、完成裁剪，随后本帧再被恢复。
				//
				// 与"用户按停止"不同：那不是让位，是终结。两者不能混。
				return outcomeSuspended
			}
			// 未处理（如轻量内核不裁剪）：落到下面按普通错误处理。
		}
		if errors.Is(llmErr, context.Canceled) && a.ctx.Err() == nil {
			if f.OutputChannel == channelConsolidation {
				f.Err = fmt.Errorf("interrupted by user input")
				return outcomeFailed
			}
			// 用户按下的「停止」：取消必须**终结本任务**，不是重跑。
			//
			// 默认行为（下面的 outcomeContinue 重跑本步）是给「被更高中断抢占」用的：
			// 取消只是把现场交出去，稍后还要继续。而停止是用户明确的“不要了”，
			// 重跑会让停止按钮看起来没反应（已实测：取消后又发了一次完整请求）。
			if a.sched.takeStop() {
				log.Printf("[agent] stop: LLM cancelled, task terminated without retry")
				if strings.TrimSpace(f.Response) == "" {
					f.Response = "已停止生成。"
				}
				return outcomeDone
			}
			f.Turn++
			return outcomeContinue // 重跑 StepLLM
		}
		f.Err = llmErr
		return outcomeFailed
	}

	f.StageCtx.LLMText = resp.Content
	f.StageCtx.ReasoningContent = resp.ReasoningContent
	// 用量同时给插件看（StageCtx）与记账（kit.Ledger）。
	//
	// StageCtx.TokenUsage 是 map[string]int，放不了布尔，
	// 所以「上游是否报了缓存」只在事件里给（见下面的 cache_reported）。
	f.StageCtx.TokenUsage = map[string]int{
		"prompt_tokens":     resp.TokenUsage.Prompt,
		"completion_tokens": resp.TokenUsage.Completion,
		"total_tokens":      resp.TokenUsage.Total,
		"cache_read_tokens": resp.TokenUsage.CacheRead,
		"cache_miss_tokens": resp.TokenUsage.CacheMiss,
		"reasoning_tokens":  resp.TokenUsage.ReasoningTokens,
	}
	f.StageCtx.ToolCalls = convertToolCalls(resp.ToolCalls)
	for i := range f.StageCtx.ToolCalls {
		if f.StageCtx.ToolCalls[i].Plugin == "" {
			f.StageCtx.ToolCalls[i].Plugin = a.ResolveToolPlugin(f.StageCtx.ToolCalls[i].Name)
		}
	}
	if a.RunStage(sdk.StagePostAction, f.StageCtx) {
		f.Response = *f.StageCtx.Response
		return outcomeDone
	}
	resp.Content = f.StageCtx.LLMText
	resp.ToolCalls = convertBackToolCalls(f.StageCtx.ToolCalls)

	chainPayload := map[string]interface{}{
		"content":    resp.Content,
		"reasoning":  resp.ReasoningContent,
		"tool_calls": resp.ToolCalls,
		"phase":      "intermediate",
		"turn":       f.Turn,
	}
	// 记账：先并入累计，再把「本次」与「会话累计」一起发出去。
	//
	// 为何把累计也带上：要回答的是「这个会话花了多少、缓存省了多少」，
	// 只有单次数字就得消费方自己一条条加。放在同一事件里，消费方无需另建状态。
	a.usageLedger.Record(resp.TokenUsage)
	// 本轮任务内的合计（一个 TaskFrame 可能多轮 LLM：工具回环）。
	// 它与 kit.Ledger 的口径不同（本次请求 vs 会话累计），两者都要留。
	f.TurnUsage.Add(resp.TokenUsage)

	if resp.TokenUsage.Total > 0 {
		// ⚠️ 必须是 map[string]interface{}：WebUI 的 handler_openai.go
		// 用 `Payload["usage"].(map[string]interface{})` 取它，而 Go 的
		// 类型断言对 map 是**精确匹配** —— 发 map[string]int 时断言为 false
		// （已实证），于是在 /v1/chat/completions 回包里 usage 静默变 nil。
		// 这是“发了但没人收到”的典型形态：两边都不报错。
		chainPayload["usage"] = map[string]interface{}{
			"prompt":           resp.TokenUsage.Prompt,
			"completion":       resp.TokenUsage.Completion,
			"total":            resp.TokenUsage.Total,
			"cache_read":       resp.TokenUsage.CacheRead,
			"cache_miss":       resp.TokenUsage.CacheMiss,
			"cache_reported":   resp.TokenUsage.CacheReported,
			"reasoning_tokens": resp.TokenUsage.ReasoningTokens,
		}
	}
	// 会话累计。命中率**不可算时不带该字段** ——
	// 让消费方显示「—」而不是把「不知道」画成 0% 命中率。
	if sess := a.usageLedger.Snapshot(); sess.Calls > 0 {
		sessPayload := map[string]interface{}{
			"prompt":               sess.Prompt,
			"completion":           sess.Completion,
			"total":                sess.Total,
			"cache_read":           sess.CacheRead,
			"cache_miss":           sess.CacheMiss,
			"reasoning":            sess.Reasoning,
			"calls":                sess.Calls,
			"cache_reported_calls": sess.CacheReportedCalls,
		}
		if rate, ok := sess.CacheHitRate(); ok {
			sessPayload["cache_hit_rate"] = rate
		}
		chainPayload["usage_session"] = sessPayload
	}
	a.PublishEvent(events.EventAgentLLMChain, chainPayload)

	if resp.ReasoningContent != "" {
		a.PublishEvent(events.EventReasoning, map[string]interface{}{
			"content": resp.ReasoningContent,
			"channel": f.OutputChannel,
		})
	}

	// ★ 必须放在分支**之前**：下一行的 len(ToolCalls)==0 会直接 return，
	// 那条路径也要记下 reasoning（否则纯文本收尾轮的思维链丢失，而它同样
	// 属于“带 tools 的请求历史”，DeepSeek 照样要它）。
	f.TurnReasoning = resp.ReasoningContent

	if len(resp.ToolCalls) == 0 {
		f.Response = resp.Content
		return outcomeDone
	}

	// 本批是否全部是输出通道发送（=模型刚交付了给用户的回复）。
	// 必须在执行前判定：执行过程中的中断/拒绝分支会 continue/break，放在循环里统计会漏。
	f.Resp = resp
	f.ReplyOnly = true
	for _, tc := range resp.ToolCalls {
		if !isOutputDeliveryTool(tc.Name) {
			f.ReplyOnly = false
			break
		}
	}
	f.ContentOnce = true
	f.PendingTools = resp.ToolCalls
	// 阶段 2c：为批内每个工具预建独立的 StageContext（Extra 逐份复制）。
	buildToolContexts(f)
	// 阶段 2a：批内消息**预置**为「一个 assistant 带全部 tool_calls」。
	//
	// 为何预置而不是逐步 append：并行执行下多个工具的**完成顺序不确定**，
	// 若等结果回来再落消息，assistant 就必须等所有结果齐了才能写；
	// 而 OpenAI 协议要求 assistant(tool_calls) 在**结果之前**。
	// 预置同时让 assistant 只出现一次（逐步 append 会产生 N 条）。
	//
	// ⚠️ 前提：before_toolcall 阶段不得改写工具参数（已核实全仓无此用法）。
	// 若某插件将来要改写 args，需在这里改为「回填后重写该条 assistant」。
	f.AssistantMsgIdx = -1
	f.ToolIdx = 0
	// 阶段 2d：全批可并发（且无需保序）⇒ 走并发 step。
	if batchRunnable(f, a) {
		f.Step = StepToolBatch
	} else {
		f.Step = StepToolBegin
	}
	return outcomeContinue
}

// stepToolBatch **并发**执行整批工具（阶段 2d）。
//
// 结构上是「fan-out → join → 顺序落消息」三段：
//
//  1. fan-out：每个工具一个 goroutine，各自跑 before_toolcall + 执行。
//     每个 goroutine 只写**自己那份** toolCtxs[i]（阶段 2c 的拆分正为此），
//     共享的 f.Msgs / f.ToolResults 在此期间**一律不碰**。
//  2. join：等全部完成。
//  3. 落消息：按 **索引顺序**（不是完成顺序）逐个跑 after_toolcall 与落消息。
//     这一步必须串行——f.Msgs 是共享切片；而且按索引落能让模型读到的
//     上下文顺序与模型自己发出的顺序一致。
//
// 落消息为何不放进 goroutine：那样完成顺序不确定 ⇒ 同一批 tool 消息
// 顺序随机 ⇒ 模型读到的因果关系与实际执行不符。
func (a *Agent) stepToolBatch(f *TaskFrame) stepOutcome {
	n := len(f.PendingTools)
	results := make([]batchItemResult, n)

	// 保证 assistant 消息**先于**任何 tool 消息存在（协议要求）。
	ensureBatchAssistant(f)

	// ⚠️ 场景必须**在 fan-out 之前**解析一次：resolveTurnScenes 会把结果
	// 记进共享的 f.SceneDone / f.TurnScene（memorypass.go:289），
	// 在 N 个 goroutine 里各调一次既是数据竞争，也会各自触发一次
	// EnterSceneWithHint（重复计入场景强度——正是 task.go 里
	// sceneDone 注释警告的「多解析一次就多给场景加一次强度」）。
	for i := 0; i < n; i++ {
		a.resolveTurnScenes(f, f.PendingTools[i].Name)
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = a.runOneTool(f, idx)
		}(i)
	}
	wg.Wait()

	// 顺序收尾：after_toolcall / 裁剪 / 落消息 / 事件。
	for i := 0; i < n; i++ {
		r := results[i]
		f.CurTool = f.PendingTools[i]
		f.CurToolPlugin = r.plugin
		f.CurResult = r.text
		f.CurRaw = r.raw
		f.ToolIdx = i
		// ★ 必须先记录**调用**，再记结果（2026-11-04 生产故障修复）。
		//
		// 此前的缺陷：并发路径只 append ToolResults，**从不调
		// appendToolCallRecord** ⇒ 上下文里 ToolResults 多于 ToolCalls。
		//
		// 后果链（实测）：
		//   渲染时每个 tool result 都会产生一条 role="tool" 消息，
		//   而与其配对的 assistant(tool_calls) 里没有这个 call_id
		//   ⇒ 上游 400：An assistant message with 'tool_calls' must be
		//     followed by tool messages responding to each 'tool_call_id'
		//   ⇒ 所有 provider 全部失败，A2A 会话彻底不可用。
		//
		// 同时漏掉的还有 ToolsUsed（它和 ToolCalls 在 appendToolCallRecord
		// 里同处维护）：并发批次的工具从未被记进"用过的工具"，
		// 记忆/统计/页化都在少算。
		//
		// 为什么放在这里而不是 fan-out 里：注释已说明 f.Msgs / f.ToolResults
		// 在 goroutine 期间一律不碰。这里是 join 之后的顺序收尾段，
		// 按索引串行，与单步路径的语义一致。
		appendToolCallRecord(f, f.CurTool)
		f.ToolResults = append(f.ToolResults, ToolResultItem{CallID: f.CurTool.ID, Name: r.name, Output: r.text})
		if out := a.stepToolAfter(f); out != outcomeContinue {
			return out
		}
	}
	// ★ 整批 tool 消息已全部落完，此刻才能追加多模态/召回附加消息
	//（否则会切断 assistant(tool_calls) 与其结果的相邻性，见 batchAttachments）。
	flushBatchAttachments(f)
	f.ToolIdx = n
	f.Step = StepTurnEnd
	return outcomeContinue
}

// batchItemResult 是单个工具在并发阶段产出的结果。
type batchItemResult struct {
	name   string
	plugin string
	text   string
	raw    interface{}
	// denied 表示被 before_toolcall 拒绝或插件不健康而未执行（已落 tool 消息）。
	denied bool
}

// runOneTool 执行**单个**工具的「before_toolcall + 实际执行」，不碰共享状态。
//
// 只写 toolCtxs[idx] 与返回值：f.Msgs / f.ToolResults / f.Cur* 全部由调用方
// （stepToolBatch 的顺序收尾段，或串行路径的 stepToolBegin/Exec）负责。
func (a *Agent) runOneTool(f *TaskFrame, idx int) batchItemResult {
	tc := f.PendingTools[idx]
	pluginName := a.ResolveToolPlugin(tc.Name)
	res := batchItemResult{name: tc.Name, plugin: pluginName}

	tctx := toolCtxFor(f, idx)
	tctx.ToolCalls = []sdk.ToolCall{{ID: tc.ID, Name: tc.Name, Plugin: pluginName, Arguments: tc.Arguments}}
	tctx.ToolResults = nil

	// before_toolcall（拒绝则不执行）
	if a.RunStage(sdk.StageBeforeToolcall, tctx) {
		res.text = denialResultText(tctx, tc.Name)
		res.denied = true
		return res
	}
	tc.Arguments = tctx.ToolCalls[0].Arguments

	// 插件崩溃态：不执行
	if pluginName != "" && !a.MainState().pluginHealth.IsHealthy(pluginName) {
		res.text = fmt.Sprintf("插件 %s 处于崩溃状态，已跳过执行，等待自动恢复重载", pluginName)
		res.denied = true
		return res
	}

	// 场景已在 stepToolBatch 的 fan-out **之前**解析完毕（避免竞争与重复计强度），
	// 这里只读取结果。
	var turn model.TurnScene
	if f != nil && f.SceneDone {
		turn = f.TurnScene
	}
	outcome := a.ExecuteToolCallOutcome(tc, f.OutputChannel, turn.Keys...)
	res.text = outcome.Text
	res.raw = outcome.Raw
	tctx.ToolResults = []sdk.ToolResult{{
		CallID: tc.ID, Name: tc.Name, Plugin: pluginName,
		Success: !kit.IsToolError(outcome.Raw), Result: outcome.Raw,
	}}
	return res
}

// stepToolBegin 取本批下一个工具；批已耗尽或发生中断则进入收尾。
func (a *Agent) stepToolBegin(f *TaskFrame) stepOutcome {
	if f.ToolIdx >= len(f.PendingTools) {
		// ★ 批已耗尽（本批全部走串行路径）：同样只能在**这里**追加附加消息，
		// 理由与并行路径一致（见 batchAttachments）。
		flushBatchAttachments(f)
		f.Step = StepTurnEnd
		return outcomeContinue
	}
	tc := f.PendingTools[f.ToolIdx]

	appendToolCallRecord(f, tc)
	pluginName := a.ResolveToolPlugin(tc.Name)
	a.Logf("executing tool: %s (plugin=%s, id=%s)", tc.Name, pluginName, tc.ID)
	if tc.RawArguments != "" {
		// ★ 脱敏后再落盘：模型给的参数正常是别名（sec_xxx），但插件工具
		// 可能被直接传明文（例如模型自己拼了一个 key）。日志是永久的，
		// 一旦落盘就再也收不回来。
		a.Logf("tool %s raw_arguments: %s", tc.Name,
			a.scrubForLog(truncateStr(tc.RawArguments, 300), "raw_args:"+tc.Name))
	}

	sdkTC := sdk.ToolCall{ID: tc.ID, Name: tc.Name, Plugin: pluginName, Arguments: tc.Arguments}
	// 阶段 2c：写**本工具自己的** ctx，不再覆写共享的 f.StageCtx。
	tctx := toolCtxFor(f, f.ToolIdx)
	tctx.ToolCalls = []sdk.ToolCall{sdkTC}
	tctx.ToolResults = nil
	if a.RunStage(sdk.StageBeforeToolcall, tctx) {
		result := denialResultText(tctx, tc.Name)
		ensureBatchAssistant(f)
		f.Msgs = append(f.Msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
		// ★ 必须与 f.Msgs 同步落 context（见 appendToolResultRecord 的说明）。
		// 漏掉这行的实测后果：qq_call_state 被权限策略拒绝后，ContextEvent 里
		// 有 call 没有 result，每一轮都触发上游 400，直到发送前自检把它剔掉。
		appendToolResultRecord(f, tc.ID, tc.Name, result)
		a.PublishEvent(events.EventToolCall, map[string]interface{}{
			"tool":    tc.Name,
			"plugin":  pluginName,
			"args":    tc.Arguments,
			"result":  result,
			"status":  "denied",
			"channel": f.OutputChannel,
		})
		f.ToolIdx++
		return outcomeContinue
	}
	tc.Arguments = tctx.ToolCalls[0].Arguments

	if pluginName != "" && !a.MainState().pluginHealth.IsHealthy(pluginName) {
		result := fmt.Sprintf("插件 %s 处于崩溃状态，已跳过执行，等待自动恢复重载", pluginName)
		a.Logf("skip tool %s: plugin %s unhealthy", tc.Name, pluginName)
		ensureBatchAssistant(f)
		f.Msgs = append(f.Msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
		// 同上：跳过执行也要落 result，否则 call/result 失配。
		appendToolResultRecord(f, tc.ID, tc.Name, result)
		f.ToolIdx++
		return outcomeContinue
	}

	f.CurTool = tc
	f.CurToolPlugin = pluginName
	f.Step = StepToolExec
	return outcomeContinue
}

// denialResultText 返回「工具被插件拒绝」时交给模型的工具结果。
//
// 插件在 before_toolcall 里用 ctx.Response 写的是**拒绝理由**（为什么被拒、
// 能不能重试）。此前这里一律丢成通用文案，模型看不到原因就会反复重试同一个
// 调用——权限门精心写的"请不要重试"等于白写。有理由就用理由。
func denialResultText(ctx *sdk.StageContext, toolName string) string {
	if ctx != nil && ctx.Response != nil {
		if reason := strings.TrimSpace(*ctx.Response); reason != "" {
			return reason
		}
	}
	return fmt.Sprintf("工具 %s 已被插件拒绝", toolName)
}

// appendToolCallRecord 记录一次工具调用（名 + 参数 + 配对键）。
//
// ## 为什么必须与 ToolsUsed 同处维护
//
// ToolsUsed 是「用过的工具名」的便捷视图，ToolCalls 是配对/页化所需的
// 事实源。两份数据若各自 append，任何一条路径漏写就出现
// 「有结果没调用记录」或反之 —— 而那种破配对在渲染时是 400，
// 在记忆里是「这一页永远配不上对」。
//
// ## 为什么要脱敏
//
// 参数由模型给出，正常是别名（sec_xxx），但插件工具可能被直接传明文
// （实测：模型自己拼了一个 key 传进 enc_shell）。落盘后永久留存、
// 且每轮被召回，所以必须过与 Input/Response 同一条闸。
// 脱敏在 Append 内还会再走一次（scrubEventSecrets），这里是落 ToolCalls
// 之前的第一道（因为 ToolCalls 是结构化的，Append 的文本闸看不到字段内部）。
func appendToolCallRecord(f *TaskFrame, tc agentAPI.ToolCall) {
	if f == nil {
		return
	}
	f.ToolsUsed = append(f.ToolsUsed, tc.Name)
	// 原样记录；脱敏统一由 scrubEventSecrets 在 Append 内做（那里是唯一咽喉）。
	//
	// ★ 为什么不在这里就地脱敏：那会造出**第二处脱敏点**。
	//   Input/Response/ToolResults/Blocks 都只在 scrubEventSecrets 一处洗，
	//   多一个局部实现就多一个会漏的地方（本仓为此栽过多次）。
	//   代价是参数里的明文会在内存里多存活一会儿，落盘前已被洗掉。
	f.ToolCalls = append(f.ToolCalls, ToolCallItem{
		CallID:       tc.ID,
		Name:         tc.Name,
		Arguments:    tc.Arguments,
		RawArguments: tc.RawArguments,
	})
}

// appendToolResultRecord 追加一条工具结果记录（与 appendToolCallRecord 成对）。
//
// ★ 为什么不变量必须由**每一次**调用 path 维护（2026-11-04 生产故障）
//
// task.go 顶部的注释写着「ToolCalls 与 ToolResults 通过 CallID 配对；
// 两者都由 appendToolCallRecord 同步追加」。但"跳过执行"的三条提前返回
// 分支只补了 f.Msgs（模型能看到），**没写 f.ToolResults**（落 context 的那份）。
//
// 后果不是"少一条记录"这么轻：
//
//	stepToolBegin   appendToolCallRecord(tc)      ← 记了 call
//	before_toolcall 钩子拒绝 → return             ← 没记 result
//	⇒ ContextEvent{ ToolCalls: 2, ToolResults: 1 }
//	⇒ 渲染出 assistant(tool_calls=[c1,c2]) 但只有 c1 的结果
//	⇒ 上游硬拒绝（HTTP 400）：insufficient tool messages following tool_calls
//	⇒ 脏数据持久化进 context.json，之后**每一轮**都带着它
//
// 实测（qq 插件的来源权限策略拒绝了 qq_call_state）：
//
//	[42] tool_calls=[qq_call_state, output_send__qq]
//	     tool_results=[output_send__qq]        ← call_state 的结果从未落盘
//
// 判定"被拒绝/被跳过"≠"没有结果"：拒绝原因本身就是结果，而且模型
// 确实看到了它（f.Msgs 里那条 tool 消息）。落 context 的必须与发给模型的一致。
func appendToolResultRecord(f *TaskFrame, callID, name, output string) {
	if f == nil {
		return
	}
	f.ToolResults = append(f.ToolResults, ToolResultItem{
		CallID: callID,
		Name:   name,
		Output: output,
	})
}

// stepToolExec 执行工具。**临界区**：见设计文档 §4.3。
func (a *Agent) stepToolExec(f *TaskFrame) stepOutcome {
	// 执行工具前先解析本轮场景：写侧要用它给记忆自动挂场景（主动+被动两条路），
	// 而工具步不一定走到下面的召回分支，所以不能等那里再解析。
	turn := a.resolveTurnScenes(f, f.CurTool.Name)
	outcome := a.ExecuteToolCallOutcome(f.CurTool, f.OutputChannel, turn.Keys...)
	result := outcome.Text
	f.CurResult = result
	f.CurRaw = outcome.Raw
	f.ToolResults = append(f.ToolResults, ToolResultItem{CallID: f.CurTool.ID, Name: f.CurTool.Name, Output: result})
	// ★ 工具输出可能含服务端回显的凭据（实测场景：enc_shell 跑 curl 拿 200，
	// 响应体里带着上游 key 的完整值）。这一行以前原样打印。
	a.Logf("tool %s result: %s", f.CurTool.Name,
		a.scrubForLog(truncateStr(result, 100), "tool_result:"+f.CurTool.Name))

	// Success 此前是**唯一**赋值点且硬编码 true ⇒ 该字段恒真、结构上不可能为
	// false。工具失败是以 nil error + 错误**值**返回的，所以判据必须看返回值。
	// ⚠️ kit.IsToolError 必须同时覆盖存量插件的两种失败约定与「成功不误判」，
	// 否则升级会把存量插件的成功判成失败（见 toolerror_test.go）。
	toolCtxFor(f, f.ToolIdx).ToolResults = []sdk.ToolResult{{
		CallID: f.CurTool.ID, Name: f.CurTool.Name, Plugin: f.CurToolPlugin,
		Success: !kit.IsToolError(outcome.Raw), Result: outcome.Raw,
	}}
	f.Step = StepToolAfter
	return outcomeContinue
}

// stepToolAfter 是工具执行后的全部后处理（阶段、裁剪、消息与事件）。
func (a *Agent) stepToolAfter(f *TaskFrame) stepOutcome {
	tc := f.CurTool
	pluginName := f.CurToolPlugin
	result := f.CurResult

	tctx := toolCtxFor(f, f.ToolIdx)
	a.RunStage(sdk.StageAfterToolcall, tctx)
	if len(tctx.ToolResults) > 0 {
		// 此前是 `Result.(string)` 类型断言，而插件返回的多是 map ⇒ 断言几乎
		// 恒失败，after_toolcall 阶段对结构化结果的改写**静默失效**。
		// 改为：字符串就替换文本；结构化值则保留其原值并按契约渲染。
		switch r := tctx.ToolResults[0].Result.(type) {
		case string:
			result = r
		case nil:
			// 插件清空结果：保持原样，不覆盖。
		default:
			f.CurRaw = r
			result = kit.ToolErrorText(f.CurTool.Name, r)
			if !kit.IsToolError(r) {
				result = fmt.Sprintf("%v", r)
			}
		}
	}
	// 工具后处理：一次相关性过程，两个**正交**声明——
	//   ContextPolicy=prune → 裁剪（踢出去，归档低相关 L0 事件）
	//   RecallPolicy=auto   → 召回（取进来，注入 L2/L3 相关记忆）
	// 两者共用同一份**清洗后**的 query，并统一走 memoryPass（同一入口、
	// 同一次预算与审计）。查询向量取清洗后的有效内容，否则噪声
	// （ANSI/base64/JSON 包装）会把相关性打分带偏，裁错事件、召回错记忆。
	var recallText string
	if def := a.MainState().stageHost.ToolDef(tc.Name); def != nil {
		needPrune := def.ContextPolicy == sdk.ContextPolicyPrune
		needRecall := def.RecallPolicy == sdk.RecallPolicyAuto
		if needPrune || needRecall {
			query := a.toolOutputForQuery(tc.Name, result)
			// 工具路召回的场景有两个来源：本轮输入的场面（如 chan:qq）
			// 与这一步工具本身（如 tool:qq_get_message）。带上工具场景，
			// 才能让「凡是要回 QQ 消息」这类规则在该步被取回。
			// 召回用两条路的并集：声明场景（注入点/通道/工具）+ 涌现场景
			scenes := a.SceneKeysFor(f.Evt, tc.Name)
			turn := a.resolveTurnScenes(f, tc.Name)
			scenes = mergeSceneKeys(scenes, turn.Keys)
			recallText = a.MemoryPass(query, "tool:"+tc.Name, needPrune, needRecall, scenes).RecallText
		}
	}

	ensureBatchAssistant(f)

	// 多模态工具结果：插件通过 SDK.SetToolBlocks 注入 image_url/audio_url block。
	//
	// 媒体不挂在 tool message 上，而是另起一条紧随其后的 user message——
	// 这也是插件文案一直在说的「注入后续对话」。
	// 为何不能挂 tool message：同一张图、同一模型、三轮实测——
	//   图在 user message      → 3/3 读到
	//   图在 tool message      → 0/3（模型答「没能读到这张图」）
	//   tool 纯文本 + 后接 user → 3/3 读到
	// tool message 那轮 prompt_tokens 反而更高（7967 vs 7089），base64 确实
	// 进了上游，但 role=tool 上的多模态 content 数组不被当作可视内容。
	//
	// 主模型不支持该模态时更不能直接塞：网关会把 image_url 静默剥离后仍
	// 返回 200，模型回答「我没有看到图片」而内核以为注入成功。改走回退链。
	toolMsg := agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result}
	var mediaMsg *agentAPI.Message
	if rawBlocks := a.io.ConsumeToolBlocks(); len(rawBlocks) > 0 {
		var blocks []agentAPI.ContentBlock
		for _, b := range rawBlocks {
			if cb, ok := b.(pubsdk.ContentBlock); ok {
				// 跨包类型拷贝（pubsdk.ContentBlock → agentAPI.ContentBlock）
				block := agentAPI.ContentBlock{Type: cb.Type, Text: cb.Text}
				if cb.ImageURL != nil {
					block.ImageURL = &agentAPI.ImageURL{URL: cb.ImageURL.URL, Detail: cb.ImageURL.Detail}
				}
				if cb.AudioURL != nil {
					block.AudioURL = &agentAPI.AudioURL{URL: cb.AudioURL.URL}
				}
				blocks = append(blocks, block)
			}
		}
		if len(blocks) > 0 {
			// 先落成块：无论下面走直视还是回退转写，媒体本体都该进记忆。
			a.stageMediaBlocks(a.CaptureBlockMedia(blocks, tc.Name)...)

			if native, fallbackText := a.prepareToolBlocks(blocks); len(native) > 0 {
				// 能直视：另起一条 user message 承载媒体，并补一句来源说明。
				mediaBlocks := append([]agentAPI.ContentBlock{{
					Type: "text",
					Text: fmt.Sprintf("[以下是 %s 注入的媒体内容]", tc.Name),
				}}, native...)
				mediaMsg = &agentAPI.Message{Role: "user", Blocks: mediaBlocks}
			} else if fallbackText != "" {
				toolMsg.Content = result + "\n\n" + fallbackText
				result = toolMsg.Content
				if len(f.ToolResults) > 0 {
					f.ToolResults[len(f.ToolResults)-1].Output = result
				}
			}
		}
	}
	f.Msgs = append(f.Msgs, toolMsg)

	// 工具结果大小处置：超限则**全文落盘 + 上下文只放预览**。
	//
	// 为什么在这里而不是 stepToolExec：那里拿到的 outcome.Text 尚未经
	// after_toolcall 阶段改写，而模型最终看到的是**这里**的内容。
	//
	// ★ 刻意**不截断**（截断会让模型拿到残缺信息且不知道被截过，与本仓
	//   「静默降级」同族）。也刻意**不原样放行**（那正是 36783 token 单条
	//   结果冲破预算、整轮终止的成因）。
	//
	// 走的是第三条路：正文完整保存到事件目录，上下文里换成
	// 「预览 + 句柄 + 总量」+ toolresult_read 工具按需取区间。
	// 取哪一段由模型决定，信息不丢（判据要求分段拼回逐字节相同）。
	if a.checkToolResultSize(tc.Name, toolMsg.Content) {
		noteOversizeTool(f, tc.Name)
		if replaced, ok := a.offloadToolResult(f, tc.Name, toolMsg.Content); ok {
			toolMsg.Content = replaced
			result = replaced
			if len(f.ToolResults) > 0 {
				f.ToolResults[len(f.ToolResults)-1].Output = replaced
			}
		}
	}
	if mediaMsg != nil {
		// 延迟到批末（见 batchAttachments 的注释）：当场追加会把后续 tool 消息
		// 与 assistant 挤开，上游判 insufficient tool messages。
		f.BatchAttachments = append(f.BatchAttachments, *mediaMsg)
	}
	// 召回同样延迟到批末：它是 system 消息，同样会断开 tool 配对。
	// （语义与原先一致：同一批内多条召回仍是最后一条生效，因为
	//  appendOrReplaceRecall 本就是替换而非累加。）
	if recallText != "" {
		f.BatchRecall = recallText
	}

	a.PublishEvent(events.EventToolCall, map[string]interface{}{
		"tool":    tc.Name,
		"plugin":  pluginName,
		"args":    tc.Arguments,
		"result":  result,
		"status":  "ok",
		"channel": f.OutputChannel,
	})

	f.ToolIdx++
	f.Step = StepToolBegin
	return outcomeContinue
}

// flushBatchAttachments 把本批积压的附加消息一次性追加到 Msgs 末尾。
//
// 必须在**整批 tool 消息全部落完之后**调用，否则会切断
// assistant(tool_calls) 与 tool 结果消息的相邻性（见 batchAttachments 注释）。
// 两个调用点：并行路径 stepToolBatch 的收尾循环之后；串行路径 stepToolBegin
// 发现批已耗尽时。
func flushBatchAttachments(f *TaskFrame) {
	if len(f.BatchAttachments) == 0 && f.BatchRecall == "" {
		return
	}
	for _, m := range f.BatchAttachments {
		f.Msgs = append(f.Msgs, m)
	}
	f.BatchAttachments = nil
	if f.BatchRecall != "" {
		f.Msgs = appendOrReplaceRecall(f.Msgs, f.BatchRecall)
		f.BatchRecall = ""
	}
}

// ensureBatchAssistant 保证本批有且只有**一条**带 tool_calls 的 assistant 消息。
//
// 阶段 2a 的落法：批内「一个 assistant 携带全部 tool_calls」+ N 条 tool 消息。
// 惰性写入（首次调用时才 append）而不是在 stepLLM 预置，因为：
//
//	· 预置会让「全部工具都被拒绝/崩溃」这类零执行分支也留下一条空 assistant
//	  （虽然无害，但会给模型一条没有结果的 tool_calls，个别网关会报错）
//	· assistant 文本（ContentOnce）要挂在**第一条**上，而那要等到有结果才知道
//
// 并行下完成顺序不确定，因此 assistant 必须**先于**任何 tool 消息存在；
// 惰性写入天然满足：第一个完成的工具触发写入，后续只补 tool 消息。
func ensureBatchAssistant(f *TaskFrame) {
	if f.AssistantMsgIdx >= 0 {
		return
	}
	msgContent := ""
	if f.ContentOnce && f.Resp != nil {
		msgContent = f.Resp.Content
		f.ContentOnce = false
	}
	f.AssistantMsgIdx = len(f.Msgs)
	f.Msgs = append(f.Msgs, agentAPI.Message{
		Role:             "assistant",
		Content:          msgContent,
		ReasoningContent: f.Resp.ReasoningContent,
		ToolCalls:        f.PendingTools,
	})
}

// stepTurnEnd 收尾本批并进入下一轮。
func (a *Agent) stepTurnEnd(f *TaskFrame) stepOutcome {
	// 供下一轮顶部选择补位文案。
	f.LastBatchReplyOnly = f.ReplyOnly
	f.Turn++
	f.Step = StepLLM
	return outcomeContinue
}

// completeLLM 走**内核的 LLM 设施**做一次补全（§3.2.1）。
//
// ★ 基座在这里只做转发：回退链、同源重试、可用性标记、活动源更新、
//
//	错误分类全在 `kernelapi.LLMClient` 内部。基座不认识"有两个源"这件事。
//
// 未装配设施（裸 agent / 单测）时回落到"单源直调"：没有回退链可走，
// 行为与改造前逐字一致（那时 `a.provider` 就是唯一源）。
func (a *Agent) completeLLM(ctx context.Context, req *agentAPI.CompletionRequest, channel string) (*agentAPI.CompletionResponse, error) {
	if a.llm != nil {
		return a.llm.Complete(ctx, &kernelapi.LLMRequest{
			AgentID:   string(a.id),
			Preferred: a.preferredProvider,
			Request:   req,
		})
	}
	if a.provider == nil {
		return nil, fmt.Errorf("%w: no provider configured", kernelapi.ErrLLMAllFailed)
	}
	resp, err := a.provider.Chat(ctx, req)
	if err != nil {
		return nil, kernelapi.ClassifyLLMError(err)
	}
	return resp, nil
}

// buildToolContexts 为本批每个工具预建一份独立的 StageContext。
//
// Extra 必须**逐份复制**而不是共享同一个 map：Extra 会被 stage handler 写
// （例如插件注入 media_blocks），共享即竞争。逐份浅拷贝即可——里面的值
// （string / []agentAPI.ContentBlock）本身是只读的。
func buildToolContexts(f *TaskFrame) {
	base := f.StageCtx
	f.ToolCtxs = make([]sdk.StageContext, len(f.PendingTools))
	for i := range f.PendingTools {
		f.ToolCtxs[i] = sdk.StageContext{
			Extra:    copyExtraMap(base.Extra),
			NoMemory: base.NoMemory,
		}
		// ★ **工具调用帧必须带来源标识**（comefrom）：
		// 通道插件的权限门（before_toolcall）靠它区分"这是用户请求"还是
		// "这是 agent 内部委派"。缺了它，驻留子的工具调用会被按**用户**
		// 权限模型判定 ⇒ 合法工作全被拦（2026-10-16 现场实证，见 comefrom.go）。
		//
		// 从 base 继承（StageCtx 已由 stageCtxFromInput 标过）；copyExtraMap
		// 会把该键一并复制，这里只在因某种路径缺失时补一个**保守值**
		// （保守 = 按旧语义当作用户请求，宁可多查一次也不再出现"静默越权"）。
		stampComeFrom(f.ToolCtxs[i].Extra, comeFromUser)
		// Phase 由 runStage 每次调用时设置，此处不预置。
	}
}

// toolCtxFor 返回第 i 个工具的 StageContext；越界或未建时回落到 f.StageCtx，
// 保证调用方不必判空（测试替身等未走 buildToolContexts 的路径）。
func toolCtxFor(f *TaskFrame, i int) *sdk.StageContext {
	if i >= 0 && i < len(f.ToolCtxs) {
		return &f.ToolCtxs[i]
	}
	return f.StageCtx
}

// copyExtraMap 浅拷贝 Extra（值视为只读）。
// nil 安全：base.Extra 为 nil 时返回 nil，写入方需自行判空。
func copyExtraMap(src map[string]interface{}) map[string]interface{} {
	if src == nil {
		return nil
	}
	dst := make(map[string]interface{}, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// stageHint 从 stage 上下文里取一个字符串提示（去掉首尾空白；空返回 ""）。
//
// 为什么从 Extra 取而不是给 TaskFrame 加字段：插件 payload 已经原样进了
// Extra（见 process.go 的 payload 拷贝），再加字段等于同一份数据存两处。
func stageHint(ctx *sdk.StageContext, key string) string {
	if ctx == nil || ctx.Extra == nil {
		return ""
	}
	v, _ := ctx.Extra[key].(string)
	return strings.TrimSpace(v)
}

// isModelNotAllowed 判断错误是不是"这个模型名不被该 key 允许"。
//
// 与"凭证失效"处置相反：前者只否掉一个 model（provider 仍可服务其他 model，
// 也仍可用于视觉回退），后者才该把整源标记不可用。
func isModelNotAllowed(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range []string{
		"model_not_allowed",
		"is not allowed for this key",
		"model not allowed",
		"model_not_found",
		"does not exist or you do not have access",
	} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
