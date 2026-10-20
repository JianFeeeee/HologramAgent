package core

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
	"log"
	"strings"
	"sync"
	"time"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	"github.com/JianFeeeee/HomeAgent/internal/events"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// Step 是任务状态机的游标。
type Step int

const (
	// StepPrepare 构建消息与工具表、应用中断标记、跑 pre_action 阶段。
	StepPrepare Step = iota
	// StepLLM 轮次顶部（中断/占位）+ LLM 调用（含 provider 回退与重试）+ post_action。
	StepLLM
	// StepToolBatch 并发执行整批工具（阶段 2d）。仅当全批可并发时使用；
	// 否则走 StepToolBegin/Exec/After 的串行路径。
	//
	// 为何要有独立 step：runTaskSteps 是**单线程**驱动状态机的，
	// 并发必须在一个 step 内 fan-out 并 join，否则"每步一个工具"的游标
	// 推进模型无法表达"一批同时跑"。
	StepToolBatch
	// StepToolBegin 取本批下一个工具，跑 before_toolcall；被拒/插件不健康则跳过。
	StepToolBegin
	// StepToolExec 执行工具。**临界区**：副作用不可回滚，执行中不是安全点。
	StepToolExec
	// StepToolAfter after_toolcall 阶段、上下文裁剪、消息与事件组装、批后中断检查。
	StepToolAfter
	// StepTurnEnd 收尾本批并进入下一轮。
	StepTurnEnd
)

// stepOutcome 是一次 step 执行的结果。
type stepOutcome int

const (
	// outcomeContinue 继续执行下一个 step（游标可能停在原地以表达"重跑本 step"）。
	outcomeContinue stepOutcome = iota
	// outcomeDone 任务成功结束，响应在 frame.Response。
	outcomeDone
	// outcomeFailed 任务失败结束，错误在 frame.Err。
	outcomeFailed
	// outcomeSuspended 任务在安全点被抢占挂起，帧已保存（M3b 起使用）。
	outcomeSuspended
)

// taskTerminal 是任务的终态种类（设计文档 §7：每个任务恰有一个终态）。
type taskTerminal int

const (
	// terminalNone 任务尚未结束，需进入 run 段。
	terminalNone taskTerminal = iota
	// terminalOK 正常完成（已提交上下文并回执）。
	terminalOK
	// terminalError 执行出错（已提交错误响应）。
	terminalError
	// terminalStageShortCircuit 被 on_input 阶段短路（响应已发出）。
	terminalStageShortCircuit
	// terminalSkipped 未进入执行：解析失败或被去重。
	terminalSkipped
	// terminalConsolidation 走记忆整理专用路径，已处理完毕。
	terminalConsolidation
	// terminalSuspended 被抢占挂起，等待恢复（M3b 起使用）。
	terminalSuspended
)

// TaskFrame 承载一个任务在安全点之间必须存活的所有状态。
//
// 不变量（设计文档 §8.1 I3）：帧是**纯数据**；不得持有任何锁或资源跨越安全点。
type TaskFrame struct {
	Input    string
	StageCtx *sdk.StageContext

	// 跨轮次状态
	Msgs               []agentAPI.Message
	Tools              []interface{}
	ToolsUsed          []string
	ToolResults        []ToolResultItem
	Turn               int
	LastBatchReplyOnly bool

	// OverflowRecover 是本帧内的超页恢复次数。
	//
	// 为什么按**帧**而不是按 agent 计：恢复预算要防的是「一次裁剪没搞定 ⇒
	// 裁剪→复原→又超页→再裁剪」这个**单帧内**的循环；跨帧累计会让一个长会话
	// 在若干帧后突然失去超页处理能力（而那时恰恰最需要它）。
	OverflowRecover int

	// 当前工具批
	PendingTools  []agentAPI.ToolCall
	ToolIdx       int
	ReplyOnly     bool
	ContentOnce   bool
	CurTool       agentAPI.ToolCall
	CurToolPlugin string
	CurResult     string
	// toolCtxs 是**每个工具各一份**的 StageContext（阶段 2c）。
	//
	// 为何必须拆：f.StageCtx 原本是单槽，批内每个工具都覆写
	// （ToolCalls=[单元素]、ToolResults 覆写、Results[0] 回读）。
	// 并行下 N 个 goroutine 同写一个 ctx = 数据竞争，且 after_toolcall
	// 插件可能读到**别的工具**的结果。
	// 拆分后每个工具只写自己那份，Extra 在构造时逐份复制
	//（output_channel / input_source / media_* —— stage.go:18 依赖前者）。
	toolCtxs []sdk.StageContext

	// oversizeTools / oversizeToolNames 记录本任务中「过大工具结果」的次数与工具名。
	// ⚠️ 结果**未被裁剪**（方案 B），这里只是记账，供调度器/状态面判断
	// 是否有工具在稳定地产出超大结果。
	oversizeTools     int
	oversizeToolNames []string

	// assistantMsgIdx 是本批 assistant(tool_calls) 消息在 Msgs 中的下标，
	// -1 表示尚未写入。阶段 2a：批内只写**一条** assistant 承载全部
	// tool_calls，工具结果各自作为 tool 消息追加在它之后。
	assistantMsgIdx int

	// batchAttachments / batchRecall 是本批**积压的附加消息**，
	// 只在整批 tool 消息全部落完之后才由 flushBatchAttachments 追加。
	//
	// ★ 为何必须积压而不是当场追加（2026-10-13 生产 400 事故）：
	//   OpenAI 协议要求 assistant(tool_calls=[c1..cn]) 之后**紧接** n 条
	//   role=tool，中间不得插入任何其他角色。而 stepToolAfter 在批内循环里
	//   对**每个工具各调一次** —— 若它当场就把 mediaMsg（user）/召回（system）
	//   追加进去，第一条 tool 之后就被塞进一条其他角色的消息，于是第二条
	//   tool 不再与 assistant 相邻。上游实测报：
	//     api error 400: An assistant message with 'tool_calls' must be followed
	//     by tool messages responding to each 'tool_call_id'.
	//     (insufficient tool messages following tool_calls message)
	//
	//   用户明确的设计约束：「多模态注入点应该在当前轮工具调用后」。
	batchAttachments []agentAPI.Message
	batchRecall      string

	// CurRaw 是本次执行的**未降级**返回值（interface{}）。
	// 存在理由：CurResult 是 string，结构化信息在此被抹平，导致 Success
	// 无法诚实化、after_toolcall 的改写静默失效。
	CurRaw interface{}
	Resp   *agentAPI.CompletionResponse

	// Scene 是本轮**涌现**出来的场景键（由场面指纹聚类得到，无人声明），
	// sceneDone 标记是否已解析过——一轮只解析一次：多解析一次就多给场景
	// 加一次强度，「工具调得多」会被误当成「这个场面更常出现」。
	Scene     string
	turnScene memory.TurnScene
	sceneDone bool

	// 游标与终态
	Step     Step
	Response string
	Err      error

	// turnUsage 是本轮任务（一个 TaskFrame）内全部 LLM 调用的用量合计。
	//
	// 为何要单独存：usageLedger 是**会话级**累计（跨用户请求），
	// 而对外回包的 usage 按 OpenAI 语义应是**本次请求**。
	// 两者口径不同，不能互相顶替：把会话累计当本次报了，
	// 第二次请求就会报出翻倍的数字。
	turnUsage agentAPI.TokenUsage

	// ---- 任务层现场（原 processInput 的局部变量）----
	//
	// 这些字段让帧覆盖 prepare → step… → finish 全生命周期：挂起发生在 run 段的
	// 安全点，恢复后由 finish 段统一提交（context.Append + emitResponse +
	// emitMemoryCandidate），因此挂起不会重复提交。
	Evt         *agentIO.InputEvent
	CleanInput  string
	IsInterrupt bool
	StartedAt   time.Time
	Terminal    taskTerminal
	// OutputChannel 是本任务的输出通道（来源通道的稳定副本）。
	//
	// 这是本任务通道的**唯一**来源：内核不持有"当前通道"可变状态（N0 已删除
	// Agent.currentOutputChannel）。那类字段会被后来的任务覆盖，而被打断任务
	// 恢复时不重新 prepare（resumeTask 只 rebase 前缀），于是两任务串台——
	// 被打断任务的回复发到中断任务的通道上（见
	// TestPreempt_ResumeKeepsOwnOutputChannel）。
	OutputChannel string

	// PrefixLen 是 stepPrepare 构建的**基础前缀**长度（system + timeline + 用户输入）。
	// 恢复时用它把「本任务自己的现场」接回重建后的前缀之上（见 rebaseFramePrefix）。
	PrefixLen int
	// InputBlocks 是本轮输入携带的多模态块；重建前缀时要重新挂回。
	InputBlocks []agentAPI.ContentBlock
}

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

// runTaskSteps 驱动状态机直到任务结束或被抢占挂起。
//
// 这是 M1 的驱动循环，M3a 从 process() 抽出来，使调用方可以拿到
// outcomeSuspended 并把帧留给调度器保存。
func (a *Agent) runTaskSteps(f *TaskFrame) stepOutcome {
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
			log.Printf("[agent] 已达最大工具轮次 %d（turn=%d），强制收尾", a.maxToolTurns, f.Turn)
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
	switch a.runTaskSteps(f) {
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
func (a *Agent) runInputTask(evt *agentIO.InputEvent) (*TaskFrame, stepOutcome) {
	// 临界区标记由调度器 goroutine 维护，任务结束（含挂起）即清。
	// interceptLoop 读它来决定“能不能取消”，因此必须是原子的。
	defer a.sched.setCritical(false)

	f, term := a.prepareInputTask(evt)
	switch term {
	case terminalSkipped, terminalStageShortCircuit, terminalConsolidation:
		return nil, outcomeDone
	}

	out := a.runTaskSteps(f)
	if out == outcomeSuspended {
		f.Terminal = terminalSuspended
		return f, outcomeSuspended
	}
	a.finishInputTask(f, out)
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
func (a *Agent) rebaseFramePrefix(f *TaskFrame) {
	if f == nil || f.PrefixLen <= 0 || f.PrefixLen > len(f.Msgs) {
		return
	}
	tail := append([]agentAPI.Message(nil), f.Msgs[f.PrefixLen:]...)

	budget := a.computeTokenBudget()
	memContext := a.buildTaskMemoryContext(f, f.Input, budget.MemoryTokens)
	sysPrompt := a.buildSystemPrompt(memContext, f.Input)
	prefix := a.buildMessages(sysPrompt, f.Input, a.contextTokenBudget(budget))

	// 重建会丢掉 prepare 段对尾部消息的两处改写，这里等价地补回。
	if f.IsInterrupt && len(prefix) > 0 {
		last := prefix[len(prefix)-1]
		last.Role = "system"
		last.Content = "[中断消息] " + last.Content
		prefix[len(prefix)-1] = last
	}
	if len(f.InputBlocks) > 0 && len(prefix) > 0 {
		prefix[len(prefix)-1].Blocks = f.InputBlocks
	}

	f.Msgs = append(prefix, tail...)
	f.PrefixLen = len(prefix)
}

// prepareInputTask 执行 processInput 的前半段（去重、通道解析、阶段、裁剪、
// 输入事件落上下文）。返回终态不为 terminalNone 时调用方不得进入 run 段。
func (a *Agent) prepareInputTask(evt *agentIO.InputEvent) (*TaskFrame, taskTerminal) {
	start := time.Now()

	in, ok := a.resolveInput(evt)
	if !ok {
		// 空输入（文本与媒体都空）：没有可处理内容，但同步调用方仍在等回执。
		a.emitSkippedReply(evt, "empty_input")
		return nil, terminalSkipped
	}

	// 去重按文本做：webui/GUI 断线重连会重放未确认消息。
	// 带媒体时跳过——媒体输入的 alt 文案（"[从 qq 收到了 image]"）对不同图片
	// 是同一句，拿它去重会把连发的两张图误判成重复。
	if len(in.blocks) == 0 && a.isDuplicateInput(evt.Source, in.text) {
		log.Printf("[agent] dropped duplicate input from %s: %s", evt.Source, truncateStr(in.text, 60))
		// 去重是「不处理」而不是「不回」，否则同步调用方（cli/clawhub 无超时）
		// 会永久挂起（设计文档 §7 不变量 I5、§11.3 X2/X4）。
		a.emitSkippedReply(evt, "duplicate")
		return nil, terminalSkipped
	}

	// 通道只从**输入事件**推导，内核不持有"当前通道"可变状态
	// （见 outputChannelOf；这消除了中断任务覆盖它导致被打断任务串台的整类问题）。
	// 进入本任务的临界区属性（记忆整理整任务不可抢占）。
	// 必须在 processConsolidation 之前设置——它就在下面同步执行。
	a.sched.setCritical(isCriticalChannel(outputChannelOf(evt)))

	if evt.OutputChannel == channelConsolidation {
		a.processConsolidation(evt, in.text)
		return nil, terminalConsolidation
	}

	// pendingMedia 让 describe_image / transcribe_audio / ocr_image 拿到本轮媒体的
	// 原始 data/url，也是这三个工具是否出现在工具表里的开关。仅对用户直接上传成立
	//（payload 里才有 data/url）；插件注入的是成品 block，取不到原始数据。
	if evt.Type == "image" || evt.Type == "audio" {
		a.pendingMedia = evt.Payload
	}

	// 媒体先变成一等块（字节进块的 Data）。不存的后果是 ContextEvent.Input
	// 只剩一句 alt 文本，base64 随 message 数组发给模型后就丢了。
	if len(in.blocks) > 0 {
		a.stageMediaBlocks(a.captureBlockMedia(in.blocks, in.captureTool)...)
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
	a.injectSourceContext(stageCtx, evt)

	if a.runStage(sdk.StageOnInput, stageCtx) {
		// 插件在 OnInput 短路：本轮一个 LLM 都没跑，用量为零是如实的。
		a.emitResponse(evt, *stageCtx.Response, agentAPI.TokenUsage{})
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
	a.publishEvent(events.EventRawInput, rawPayload)

	// 裁剪与审计统一在 memoryPass 内（日志已带 trigger）。
	a.pruneOnInput(evt, cleanInput)

	// 本轮 inputch（处理表按它记账）+ contextfull 检测（只有驻留子设了钩子）。
	a.tableMu.Lock()
	a.currentInputch = outputChannelOf(evt)
	a.tableMu.Unlock()

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
func (a *Agent) finishInputTask(f *TaskFrame, out stepOutcome) {
	evt := f.Evt

	// pendingMedia 是「本轮」语义：任务结束即清（挂起时保留，见 runInputTask）。
	if evt != nil && (evt.Type == "image" || evt.Type == "audio") {
		a.pendingMedia = nil
	}

	if out == outcomeFailed {
		log.Printf("[agent] process %s error: %v", evt.Type, f.Err)
		resp := fmt.Sprintf("处理错误: %v", f.Err)
		a.emitResponse(evt, resp, f.turnUsage)
		a.context.Append(ContextEvent{Timestamp: time.Now(), Source: "agent", Input: f.Input, Response: resp})
		f.Terminal = terminalError
		return
	}

	// inputch 处理表：本轮**未主动写入**时由系统自动写（保证每轮必有记录）。
	// 只有驻留子会用到（根 agent 的 children 为 0 时这只是几个空操作）。
	a.autoRecordInputch(f)

	elapsed := time.Since(f.StartedAt)
	log.Printf("[agent] %s from %s → response (%dms, tools=%v)",
		evt.Type, evt.Source, elapsed.Milliseconds(), f.ToolsUsed)

	// 本轮捕获的媒体一起挂到这条事件上：用户上传的、插件注入的，以及模型调
	// multimodal_see_picture / see_video 时经 SetToolBlocks 注入的。
	turnEvt := ContextEvent{
		Timestamp:   time.Now(),
		Source:      "agent",
		Input:       f.CleanInput,
		Response:    f.Response,
		ToolsUsed:   f.ToolsUsed,
		ToolResults: f.ToolResults,
	}
	a.bindEventMedia(&turnEvt, a.drainMediaBlocks())
	// ★ 明文拦截发生在 a.context.Append **内部**（见 context.go 的 Append），
	// 不是在每个调用点 —— 写 context 的地方有 5 处，逐个补必漏。
	a.context.Append(turnEvt)

	a.emitResponse(evt, f.Response, f.turnUsage)

	if !f.StageCtx.NoMemory {
		a.emitMemoryCandidate(evt.Source, f.CleanInput, f.Response, f.ToolResults, f.ToolsUsed)
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
	budget := a.computeTokenBudget()

	memContext := a.buildTaskMemoryContext(f, f.Input, budget.MemoryTokens)
	sysPrompt := a.buildSystemPrompt(memContext, f.Input)
	f.Tools = a.buildToolDefs()

	f.Msgs = a.buildMessages(sysPrompt, f.Input, a.contextTokenBudget(budget))
	// 工具提醒（interrupt）：以 system 角色注入，不让模型误认为用户发言
	if a.interruptInput {
		last := f.Msgs[len(f.Msgs)-1]
		last.Role = "system"
		last.Content = "[中断消息] " + last.Content
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

	log.Printf("[agent] tool call loop start, max_ctx=%d target=%d fixed=%d mem=%d ctx=%d %d tools, %d events, personality=%t, docs=%d",
		budget.MaxContext, budget.TargetUsage, budget.FixedTokens, budget.MemoryTokens, budget.ContextTokens,
		len(f.Tools), a.context.Len(),
		a.personality != nil && a.personality.Content != "",
		a.docStoreSize())

	if a.runStage(sdk.StagePreAction, f.StageCtx) {
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

	// 上下文占满检测：此刻 f.Msgs 已建好（含 system + timeline + 本轮输入）。
	// 只有驻留子设了 onContextFull ⇒ 对根 agent 是 no-op。
	a.checkContextFull(f)

	f.Step = StepLLM
	return outcomeContinue
}

// stepLLM 是轮次顶部与 LLM 调用。
//
// 取消（context.Canceled 且 agent 未退出）时**留在本 step 并 Turn++**——等价于
// 原实现的 `continue`：重新排空中断、补占位、重新请求。抢占挂起将在 M3 从这里接管。
func (a *Agent) stepLLM(f *TaskFrame) stepOutcome {
	// ★ 本地超页预判：每次发 LLM 请求**之前**检查积累上下文，覆盖轮内 tool 回环。
	//   轮内工具回灌会把积累量推到窗口数倍，而现状只在轮首检查
	//   （checkContextFull，且根 agent no-op）—— 等轮首才发现时，
	//   整轮工具调用的代价已经付掉了。
	//   必须在这里做而不能更早：f.Msgs 在上一行之前已经拼好，
	//   而本次裁剪会让它过期。
	if handled, _ := a.maybeHandleContextOverflow(f, ""); handled {
		if f.Terminal == terminalError {
			return outcomeFailed // 恢复预算耗尽，f.Err 已写明
		}
		// 已裁剪：f.Msgs 必须在 stepPrepare 重建，这里那份就是超限那份。
		f.Step = StepPrepare
		return outcomeContinue
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

	req := &agentAPI.CompletionRequest{
		Messages:        f.Msgs,
		MaxTokens:       4096,
		Tools:           f.Tools,
		ToolChoice:      "auto",
		DisableThinking: !a.thinkingEnabled,
	}

	providers := a.resolveProviders(req)
	resp, llmErr := a.callLLMWithFallback(req, providers, f.OutputChannel)

	if llmErr != nil {
		// ★ 上下文超页（上游 ErrContextFull）必须在 provider fallback **之前**处理：
		//   fallback 会换 provider 重发**同一个**超限请求，换谁都一样超。
		//   与本地预判（maybeHandleContextOverflow）走同一条裁剪路径。
		if a.handleUpstreamContextFull(llmErr) {
			// 已裁剪。**必须退回 StepPrepare 重建 f.Msgs**：
			// f.Msgs 只在 stepPrepare 里由 buildMessages 装配，stepLLM 不重建；
			// 而挂起帧里那份就是刚刚超限的那份，复用它等于原样再发一次。
			f.Turn++
			f.Step = StepPrepare
			return outcomeContinue
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
		f.Err = fmt.Errorf("all %d providers failed, last error: %w", len(providers), llmErr)
		return outcomeFailed
	}

	f.StageCtx.LLMText = resp.Content
	f.StageCtx.ReasoningContent = resp.ReasoningContent
	// 用量同时给插件看（StageCtx）与记账（usageLedger）。
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
			f.StageCtx.ToolCalls[i].Plugin = a.resolveToolPlugin(f.StageCtx.ToolCalls[i].Name)
		}
	}
	if a.runStage(sdk.StagePostAction, f.StageCtx) {
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
	a.usageLedger.record(resp.TokenUsage)
	// 本轮任务内的合计（一个 TaskFrame 可能多轮 LLM：工具回环）。
	// 它与 usageLedger 的口径不同（本次请求 vs 会话累计），两者都要留。
	f.turnUsage.Add(resp.TokenUsage)

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
	if sess := a.usageLedger.snapshot(); sess.Calls > 0 {
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
	a.publishEvent(events.EventAgentLLMChain, chainPayload)

	if resp.ReasoningContent != "" {
		a.publishEvent(events.EventReasoning, map[string]interface{}{
			"content": resp.ReasoningContent,
			"channel": f.OutputChannel,
		})
	}

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
	f.buildToolContexts()
	// 阶段 2a：批内消息**预置**为「一个 assistant 带全部 tool_calls」。
	//
	// 为何预置而不是逐步 append：并行执行下多个工具的**完成顺序不确定**，
	// 若等结果回来再落消息，assistant 就必须等所有结果齐了才能写；
	// 而 OpenAI 协议要求 assistant(tool_calls) 在**结果之前**。
	// 预置同时让 assistant 只出现一次（逐步 append 会产生 N 条）。
	//
	// ⚠️ 前提：before_toolcall 阶段不得改写工具参数（已核实全仓无此用法）。
	// 若某插件将来要改写 args，需在这里改为「回填后重写该条 assistant」。
	f.assistantMsgIdx = -1
	f.ToolIdx = 0
	// 阶段 2d：全批可并发（且无需保序）⇒ 走并发 step。
	if f.batchRunnable(a) {
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
	f.ensureBatchAssistant()

	// ⚠️ 场景必须**在 fan-out 之前**解析一次：resolveTurnScenes 会把结果
	// 记进共享的 f.sceneDone / f.turnScene（memorypass.go:289），
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
		f.ToolResults = append(f.ToolResults, ToolResultItem{Name: r.name, Output: r.text})
		if out := a.stepToolAfter(f); out != outcomeContinue {
			return out
		}
	}
	// ★ 整批 tool 消息已全部落完，此刻才能追加多模态/召回附加消息
	//（否则会切断 assistant(tool_calls) 与其结果的相邻性，见 batchAttachments）。
	f.flushBatchAttachments()
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
	pluginName := a.resolveToolPlugin(tc.Name)
	res := batchItemResult{name: tc.Name, plugin: pluginName}

	tctx := f.toolCtxFor(idx)
	tctx.ToolCalls = []sdk.ToolCall{{ID: tc.ID, Name: tc.Name, Plugin: pluginName, Arguments: tc.Arguments}}
	tctx.ToolResults = nil

	// before_toolcall（拒绝则不执行）
	if a.runStage(sdk.StageBeforeToolcall, tctx) {
		res.text = denialResultText(tctx, tc.Name)
		res.denied = true
		return res
	}
	tc.Arguments = tctx.ToolCalls[0].Arguments

	// 插件崩溃态：不执行
	if pluginName != "" && !a.pluginHealth.isHealthy(pluginName) {
		res.text = fmt.Sprintf("插件 %s 处于崩溃状态，已跳过执行，等待自动恢复重载", pluginName)
		res.denied = true
		return res
	}

	// 场景已在 stepToolBatch 的 fan-out **之前**解析完毕（避免竞争与重复计强度），
	// 这里只读取结果。
	var turn memory.TurnScene
	if f != nil && f.sceneDone {
		turn = f.turnScene
	}
	outcome := a.executeToolCallOutcome(tc, f.OutputChannel, turn.Keys...)
	res.text = outcome.Text
	res.raw = outcome.Raw
	tctx.ToolResults = []sdk.ToolResult{{
		CallID: tc.ID, Name: tc.Name, Plugin: pluginName,
		Success: !isToolError(outcome.Raw), Result: outcome.Raw,
	}}
	return res
}

// stepToolBegin 取本批下一个工具；批已耗尽或发生中断则进入收尾。
func (a *Agent) stepToolBegin(f *TaskFrame) stepOutcome {
	if f.ToolIdx >= len(f.PendingTools) {
		// ★ 批已耗尽（本批全部走串行路径）：同样只能在**这里**追加附加消息，
		// 理由与并行路径一致（见 batchAttachments）。
		f.flushBatchAttachments()
		f.Step = StepTurnEnd
		return outcomeContinue
	}
	tc := f.PendingTools[f.ToolIdx]

	f.ToolsUsed = append(f.ToolsUsed, tc.Name)
	pluginName := a.resolveToolPlugin(tc.Name)
	log.Printf("[agent] executing tool: %s (plugin=%s, id=%s)", tc.Name, pluginName, tc.ID)
	if tc.RawArguments != "" {
		// ★ 脱敏后再落盘：模型给的参数正常是别名（sec_xxx），但插件工具
		// 可能被直接传明文（例如模型自己拼了一个 key）。日志是永久的，
		// 一旦落盘就再也收不回来。
		log.Printf("[agent] tool %s raw_arguments: %s", tc.Name,
			scrubForLog(truncateStr(tc.RawArguments, 300), "raw_args:"+tc.Name))
	}

	sdkTC := sdk.ToolCall{ID: tc.ID, Name: tc.Name, Plugin: pluginName, Arguments: tc.Arguments}
	// 阶段 2c：写**本工具自己的** ctx，不再覆写共享的 f.StageCtx。
	tctx := f.toolCtxFor(f.ToolIdx)
	tctx.ToolCalls = []sdk.ToolCall{sdkTC}
	tctx.ToolResults = nil
	if a.runStage(sdk.StageBeforeToolcall, tctx) {
		result := denialResultText(tctx, tc.Name)
		f.ensureBatchAssistant()
		f.Msgs = append(f.Msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
		a.publishEvent(events.EventToolCall, map[string]interface{}{
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

	if pluginName != "" && !a.pluginHealth.isHealthy(pluginName) {
		result := fmt.Sprintf("插件 %s 处于崩溃状态，已跳过执行，等待自动恢复重载", pluginName)
		log.Printf("[agent] skip tool %s: plugin %s unhealthy", tc.Name, pluginName)
		f.ensureBatchAssistant()
		f.Msgs = append(f.Msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
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

// stepToolExec 执行工具。**临界区**：见设计文档 §4.3。
func (a *Agent) stepToolExec(f *TaskFrame) stepOutcome {
	// 执行工具前先解析本轮场景：写侧要用它给记忆自动挂场景（主动+被动两条路），
	// 而工具步不一定走到下面的召回分支，所以不能等那里再解析。
	turn := a.resolveTurnScenes(f, f.CurTool.Name)
	outcome := a.executeToolCallOutcome(f.CurTool, f.OutputChannel, turn.Keys...)
	result := outcome.Text
	f.CurResult = result
	f.CurRaw = outcome.Raw
	f.ToolResults = append(f.ToolResults, ToolResultItem{Name: f.CurTool.Name, Output: result})
	// ★ 工具输出可能含服务端回显的凭据（实测场景：enc_shell 跑 curl 拿 200，
	// 响应体里带着上游 key 的完整值）。这一行以前原样打印。
	log.Printf("[agent] tool %s result: %s", f.CurTool.Name,
		scrubForLog(truncateStr(result, 100), "tool_result:"+f.CurTool.Name))

	// Success 此前是**唯一**赋值点且硬编码 true ⇒ 该字段恒真、结构上不可能为
	// false。工具失败是以 nil error + 错误**值**返回的，所以判据必须看返回值。
	// ⚠️ isToolError 必须同时覆盖存量插件的两种失败约定与「成功不误判」，
	// 否则升级会把存量插件的成功判成失败（见 toolerror_test.go）。
	f.toolCtxFor(f.ToolIdx).ToolResults = []sdk.ToolResult{{
		CallID: f.CurTool.ID, Name: f.CurTool.Name, Plugin: f.CurToolPlugin,
		Success: !isToolError(outcome.Raw), Result: outcome.Raw,
	}}
	f.Step = StepToolAfter
	return outcomeContinue
}

// stepToolAfter 是工具执行后的全部后处理（阶段、裁剪、消息与事件）。
func (a *Agent) stepToolAfter(f *TaskFrame) stepOutcome {
	tc := f.CurTool
	pluginName := f.CurToolPlugin
	result := f.CurResult

	tctx := f.toolCtxFor(f.ToolIdx)
	a.runStage(sdk.StageAfterToolcall, tctx)
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
			result = toolErrorText(f.CurTool.Name, r)
			if !isToolError(r) {
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
	if def := a.stageHost.ToolDef(tc.Name); def != nil {
		needPrune := def.ContextPolicy == sdk.ContextPolicyPrune
		needRecall := def.RecallPolicy == sdk.RecallPolicyAuto
		if needPrune || needRecall {
			query := a.toolOutputForQuery(tc.Name, result)
			// 工具路召回的场景有两个来源：本轮输入的场面（如 chan:qq）
			// 与这一步工具本身（如 tool:qq_get_message）。带上工具场景，
			// 才能让「凡是要回 QQ 消息」这类规则在该步被取回。
			// 召回用两条路的并集：声明场景（注入点/通道/工具）+ 涌现场景
			scenes := a.sceneKeysFor(f.Evt, tc.Name)
			turn := a.resolveTurnScenes(f, tc.Name)
			scenes = mergeSceneKeys(scenes, turn.Keys)
			recallText = a.memoryPass(query, "tool:"+tc.Name, needPrune, needRecall, scenes).RecallText
		}
	}

	f.ensureBatchAssistant()

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
			a.stageMediaBlocks(a.captureBlockMedia(blocks, tc.Name)...)

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

	// 工具结果大小统计（方案 B：**只统计不裁剪**）。
	//
	// 为什么在这里而不是 stepToolExec：那里拿到的 outcome.Text 尚未经
	// after_toolcall 阶段改写，而模型最终看到的是**这里**的内容。
	// ⚠️ 刻意不截断 —— 截断会让模型基于残缺数据下结论，且它不知道被截过
	// （与本仓「静默降级」同族）。处置权交回调度器/上层。
	if a.checkToolResultSize(tc.Name, toolMsg.Content) {
		f.noteOversizeTool(tc.Name)
	}
	if mediaMsg != nil {
		// 延迟到批末（见 batchAttachments 的注释）：当场追加会把后续 tool 消息
		// 与 assistant 挤开，上游判 insufficient tool messages。
		f.batchAttachments = append(f.batchAttachments, *mediaMsg)
	}
	// 召回同样延迟到批末：它是 system 消息，同样会断开 tool 配对。
	// （语义与原先一致：同一批内多条召回仍是最后一条生效，因为
	//  appendOrReplaceRecall 本就是替换而非累加。）
	if recallText != "" {
		f.batchRecall = recallText
	}

	a.publishEvent(events.EventToolCall, map[string]interface{}{
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
func (f *TaskFrame) flushBatchAttachments() {
	if len(f.batchAttachments) == 0 && f.batchRecall == "" {
		return
	}
	for _, m := range f.batchAttachments {
		f.Msgs = append(f.Msgs, m)
	}
	f.batchAttachments = nil
	if f.batchRecall != "" {
		f.Msgs = appendOrReplaceRecall(f.Msgs, f.batchRecall)
		f.batchRecall = ""
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
func (f *TaskFrame) ensureBatchAssistant() {
	if f.assistantMsgIdx >= 0 {
		return
	}
	msgContent := ""
	if f.ContentOnce && f.Resp != nil {
		msgContent = f.Resp.Content
		f.ContentOnce = false
	}
	f.assistantMsgIdx = len(f.Msgs)
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

// resolveProviders 按请求模型解析候选 provider（保持原语义）。
func (a *Agent) resolveProviders(req *agentAPI.CompletionRequest) []agentAPI.Provider {
	var providers []agentAPI.Provider
	if a.providerManager != nil {
		var allProviders []agentAPI.Provider
		if req.Model != "" && !strings.EqualFold(req.Model, "AUTO") {
			allProviders = a.providerManager.ResolveForModel(req.Model)
		} else {
			allProviders = a.providerManager.OrderedProviders()
		}
		providers = make([]agentAPI.Provider, 0, len(allProviders))
		for _, p := range allProviders {
			if a.providerManager.IsAvailable(p.Name()) {
				providers = append(providers, p)
			}
		}
	}
	if len(providers) == 0 {
		providers = []agentAPI.Provider{a.provider}
	}
	return providers
}

// callLLMWithFallback 在候选 provider 间回退，并把同源瞬时错误重试一次。
// 逐行等价于原 process() 内的双层循环。
func (a *Agent) callLLMWithFallback(req *agentAPI.CompletionRequest, providers []agentAPI.Provider, channel string) (*agentAPI.CompletionResponse, error) {
	var resp *agentAPI.CompletionResponse
	var llmErr error

	for pi, fbProvider := range providers {
		if pi > 0 {
			log.Printf("[agent] LLM fallback: trying provider %q (fallback #%d/%d)",
				fbProvider.Name(), pi, len(providers)-1)
		}

		// 同源瞬时错误重试：网关瞬断（502/503/504/429/网络抖动）通常秒级恢复，
		// 直接跳下一个 provider（或直接报错）会丢掉本可成功的请求。
		// 凭证错误（401/403）与用户中断不重试。
		const maxAttempts = 2
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if attempt > 1 {
				log.Printf("[agent] provider %q transient failure, retry %d/%d in 2s: %v",
					fbProvider.Name(), attempt, maxAttempts, llmErr)
				select {
				case <-time.After(2 * time.Second):
				case <-a.ctx.Done():
					llmErr = a.ctx.Err()
				}
				if llmErr == nil || errors.Is(llmErr, context.Canceled) || errors.Is(llmErr, context.DeadlineExceeded) {
					break
				}
			}

			// opt-in 提示词抓包（HOMED_PROMPT_DUMP）：见 promptdump.go。
			dumpPrompt(channel, req)

			fCtx, fCancel := context.WithCancel(a.ctx)
			a.llmMu.Lock()
			a.cancelLLM = fCancel
			a.llmMu.Unlock()

			resp, llmErr = chatStreamWithFallback(fCtx, fbProvider, req, a, channel)

			a.llmMu.Lock()
			a.cancelLLM = nil
			a.llmMu.Unlock()
			fCancel()

			if llmErr == nil {
				a.providerManager.ResetAvailability(fbProvider.Name())
				if fbProvider != a.provider {
					a.provider = fbProvider
					log.Printf("[agent] switched active provider to %q after fallback", fbProvider.Name())
				}
				break
			}

			// 用户中断：立即终止，不重试也不换 provider
			if errors.Is(llmErr, context.Canceled) {
				break
			}
			// 凭证错误：重试无意义，跳出重试循环进入 provider 标记/切换
			var pe *agentAPI.ProviderError
			if errors.As(llmErr, &pe) && (pe.StatusCode == 401 || pe.StatusCode == 403) {
				break
			}
			// 其余错误（含 5xx/429/网络）：还有重试机会则继续，否则跳出
		}

		if llmErr == nil {
			break
		}
		if errors.Is(llmErr, context.Canceled) {
			break
		}
		var pe *agentAPI.ProviderError
		if errors.As(llmErr, &pe) && (pe.StatusCode == 401 || pe.StatusCode == 403) {
			a.providerManager.ReportStatus(fbProvider.Name(), pe.StatusCode)
			log.Printf("[agent] provider %q marked unavailable (HTTP %d)", fbProvider.Name(), pe.StatusCode)
		} else {
			a.providerManager.MarkUnavailable(fbProvider.Name())
		}
		log.Printf("[agent] provider %q failed: %v", fbProvider.Name(), llmErr)
	}

	return resp, llmErr
}

// buildToolContexts 为本批每个工具预建一份独立的 StageContext。
//
// Extra 必须**逐份复制**而不是共享同一个 map：Extra 会被 stage handler 写
// （例如插件注入 media_blocks），共享即竞争。逐份浅拷贝即可——里面的值
// （string / []agentAPI.ContentBlock）本身是只读的。
func (f *TaskFrame) buildToolContexts() {
	base := f.StageCtx
	f.toolCtxs = make([]sdk.StageContext, len(f.PendingTools))
	for i := range f.PendingTools {
		f.toolCtxs[i] = sdk.StageContext{
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
		stampComeFrom(f.toolCtxs[i].Extra, comeFromUser)
		// Phase 由 runStage 每次调用时设置，此处不预置。
	}
}

// toolCtxFor 返回第 i 个工具的 StageContext；越界或未建时回落到 f.StageCtx，
// 保证调用方不必判空（测试替身等未走 buildToolContexts 的路径）。
func (f *TaskFrame) toolCtxFor(i int) *sdk.StageContext {
	if i >= 0 && i < len(f.toolCtxs) {
		return &f.toolCtxs[i]
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
