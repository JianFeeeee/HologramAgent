package resident

// primitives.go —— **驻留子自己的**帧推进原语（不借主 agent 的任何一份）。
//
// ## 对照主 agent 少了什么（"没有就不做"，不是漏接线）
//
//	人格 / 插件健康 / 思考开关 / 蒸馏 / 媒体整理 / 转投 —— 驻留子都没有。
//	它有：整窗上下文、召回注入、工具表、LLM 调用、工具批、收尾。

import (
	"strings"

	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	rctx "github.com/JianFeeeee/HologramAgent/internal/agent/resident/ctx"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// coldStore 返回冷文档存储。
//
// ★ 驻留子**没有**冷文档（那是主 agent 的记忆面）⇒ 恒 nil。
//
//	这是明确的"没有就不做"，不是漏接线。

// ComputeTokenBudget 返回本驻留子的 token 预算（整窗型：用整个模型窗口）。
func (m *M) ComputeTokenBudget() kit.TokenBudget {
	return kit.ComputeTokenBudget(m.A.LLMMaxContextTokens(), m.A.SystemPrompt(), m.A.CtxTuning())
}

// BuildTaskMemoryContext 拼本轮的记忆上下文。
//
// ★ 驻留子**不直接读图库** —— 它没有库。注入用的召回与模型自己调用的
//
//	是**同一个工具**（`graph_recall`），走同一条执行链。
//	空串 = 没有可注入的记忆（明确事实）。
func (m *M) BuildTaskMemoryContext(f *ResidentFrame, query string, budget int) string {
	if m == nil || f == nil || strings.TrimSpace(query) == "" {
		return ""
	}
	res := m.A.RunToolBatch(m.A.Ctx(), []agentPkg.ToolCall{{
		ID: "recall_" + query, Name: "graph_recall",
		Arguments: map[string]interface{}{"keywords": []interface{}{query}, "depth": float64(1)},
	}})
	if len(res) == 0 || strings.TrimSpace(res[0].Text) == "" || res[0].Denied {
		return ""
	}
	return "[记忆召回]\n" + res[0].Text
}

// BuildSystemPrompt 拼本类型的系统提示词（素材继承父，渲染归本类型）。
func (m *M) BuildSystemPrompt(memContext, input string) string {
	base := strings.TrimSpace(m.A.SystemPrompt())
	if base == "" {
		base = "You are a resident sub-agent. Answer only from what the tools return."
	}
	if memContext == "" {
		return base
	}
	return base + "\n\n[记忆上下文]\n" + memContext
}

// EffectiveProtectedCount 是恢复/裁剪时要保护的最近条数。
//
// ★ 整窗型不裁剪 ⇒ 0（明确策略：撞窗口上报父决定）。
func (m *M) EffectiveProtectedCount() int { return 0 }

// PruneOnInput 按声明裁剪上下文（驻留子整窗型，**不裁**）。
func (m *M) PruneOnInput(evt *agentIO.InputEvent, cleanInput string) int { return 0 }

// renderL1 返回上下文的 L1 渲染（本包 ctx 自己的实现）。
func (m *M) renderL1Tokens() []rctx.ContextEvent { return nil }

var _ = agentAPI.Message{}

// selfEvent 把内核自循环消息归一成输入事件（本类型自己的归一化）。
func selfEvent(msg agentPkg.SelfInputMsg) *agentIO.InputEvent {
	ch := msg.Channel
	if ch == "" {
		ch = agentIO.ChannelConsolidation
	}
	payload := map[string]interface{}{"content": msg.Text}
	if msg.Kind != "" {
		payload["self_kind"] = msg.Kind
	}
	return &agentIO.InputEvent{
		Source:        "system",
		Type:          "text",
		Payload:       payload,
		OutputChannel: ch,
	}
}

// inputParts 是一条输入解析后的形态。
type inputParts struct {
	text        string
	blocks      []agentAPI.ContentBlock
	mediaType   string
	captureTool string
	selfKind    string
}

// ResolveInput 解析一条输入（文本 + 媒体块）。
//
// ★ 驻留子**不做多模态**：块被解析出来但不落图库（见 prepareInputTask）。
//
//	文本仍然照常处理。
func (m *M) ResolveInput(evt *agentIO.InputEvent) (inputParts, bool) {
	if evt == nil {
		return inputParts{}, false
	}
	text, _ := evt.Payload["content"].(string)
	if text == "" {
		text, _ = evt.Payload["text"].(string)
	}
	out := inputParts{text: text}
	if k, ok := evt.Payload["self_kind"].(string); ok {
		out.selfKind = k
	}
	if blocks, ok := evt.Payload["media_blocks"].([]agentAPI.ContentBlock); ok {
		out.blocks = blocks
	}
	if mt, ok := evt.Payload["media_type"].(string); ok {
		out.mediaType = mt
	}
	if strings.TrimSpace(out.text) == "" && len(out.blocks) == 0 {
		return inputParts{}, false
	}
	return out, true
}

// stageCtxFromInput 建一个阶段上下文（类型自己的渲染入口）。
func (m *M) stageCtxFromInput(text, source, channel string) *sdk.StageContext {
	ctx := &sdk.StageContext{RawMessage: text, Extra: map[string]interface{}{}}
	if source != "" {
		ctx.Extra["input_source"] = source
	}
	if channel != "" {
		ctx.Extra["output_channel"] = channel
	}
	return ctx
}

// InjectSourceContext 把输入事件的**非保留 payload** 透传给阶段上下文。
func (m *M) InjectSourceContext(ctx *sdk.StageContext, evt *agentIO.InputEvent) {
	if ctx == nil || evt == nil || evt.Payload == nil {
		return
	}
	if ctx.Extra == nil {
		ctx.Extra = map[string]interface{}{}
	}
	for k, v := range evt.Payload {
		if agentIO.IsReservedPayloadKey(k) {
			continue
		}
		ctx.Extra[k] = v
	}
}

// EmitResponse 交付本帧的回复（**基座的出口原语**，类型只负责跑阶段）。
//
// ★ 驻留子与主 agent 的差别只在这里：驻留子**没有** before/after_output
//
//	的定制（它不继承父的插件阶段面），所以直接交给基座投递。
func (m *M) EmitResponse(evt *agentIO.InputEvent, response string, usage agentAPI.TokenUsage) {
	if m == nil || m.A == nil {
		return
	}
	m.A.EmitReply(evt, response)
}

func (m *M) drainMediaBlocks() []interface{} { return nil }

// emitMemoryCandidate：记忆候选是**主 agent 的**记忆事务（它有自己的文本记忆
// 与蒸馏器）。驻留子没有那些服务 ⇒ 不产生候选。
func (m *M) emitMemoryCandidate(source, input, response, reasoning string, toolResults []ToolResultItem, toolsUsed []string) {
}

// interruptInput 是本轮输入是否为中断（帧事实，本类型自己的记账）。
// 声明在 M 上（不是 state），因为它是**当前任务**的事实。
func (m *M) setInterruptInput(v bool) { m.currentInterrupt = v }
func (m *M) isInterruptTurn() bool    { return m.currentInterrupt }

// BuildToolDefs 渲染本轮的工具体。
//
// ★ 公有面由**基座**给（`PublicToolDefs`：内核族 + IO 设备工具 + 插件工具 +
//
//	registry 表，已按侧别与 allow/deny 过滤）。驻留子没有自己的私有工具族，
//	所以这里不需要再追加任何东西 —— 与主 agent 走的是**同一个**公有面函数，
//	差别只在私有族有无（那正是"类型差异来自声明"）。
func (m *M) BuildToolDefs() []interface{} { return m.A.PublicToolDefs() }

// ResolveToolPlugin 查工具的归属插件（阶段上下文里要填它）。
func (m *M) ResolveToolPlugin(name string) string { return registry.ToolOwner(name) }

// resolveTurnScenes 解析本轮的场景键（驻留子没有场景记忆面 ⇒ 空）。

// executeOrDelegateTool 执行本类型的工具或交给内核执行设施。
//
// ★ 驻留子**没有**私有工具族（那些是主 agent 的记忆/媒体/插件面）⇒ 全部
//
//	交给内核（`RunToolBatch` 的唯一入口）。
func (m *M) executeOrDelegateTool(tc agentAPI.ToolCall, channel string, scenes []string) toolOutcome {
	results := m.A.RunToolBatch(m.A.Ctx(), []agentPkg.ToolCall{{
		ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
	}})
	if len(results) == 0 {
		return toolOutcome{Text: "内核工具执行设施未就绪：本次工具调用未被执行"}
	}
	r := results[0]
	return toolOutcome{Text: r.Text, Raw: r.Raw, Denied: r.Denied, Tool: r.Tool}
}

// batchRunnable 判整批工具能否并发（读的是注册表声明，不猜）。
func (m *M) batchRunnable(f *ResidentFrame) bool {
	if len(f.PendingTools) <= 1 {
		return false
	}
	seenOut := map[string]bool{}
	for _, tc := range f.PendingTools {
		if !m.A.ToolParallelSafe(tc.Name) {
			return false
		}
		if ch, isOut := outputSendChannel(tc.Name); isOut {
			if seenOut[ch] {
				return false
			}
			seenOut[ch] = true
		}
	}
	return true
}

// outputSendChannel 从 `output_send__<通道>` 取通道名（同通道必须保序）。
func outputSendChannel(name string) (string, bool) {
	const p = "output_send__"
	if len(name) <= len(p) || name[:len(p)] != p {
		return "", false
	}
	ch := name[len(p):]
	if len(ch) > 5 && ch[len(ch)-5:] == "_help" {
		return "", false
	}
	return ch, true
}

// pluginHealth 返回插件健康表。
//
// ★ 驻留子**没有插件面**（它不加载插件、不持有插件登记表）⇒ 恒健康。
//
//	真实的 "插件崩溃就跳过工具" 是主 agent 的语义。

// toolOutcome 是一次工具执行的结果（本类型自己的形状）。
type toolOutcome struct {
	Text   string
	Raw    interface{}
	Denied bool
	Tool   any
}

// ★ 已删：SceneKeysFor / mergeSceneKeys / MemoryPass / checkToolResultSize /
// offloadToolResult / noteOversizeTool / coldStore / BindEventMedia /
// drainMediaBlocks / emitMemoryCandidate / resolveTurnScenes。
//
// 它们全都是**主 agent 的记忆整理面**（场景体系、索引器、蒸馏器、事件目录、
// 多模态块）。驻留子没有那一面 ⇒ 调用点已删，函数也不该留 ——
// 留一个空函数等于把"这个能力存在但什么都不做"变成永久盲区。
