package agent

// toolexec.go —— **工具执行设施**的契约面（内核实现，基座转发）。
//
// ★ 分层（用户裁定）：
//
//	类型 agentloop   提交批请求（何时、哪些工具、什么约束）
//	基座             一次转发（不认识工具语义）
//	内核执行设施     路由 → 安全闸 → device 授权 → 并发判据 → 返回值清洗
//
// 工具执行链归内核，是因为它需要跨 agent 的通道/设备授权与统一清洗，
// SDK 做不到（§1.2 的判据）。

import (
	"context"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// ToolExecutor 是内核工具执行链的入口。
//
// ## 谁实现它
//
// `internal/kernelmod/toolexec`（内核设施模块），装配时
// `ks.Provide(kernelapi.FacToolExec, impl)`。基座从挂载点取它并转发。
//
// ## 为什么契约在基座而不是 kernelapi
//
// `kernelapi` 是叶子层，而契约值里带 `sdk.ToolDef`，`sdk` 又 import `kernelapi`
// —— 放进去直接成环。基座本来就 import sdk，所以契约放在这里零代价；
// 而 `agent` 不被任何内核模块 import（`kernelmod/toolexec` import 它是合法方向）。
//
// ## 为什么不用 `any` 传参
//
// 试过：`Execute(ctx, calls any, opts any) any` + 装配侧断言。
// 那是把**类型安全**换成"运行期断言失败"，而调用点只有一处 —— 不值得。
type ToolExecutor interface {
	// Execute 执行一批工具调用，返回与入参**同序**的结果。
	//
	// ★ 只有三个参数（2026-11-27 收敛）：`ctx` 取消域、`h` 实例句柄、
	//   `calls` 要执行什么。此前还有 `BatchOpts` 的 7 个字段，其中 4 个是
	//   死字段或逃逸（见 kernelmod/device/toolexec_batch.go 的注释）。
	Execute(ctx context.Context, h AgentHandleFace, calls []ToolCall) []BatchResult
}

// AgentHandleFace 是执行链需要的**实例句柄**（由基座实现）。
//
// 只含执行链替代不了的四样：身份、取消登记、交回基座工具、工具阶段。
// 安全闸 / io / 插件工具表 / 工具声明全部由 device 从设施自取。
type AgentHandleFace interface {
	AgentID() string
	IsChild() bool
	DialTool(ctx context.Context, call ToolCall) (string, bool)
	RegisterToolCancel(cancel context.CancelFunc) uint64
	UnregisterToolCancel(id uint64)
	RunToolStage(stage sdk.Stage, tc ToolCall, result *sdk.ToolResult) (bool, *sdk.StageContext)
	// ArchiveBeforeWrite 在写操作前归档文件（受保护路径的可回滚基线）。
	//
	// ★ 为什么归实例而不是设施：归档目录是**本实例的 dataDir**
	//   （`<data>/agents/<type>/<id>/file_baseline/`），设施不知道它是谁。
	ArchiveBeforeWrite(path string) (bool, error)
}

// ToolExecOf 向内核取**工具执行设施**（未装配 ⇒ nil）。
//
// 与 `ToolRegistryOf` / `StageHostOf` 同一形态：基座按**具名设施**取内核组件。
//
// ★ 为什么必须是"取"而不是"装配点塞"（2026-11-26 压测实测）：
//
//	装配点只给根实例调了一次 `SetToolExec`，而子实例由实例管理器造、
//	没有那一次调用 ⇒ 子 agent 的工具执行设施恒 nil ⇒ 一调工具就报
//	「工具执行设施未装配」。压测现场：三个驻留子被派活后 9 次工具调用
//	全部未执行，而主 agent 一切正常。
//
//	设施在内核里是**一份**（无状态纯逻辑，实例句柄随调用传入），
//	所以"每个实例按名取"与"只有一个实例"不矛盾。
func ToolExecOf() ToolExecutor {
	ks := kstate.Get()
	if ks == nil {
		return nil
	}
	if impl, ok := ks.Require(kernelapi.FacToolExec); ok {
		if te, _ := impl.(ToolExecutor); te != nil {
			return te
		}
	}
	return nil
}
