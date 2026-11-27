package webui

// push.go —— 输出通道与 stage 处理器把内容**直接**送到当前浏览器连接。
//
// ## 为什么没有总线
//
// 用户裁定（2026-11-25）：「这是传递 agent 本身的状态，而非基座状态，
// 不应该借用内部 bus」。
//
// 这里的三条推送源全是**本插件自己的闭包**：
//
//	输出通道 handler   agent 逐 chunk 调 EmitOutputTo（docs §9.2 第 5 行）
//	stage handler      agent 跑 RunStage → 本插件的回调
//
// 它们与"当前有哪些 SSE 连接"住在同一个进程、同一个 Plugin 实例里。
// 中间加一层发布/订阅等于把内部调用改成异步消息 —— 凭空多一份状态
// （订阅者表、事件类型、重放语义）而没有任何收益。
//
// ★ 与内核事件总线的界限（docs §9.4）：总线是**内核模块之间**的通道。
//   webui 的浏览器连接、终端输出、进度提示都是插件侧状态，不进总线。

import (
	"encoding/json"
	"fmt"
	"time"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// PushChannelOutput 把一个输出通道调用（args）转成对浏览器的推送。
//
// ★ 生产路径：插件的 `RegisterOutputChannel("webui", ...)` handler 调它。
// ★ 判据路径：测试用同一个入口（device.Execute("output", args) → 这里）。
func (h *Handler) PushChannelOutput(args map[string]interface{}) {
	if h == nil || args == nil {
		return
	}
	// ★ 本入口只处理**完整输出**（output_send__webui 的 payload/type）。
	//   流式增量走 on_llm_call 阶段（pushLLMDelta），不经这里 ——
	//   输出通道是工具的执行后端，不是流式载体。
	// 完整输出：写聊天历史 + 推卡片
	out := map[string]interface{}{
		"type":    "agent_output",
		"channel": "webui",
		"kind":    "channel_output",
		"content": args["payload"],
	}
	if rawType, _ := args["type"].(string); rawType == "image" || rawType == "file" {
		out["output_type"] = rawType
		if u, _ := args["url"].(string); u != "" {
			out["url"] = u
		}
		if s, _ := args["size"]; s != nil {
			out["size"] = s
		}
	}
	h.RecordChannelOutput(out)
	h.pushOutputFrame(out)
}

// pushDelta 推送一条**流式增量**（逐 token）。
//
// ★ 事件名沿用 `content_delta` / `reasoning_delta`：前端已有渲染逻辑，
// 换名字就是强制前端同步改。变的是**来源**（输出通道调用），不是名字。
//
// ★ 增量不进 sseEventRing：断线重连只重放聚合事件，重放增量会与聚合内容

// pushOutput 推送一条**完整输出**（含图片/文件卡片）。
//
// 这条**进** sseEventRing：断线重连要能补到"刚才发了一张图"，
// 否则用户重连后那张卡片永久消失。
func (p *Plugin) pushOutput(payload map[string]interface{}) {
	if p == nil || p.handler == nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	id := deltaID()
	if p.handler.sseEvents != nil {
		p.handler.sseEvents.Append(id, "agent_output", data)
	}
	p.handler.sendToStreams("agent_output", id, string(data))
}

// pushStage 推送一条**阶段进度**（thinking / tool:xxx / output）。
func (p *Plugin) pushStage(payload map[string]interface{}) {
	if p == nil || p.handler == nil {
		return
	}
	payload["type"] = "stage"
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	id := deltaID()
	if p.handler.sseEvents != nil {
		p.handler.sseEvents.Append(id, "stage", data)
	}
	p.handler.sendToStreams("stage", id, string(data))
}

// deltaID 生成一个 SSE 事件 id（时间戳 + 纳秒后缀，天然单调）。
func deltaID() string {
	now := time.Now()
	return fmt.Sprintf("%d-%d", now.UnixNano(), now.Nanosecond()%1000)
}

// pushOutputFrame 推送一条完整输出（Handler 版，供 device 路径用）。
func (h *Handler) pushOutputFrame(payload map[string]interface{}) {
	if h == nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	id := deltaID()
	if h.sseEvents != nil {
		h.sseEvents.Append(id, "agent_output", data)
	}
	h.sendToStreams("agent_output", id, string(data))
}

// addStageAgent 把**这次阶段属于哪个 agent** 附进帧。
//
// 同一内核里主 agent 与子 agent 并发跑，二者各自的文本与 toolcall 是
// **不同的现场**。不带身份，前端只能把它们混成一条流 —— 看起来像一个
// agent 在同时干两件事，且无从知道"这一步是谁在做"。
//
// 字段由基座填（`StageContext.AgentID/AgentType/IsChild`，见
// `agent.stampStageIdentity`），插件只转发，不自己推断。
func addStageAgent(frame map[string]interface{}, ctx *sdk.StageContext) {
	if frame == nil || ctx == nil {
		return
	}
	if ctx.AgentID != "" {
		frame["agent_id"] = ctx.AgentID
	}
	if ctx.AgentType != "" {
		frame["agent_type"] = ctx.AgentType
	}
	frame["is_child"] = ctx.IsChild
}

// pushLLMDelta 消费**流式增量**（基座在 on_llm_call 上逐 chunk 推送）。
//
// 契约（见 sdk.StageContext 注释）：
//
//	Extra["streaming"] = true   增量帧：LLMText 是**当前累计正文**
//	Extra["streaming"] = false  结束帧：带完整正文 / 思维链 / ToolCalls / usage
//
// ★ 为什么用累计值：消费方不必自己拼接，丢片后也能恢复。
//
//	SSE 帧只推**新增的那部分**（前端做 append），所以这里做 diff ——
//	那是显示层的事，不是协议的一部分。
//
// PushLLMDelta 消费一次 `on_llm_call` 阶段（生产与判据共用同一个处理体）。
func (h *Handler) PushLLMDelta(ctx *sdk.StageContext) { h.pushLLMDelta(ctx) }

func (h *Handler) pushLLMDelta(ctx *sdk.StageContext) {
	if h == nil || ctx == nil {
		return
	}
	streaming, _ := ctx.Extra["streaming"].(bool)
	key := ctx.AgentID
	if key == "" {
		key = "_"
	}

	h.llmShownMu.Lock()
	cur := h.llmShown[key]
	var text, reasoning string
	if streaming {
		if len(ctx.LLMText) > cur[0] {
			text = ctx.LLMText[cur[0]:]
		}
		if len(ctx.ReasoningContent) > cur[1] {
			reasoning = ctx.ReasoningContent[cur[1]:]
		}
		if h.llmShown == nil {
			h.llmShown = map[string][2]int{}
		}
		h.llmShown[key] = [2]int{len(ctx.LLMText), len(ctx.ReasoningContent)}
	} else {
		delete(h.llmShown, key)
	}
	h.llmShownMu.Unlock()

	if reasoning != "" {
		h.pushDeltaFrame("reasoning_delta", reasoning, ctx)
	}
	if text != "" {
		h.pushDeltaFrame("content_delta", text, ctx)
	}
}

// pushDeltaFrame 推一条增量 SSE 帧（带 agent 标识，前端可按 agent 分流显示）。
func (h *Handler) pushDeltaFrame(eventType, content string, ctx *sdk.StageContext) {
	if h == nil {
		return
	}
	payload := map[string]interface{}{"type": eventType, "channel": "webui", "content": content}
	addStageAgent(payload, ctx)
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h.sendToStreams(eventType, deltaID(), string(data))
	// OpenAI 兼容端点按自己的帧格式处理同一条增量。
	h.fanoutDelta(payload)
}
