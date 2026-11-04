package child

// kind.go 定义 ChildKind —— 一种子 agent 类型的**全部差异点**。
//
// # 设计依据（2026-11-04 与用户对齐）
//
// 用户裁定的差异只有两条：
//
//	① 上下文管理策略
//	② 工具异步调用（含输出通道异步调用）
//
// 实测这条判断是准确的。三种类型的**上下文管理**确实完全不同：
//
//	类型      上下文载体              裁剪方式                    溢出处理
//	主 agent  RelevanceContext        相关性 topK + 冷上下文下沉   enforceContextBudget
//	驻留子     RelevanceContext(轻量)  同 + 三层压缩协议            enforceContextBudget
//	                                                                + contextfull 上报
//	                                                                + 父驱动压缩
//	rtchild   跨轮 history 切片       按窗口丢最旧组（只裁视图）   无（视图裁完继续）
//	工作式子  局部 msgs               无（跑完即弃）               maxTurns 硬上限
//
// 而"输出通道异步调用"也一样：rt 逐 chunk、其余整段。
//
// ⇒ 所以接口的主轴就是这两条，其余（登记/停止/撤销策略/注销入站）全在骨架里。

import (
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// ChildKind 是一种子 agent 类型的完整差异实现。
//
// 实现者放在 core 包里（`*_child.go`），**不放本包**：
// 实现需要载体 Agent 的全部能力（provider / 工具执行 / 记忆 / 安全），
// 把它们抽象成接口会造出一个比 Agent 还大的假接口 —— 那是把复杂度
// 换个地方放，不是消除。本包只提供接口、骨架与登记表。
type ChildKind interface {
	// Kind 返回类型标签（与 kinds.go 的常量一致）。
	Kind() Kind

	// ── ① 输入策略 ──

	// Deliver 投递一条输入。
	//
	// 队列型（驻留子）返回 false 表示"交回默认路径"（DeliverRouted 进调度器）。
	// 单槽型（rt）自行覆盖并返回 true。
	Deliver(e Entry, evt *agentIO.InputEvent, isInterrupt bool) bool

	// ── ② 上下文管理策略（本接口的主轴之一）──

	// BuildContext 装配本轮送入模型的上下文视图。
	//
	// 必须遵守"视图与存储分离"：返回的切片可以是裁剪后的视图，
	// 但**不得**修改本类型的长期存储（rt 的 history 永不裁剪）。
	BuildContext(e Entry, input string) []agentAPI.Message

	// AppendHistory 把一轮产出写回长期存储。
	// 队列型（驻留子）用 RelevanceContext 承载，不需要它 ⇒ no-op。
	AppendHistory(e Entry, msgs ...agentAPI.Message)

	// Compact 按本类型的上下文策略压缩。返回被处理的事件数。
	//
	// 只有驻留子实现了真正的压缩（摘要替换 + 失败回退截断）。
	// 不支持的类型返回 (0, ErrCompactUnsupported)。
	Compact(e Entry, keepRecent int) (int, error)

	// ── ③ 工具与输出异步调用（主轴之二）──

	// Emit 把一段增量文本送到输出通道。
	//
	// 异步批大小由实现决定：rt 逐 chunk、其余整段。**实现必须异步**，
	// 插件延迟不得节流调用方的生成循环。
	Emit(e Entry, text string)

	// RunTools 执行一批工具，返回与入参**同序**的结果。
	//
	// 必须让整批经过 before_toolcall / after_toolcall 插件钩子。
	RunTools(e Entry, calls []agentAPI.ToolCall) []ToolResult

	// ── ④ 销毁语义 ──

	// Dispose 按本类型语义收尾。policy 只对支持残余处置的类型有意义。
	Dispose(e Entry, policy ResidualPolicy) string

	// ── ⑤ 工具面 ──

	// ParentTools 声明该类型暴露给**父模型**的控制工具。
	//
	// 定义与派发都从这一份派生 —— 这是消灭 toolcall.go 与 toolroute
	// 两份工具表不同步的落点。
	ParentTools() []ToolSpec
}

// ErrCompactUnsupported 表示该类型不支持压缩。
var ErrCompactUnsupported = "该类型不支持上下文压缩"
