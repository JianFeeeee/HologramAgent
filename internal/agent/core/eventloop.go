package core

import (
	"fmt"
	"log"
	"runtime/debug"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// eventLoop 已由 scheduler.go 的 schedulerLoop 取代（M2）。
//
// 原实现直接在 select 里处理 inputCh/selfInputCh，没有任何可枚举的队列、
// 无法承载优先级与抢占；现在任务先入就绪队列，由选择函数 pickTaskIndex 决定下一个。
// 兼容性说明：M2 全部任务为 LevelBackground，因此行为等价于原先的 FIFO。

func (a *Agent) interceptLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] interceptLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.interceptLoop()
		}
	}()
	for {
		select {
		case evt := <-a.io.InputInterruptChan():
			text, _ := evt.Payload["content"].(string)
			stop, _ := evt.Payload["stop"].(bool)
			if text == "" && !stop {
				// 没有内容也不是停止指令：没有可处理的东西（旧行为）。
				//
				// 注意：**不能**把“空内容”一律当成空操作。客户端停止按钮
				// 本来就不带消息（/chat/interrupt 收 body 空的 {}），
				// 旧代码在这里 continue 掉，于是停止按钮毫无反应，
				// 而且接口还回 200 骗调用方——已实测：HTTP 200 但内核零日志、
				// 生成继续跑到自然结束。
				continue
			}
			if stop {
				// 停止：①立即结束当前 LLM 推理；②登记短路配额。
				//
				// 注意这里**只 arm、不 take**：takeStop 必须由 stepLLM 去消费，
				// 它才是决定“取消后不重跑”的那个人。曾经写成
				//
				//	if n := armStop(); n > 0 || takeStop() { ... }
				//
				// 这个 `||` 在 queued=0 时会短路到 takeStop()，把标记先消费掉，
				// 于是 stepLLM 永远看不到它 → 取消后照样重跑一轮。
				// 实测：停止被正确记录（`stop requested ... queued=0`）但生成仍跑到自然结束。
				// pending 必须算上**停在输入 channel 里**的那一段：停止时
				// 调度器多在半路忙当前任务，其余消息还没被 pumpInbox 搬进队列，
				// 只数 sched.queue 会得到 0，配额随之失效（实测过）。
				pending := 0
				if a.io != nil {
					pending = a.io.PendingInputs()
				}
				n := a.sched.armStop(pending)
				log.Printf("[agent] stop requested by %s/%s (queued=%d will be short-circuited at pre-action)",
					evt.Source, evt.OutputChannel, n)
				a.cancelCurrentLLM()
				if text == "" {
					// 纯停止：不进中断队列、不产生新任务。旧实现把空停止当成一条
					// 中断入队，取消后会以空内容重跑一轮，停下之后又“活着”。
					continue
				}
				// 带注释的停止（/stop 说句话）：注释本身仍作为中断处理，
				// 走下面的正常路径——用户想看模型对被停下话题的回应。
			}
			log.Printf("[agent] interrupt from %s/%s: %s", evt.Source, evt.OutputChannel, truncateStr(text, 80))

			clone := &agentIO.InputEvent{
				RequestID:     evt.RequestID,
				Source:        evt.Source,
				Type:          evt.Type,
				Payload:       map[string]interface{}{},
				OutputChannel: evt.OutputChannel,
			}
			for k, v := range evt.Payload {
				clone.Payload[k] = v
			}
			clone.Payload["interrupt"] = true
			clone.Payload["interrupt_source"] = evt.Source
			clone.Payload["interrupt_channel"] = evt.OutputChannel

			// 决策交给调度器：requestPreempt 总会登记中断（进中断队列或 immediate，
			// 因而不会丢），仅当它会真抢占时才告诉我“该取消可取消的步骤”。
			// 本 goroutine 不碰任何帧——只写中断队列与让位信号。
			//
			// 级别由来源声明（InjectOptions.Priority → payload["priority"]）；
			// 未声明一律 L1。L4（“立即打断”）只有内核级插件能声明，
			// 外部插件即便报了 L4 也会被夹到 L3；内核自身另有 raiseKernelInterrupt。
			level := interruptLevel(evt, a.isKernelLevelSource(evt.Source))
			if a.sched.requestPreempt(clone, level) {
				a.cancelCurrentLLM()
			}

		case <-a.ctx.Done():
			return
		}
	}
}

// cancelCurrentLLM 取消正在进行的 LLM 请求（若有）。
//
// 只有 LLM 流式步骤是可取消的；工具 RPC / ONNX / CAS 在 v1 是临界区，
// 取消对它们无效——让位信号会等它们自然结束后的安全点（设计文档 D2）。
func (a *Agent) cancelCurrentLLM() {
	a.llmMu.Lock()
	cancel := a.cancelLLM
	a.llmMu.Unlock()
	if cancel != nil {
		cancel()
		log.Printf("[agent] LLM request cancelled by preemption")
	}
}

// channelConsolidation 标记记忆整理类自输入：无记忆路径处理，
// 不写入对话上下文、不向任何输出通道 emit 响应。
const channelConsolidation = "_consolidation_"

// selfInputMsg 自循环输入消息。channel 决定处理路径：
//   - channelConsolidation：记忆整理，无记忆（不污染上下文/知识库）
//   - 其他值（如 "cli"、"webui"）：正常输入路径，写入上下文并 emit 响应
//     （典型场景：子 Agent 完成通知，需让父 Agent 感知并可回复用户）
type selfInputMsg struct {
	text    string
	channel string
}

// selfEvent 把内核自循环消息归一成输入事件。
func selfEvent(msg selfInputMsg) *agentIO.InputEvent {
	if msg.channel == "" {
		msg.channel = channelConsolidation // 兼容空值：默认走整理路径
	}
	return &agentIO.InputEvent{
		Source:        "system",
		Type:          "text",
		Payload:       map[string]interface{}{"content": msg.text},
		OutputChannel: msg.channel,
	}
}

func (a *Agent) handleSelfInput(msg selfInputMsg) {
	_, _ = a.runInputTask(selfEvent(msg))
}

func (a *Agent) handleInput(evt *agentIO.InputEvent) {
	switch evt.Type {
	case "text", "image", "audio":
		_, _ = a.runInputTask(evt)

	case "event":
		log.Printf("[agent] event from %s: %v", evt.Source, evt.Payload)

	case "command":
		cmd, _ := evt.Payload["command"].(string)
		log.Printf("[agent] command from %s: %s", evt.Source, cmd)

	default:
		log.Printf("[agent] unknown event type from %s: %s", evt.Source, evt.Type)
	}
}

// inputPayload 是一次输入在「模态」这个维度上的全部内容。
//
// 拆出这个结构，是为了让 processInput 只有一条主干：模态不再决定走哪个函数，
// 只决定这里的字段填不填。此前 text 与 image/audio 各有一个 process 函数，
// 媒体那条缺了去重、no_memory、通道 Cleaner、中断语义、EventRawInput 五项——
// 不是因为媒体不需要，而是复制粘贴之后文本那条继续演进、媒体那条没跟上。
type inputPayload struct {
	// text 是进 LLM 与记忆的文本。纯媒体输入时它是 mediaToBlocks 给的 alt 文案。
	text string
	// blocks 非空表示本轮带多模态内容，随当前轮的 message 一起发给模型。
	blocks []agentAPI.ContentBlock
	// mediaType 供插件在 stage 里判断本轮媒体的模态。
	mediaType string
	// captureTool 是媒体落进 CAS 时记录的来源标签。
	captureTool string
	// airInterrupt 表示这是一次**空中断**：内核发的 L4，没有正文。
	//
	// 它的唯一用途是触发一次语义相关性裁剪（contextoverflow 的原始设计）。
	// 空正文的普通输入不是空中断（见 isAirInterrupt）。
	airInterrupt bool
}

// resolveInput 把 InputEvent 归一成 inputPayload。
//
// 三种来源在这里合流：
//  1. evt.Type 是 image/audio —— 用户直接发的媒体，payload 里是 data/url；
//  2. evt.Type 是 text 且 payload 带 media_blocks —— 插件经 IOInjector 的
//     InjectInputMedia / InjectInputMediaSync / InjectInterruptMedia 注入的
//     媒体，块已经是成品；
//  3. 纯文本。
//
// 第 2 种此前无处可去：注入方把块放进 payload，而文本路径不看这个键，
// 于是插件注入的媒体到 payload 就断了，且不报错。
func (a *Agent) resolveInput(evt *agentIO.InputEvent) (inputPayload, bool) {
	switch evt.Type {
	case "image", "audio":
		blocks, alt := a.mediaToBlocks(evt.Payload, evt.Type, evt.Source)
		return inputPayload{
			text:        alt,
			blocks:      blocks,
			mediaType:   evt.Type,
			captureTool: "input_" + evt.Type,
		}, true
	}

	text, _ := evt.Payload["content"].(string)
	blocks, mediaType := injectedBlocks(evt.Payload)

	// ★ 空中断（kernel/overflow）：**没有正文也要算有效输入**。
	//
	// 这是 contextoverflow 的原始设计：L4 中断本身不携带任何消息，
	// 它唯一的用途是"把当前任务抢占下来，让语义相关性裁剪发生"。
	// 空 content 在这里曾被判成无效输入直接丢弃 —— 于是那个 L4 变成了
	// 一条什么也不做的空转任务。
	//
	// 判定条件刻意窄：必须是内核发出的中断，且确实无正文、无媒体。
	// 普通输入的空内容仍然算无效（否则客户端空包会白白唤醒一轮）。
	if text == "" && len(blocks) == 0 && isAirInterrupt(evt) {
		return inputPayload{text: "", airInterrupt: true}, true
	}
	// 文本与媒体都空才算无效输入：只带图不带字是合法的（插件注入常这样）。
	if text == "" && len(blocks) == 0 {
		return inputPayload{}, false
	}
	return inputPayload{
		text:        text,
		blocks:      blocks,
		mediaType:   mediaType,
		captureTool: "inject_" + evt.Source,
	}, true
}

// injectedBlocks 取出 payload 里插件注入的多模态块。
//
// 两种静态类型都要认：内核内部注入直接给 []agentAPI.ContentBlock，
// 而经公共 SDK 的 IOInjector 过来的是 []pubsdk.ContentBlock。两者字段完全一致，
// 但 Go 不会自动转换，只认一种的后果是另一种被静默丢弃。
func injectedBlocks(payload map[string]interface{}) ([]agentAPI.ContentBlock, string) {
	var blocks []agentAPI.ContentBlock
	switch v := payload["media_blocks"].(type) {
	case []agentAPI.ContentBlock:
		blocks = v
	case []pubsdk.ContentBlock:
		blocks = make([]agentAPI.ContentBlock, 0, len(v))
		for _, b := range v {
			nb := agentAPI.ContentBlock{Type: b.Type, Text: b.Text}
			if b.ImageURL != nil {
				nb.ImageURL = &agentAPI.ImageURL{URL: b.ImageURL.URL, Detail: b.ImageURL.Detail}
			}
			if b.AudioURL != nil {
				nb.AudioURL = &agentAPI.AudioURL{URL: b.AudioURL.URL}
			}
			blocks = append(blocks, nb)
		}
	}
	if len(blocks) == 0 {
		return nil, ""
	}
	// 模态由块自身判定，注入方不必额外声明。图优先：一次注入里图片是主体。
	mediaType := ""
	for _, b := range blocks {
		if b.ImageURL != nil {
			return blocks, "image"
		}
		if b.AudioURL != nil {
			mediaType = "audio"
		}
	}
	return blocks, mediaType
}

func (a *Agent) mediaToBlocks(payload map[string]interface{}, mediaType string, source string) ([]agentAPI.ContentBlock, string) {
	data, _ := payload["data"].(string)
	mime, _ := payload["mime"].(string)
	url, _ := payload["url"].(string)
	alt, _ := payload["alt"].(string)
	if alt == "" {
		if source == "" {
			source = "unknown"
		}
		alt = fmt.Sprintf("[received %s from %s]", mediaType, source)
	}

	var blocks []agentAPI.ContentBlock

	desc := ""
	switch mediaType {
	case "image":
		desc = a.inputCfg.Image.DescribePrompt
		if desc == "" {
			// ★★ 2026-10-06：原文写的是「请使用 describe_image 工具查看详情」，
			// 但下面已经**内联**了 image_url block —— 模型直接能看。
			// 更糟的是：该工具已于本日移出为内置插件 multimodal，
			// 它需要 path 或 digest，而用户直传的图片两者都没有 ⇒
			// 模型照着提示词调它，必然拿到「没有可处理的媒体」错误。
			desc = fmt.Sprintf("An image arrived from %s (attached to this message; look at it directly).", source)
		}
	case "audio":
		desc = a.inputCfg.Audio.DescribePrompt
		if desc == "" {
			// 同上：audio_url block 已内联。不要指示调 transcribe_audio
			// （它现在需要 path/digest）；若模型本身不处理音频，
			// 应如实告知用户，而不是调一个注定失败的工具。
			desc = fmt.Sprintf("从 %s 收到了一段音频（已随本消息附上）。"+
				"若你能直接处理音频请转写；若不能，请如实告知，不要编造内容。", source)
		}
	}
	blocks = append(blocks, agentAPI.ContentBlock{Type: "text", Text: desc})

	if data != "" || url != "" {
		imgURL := url
		if data != "" {
			if mime == "" {
				mime = "image/png"
			}
			imgURL = "data:" + mime + ";base64," + data
		}
		if mediaType == "image" {
			blocks = append(blocks, agentAPI.ContentBlock{
				Type:     "image_url",
				ImageURL: &agentAPI.ImageURL{URL: imgURL, Detail: "auto"},
			})
		} else if mediaType == "audio" {
			blocks = append(blocks, agentAPI.ContentBlock{
				Type:     "audio_url",
				AudioURL: &agentAPI.AudioURL{URL: imgURL},
			})
		}
	}

	return blocks, alt
}

// emitSkippedReply 给被跳过任务的调用方一个终态。
//
// 为什么要单独一条路径而不是复用 emitResponse：跳过意味着“我们没有处理这条输入”，
// 不应对外发 agent_output 事件（否则 WebUI 聊天记录会凭空多出一条空消息），
// 但必须写 ResponseCh——否则 cli/clawhub 这类无超时的同步注入会永久挂起。
//
// ❗异步来源（qq / wechat / rss 等）**没有 ResponseCh**，于是这里以前是直接 return。
// 后果是任务被丢弃时**完全无声**：用户什么都没收到、日志里也没痕迹，
// 他只会以为消息丢了。转投子被回收/销毁时队列里的积压正落在这个盲区里
// （父可随时对子 reclaim/destroy，而子手上可能还握着几条 QQ 消息）。
// 现在至少留一条带来源与通道的日志，让“这条消息为什么没回”可被追溯。
//
// 非阻塞写：ResponseCh 由同步调用方以 cap=1 创建，调用方超时离开后仍可写入。
func (a *Agent) emitSkippedReply(evt *agentIO.InputEvent, reason string) {
	if evt == nil {
		return
	}
	if evt.ResponseCh == nil {
		// 无可回执的通道：不静默。异步来源本就靠 agent 主动 output_send，
		// 丢弃后没有任何东西会告诉用户，因此这条日志是唯一的线索。
		log.Printf("[agent] %s: dropping input on channel without receipt support (source=%s channel=%s request=%s reason=%s)",
			a.id, evt.Source, evt.OutputChannel, evt.RequestID, reason)
		return
	}
	ch := evt.OutputChannel
	if ch == "" {
		ch = evt.Source
	}
	payload := map[string]interface{}{
		"content":    "",
		"request_id": evt.RequestID,
		"skipped":    true,
		"reason":     reason,
	}
	select {
	case evt.ResponseCh <- &agentIO.OutputEvent{
		RequestID:     evt.RequestID,
		Target:        evt.Source,
		Type:          "text",
		Payload:       payload,
		Done:          true,
		OutputChannel: ch,
	}:
	default:
	}
}

// emitResponse 发送终态回执（不变量 I5：每任务恰一次）。
//
// turnUsage 是本轮任务内全部 LLM 调用的用量合计，由调用方从 TaskFrame 传入。
//
// 为何要它：同步注入方（WebUI 的 /v1/chat/completions、cli.sock、
// clawhubadapter）的回包就出自这里，而此前这里**只从插件侧**读 TokenUsage
// （下面新建的 stageCtx），agent 自己算出来的用量从未传进去 ⇒ 回包没有 usage。
// 实测过：部署实例的 /v1/chat/completions 回包里 "usage" 字段根本不存在。
//
// 口径：按 OpenAI 语义报**本次请求**（turnUsage），不是会话累计 ——
// 会话累计在链事件里的 usage_session（见 task.go 的记账处），两者不可互换，
// 否则第二次请求会报出翻倍的数字。
func (a *Agent) emitResponse(evt *agentIO.InputEvent, response string, turnUsage agentAPI.TokenUsage) {
	// 通道一律从**输入事件**推导（内核不持有"当前通道"）。
	ch := outputChannelOf(evt)
	stageCtx := &sdk.StageContext{
		FinalText: response,
		Phase:     sdk.StageBeforeOutput,
		Extra:     map[string]interface{}{"output_channel": ch},
	}
	// ★ 把输入事件的 payload **原样透传**给 before_output / after_output。
	//
	// ## 为什么必须在这里透传（2026-10-18）
	//
	// 本函数**新建**了 stageCtx，而 `on_input` 的 stageCtx 是局部变量
	// （stageCtxFromInput）—— 两者**不共享 Extra**。
	//
	// 后果：插件在 before_output 里只能读全局状态猜“这轮是谁引起的”，
	// 并发时会串号（QQ 的「对方正在输入中」就踩到这个：A 的指示打到 B 身上）。
	//
	// ## 为什么是“透传 payload”而不是新造一个字段
	//
	// `InputEvent.Payload` 本就是通道送入内核的请求体，身份信息本就该在那里。
	// 我第一版加了 `InjectOptions.Meta` 专用的透传键 —— 那是把同一件事做了两遍，
	// 而且只服务注入路径，HTTP/CLI/ACP 无法共用。
	//
	// 现在：插件用 `InjectOptions.Payload` 声明业务字段（内核不解释），
	// 这里把它连同 `input_source` 一起带到 stageCtx.Extra。
	// 任何注入路径都能用同一套机制。
	if evt != nil {
		stageCtx.Extra["input_source"] = evt.Source
		for k, v := range evt.Payload {
			// 内核语义键不往 Extra 里塞：它们的归宿是内核自己的处理逻辑，
			// 不是插件可见的上下文（塞了反而干扰插件读 Extra）。
			if agentIO.IsReservedPayloadKey(k) {
				continue
			}
			stageCtx.Extra[k] = v
		}
	}
	a.runStage(sdk.StageBeforeOutput, stageCtx)
	response = stageCtx.FinalText

	payload := map[string]interface{}{
		"content":    response,
		"request_id": evt.RequestID,
	}
	if stageCtx.ReasoningContent != "" {
		payload["reasoning_content"] = stageCtx.ReasoningContent
	}
	// 用量优先级：插件显式设置的赢（那是既有契约，也是此前唯一能拿到 usage 的路径），
	// 插件没设则回落到 agent 自己的本轮记账。
	//
	// ⚠️ 两条路都必须归一成 map[string]interface{}：消费方 handler_openai.go 用
	// `.(map[string]interface{})` 取值，而 Go 的 map 类型断言是**精确匹配** ——
	// 插件给的 map[string]int 会断言失败，usage 在回包里静默变 nil，两边都不报错。
	// （这正是链事件里那个 "发了但没人收到" 的同款坑，已实证。）
	// SDK 侧 TokenUsage 的类型仍是 map[string]int（公开接口不动），
	// 转换只发生在这一个出口。
	switch {
	case stageCtx.TokenUsage != nil:
		payload["usage"] = usageMapFromInts(stageCtx.TokenUsage)
	case !turnUsage.IsZero():
		payload["usage"] = turnUsageMap(turnUsage)
	}
	if evt.ResponseCh != nil {
		// 非阻塞写：ResponseCh 由同步调用方以 cap=1 创建。按不变量 I5（每任务恰一次
		// 终态）这里永远写得进去；但一旦哪天写出第二次，阻塞会卡死**调度器 goroutine**
		// （整个 agent 停摆），而丢弃只是丢一条回执——与 emitSkippedReply 对称。
		select {
		case evt.ResponseCh <- &agentIO.OutputEvent{
			RequestID:     evt.RequestID,
			Target:        evt.Source,
			Type:          "text",
			Payload:       payload,
			Done:          true,
			OutputChannel: ch,
		}:
		default:
			log.Printf("[agent] ResponseCh full, terminal receipt dropped (request=%s, possible invariant I5 violation)", evt.RequestID)
		}
	}

	out := map[string]interface{}{
		"content": response,
		"channel": ch,
		"source":  evt.Source,
	}
	if stageCtx.ReasoningContent != "" {
		out["reasoning_content"] = stageCtx.ReasoningContent
	}
	a.publishEvent(events.EventAgentOutput, out)
	stageCtx.Phase = sdk.StageAfterOutput
	a.runStage(sdk.StageAfterOutput, stageCtx)
}

// pruneOnInput 按声明的上下文策略裁剪上下文，返回归档的事件数。
//
// 默认**不裁剪**：ContextPolicy 必须在注入点（payload 的 context_policy）
// 或通道定义（ChannelDef.ContextPolicy）上显式声明为 prune 才会裁剪。
//
// 为什么把无条件裁剪改成需声明：裁剪会把低相关事件归档到文档记忆并从上下文里
// 移走，是破坏性的。此前每条输入都裁一次，于是「谁把上下文裁了」在排查时无从
// 得知；而插件注入的内容也会被不相关的内容挤掉。按来源/注入点声明后，触发条件
// 是可枚举、可审计的。
//
// 查询向量取**清洗后**的输入（通道 Cleaner 的输出），与工具侧同一套语义：
// 原始输入里的 ANSI/base64/JSON 包装会把相关性打分带偏，裁掉本该保留的事件。
//
// 实际执行交由 memoryPass（与召回共用入口、query、预算与审计）。
func (a *Agent) pruneOnInput(evt *agentIO.InputEvent, cleanInput string) int {
	if !a.pruneDeclared(evt) {
		return 0
	}
	return a.memoryPass(cleanInput, "input:"+evt.Source, true, false, a.sceneKeysFor(evt, "")).Archived
}

// pruneDeclared 判定这次输入是否显式声明了裁剪。
//
// 优先级：注入点声明的（payload）> 通道声明的（ChannelDef）> 默认不裁剪。
// 注入点是更窄的声明面，同一通道下的不同注入可以有不同意图。
func (a *Agent) pruneDeclared(evt *agentIO.InputEvent) bool {
	if p, ok := evt.Payload["context_policy"].(string); ok && p != "" {
		return p == pubsdk.ContextPolicyPrune
	}
	if a.io != nil {
		if chDef, ok := a.io.GetInputChannelDef(evt.Source); ok {
			return chDef.ContextPolicy == pubsdk.ContextPolicyPrune
		}
	}
	return false
}

// recallDeclared 判定这次输入是否要触发记忆召回（注入）。
//
// 与 pruneDeclared **正交**：prune 管“踢出去”（归档低相关 L0 事件），
// recall 管“取进来”（把 L2/L3 相关记忆注入本轮）。
//
// 默认值与 prune 刻意相反：召回是只读增量、日常对话本就需要，所以**默认 auto**；
// 只有显式声明 recall_policy=none（如中断通知的 meta 文本）才关闭。
// 优先级同 prune：注入点（payload）> 通道（ChannelDef）> 默认 auto。
func (a *Agent) recallDeclared(evt *agentIO.InputEvent) bool {
	if evt == nil {
		return true
	}
	if p, ok := evt.Payload["recall_policy"].(string); ok && p != "" {
		return p != pubsdk.RecallPolicyNone
	}
	if a.io != nil {
		if chDef, ok := a.io.GetInputChannelDef(evt.Source); ok && chDef.RecallPolicy != "" {
			return chDef.RecallPolicy != pubsdk.RecallPolicyNone
		}
	}
	return true
}

// cleanInputFor 解析这条输入在计算层应当使用的清洗文本。
//
// 优先级：注入点声明的 cleaner（payload.cleaner_name，引用某个已注册的通道
// cleaner）> 按 source 查到的通道 cleaner > 原文。
//
// 声明的 cleaner 名字查不到时**记日志并回退**，而不是静默当没声明：
// 注入是 fire-and-forget 的，插件那边看不到错误；至少要在内核日志里留下
// 「你声明的清洗没生效」的痕迹，否则排查时只能看到「记忆里的内容很脏」。
func (a *Agent) cleanInputFor(evt *agentIO.InputEvent, input string) string {
	if a.io == nil {
		return input
	}
	if name, ok := evt.Payload["cleaner_name"].(string); ok && name != "" {
		if chDef, ok := a.io.GetInputChannelDef(name); ok && chDef.Cleaner != nil {
			return chDef.Cleaner(input)
		}
		log.Printf("[agent] injection declared cleaner_name=%q but no Cleaner was registered for that channel, falling back", name)
	}
	if chDef, ok := a.io.GetInputChannelDef(evt.Source); ok && chDef.Cleaner != nil {
		return chDef.Cleaner(input)
	}
	return input
}

// isAirInterrupt 判定一个输入事件是否为「空中断」。
//
// 空中断 = 内核发起、type=interrupt、payload 里没有 content。
// 用途见 contextoverflow 的中断向量（VectorContextOverflow）：
// 它只是一次调度信号，用来把当前任务抢占下来，好在任务边界做语义相关性裁剪。
//
// 为什么必须由内核来源限定：插件也能注入 interrupt，但插件的中断**总是**
// 带正文（"某设备上线了""某任务失败了"）。把它们的空包也当空中断，
// 会让"插件发了个没内容的中断"变成一次无声的裁剪，行为不可预测。
func isAirInterrupt(evt *agentIO.InputEvent) bool {
	if evt == nil || evt.Type != "interrupt" {
		return false
	}
	if v, _ := evt.Payload["kernel"].(bool); !v {
		return false
	}
	text, _ := evt.Payload["content"].(string)
	return text == ""
}
