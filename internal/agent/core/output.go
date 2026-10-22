package core

import (
	"fmt"
	"log"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/internal/security"
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
// outputItem 是一条待发送的输出项。
//
// 内核只承载三个**插件自定义字段**，不懂它们的语义：
//
//	payload  载荷内容（文本 / URL / 路径…由插件解释）
//	type     载荷类型（插件自定义枚举，内核不校验）
//	meta     发送元数据（JSON 字符串，插件解释）
type outputItem struct {
	payload string
	typ     string
	meta    string
}

// parseOutputItems 把工具参数解析成 1..N 条待发送项。
//
// ## 一次调用发多条（2026-10-22）
//
// 模型可以传数组来实现"一条回复里发多条、多种类型"：
//
//	payload: ["收到，我看看", "https://.../a.png", "/tmp/report.pdf"]
//	type:    ["text",        "image",                "file"]
//	meta:    "{\"user_id\":123}"          ← 单值：广播给每一条
//	      或 ["{...}", "{...}", "{...}"]     ← 数组：一一对应
//
// ## 对齐规则（用户拍板）
//
//   - payload 与 type 必须等长；type 省略/单值 ⇒ 全部复用该值（缺省 text）
//   - 长度不符 ⇒ **整 call 拒绝，一条都不发**（不能发一半再报错）
//   - meta 允许单值广播
//
// ## 向后兼容
//
// payload 为字符串时走单条路径，行为与改动前逐字相同。
func parseOutputItems(args map[string]interface{}) ([]outputItem, error) {
	payloads := stringOrArray(args["payload"])
	types := stringOrArray(args["type"])
	metas := stringOrArray(args["meta"])

	if len(payloads) == 0 {
		return nil, fmt.Errorf("payload 不能为空")
	}
	// ★ 空字符串元素**不在这里拒绝**（2026-10-22 实测踩到）。
	//
	// 内核不知道 payload 的语义：它可能是"要发的内容"，也可能是占位 ——
	// 例如 QQ 的 `type=sticker` + `meta.sticker_index=1`，真实 URL 由插件从
	// 收藏表情里取，模型根本不知道该往 payload 填什么。内核在这里判空，
	// 等于用一个它不理解的字段把插件的合法用法挡掉（实测报
	// "payload 不能为空"，而插件侧的放行分支永远走不到）。
	//
	// 正确边界：**参数是否存在归内核管，参数值是否合法归插件管**。
	// 所以只拒绝"整条数组里没有任何一项"这种结构性错误（上面那条），
	// 逐项内容交给插件判断并返回 error。

	// type 对齐：省略 / 单值 ⇒ 广播；否则必须与 payload 等长
	if len(types) == 0 {
		types = []string{"text"}
	}
	if len(types) == 1 && len(payloads) > 1 {
		broadcast := types[0]
		types = make([]string, len(payloads))
		for i := range types {
			types[i] = broadcast
		}
	}
	if len(types) != len(payloads) {
		return nil, fmt.Errorf("payload 有 %d 条，type 有 %d 条 —— 数量必须一致"+
			"（或只给一个 type 表示全部同类型）", len(payloads), len(types))
	}
	for i, ty := range types {
		if strings.TrimSpace(ty) == "" {
			types[i] = "text"
		}
	}

	// meta 对齐：省略 / 单值 ⇒ 广播
	if len(metas) == 1 && len(payloads) > 1 {
		broadcast := metas[0]
		metas = make([]string, len(payloads))
		for i := range metas {
			metas[i] = broadcast
		}
	}
	if len(metas) == 0 {
		metas = make([]string, len(payloads))
	}
	if len(metas) != len(payloads) {
		return nil, fmt.Errorf("payload 有 %d 条，meta 有 %d 条 —— 数量必须一致"+
			"（或只给一个 meta 表示全部同一目标）", len(payloads), len(metas))
	}

	items := make([]outputItem, len(payloads))
	for i := range payloads {
		items[i] = outputItem{payload: payloads[i], typ: types[i], meta: metas[i]}
	}
	return items, nil
}

// stringOrArray 把一个 JSON 参数读成 []string。
//
// 同时接受字符串、字符串数组、以及 []interface{}（encoding/json 的默认形态）。
// 空字符串视为"未提供"（返回 nil），与改动前的 `payload == ""` 判据一致。
func stringOrArray(v interface{}) []string {
	switch t := v.(type) {
	case string:
		// 空字符串**保留**为一个元素，不当"未提供"处理。
		//
		// 区别很重要：`payload: ""` 是"模型明确给了一个空载荷占位"
		// （插件可能从 meta 里取真实内容，如 QQ 的 sticker_index），
		// 而"没传 payload 这个键"才是真的没提供。前者归插件判断合法性，
		// 后者由 parseOutputItems 的 len==0 拦下。
		return []string{t}
	case []string:
		return t
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (a *Agent) executeOutputSendTool(tc agentAPI.ToolCall) string {
	channel := strings.TrimPrefix(tc.Name, "output_send__")
	if channel == "" {
		return "工具名称格式: output_send__{channel}"
	}
	// 授权闸（纵深防御）：模型可能凭名字直接调未授权的输出门。
	if !a.IsOutputAllowed(channel) {
		return fmt.Sprintf("通道 [%s] 未授权给本 agent。可用通道见 output_list_channels", channel)
	}
	// 通道存在性是**内核唯一该管的校验**。type 与能力的匹配下放给插件：
	// type 是插件自定义枚举，内核不认识它（写死 text/image/file 就是内核在
	// 理解插件语义，且插件的自定义类型会被无端拒绝）。
	if a.io.GetChannelCapabilities(channel) == 0 {
		return fmt.Sprintf("通道 [%s] 不存在或不可用。可用输出工具列表见 output_list_channels", channel)
	}

	items, err := parseOutputItems(tc.Arguments)
	if err != nil {
		// 整 call 拒绝：一条都不发。发一半再报错会让模型以为部分成功，
		// 而它无法知道该从第几条重试。
		return fmt.Sprintf("output_send__%s 参数有误，未发送任何内容：%v", channel, err)
	}

	// 单条且成功 ⇒ 仍返回 "ok"（与改动前逐字一致）。
	// 多条或多条里有失败 ⇒ 返回逐项结果，让模型知道哪条没发出去。
	type sendResult struct {
		ok  bool
		msg string
	}
	results := make([]sendResult, len(items))

	for i := range items {
		text, ok := a.sendOneOutput(channel, items[i])
		results[i] = sendResult{ok: ok, msg: text}
	}

	if len(items) == 1 {
		if results[0].ok {
			return "ok"
		}
		return results[0].msg
	}

	// 多条：如实逐项汇报。失败项必须列出 —— 模型据此决定重发哪条，
	// 静默吞掉会让它以为全发出去了。
	okCount := 0
	var b strings.Builder
	for _, r := range results {
		if r.ok {
			okCount++
		}
	}
	if okCount == len(items) {
		// 全部成功也给出条数：多条时不报"ok"会让模型不确定发了几条。
		return fmt.Sprintf("已通过 [%s] 通道发送 %d 条", channel, len(items))
	}
	b.WriteString(fmt.Sprintf("已通过 [%s] 通道发送 %d/%d 条：\n", channel, okCount, len(items)))
	for i, r := range results {
		typ := items[i].typ
		if r.ok {
			b.WriteString(fmt.Sprintf("  [%d] %s ok\n", i+1, typ))
		} else {
			b.WriteString(fmt.Sprintf("  [%d] %s 失败：%s\n", i+1, typ, r.msg))
		}
	}
	b.WriteString("失败的条目请单独重发。")
	return b.String()
}

// sendOneOutput 发送一条输出项，返回 (结果文本, 是否成功)。
//
// 这是从 executeOutputSendTool 里拆出来的**单条**路径 —— 批量只是把它循环 N 次。
// 拆出来的理由：before_output / 安全回填 / after_output 这套顺序对**每一条**
// 都必须完整走一遍，混在批量循环里极易漏掉某一环（例如只给第一条跑 stage）。
func (a *Agent) sendOneOutput(channel string, item outputItem) (string, bool) {
	args := map[string]interface{}{
		"payload": item.payload,
		"type":    item.typ,
	}
	if item.meta != "" {
		args["meta"] = item.meta
	}

	// ★ Extra 必须有 output_channel：本函数是"发往某通道"，而这个键是
	// 插件在该时点唯一能据以判断目标通道的凭据。
	// Extra 必须带三个键，缺一不可：
	//
	//	output_channel  目标通道（插件据此判断"是不是发给我的"）
	//	output_meta     本次发送的 meta 原文（QQ 的 user_id/group_id 在里面）
	//	output_type     输出类型（text/image/file…）
	stageCtx := &sdk.StageContext{
		FinalText: item.payload,
		Phase:     sdk.StageBeforeOutput,
		Extra: map[string]interface{}{
			"output_channel": channel,
			"output_meta":    item.meta,
			"output_type":    item.typ,
		},
	}
	a.runStage(sdk.StageBeforeOutput, stageCtx)
	if stageCtx.Response != nil {
		return fmt.Sprintf("输出被插件拦截: %s", *stageCtx.Response), false
	}
	// 只有"原本非空、被插件清空"才是异常（防插件静默吞掉内容却仍算成功）。
	//
	// 原本就空 ⇒ 合法占位（如 type=sticker + meta.sticker_index，真实 URL
	// 由插件自己取），不能报"被清空"——那是拿内核不理解的字段做判断。
	if item.payload != "" && stageCtx.FinalText == "" {
		return "输出被插件清空", false
	}
	args["payload"] = stageCtx.FinalText

	if dev := a.io.GetDevice(channel); dev != nil {
		// ★ 安全模块入向回填：把 @{别名} 换回明文。
		//
		// 位置在**插件 stage 之后、设备 Execute 之前**是有讲究的：
		//   · 放在 stage 之前 ⇒ 插件会看到明文（不可，插件在信任边界外）
		//   · 放在 Execute 之后 ⇒ 设备已收到带别名的载荷（功能坏了）
		// 所以只能夹在中间：插件看不到，设备拿到真的。
		if sc := security.Global(); sc != nil {
			if filled, n, used := sc.Fill(args["payload"].(string)); n > 0 {
				args["payload"] = filled
				log.Printf("[security] 通道 %s 载荷回填 %d 处（别名 %v）", channel, n, used)
			}
		}
		result, err := dev.Execute("output", args)
		// ★ after_output：消息**已真正送达**之后触发一次。
		// 与 before_output 成对（同一份 Extra）。
		stageCtx.Phase = sdk.StageAfterOutput
		a.runStage(sdk.StageAfterOutput, stageCtx)
		if err != nil {
			return fmt.Sprintf("通过 [%s] 通道发送失败: %v", channel, err), false
		}
		// 通道可能回报「未确认」（已提交但超时未拿到发送确认）——此时不能对模型
		// 谎报「已发送」，否则模型不会重试/核实。
		if m, ok := result.(map[string]interface{}); ok {
			if status, _ := m["status"].(string); status == "unconfirmed" || status == "queued" {
				note, _ := m["note"].(string)
				if note == "" {
					note = "发送已提交但未收到通道确认，结果未知"
				}
				return fmt.Sprintf("[%s] 通道发送结果未确认：%s", channel, note), false
			}
		}
		return "ok", true
	}

	a.io.EmitTextTo("agent_io", channel, item.payload)
	return fmt.Sprintf("已通过 [%s] 通道发送", channel), true
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
