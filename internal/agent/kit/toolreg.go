package kit

// toolreg.go —— **工具登记簿与执行**（独立于阶段管道）。
//
// ## 为什么从 StageHostType 里拆出来
//
// 用户裁定 2026-11-25：
//
//	「tool 应该是一个独立的模块，与 stagehost 在内核通过 kstate 协作」
//	「tool 和 stagehost 耦合是什么意思」
//
// 此前 `StageHostType` 一个类型同时是三样东西：
//
//	阶段管道     RegisterStage / RunStage / stages
//	工具登记簿   RegisterTool / tools / toolDefs / ToolDef / ExecuteTool
//	凭证回填     SetSecretStore / FillMap
//
// 它们唯一的共同点是"插件注册进来的东西"，而不是同一种能力：
//
//	阶段管道  跑的是"某一轮的前置/后置动作"，不产出工具、不被模型看见
//	工具登记  ���的是"一个可被模型调用的名字 + schema + 执行体"
//
// 结果就是上一轮一次误改（在渲染面拼 `ToolsFor` 造成重名）之后，
// 想讲清"工具表由谁产"反而讲不清 —— 因为登记簿寄生在阶段管道上。
//
// ## 现在的关系
//
//	ToolRegistry    工具登记 + 执行（本文件）
//	StageHostType   只做阶段管道（stagehost.go）
//	两者在**内核装配点**通过 kstate 协作，谁也不持有谁。

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sync"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// ToolRegistry 是**插件工具**的登记簿与执行面。
//
// ★ 它只管"插件注册进来的工具"。基座自己的工具面（内核族 / IO 设备 /
//
//	registry 表）分别有各自的来源 —— 那三者由 `agent.PublicToolDefs` 合并，
//	本类型不参与。
type ToolRegistry struct {
	mu sync.RWMutex

	tools      map[string]sdk.ToolHandler
	defs       []sdk.ToolDef
	plugins    map[string]string
	cancelable map[string]bool

	// secretStore 是**变量库**（凭据别名），用于插件工具参数的回填。
	//
	// ★ 注入字段而不是包级单例（2026-11-04）：单例在正常装配下"恰好"同一份，
	//   但测试替身 / 多 Runtime / 热重载任一路径都可能分离，症状是
	//   "策略生效但回填不生效"，极难定位。
	secretMu sync.RWMutex
	secretV  kernelapi.SecretStore
}

// NewToolRegistry 构造一个空的工具登记簿。
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools:      make(map[string]sdk.ToolHandler),
		plugins:    make(map[string]string),
		cancelable: make(map[string]bool),
	}
}

// SetSecretStore 注入变量库（装配期由 Agent 调用）。
func (r *ToolRegistry) SetSecretStore(s kernelapi.SecretStore) {
	if r == nil {
		return
	}
	r.secretMu.Lock()
	r.secretV = s
	r.secretMu.Unlock()
}

// secretStore 返回注入的变量库（未注入为 nil）。
func (r *ToolRegistry) secretStore() kernelapi.SecretStore {
	if r == nil {
		return nil
	}
	r.secretMu.RLock()
	defer r.secretMu.RUnlock()
	return r.secretV
}

// RegisterTool 登记一个插件工具。
func (r *ToolRegistry) RegisterTool(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
	if r == nil {
		return fmt.Errorf("toolreg: 未初始化")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool %s already registered", name)
	}
	if def.Plugin == "" {
		def.Plugin = inferToolPlugin(name)
	}
	r.tools[name] = handler
	r.plugins[name] = def.Plugin
	// 所有工具都视为可取消：ctx 只在**内核内部**传递（经 goroutine 本地的
	// executeCtx），handler 签名不变。对不消费 ctx 的执行体而言这只是个
	// 空操作，不存在"误取消"的风险。
	r.cancelable[name] = true
	r.defs = append(r.defs, def)
	return nil
}

// GetToolDefs 返回全部工具声明的**副本**。
func (r *ToolRegistry) GetToolDefs() []sdk.ToolDef {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]sdk.ToolDef, len(r.defs))
	copy(out, r.defs)
	return out
}

// ToolDef 返回某个工具声明的**副本**（不存在 ⇒ nil）。
//
// 只需"是否存在 + 声明项"的热路径应改用 ConcurrencySafeOf / HasTool，
// 避免结构体拷贝。需要完整声明（如 Cleaner）才用本方法。
func (r *ToolRegistry) ToolDef(name string) *sdk.ToolDef {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if def, ok := r.findLocked(name); ok {
		return &def
	}
	return nil
}

// ConcurrencySafeOf 报告工具是否可并发执行，**不做结构体拷贝**。
func (r *ToolRegistry) ConcurrencySafeOf(name string) (safe, found bool) {
	if r == nil {
		return false, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.findLocked(name)
	if !ok {
		return false, false
	}
	return def.ParallelSafe && !def.Serial, true
}

// NoMemoryOf 报告工具是否声明 NoMemory，同样不拷贝。
func (r *ToolRegistry) NoMemoryOf(name string) (v, found bool) {
	if r == nil {
		return false, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.findLocked(name)
	if !ok {
		return false, false
	}
	return def.NoMemory, true
}

// HasTool 报告工具是否存在，不拷贝。
func (r *ToolRegistry) HasTool(name string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.findLocked(name)
	return ok
}

// findLocked 必须在持有 r.mu 时调用。
func (r *ToolRegistry) findLocked(name string) (sdk.ToolDef, bool) {
	for _, def := range r.defs {
		if def.Name == name {
			return def, true
		}
	}
	return sdk.ToolDef{}, false
}

// ToolPlugin 返回工具归属的插件名。
func (r *ToolRegistry) ToolPlugin(name string) string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.plugins[name]
}

// ToolCount 返回登记的工具总数。
func (r *ToolRegistry) ToolCount() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.defs)
}

// NoMemoryToolNames 返回声明了 NoMemory 的全部工具名。
func (r *ToolRegistry) NoMemoryToolNames() map[string]bool {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]bool)
	for _, def := range r.defs {
		if def.NoMemory {
			out[def.Name] = true
		}
	}
	return out
}

// ToolDefCleaner 返回工具声明的 Cleaner（无 ⇒ nil）。
func (r *ToolRegistry) ToolDefCleaner(name string) func(string) string {
	def := r.ToolDef(name)
	if def == nil {
		return nil
	}
	return def.Cleaner
}

// UnregisterPluginTools 摘除某插件登记的全部工具。
func (r *ToolRegistry) UnregisterPluginTools(pluginName string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var keep []sdk.ToolDef
	for _, def := range r.defs {
		if def.Plugin == pluginName {
			delete(r.tools, def.Name)
			delete(r.plugins, def.Name)
			delete(r.cancelable, def.Name)
		} else {
			keep = append(keep, def)
		}
	}
	r.defs = keep
}

// ExecuteTool 执行工具（无取消语义）。
func (r *ToolRegistry) ExecuteTool(name string, args map[string]interface{}) (ret interface{}, err error) {
	return r.ExecuteToolCtx(context.Background(), name, args)
}

// ExecuteToolCtx 执行工具，并把 ctx 交给**内核侧的执行体**。
//
// ctx 沿两条路生效，都不需要插件改代码：
//   - 插件工具（proc 桥）：经 goroutine 本地 ctx 传给 CallContext，
//     取消时向插件进程发 call.cancel；
//   - io 设备工具：同样经 goroutine 本地 ctx 下传。
//
// handler 本身仍是旧签名；它若不消费 ctx，行为与今天逐字一致。
func (r *ToolRegistry) ExecuteToolCtx(ctx context.Context, name string, args map[string]interface{}) (ret interface{}, err error) {
	if r == nil {
		return nil, agentIO.ToolNotFound(name)
	}
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[stage] tool %s handler panic: %v\n%s", name, rec, debug.Stack())
			err = fmt.Errorf("tool %s handler panic: %v", name, rec)
		}
	}()
	r.mu.RLock()
	handler, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		// 类型化哨兵：工具是动态注册的，调用方需要能**精确**区分
		// 「不存在」（插件挂了吗）与「执行失败」（本次业务失败）—— 二者对
		// on_error/retry 的处置完全不同。
		return nil, agentIO.ToolNotFound(name)
	}
	if handler == nil {
		return nil, fmt.Errorf("tool %s has nil handler", name)
	}
	// ★ 安全模块入向回填：这里是**插件工具调用的唯一入口**，
	// 填在这里就覆盖了全部插件 —— 插件本身完全无感知。
	//
	// 为何必须填而不是拒绝：模型手上只有别名（它从未见过明文），
	// 而插件需要真凭据。回填是二者之间唯一的通道。
	if s := r.secretStore(); s != nil && args != nil {
		if n, used := s.FillMap(args); n > 0 {
			log.Printf("[security] tool %s backfilled %d argument(s) (aliases %v)", name, n, used)
		}
	}
	// 把 ctx 放进**执行栈**而不是 handler 签名：proc 桥在自己的 CallContext
	// 里用 CurrentExecuteCtx 取它。这样取消能力只在内核内部流动，
	// 插件作者的 handler 签名与源码一行不变。
	return handler(withExecuteCtx(ctx, args))
}

// ── 执行上下文旁路 ──

// executeCtxArgKey 是**内核内部**传递执行上下文的私有键（放在 args map 里旁路传）。
//
// ⚠️ 必须与 `internal/plugin/proc` 的 `execCtxArgKey` **逐字一致**：
// 那两处是同一个约定，任一侧改了另一侧就静默失效（症状是"取消不生效"，
// 工具照旧跑完 —— 不是功能错误，所以最难发现）。
const executeCtxArgKey = "\x00kernel.execute_ctx\x00"

// withExecuteCtx 把 ctx 绑定到本次调用上，随 goroutine 走。
//
// 实现见 proc/execctx.go：Go 没有 goroutine-local storage，这里用
// **参数旁路**（在 args 里放一个不可见的私有键）把 ctx 带进执行栈。
//
// ★ 必须**复制**而不是原地写：
//
//	调用方（executeToolCallInner）收到的是 taskFrame 上那份 tc.Arguments，
//	而工具执行在自己的 goroutine 里跑。超时/取消路径下，主 goroutine 会在
//	goroutine 仍存活时继续读同一份 args —— 原地写就是数据竞争。
//	-race 下实测复现（且 core 包整体被判红，掩盖其它真正的竞争）。
//
//	复制后：原 map 只读、新 map 一 goroutine 一份，才真的"天然无并发歧义"。
//	代价是一次浅拷贝（工具参数本来就在 64KB 预算内，可忽略）。
func withExecuteCtx(ctx context.Context, args map[string]interface{}) map[string]interface{} {
	if ctx == nil {
		return args
	}
	out := make(map[string]interface{}, len(args)+1)
	for k, v := range args {
		out[k] = v
	}
	out[executeCtxArgKey] = ctx
	return out
}

// ── 内部小工具 ──

func inferToolPlugin(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '_' {
			return name[:i]
		}
	}
	return ""
}
