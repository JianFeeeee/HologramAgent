package device

// toolexec_exec.go —— 工具执行设施的**实现载体**。
//
// # 它持有设施，不持有宿主
//
// 改造前它是个空结构，靠调用方每次传 `BatchOpts.Host`（就是个 `any`，
// 实际装的是 `*Agent`），再回调 15 个方法 —— 那是**反向依赖**：
// 内核设施向基座索取自己的能力。
//
// 现在：需要的设施在装配期注入，执行时只收「我是谁」（agentID）。
//
//	security   → 工具授权 / 路径 / 网络 / 产出物审查
//	stageHost  → 工具阶段（on_toolcall / before / after）
//	toolReg    → 插件工具表（含 ParallelSafe 声明）
//	io         → 设备与通道工具
//	tracker    → 写前归档
//
// # per-agent 状态仍然没有
//
// Executor 服务全部实例：任何 per-agent 字段都会让"主 agent 的设置泄漏给子"
// （本仓在安全档位上踩过这个形态）。取消登记按 agentID 分桶，见 cancel.go。

import (
	"context"

	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// Executor 是工具执行设施（持有内核设施，按 agentID 区分消费者）。
type Executor struct {
	deps *ExecDeps
}

// ExecDeps 是执行链需要的内核设施（装配期注入）。
type ExecDeps struct {
	IO       IOAccess
	Security SecurityAccess
	Tools    ToolAccess
}

// AgentHandle 是**实例句柄**——直接复用 agent 包的契约，不另立一份。
//
// ★ 为什么不在这里再声明一个接口：两个同形接口会让 `*Agent` 实现其中一个
//
//	而不实现另一个，装配期表现为"值类型不匹配"的难查错误（本轮踩过）。
//	`internal/agent/agent` 是**纯数据类型 + 接口**包，零 kernelmod 依赖，
//	device import 它是单向的。
type AgentHandle = agentPkg.AgentHandleFace

// Execute 执行一批工具调用，返回与入参**同序**的结果。
func (x *Executor) Execute(ctx context.Context, h AgentHandle, calls []agentPkg.ToolCall) []agentPkg.BatchResult {
	if x == nil || x.deps == nil {
		return failAll(calls, "工具执行设施未注入依赖（装配错误）：本次调用未执行")
	}
	if h == nil {
		return failAll(calls, "工具执行缺少实例句柄（装配错误）：本次调用未执行")
	}
	return runBatch(ctx, x.deps, h, calls)
}

// failAll 给每条调用一个**响亮**的失败结果（不静默返回空）。
func failAll(calls []agentPkg.ToolCall, msg string) []agentPkg.BatchResult {
	out := make([]agentPkg.BatchResult, len(calls))
	for i, c := range calls {
		out[i] = agentPkg.BatchResult{CallID: c.ID, Name: c.Name, Text: msg, Denied: true}
	}
	return out
}

// NewExecutor 构造执行器（**装配与判据共用**）。
//
// 三个参数就是执行链需要的全部设施 —— 没有"宿主"，因为它不再是执行链的
// 依赖（见本文件头的说明）。生产装配由 `Module.Init` 从 kstate 取这三个；
// 判据自己当装配侧时直接传受控实现。
func NewExecutor(io *agentIO.IOManager, gate kernelapi.SecurityGate, tools *kit.ToolRegistry) *Executor {
	d := &ExecDeps{}
	if io != nil {
		d.IO = ioAccess{io}
	}
	if gate != nil {
		d.Security = securityAccess{gate}
	}
	if tools != nil {
		d.Tools = toolAccess{tools}
	}
	return &Executor{deps: d}
}
