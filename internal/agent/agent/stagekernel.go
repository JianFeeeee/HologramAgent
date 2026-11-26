package agent

// stagekernel.go —— **阶段执行原语**（基座侧）。
//
// ★ 为什么在基座：阶段执行是"发事件 + 转给插件面"，两条都是内核设施
//   （EventBus 与 StageHost 挂在 kstate 上）。基座不需要认识任何类型：
//   谁的阶段处理器注册在 StageHost 里，就由 StageHost 调它。
//
// ★ 为什么不是方法：它不读 agent 的私有字段（通道从 StageContext 取），
//   做成包级函数，任何类型都能用、基座也不持有 stageHost 句柄。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"log"
	"runtime/debug"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// RunStage 执行一个阶段（返回阶段是否产生了响应）。
//
// ## 分层调度（用户裁定 2026-11-19）
//
// SDK 的 7 个 stage 分属 4 个层（见 stagehooks.go 的 stageLayers）：
//
//	LayerInput    on_input                    整条输入一次
//	LayerLLM      pre_action / post_action    一轮一次
//	LayerTool     before/after_toolcall       **内核驱动**，每个工具一次
//	                                          （不在 agentloop 里跑，见 DriverOfStage）
//	LayerOutput   before/after_output         整条输出一次
//
// 层决定"基座一次跑几个 ctx"；**层内**按 Order 依次跑（后跑的看到前面的结果）。
//
// ## 执行顺序（基座只按层驱动，不解释业务）
//
//	① 本类型的**阶段钩子**（按 Order 依次）
//	② 插件面的阶段处理器（`stageHost`，外部能力）
//	③ 发 `EventStage` 进度事件（订阅方是 cli / webui）
//
// 类型在先：类型持有上下文哲学（裁剪/记忆/渲染），它决定了插件看到的现场；
// 插件是观察者与外部能力，不该反向决定类型的上下文。
//
// ★ 任何 stage 都被**拦下**（ctx.Response 非空）⇒ 不再进插件、不再往下走。
func (a *Agent) RunStage(stage sdk.Stage, ctx *sdk.StageContext) bool {
	if a == nil || ctx == nil {
		return false
	}
	if DriverOfStage(stage) == DrivenByKernel {
		// ★ 工具类阶段由**内核工具执行链**驱动，不由 agentloop 发起。
		//
		//   agentloop 提交批请求（`RunToolBatch`），内核在每个工具前后各驱动
		//   一次 before/after_toolcall —— 那样工具阶段与授权闸、设备授权、
		//   结果清洗在同一条链上，不会出现"agentloop 也驱一遍"的双重触发。
		a.Logf("stage %q is kernel-driven (tool layer); agentloop must not run it", stage)
		return false
	}
	ctx.Phase = stage

	// ① 类型钩子（快照出锁后依次执行）。
	runStageHooks(a.stageHooksFor(stage), ctx)
	if ctx.Response != nil {
		// 类型拦下了这个阶段（例如 must-not-proceed），插件不再介入。
		return true
	}

	// ② 插件面（外部能力）。
	a.runPluginStages(stage, ctx)

	// ③ 进度事件。
	return ctx.Response != nil
}

// runPluginStages 把阶段转给**插件面**（外部能力）。
//
// ★ 两条调用路径共用它：`RunStage`（类型驱动）与 `RunKernelDrivenStage`
//
//	（内核工具链驱动）。插件面不该因为有两条入口就各写一份 —— 那会让
//	"插件在工具阶段收不到事件"只在其中一条路上发生。
func (a *Agent) runPluginStages(stage sdk.Stage, ctx *sdk.StageContext) {
	host := a.stageHost
	if host == nil {
		host = StageHostOf()
	}
	if host == nil {
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[agent] stage %q plugin panic: %v\n%s", stage, r, debug.Stack())
			}
		}()
		host.RunStage(stage, ctx)
	}()
}

// RunKernelDrivenStage 是**内核工具执行链**驱动工具阶段的入口。
//
// ## 为什么需要独立入口（而不是让 RunStage 放行）
//
// `RunStage` 是**类型/agentloop 发起**的入口：工具阶段在那里必须被拒绝
// （agentloop 不该自己跑工具阶段，否则与内核执行链双重触发）。
//
// 而工具阶段的真实触发点在内核链里 —— 内核在真正执行每个工具的前后，
// 手里握着"正在执行的是哪个工具、参数是什么、结果是什么"，那一刻把事实
// 推给注册方（类型钩子 + 插件面）。这条路径只能由**内核**调，所以是独立
// 入口：调用者身份不同，语义就不同。
//
// 返回阶段是否被拦下（`ctx.Response` 非空）。
func (a *Agent) RunKernelDrivenStage(stage sdk.Stage, ctx *sdk.StageContext) bool {
	if a == nil || ctx == nil {
		return false
	}
	if DriverOfStage(stage) != DrivenByKernel {
		// 只有工具层阶段由内核链驱动。其余阶段由调用方经 `RunStage` 发起 ——
		// 走错入口是装配/接线错误，响亮记下而不是静默跑。
		a.Logf("stage %q is not kernel-driven; RunKernelDrivenStage must only be used for the tool layer", stage)
		return false
	}
	ctx.Phase = stage
	runStageHooks(a.stageHooksFor(stage), ctx)
	if ctx.Response != nil {
		return true
	}
	a.runPluginStages(stage, ctx)
	return ctx.Response != nil
}

// ToolRegistryOf 取内核的**插件工具登记簿**（装配期用；未装配 ⇒ nil）。
func ToolRegistryOf() *kit.ToolRegistry {
	if ks := kstate.Get(); ks != nil {
		if impl, ok := ks.Require(kernelapi.FacToolReg); ok {
			if tr, _ := impl.(*kit.ToolRegistry); tr != nil {
				return tr
			}
		}
	}
	return nil
}

// StageHostOf 取内核的**阶段管道**（装配期用；未装配 ⇒ nil）。
func StageHostOf() *kit.StageHostType {
	if ks := kstate.Get(); ks != nil {
		if impl, ok := ks.Require(kernelapi.FacStageHost); ok {
			if host, _ := impl.(*kit.StageHostType); host != nil {
				return host
			}
		}
	}
	return nil
}

// StageHost 返回基座**持有**的阶段管道（未装配 ⇒ nil）。
func (a *Agent) StageHost() *kit.StageHostType {
	if a == nil {
		return nil
	}
	return a.stageHost
}

// ToolRegistry 返回**插件工具登记簿**（装配期由内核注入，未注入 ⇒ nil）。
//
// ★ 与 `StageHost()` 的分工（用户裁定 2026-11-25）：
//
//	stageHost   阶段管道（RegisterStage / RunStage）—— 跑的是"某一轮的前后动作"
//	toolReg     工具登记与执行 —— 持有的是"一个可被模型调用的名字 + schema"
//
// 两者此前塞在同一个类型里，于是"工具表由谁产"永远讲不清。
func (a *Agent) ToolRegistry() *kit.ToolRegistry {
	if a == nil {
		return nil
	}
	return a.toolReg
}

// MountToolRegistry 把插件工具登记簿挂到本实例（装配期一次）。
func (a *Agent) MountToolRegistry(tr *kit.ToolRegistry) {
	if a == nil {
		return
	}
	a.semanticMu.Lock()
	a.toolReg = tr
	a.semanticMu.Unlock()
}
