package agent

// toolhost.go —— 工具执行设施看到的**宿主能力束**（基座侧契约）。
//
// ## 为什么需要它
//
// 工具执行链在内核设施里（`kernelmod/toolexec`），而链上要读的东西全都
// 挂在 agent 上：io（设备/通道工具）、stageHost（插件工具）、安全闸、
// 变量库、取消栈、工具取消编号、身份（父/子）。
//
// 设施不能 import `internal/agent/agent`（agent 会 import 它？不——是它会
// import agent 模块的挂载点，而 agent 属于内核模块，方向可以；但它要的
// 方法和基座的具体字段一一对应，那会把"设施"与"基座实现"焊死）。
//
// 所以设施按**它实际用到的能力**声明接口，基座提供 `ToolHost()` 返回自己。
// 这份契约是本文件，写清"设施能从宿主读到什么"。
//
// ★ 与 `api.ToolHost`（device 用的两个方法）不是一回事：那个是 device
//   数据面的最小面；本文件是**内核执行设施**需要的那一组。

import (
	"context"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// ToolHost 是工具执行设施对宿主的**最小能力要求**。
//
// 这一组方法都有明确归属：它们全是"基座的设施"（io / stageHost / 安全闸 /
// 取消栈 / 身份），没有一个是某个类型的私有状态 —— 那正是"执行链归内核"
// 的前提。
type ToolHost interface {
	// ID 返回本 agent 的标识（授权判定与日志用）。
	ID() types.AgentID
	// IsChildAgent 报告本 agent 是否为子 agent（层级事实，读 ParentID）。
	IsChildAgent() bool
	// ToolParallelSafe 报告工具是否可并发（读**声明**，不猜）。
	ToolParallelSafe(name string) bool
	// ArchiveBeforeWrite 是**写前留档**（受保护路径被覆盖前先备份）。
	ArchiveBeforeWrite(path string) (bool, error)
	// IO 返回通道/设备数据面（io 工具的执行出口）。
	IO() *agentIO.IOManager
	// StageHost 返回插件工具面（nil = 未装配插件宿主）。
	StageHost() *kit.StageHostType
	// SecurityDecisionFor 判"这个工具准不准用"（安全模块唯一判定点）。
	SecurityDecisionFor(tool string) model.Decision
	// CheckPath 判沙箱路径。
	CheckPath(direction, path string) model.Decision
	// CheckNetwork 判网络闸。
	CheckNetwork() model.Decision
	// CheckOutputGate 判输出通道 / 设备授权。
	CheckOutputGate(channel string) bool
	// SecretStore 返回变量库（入向回填 + 出向脱敏）。
	SecretStore() kernelapi.SecretStore
	// ReviewToolResultContent 做出向内容审查（工具结果的注入检测）。
	ReviewToolResultContent(tool, text string) []model.Finding
	// RegisterToolCancel / UnregisterToolCancel 让"停止"能取消正在跑的工具。
	RegisterToolCancel(cancel context.CancelFunc) uint64
	UnregisterToolCancel(id uint64)
	// RunToolStage 在内核执行链里跑一个工具阶段（同步），返回处理结果。
	RunToolStage(stage sdk.Stage, tc agentAPI.ToolCall, result *sdk.ToolResult) (bool, *sdk.StageContext)
	// Logf 记日志（设施不持有 logger）。
	Logf(format string, args ...any)
}

// ToolHost 返回本 agent 作为**工具执行宿主**（实现 AgentHandleFace）。
//
// 这是设施与基座之间唯一的接线点：设施按 `ToolHost` 的能力集调用，
// 基座提供自己。任何类型都通过 `Agent.RunToolBatch` 隐式传它。
func (a *Agent) ToolHost() ToolHost {
	if a == nil {
		return nil
	}
	var h ToolHost = a
	return h
}

// ToolParallelSafe 报告工具是否可并发执行。
//
// 判据只读**声明**（`ParallelSafe && !Serial`），三处按序问：
//
//	toolReg    插件工具（ConcurrencySafeOf，不拷贝结构体）
//	io         设备/通道工具
//	registry   内核侧登记的工具（含类型私有族的归属声明）
//
// 都查不到 ⇒ 保守串行（false）。"没声明"不等于"安全"。
func (a *Agent) ToolParallelSafe(name string) bool {
	if a == nil {
		return false
	}
	if tr := a.ToolRegistry(); tr != nil {
		if safe, found := tr.ConcurrencySafeOf(name); found {
			return safe
		}
	}
	if ioM := a.IO(); ioM != nil {
		if def, ok := ioM.ToolDefOf(name); ok {
			return def.ParallelSafe && !def.Serial
		}
	}
	if def, ok := registry.LookupTool(name); ok {
		return def.Parallel
	}
	return false
}

// ── 宿主契约的基座实现（薄转发到安全模块与 io，不含任何判定逻辑）──

// CheckPath 判沙箱路径（转安全模块；未装配 ⇒ 放行，与 CheckTool 同语义）。
func (a *Agent) CheckPath(direction, path string) model.Decision {
	if g := a.Gate(); g != nil {
		return g.CheckPath(direction, path)
	}
	return model.Decision{Allowed: true}
}

// CheckNetwork 判网络闸（转安全模块；未装配 ⇒ 放行）。
func (a *Agent) CheckNetwork() model.Decision {
	if g := a.Gate(); g != nil {
		return g.CheckNetwork()
	}
	return model.Decision{Allowed: true}
}
