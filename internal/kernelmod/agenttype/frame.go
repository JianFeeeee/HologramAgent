package agenttype

// frame.go —— 给插件的**中立帧视图**（AgentFrame 的实现），以及**本类型自己的**帧。
//
// ★ 为什么帧在本包（docs §0.3 隔离铁律）：
//   原先用 `agenttypes.Frame`（公共值层）。帧是"谁用它谁拥有"的东西 ——
//   主 agent 有自己的 `TaskFrame`、驻留子有自己的 `ResidentFrame`、
//   插件类型有这一份。三份今天字段相近，那只是现状。
//
// ★ 视图刻意小：只暴露线性流程要用的东西（输入 / 消息 / 工具批 / 步进）。
//   主 agent 那种 7 阶段可抢占状态机是**它自己的复杂度**，不外包给插件；
//   插件要更复杂的行为改 `Spec`（内核组装），而不是在 Loop 里重写内核机制。

import (
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// pluginTask 是**插件类型自己的**帧（一帧一个任务）。
type pluginTask struct {
	Input string
	Msgs  []agentAPI.Message

	PendingTools []agentAPI.ToolCall
	ToolResults  []toolResultItem
	Response     string
	Turn         int

	evt *agentIO.InputEvent
}

// toolResultItem 是进上下文的那一份工具结果。
type toolResultItem struct {
	CallID string
	Name   string
	Output string
}

// pluginFrame 是 AgentFrame 的实现（一帧一个，短暂生命周期）。
type pluginFrame struct {
	a *agent.Agent
	f *pluginTask
}

func newFrame(a *agent.Agent, f *pluginTask) pubsdk.AgentFrame {
	return &pluginFrame{a: a, f: f}
}

func (fr *pluginFrame) Input() string {
	if fr.f == nil {
		return ""
	}
	return fr.f.Input
}

func (fr *pluginFrame) Messages() []pubsdk.Message {
	if fr.f == nil {
		return nil
	}
	out := make([]pubsdk.Message, 0, len(fr.f.Msgs))
	for _, m := range fr.f.Msgs {
		pm := pubsdk.Message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			pm.ToolCalls = append(pm.ToolCalls, pubsdk.ToolCall{
				ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
			})
		}
		out = append(out, pm)
	}
	return out
}

func (fr *pluginFrame) AppendMessage(m pubsdk.Message) {
	if fr.f == nil {
		return
	}
	am := agentAPI.Message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
	for _, tc := range m.ToolCalls {
		am.ToolCalls = append(am.ToolCalls, agentAPI.ToolCall{
			ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
		})
	}
	fr.f.Msgs = append(fr.f.Msgs, am)
}

func (fr *pluginFrame) SetResponse(text string) {
	if fr.f == nil {
		return
	}
	fr.f.Response = text
}

func (fr *pluginFrame) Response() string {
	if fr.f == nil {
		return ""
	}
	return fr.f.Response
}

// Marks 把本帧输入携带的标记**原样**交给插件（内核不解释任何键）。
func (fr *pluginFrame) Marks() map[string]interface{} {
	if fr.f == nil || fr.f.evt == nil || fr.f.evt.Payload == nil {
		return nil
	}
	out := make(map[string]interface{}, len(fr.f.evt.Payload))
	for k, v := range fr.f.evt.Payload {
		out[k] = v
	}
	return out
}

func (fr *pluginFrame) PendingTools() []pubsdk.ToolCall {
	if fr.f == nil {
		return nil
	}
	out := make([]pubsdk.ToolCall, 0, len(fr.f.PendingTools))
	for _, tc := range fr.f.PendingTools {
		out = append(out, pubsdk.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
	}
	return out
}

func (fr *pluginFrame) AppendToolResult(callID, name, text string) {
	if fr.f == nil {
		return
	}
	fr.f.ToolResults = append(fr.f.ToolResults, toolResultItem{
		CallID: callID, Name: name, Output: text,
	})
}

func (fr *pluginFrame) Turn() int {
	if fr.f == nil {
		return 0
	}
	return fr.f.Turn
}

func (fr *pluginFrame) SetTurn(n int) {
	if fr.f == nil {
		return
	}
	fr.f.Turn = n
}
