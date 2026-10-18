package core

import (
	"fmt"
	"log"
	"strings"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	"github.com/JianFeeeee/HomeAgent/internal/security"
)

// executeOutputSendTool 执行一次 output_send__{通道}。
//
// turnExtra 是**本轮输入事件的非保留 payload 字段**（由调用方从帧上取）。
//
// ## 为什么需要它（2026-10-18）
//
// `before_output` 被两个语义共用：
//
//	emitResponse（eventloop.go）  = 一轮的回复，turn 边界
//	本函数（output.go）           = 单次 output_send__* 的工具输出
//
// **真正把消息发出去的是后者**。而它原先构造 stageCtx 时连 Extra 都没初始化
// （实测日志 `extra_keys=0`），插件在这里读不到任何本轮身份 ——
// QQ 的「对方正在输入中」因此一次都没能触发。
//
// 通道插件在这个时点需要知道"这条消息是发给谁的"，所以把本轮 payload
// 透传进来（与 emitResponse 用同一套 Extra 语义）。
func (a *Agent) executeOutputSendTool(tc agentAPI.ToolCall) string {
	channel := strings.TrimPrefix(tc.Name, "output_send__")
	payload, _ := tc.Arguments["payload"].(string)
	rawType, _ := tc.Arguments["type"].(string)
	if channel == "" || payload == "" {
		return "工具名称格式: output_send__{channel}，payload 不能为空"
	}
	// type 缺省按 text 处理：绝大多数输出就是文本，让模型为"省略一个默认值"付一次
	// 失败重试没有意义（判据该拦的是"不知道发什么"，不是"没写众所周知的默认值"）。
	if rawType == "" {
		rawType = "text"
	}
	// 授权闸（纵深防御）：模型可能凭名字直接调未授权的输出门。
	if !a.IsOutputAllowed(channel) {
		return fmt.Sprintf("通道 [%s] 未授权给本 agent。可用通道见 output_list_channels", channel)
	}

	meta, _ := tc.Arguments["meta"].(string)

	caps := a.io.GetChannelCapabilities(channel)
	if caps == 0 {
		return fmt.Sprintf("通道 [%s] 不存在或不可用。可用输出工具列表见 output_list_channels", channel)
	}
	switch rawType {
	case "text":
		if !caps.Supports(agentIO.CapText) {
			return fmt.Sprintf("通道 [%s] 不支持文本输出（能力: %s）", channel, caps.String())
		}
	case "voice", "audio":
		if !caps.Supports(agentIO.CapAudio) {
			return fmt.Sprintf("通道 [%s] 不支持语音输出（能力: %s）", channel, caps.String())
		}
	case "image":
		if !caps.Supports(agentIO.CapImage) {
			return fmt.Sprintf("通道 [%s] 不支持图片输出（能力: %s）", channel, caps.String())
		}
	case "file":
		if !caps.Supports(agentIO.CapFile) {
			return fmt.Sprintf("通道 [%s] 不支持文件输出（能力: %s）", channel, caps.String())
		}
	}

	args := map[string]interface{}{
		"payload": payload,
		"type":    rawType,
	}
	if meta != "" {
		args["meta"] = meta
	}

	// ★ Extra 必须有 output_channel：本函数是"发往某通道"，而这个键是
	// 插件在该时点唯一能据以判断目标通道的凭据。
	// 原先这里连 Extra 都没初始化（nil）——插件读不到任何上下文。
	// Extra 必须带三个键，缺一不可：
	//
	//	output_channel  目标通道（插件据此判断"是不是发给我的"）
	//	output_meta     本次发送的 meta 原文（QQ 的 user_id/group_id 在里面）
	//	output_type     输出类型（text/image/file…）
	//
	// ★ output_meta 是关键（2026-10-18）：本函数是**真正把消息发出去**的
	// 时点，通道插件在这里需要知道"这条发给谁"才能驱动「对方正在输入中」。
	// 原先 stageCtx 连 Extra 都没初始化（nil）⇒ 实测 `extra_keys=0`
	// ⇒ QQ 的打字指示器一次都没能触发。
	stageCtx := &sdk.StageContext{
		FinalText: payload,
		Phase:     sdk.StageBeforeOutput,
		Extra: map[string]interface{}{
			"output_channel": channel,
			"output_meta":    meta,
			"output_type":    rawType,
		},
	}
	a.runStage(sdk.StageBeforeOutput, stageCtx)
	if stageCtx.Response != nil {
		return fmt.Sprintf("输出被插件拦截: %s", *stageCtx.Response)
	}
	if stageCtx.FinalText == "" {
		return "输出被插件清空"
	}
	args["payload"] = stageCtx.FinalText

	if dev := a.io.GetDevice(channel); dev != nil {
		// ★ 安全模块入向回填：把 @{别名} 换回明文。
		//
		// 位置在**插件 stage 之后、设备 Execute 之前**是有讲究的：
		//   · 放在 stage 之前 ⇒ 插件会看到明文（不可，插件在信任边界外）
		//   · 放在 Execute 之后 ⇒ 设备已收到带别名的载荷（功能坏了）
		// 所以只能夹在中间：插件看不到，设备拿到真的。
		//
		// 注意单独处理 payload：args 里的其它字段（channel 等）不需要回填。
		if s := security.Global(); s != nil {
			if filled, n, used := s.Fill(args["payload"].(string)); n > 0 {
				args["payload"] = filled
				log.Printf("[security] 通道 %s 载荷回填 %d 处（别名 %v）", channel, n, used)
			}
		}
		result, err := dev.Execute("output", args)
		// ★ after_output：消息**已真正送达**之后触发一次。
		//
		// 与前面的 before_output 成对（同一份 Extra）。通道插件用它收尾
		// 打字指示器之类"发出前开始、发出后结束"的状态。
		//
		// 为什么必须在这里：output.go 这条路径此前**没有** after_output，
		// 而它才是"消息真的发出去了"的时刻。缺了它，插件只能在轮次边界
		// （emitResponse 的 after_output）收尾 —— 而一轮可能发多条消息，
		// 中间那些永远收不了尾（实测：状态一直挂着）。
		stageCtx.Phase = sdk.StageAfterOutput
		a.runStage(sdk.StageAfterOutput, stageCtx)
		if err != nil {
			return fmt.Sprintf("通过 [%s] 通道发送失败: %v", channel, err)
		}
		// 通道可能回报「未确认」（已提交但超时未拿到发送确认）——此时不能对模型
		// 谎报「已发送」，否则模型不会重试/核实。
		if m, ok := result.(map[string]interface{}); ok {
			if status, _ := m["status"].(string); status == "unconfirmed" || status == "queued" {
				note, _ := m["note"].(string)
				if note == "" {
					note = "发送已提交但未收到通道确认，结果未知"
				}
				return fmt.Sprintf("[%s] 通道发送结果未确认：%s", channel, note)
			}
		}
		// 成功回执：只返回极简标记，不回传完整插件响应。
		// 「已通过 [qq] 通道发送: map[status:sent message_id:xxx]」这类富回执
		// 会驱动模型继续调用 output_send（回声效应），是 output loop 的根源之一。
		return "ok"
	}

	a.io.EmitTextTo("agent_io", channel, payload)
	return fmt.Sprintf("已通过 [%s] 通道发送", channel)
}

func (a *Agent) executeOutputSendHelp(tc agentAPI.ToolCall) string {
	suffix := strings.TrimPrefix(tc.Name, "output_send__")
	channel := strings.TrimSuffix(suffix, "_help")
	if channel == "" {
		return "工具名称格式: output_send__{channel}_help"
	}

	dev := a.io.GetDevice(channel)
	if dev == nil {
		return fmt.Sprintf("通道 [%s] 不存在", channel)
	}

	caps := a.io.GetChannelCapabilities(channel)
	capStr := "无"
	if caps != 0 {
		capStr = caps.String()
	}

	desc := dev.Description()
	if desc == "" {
		desc = channel + " 输出通道"
	}

	return fmt.Sprintf(`通道 [%s]
描述: %s
能力: %s

【参数说明】
payload — 消息载荷（必填）。type=text 时直接填文字，type=file/image 时填 URL 或路径
meta    — JSON 对象，发送所需的元数据（可选，取决于通道是否需要路由信息）
type    — 载荷类型（必填），枚举值见下方

【type 枚举】
- text    — 文本消息
- voice   — 语音消息
- image   — 图片
- file    — 文件

【meta JSON 格式】
由通道描述定义，通常包含：
- "group_id"  群号（群聊时必填）
- "user_id"   目标用户 QQ 号（私聊时必填）
- "reply_to"  回复某条消息 ID（可选）

示例: output_send__%s(payload="你好", meta="{\"group_id\": 123456789}", type="text")`, channel, desc, capStr, channel)
}

func (a *Agent) executeOutputListChannels() string {
	channels := a.io.ListChannels()
	if len(channels) == 0 {
		return "没有可用通道"
	}
	var parts []string
	parts = append(parts, "可用通道:")
	for _, ch := range channels {
		if ch.OutputCaps == 0 {
			continue
		}
		if !a.IsOutputAllowed(ch.Name) {
			continue
		}
		line := fmt.Sprintf("  - %s: [%s] %s", ch.Name, ch.OutputCaps.String(), ch.Description)
		if t, ok := a.ResolveOutputTarget(ch.Name); ok {
			line += fmt.Sprintf("（目标: %s / inputch %s）", orDash(t.AgentID), orDash(t.InputCh))
		}
		parts = append(parts, line)
		for _, t := range ch.Tools {
			parts = append(parts, fmt.Sprintf("     工具: %s - %s", t.Name, t.Description))
		}
	}
	return strings.Join(parts, "\n")
}
