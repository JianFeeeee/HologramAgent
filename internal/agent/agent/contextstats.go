package agent

// contextstats.go —— **上下文的只读统计面**（类型实现，基座只读数字）。
//
// ★ 基座不持有上下文实现（§6.2 #1），但状态面与诊断要报"当前几条 / 水位多少"。
//   所以类型在装配期挂一个只读闭包面进来 —— 基座读数字，不碰实现。

import agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"

// ContextStats 是上下文统计的**只读面**。
type ContextStats interface {
	// Events 返回当前事件条数。
	Events() int
	// Accumulated 返回未裁剪的积累 token 数。
	Accumulated() int
	// Window 返回模型窗口上限。
	Window() int
}

// ContextAccumulated 返回积累 token 数（状态面用）。
func (a *Agent) ContextAccumulated() int {
	if a == nil || a.contextStatsFace() == nil {
		return 0
	}
	return a.contextStatsFace().Accumulated()
}

// ContextWindow 返回模型窗口上限。
func (a *Agent) ContextWindow() int {
	if a == nil || a.contextStatsFace() == nil {
		return 0
	}
	return a.contextStatsFace().Window()
}

// ToolDefsFace 是**类型注册的工具清单渲染面**。
//
// ★ 它只回答"现在模型能看见哪些工具"，不参与执行、不参与授权：
//
//	执行走 `RunToolBatch`（内核执行设施），授权走安全闸。
type ToolDefsFace func() []agentAPI.ToolDefinition
