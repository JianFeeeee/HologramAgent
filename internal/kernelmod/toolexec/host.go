package toolexec

// host.go —— 设施对**执行宿主**的最小要求（与 `agent.ToolHost` 同形）。
//
// ## 为什么不直接 import `internal/agent/agent`
//
// 方向上是合法的（内核模块 → agent），但那样设施会绑死在基座的具体类型上：
// 装配期一旦把宿主换成另一个实现（裁剪版内核 / 判据替身 / SDK 内的独立工具），
// 设施就要改。按**能力**声明接口，宿主只要能满足这组方法就行。
//
// ★ 这份接口不是"多一层包装"：它只有声明，没有实现体。
//   真实转发在 `agent.ToolHost()`（返回 `ToolHost`，基座自己实现）。

import (
	"context"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// hostFace 是设施执行时读到的全部宿主能力。
type hostFace interface {
	ID() types.AgentID
	IsChildAgent() bool
	IO() *agentIO.IOManager
	ToolRegistry() *kit.ToolRegistry
	BuiltinTools() agentAPI.BuiltinToolHandlers
	SecurityDecisionFor(tool string) model.Decision
	CheckPath(direction, path string) model.Decision
	CheckNetwork() model.Decision
	CheckOutputGate(channel string) bool
	SecretStore() kernelapi.SecretStore
	ReviewToolResultContent(tool, text string) []model.Finding
	ToolParallelSafe(name string) bool
	ArchiveBeforeWrite(path string) (bool, error)
	RegisterToolCancel(cancel context.CancelFunc) uint64
	UnregisterToolCancel(id uint64)
	// RunToolStage 在内核执行链里跑一个工具阶段，返回**处理结果**。
	//
	// ★ 为什么是"跑阶段"而不是"推事件"（2026-11-26 修）：stage 是**处理**
	//   接口 —— 注册方（类型钩子 / 插件）拿到 ctx 后可以
	//
	//	· 设 ctx.Response ⇒ 拦下这次工具调用（不再执行）
	//	· 改 ctx.ToolCalls[0].Arguments ⇒ 改写参数
	//	· 改 ctx.ToolResults[0].Result ⇒ 改写结果
	//
	//   一次发射不管结果的"push"把这些全丢了 —— 那是事件总线的语义，
	//   不是 stage 的。
	//
	// ★ 为什么由内核链调：工具阶段是**工具执行链的内部阶段**，只有那条链
	//   知道自己正握着哪个工具、参数是什么、结果是什么。
	//
	// 返回 (是否被拦下, 阶段上下文)。调用方按 ctx 的新值继续或短路。
	RunToolStage(stage sdk.Stage, tc agentAPI.ToolCall, result *sdk.ToolResult, channel string) (bool, *sdk.StageContext)
	Logf(format string, args ...any)
}

// sdkToolDef 是工具声明的别名（标记的真实来源）。
type sdkToolDef = sdk.ToolDef
