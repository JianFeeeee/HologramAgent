package agent

// stagehooks.go —— **阶段钩子槽位 + 分层调度**（基座持有，类型注册）。
//
// ## 用户裁定（2026-11-19，三段话）
//
// > 「stage 钩子默认对所有 agent 生效，怎么处理通过 agent 内部实现；
// >    基座暴露一组回调让 agent 注册收到对应 stage 时的行为，基座保证并发安全」
//
// > 「stage 是面向对应阶段的钩子，有些面向 LLM 中间态、有些面向 toolcall，
// >    需要分层次地依次处理」
//
// > 「有些 stage 是面向工具的，还有 stage 是面向输出的，它们需要被分层化处理」
//
// ## SDK 定义的 7 个 stage → 4 个层次
//
//	sdk.StageOnInput        → LayerInput   整条输入一次
//	sdk.StagePreAction      → LayerLLM     一轮一次（LLM 调用前）
//	sdk.StagePostAction     → LayerLLM     一轮一次（LLM 返回后）
//	sdk.StageOnToolcall     → LayerTool    **每个工具一次**（内核执行链触发）
//	sdk.StageBeforeToolcall → LayerTool    **每个工具一次**（内核执行链触发）
//	sdk.StageAfterToolcall  → LayerTool    **每个工具一次**（内核执行链触发）
//	sdk.StageBeforeOutput   → LayerOutput  整条输出一次
//	sdk.StageAfterOutput    → LayerOutput  整条输出一次
//
// **层决定调度语义**（基座只按层执行，不解释业务）：
//
//	input   一次；可被钩子**拦下**（ctx.Response 非空 ⇒ 不进入后续）
//	llm     一次；钩子可改 LLMText / ReasoningContent / Memory
//	tool    每个工具一份 ctx；scope=own-tools 的钩子只看自己那一族工具
//	output  一次；可被钩子**拦下**（before_output 的 Response 非空 ⇒ 不发送）
//
// ## 层次内的顺序（Order）
//
// 同一 stage 内多个注册者按 `Order` 升序依次跑（同 Order 按注册顺序）：
//
//	OrderPre(0) → OrderDefault(100) → OrderPost(200)
//
// 后跑的看到前面的结果 —— 这正是"依次处理"的落点。
//
// ## 并发安全
//
//	注册表        RWMutex（装配期写、运行期读）
//	调度          **快照后出锁**：拿一份拷贝再跑，不在持锁时跑用户代码
//	StageContext  SDK 自带 mu（RLock/Lock），钩子内改字段自己上锁
//
// ## 基座不认识什么
//
// 它不知道 `pre_action` 该干什么、不知道 before_toolcall 要不要拦。
// 它只知道：这个 stage 属于哪一层、层内怎么排序、怎么并发安全地执行。

import (
	"log"
	"runtime/debug"
	"sort"
	"sync"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 层内顺序（小的先跑）。
const (
	OrderPre     = 0
	OrderDefault = 100
	OrderPost    = 200
)

// StageLayer 是阶段所属的**调度层**（决定"一次跑几个 ctx"）。
type StageLayer string

const (
	// LayerInput：整条输入一次（可拦下）。
	LayerInput StageLayer = "input"
	// LayerLLM：一轮一次（LLM 调用前后）。
	LayerLLM StageLayer = "llm"
	// LayerTool：**每个工具一次**（调用前后各一次，ctx 是那个工具自备的）。
	LayerTool StageLayer = "tool"
	// LayerOutput：整条输出一次（可拦下）。
	LayerOutput StageLayer = "output"
)

// stageLayers 是 SDK 的 7 个 stage → 4 个层的**唯一映射**。
//
// ★ 这是基座对 stage 的**全部理解**：不涉及业务语义，只回答
//
//	"它属于哪一层、由谁驱动"。
var stageLayers = map[sdk.Stage]StageLayer{
	sdk.StageOnInput:        LayerInput,
	sdk.StagePreAction:      LayerLLM,
	sdk.StagePostAction:     LayerLLM,
	sdk.StageOnToolcall:     LayerTool,
	sdk.StageBeforeToolcall: LayerTool,
	sdk.StageAfterToolcall:  LayerTool,
	sdk.StageBeforeOutput:   LayerOutput,
	sdk.StageAfterOutput:    LayerOutput,
}

// StageDriver 声明某个 stage 由**谁驱动**。
type StageDriver int

const (
	// DrivenByAgent：agentloop 在阶段点上发起（输入 / LLM / 输出）。
	DrivenByAgent StageDriver = iota
	// DrivenByKernel：**内核工具执行链**驱动（每个工具各一次）。
	//
	// ★ 为什么工具阶段的驱动不在 agentloop：工具执行链本身就在内核
	//   （toolroute 路由 → security 授权闸 → device 通道授权 → 结果清洗）。
	//   `before_toolcall` / `after_toolcall` 是那条链的**内部**阶段，
	//   由内核在每个工具前后各驱动一次 —— agentloop 只提交批请求、拿回结果。
	DrivenByKernel
)

// DriverOfStage 返回某个 stage 由谁驱动。
func DriverOfStage(stage sdk.Stage) StageDriver {
	if stageLayers[stage] == LayerTool {
		return DrivenByKernel
	}
	return DrivenByAgent
}

// LayerOfStage 返回某个 stage 所属的层（未登记的 stage ⇒ 空）。
func LayerOfStage(stage sdk.Stage) StageLayer { return stageLayers[stage] }

// StageScope 声明钩子的**作用范围**（与 SDK 的 StageScope 同构）。
type StageScope int

const (
	// ScopeGlobal：所有 ctx 都收到（默认）。
	ScopeGlobal StageScope = iota
	// ScopeOwnTools：只在 LayerTool 生效，且只处理**本注册者拥有**的工具。
	//
	// ★ 用于"这个插件/类型只想管自己的工具"：否则它会收到别人的工具调用，
	//   既读到了不该看的参数，也可能改坏别人的结果。
	ScopeOwnTools
)

// StageHook 是一个注册者对一个阶段的处理行为。
type StageHook struct {
	// Name 是注册者名（诊断用）。
	Name string
	// Order 是层内顺序：小的先跑。
	Order int
	// Scope 是作用范围（全局 / 只看自己的工具）。
	Scope StageScope
	// OwnsTool 在 Scope=ScopeOwnTools 时判定"这个工具是不是我的"。
	OwnsTool func(toolName string) bool
	// Fn 是处理体。返回 error 会记进 ctx.Errors（不中断后续钩子）。
	Fn func(ctx *sdk.StageContext) error
}

// stageHookTable 是 stage → 钩子的注册表（并发安全）。
type stageHookTable struct {
	mu   sync.RWMutex
	byID map[sdk.Stage][]StageHook
}

// OnStage 注册一个阶段钩子（装配期调用；同名同阶段被拒）。
func (a *Agent) OnStage(stage sdk.Stage, h StageHook) {
	if a == nil || h.Fn == nil {
		return
	}
	a.stageHooks.mu.Lock()
	defer a.stageHooks.mu.Unlock()
	if a.stageHooks.byID == nil {
		a.stageHooks.byID = map[sdk.Stage][]StageHook{}
	}
	for _, ex := range a.stageHooks.byID[stage] {
		if ex.Name == h.Name {
			a.Logf("stage hook %q on %q already registered — ignored", h.Name, stage)
			return
		}
	}
	a.stageHooks.byID[stage] = append(a.stageHooks.byID[stage], h)
}

// stageHooksFor 取某阶段的钩子快照（按 Order 升序，同 Order 保注册顺序）。
//
// ★ 返回副本：出锁之后才跑用户代码（慢钩子不能阻塞别人注册）。
func (a *Agent) stageHooksFor(stage sdk.Stage) []StageHook {
	if a == nil {
		return nil
	}
	a.stageHooks.mu.RLock()
	src := a.stageHooks.byID[stage]
	out := make([]StageHook, len(src))
	copy(out, src)
	a.stageHooks.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// runStageHooks 依次执行钩子（层内串行；panic 与 error 都收进 ctx.Errors）。
//
// ★ 为什么层内串行：后跑的钩子要看到前面的结果（"依次处理"）。
//
//	层与层之间（LLM 前 / 工具前 / 输出前）本来就是不同的 ctx，天然分开。
func runStageHooks(hooks []StageHook, ctx *sdk.StageContext) {
	for _, h := range hooks {
		if !hookApplies(h, ctx) {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[agent] stage hook %q panic: %v\n%s", h.Name, r, debug.Stack())
					if ctx != nil {
						ctx.Errors = append(ctx.Errors, "hook "+h.Name+" panic")
					}
				}
			}()
			if err := h.Fn(ctx); err != nil && ctx != nil {
				ctx.Errors = append(ctx.Errors, "hook "+h.Name+": "+err.Error())
			}
		}()
	}
}

// hookApplies 判定钩子是否该处理这个 ctx（ScopeOwnTools 的工具过滤）。
func hookApplies(h StageHook, ctx *sdk.StageContext) bool {
	if h.Scope != ScopeOwnTools || ctx == nil {
		return true
	}
	if len(ctx.ToolCalls) == 0 {
		return false
	}
	if h.OwnsTool == nil {
		return false
	}
	// 一个 ctx 可能带多个工具调用（before_toolcall 的批形态）；
	// 只要其中**有一个**属于本注册者，就让它处理（钩子自己按名字再挑）。
	for _, tc := range ctx.ToolCalls {
		if h.OwnsTool(tc.Name) {
			return true
		}
	}
	return false
}
