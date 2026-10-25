package core

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sync"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/internal/security"
)

type StageHost struct {
	// frozenFn 报告内核是否处于伪关机（迁移中）。nil = 永不冻结。
	// 用函数而不是直接引用 plugin.Registry，避免 agent/core 反向依赖 plugin 包。
	frozenFn    func() bool
	mu          sync.RWMutex
	toolDefs    []sdk.ToolDef
	tools       map[string]sdk.ToolHandler
	toolPlugins map[string]string
	stages      map[sdk.Stage][]stageEntry

	// cancelable 记录本工具的执行体是否**接收取消信号**。
	//
	// 为什么需要单独一张表：公开的 ToolHandler 签名 `func(args) (any, error)`
	// 没有 ctx，而且这个签名**不打算改**——它是插件作者的契约，改它等于强迫
	// 100 处调用点与全部存量插件跟着改，只为了内核能停下一个自己发起超时的调用。
	//
	// 真正需要 ctx 的只有**内核与插件之间的那一层**：proc 桥把 ctx 传到
	// CallContext，由它向子进程发 call.cancel。插件进程内部的 handler 仍然
	// 是旧签名，取消由模板运行时接住（让阻塞中的 handler 拿到 context.Canceled）。
	//
	// 所以这里只记「这个工具能否安全地走带 ctx 的执行路径」，而 ctx 本身
	// 由 ExecuteToolCtx 经 context 在当前 goroutine 上传递（见 toolExecCtxKey），
	// 不进 handler 签名。
	cancelable map[string]bool
}

// stageEntry 把 stage handler 与它的归属插件绑定。
//
// 为何需要归属：子进程插件崩溃后，它注册的 handler 闭包仍在这张表里，
// 每次 RunStage 都会经 RPC 打向已死进程并报 ErrProcessExited；重启后新 handler
// 又追加进来，旧的永不退场——错误与重复执行随重启次数线性累积。
// 有了归属才能在卸载/崩溃时成组摘除。
type stageEntry struct {
	plugin string
	fn     sdk.StageHandler
}

func NewStageHost() *StageHost {
	return &StageHost{
		tools:       make(map[string]sdk.ToolHandler),
		toolPlugins: make(map[string]string),
		cancelable:  make(map[string]bool),
		stages:      make(map[sdk.Stage][]stageEntry),
	}
}

func (h *StageHost) RegisterTool(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.tools[name]; exists {
		return fmt.Errorf("tool %s already registered", name)
	}
	if def.Plugin == "" {
		def.Plugin = inferToolPlugin(name)
	}
	h.tools[name] = handler
	h.toolPlugins[name] = def.Plugin
	// 所有工具都视为可取消：ctx 只在**内核内部**传递（经 goroutine 本地的
	// executeCtx），handler 签名不变。对不消费 ctx 的执行体而言这只是个
	// 空操作，不存在"误取消"的风险。
	h.cancelable[name] = true
	h.toolDefs = append(h.toolDefs, def)
	return nil
}

func (h *StageHost) RegisterStage(stage sdk.Stage, handler sdk.StageHandler) {
	h.RegisterStageFor("", stage, handler)
}

// RegisterStageFor 注册带归属插件名的 stage handler。
// plugin 为空时等同 RegisterStage（内核自身注册的 handler，不参与成组摘除）。
func (h *StageHost) RegisterStageFor(plugin string, stage sdk.Stage, handler sdk.StageHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stages[stage] = append(h.stages[stage], stageEntry{plugin: plugin, fn: handler})
}

func (h *StageHost) GetToolDefs() []sdk.ToolDef {
	h.mu.RLock()
	defer h.mu.RUnlock()
	defs := make([]sdk.ToolDef, len(h.toolDefs))
	copy(defs, h.toolDefs)
	return defs
}

// ToolDef 返回工具声明的**副本**。
//
// 保留"返回副本"是有意的：ToolDef 里有 map 与函数指针（Cleaner），
// 返回内部切片元素会把可变引用交出去。Go 1.22+ 循环变量每轮独立，
// 所以 &def 不是逃逸漏洞。
//
// 但代价真实存在：结构体含 3 个 string + 2 个 map 头 + 2 个 bool，
// 每次调用都是一次结构体拷贝。而 toolParallelSafe 在**每批**判断里对
// 每个工具各调一次，1000 并发时就是 1000 次拷贝。
//
// 只需"是否存在 + 声明项"的调用方（toolParallelSafe、validateArgsAgainstSchema
// 等热路径）应改用 ConcurrencySafeOf / HasTool，避免拷贝。
// 需要拿到完整声明的（如 Cleaner）才用 ToolDef。
func (h *StageHost) ToolDef(name string) *sdk.ToolDef {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if def, ok := h.findLocked(name); ok {
		return &def
	}
	return nil
}

// ConcurrencySafeOf 报告工具是否可并发执行，**不做结构体拷贝**。
//
// 与 ToolDef(name).ParallelSafe && !Serial 等价，但只读两个 bool 字段。
// 热路径（每批并发判据）必须走这个。
func (h *StageHost) ConcurrencySafeOf(name string) (safe, found bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	def, ok := h.findLocked(name)
	if !ok {
		return false, false
	}
	return def.ParallelSafe && !def.Serial, true
}

// NoMemoryOf 报告工具是否声明 NoMemory，同样不拷贝。
func (h *StageHost) NoMemoryOf(name string) (v, found bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	def, ok := h.findLocked(name)
	if !ok {
		return false, false
	}
	return def.NoMemory, true
}

// HasTool 报告工具是否存在，不拷贝。
func (h *StageHost) HasTool(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.findLocked(name)
	return ok
}

// findLocked 必须在持有 h.mu 时调用。
func (h *StageHost) findLocked(name string) (sdk.ToolDef, bool) {
	for _, def := range h.toolDefs {
		if def.Name == name {
			return def, true
		}
	}
	return sdk.ToolDef{}, false
}

// executeCtxKey 是**内核内部**传递执行上下文的私有键（放在 args map 里旁路传）。
//
// 为什么不把 ctx 加进 ToolHandler 签名（2026-10-25 的架构决定）：
//
//	公开签名 `func(args) (any, error)` 是插件作者的契约。为了内核能停下一个
//	自己超时的调用而改它是本末倒置——要改 100 处调用点 + 全部存量插件，
//	而受益者本应是插件（它们一行都不用动）。
//
//	真正需要 ctx 的只有**内核 ↔ 插件进程**之间那一层：proc 桥要把它传给
//	CallContext，由后者向子进程发 call.cancel。这一段完全在内核内部，
//	用 args 上的私有键旁路传递即可，无需污染公开 API。
//
// 为什么键是这种形态：args 是 map[string]interface{}，键只能取字符串。
// 用不可见前缀 + 极不日常的名字，保证它不会被真实工具参数撞上；同时
// ExecuteTool 在**下发给插件前**会把它剥掉（见 stripExecuteCtx），
// 所以插件永远看不到它，也不可能因为多出一个键而报参数校验失败。
const executeCtxArgKey = "\x00kernel.execute_ctx\x00"

// CurrentExecuteCtx 取出本次工具调用的执行上下文（proc 桥用）。
//
// 没有绑定时返回 context.Background()，调用方可直接使用、无需判空。
func CurrentExecuteCtx(args map[string]interface{}) context.Context {
	if args != nil {
		if ctx, ok := args[executeCtxArgKey].(context.Context); ok && ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

// stripExecuteCtx 返回去掉执行上下文键的**副本**，供下发给插件/设备用。
//
// 为什么必须剥：插件侧可能对参数做严格校验（"未知参数"报错），
// 而这个键是内核内部约定，不该出现在任何插件眼里。
func stripExecuteCtx(args map[string]interface{}) map[string]interface{} {
	if args == nil {
		return nil
	}
	if _, ok := args[executeCtxArgKey]; !ok {
		return args
	}
	out := make(map[string]interface{}, len(args))
	for k, v := range args {
		if k == executeCtxArgKey {
			continue
		}
		out[k] = v
	}
	return out
}

// ExecuteTool 执行工具（无取消语义）。
func (h *StageHost) ExecuteTool(name string, args map[string]interface{}) (ret interface{}, err error) {
	return h.ExecuteToolCtx(context.Background(), name, args)
}

// ExecuteToolCtx 执行工具，并把 ctx 交给**内核侧的执行体**。
//
// ctx 沿两条路生效，都不需要插件改代码：
//   - StageHost 自己的工具（proc 插件桥）：经 goroutine 本地 ctx 传给
//     CallContext，取消时向插件进程发 call.cancel；
//   - io 设备工具：同样经 goroutine 本地 ctx 下传（见 ExecuteTool 的调用方）。
//
// handler 本身仍是旧签名；它若不消费 ctx，行为与今天逐字一致。
func (h *StageHost) ExecuteToolCtx(ctx context.Context, name string, args map[string]interface{}) (ret interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[stage] tool %s handler panic: %v\n%s", name, r, debug.Stack())
			err = fmt.Errorf("tool %s handler panic: %v", name, r)
		}
	}()
	h.mu.RLock()
	handler, ok := h.tools[name]
	h.mu.RUnlock()
	if !ok {
		// 类型化哨兵：工具是动态注册的，调用方需要能**精确**区分
		// 「不存在」（插件挂了吗）与「执行失败」（本次业务失败）—— 二者对
		// on_error/retry 的处置完全不同。见 agentIO.ErrToolNotFound。
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
	if s := security.Global(); s != nil && args != nil {
		if n, used := s.FillMap(args); n > 0 {
			log.Printf("[security] 工具 %s 参数回填 %d 处（别名 %v）", name, n, used)
		}
	}
	// 把 ctx 放进**执行栈**而不是 handler 签名：proc 桥在自己的 CallContext
	// 里用 CurrentExecuteCtx 取它。这样取消能力只在内核内部流动，
	// 插件作者的 handler 签名与源码一行不变。
	//
	// 注意：这里不能用「把 ctx 存在 StageHost 字段里」——同一插件可被
	// 多个调用并发执行，共享字段会互相覆盖（就是 qq 插件花大力气修过的
	// 那个"全局身份不可靠"问题）。
	return handler(withExecuteCtx(ctx, args))
}

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

func (h *StageHost) ToolPlugin(name string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.toolPlugins[name]
}

func (h *StageHost) UnregisterPluginTools(pluginName string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	var keepDefs []sdk.ToolDef
	for _, def := range h.toolDefs {
		if def.Plugin == pluginName {
			delete(h.tools, def.Name)
			delete(h.toolPlugins, def.Name)
		} else {
			keepDefs = append(keepDefs, def)
		}
	}
	h.toolDefs = keepDefs
}

// UnregisterPluginStages 摘除某插件注册的全部 stage handler，返回摘除数量。
//
// 与 UnregisterPluginTools 成对：卸载/重载/崩溃时两者都得做，
// 否则插件的工具没了但 stage handler 还在，继续打向不存在的插件。
func (h *StageHost) UnregisterPluginStages(pluginName string) int {
	if pluginName == "" {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	removed := 0
	for stage, entries := range h.stages {
		keep := entries[:0:0]
		for _, e := range entries {
			if e.plugin == pluginName {
				removed++
				continue
			}
			keep = append(keep, e)
		}
		if len(keep) == 0 {
			delete(h.stages, stage)
			continue
		}
		h.stages[stage] = keep
	}
	return removed
}

func inferToolPlugin(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '_' {
			return name[:i]
		}
	}
	return ""
}

// RunStage 并行调用同阶段所有注册的处理函数。
// 各 handler 共享 *StageContext，通过其内置 RWMutex 安全读写：
//   - 只读操作先调用 ctx.RLock() / defer ctx.RUnlock()
//   - 写操作（如设置 ctx.Response）先调用 ctx.Lock() / defer ctx.Unlock()
//
// 如果任意 handler 设置了 Response，后续 handler 可通过 ctx.IsResponded() 判断后提前返回。
// handler 返回的 error 会被收集到 ctx.Errors 中并记录日志，不会中断其他 handler 的执行。
func (h *StageHost) RunStage(stage sdk.Stage, ctx *sdk.StageContext) {
	h.mu.RLock()
	entries := make([]stageEntry, len(h.stages[stage]))
	copy(entries, h.stages[stage])
	h.mu.RUnlock()
	if len(entries) == 0 {
		return
	}
	var wg sync.WaitGroup
	errCh := make(chan error, len(entries))
	var errChOnce sync.Once
	for _, entry := range entries {
		wg.Add(1)
		go func(fn sdk.StageHandler) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[stage] handler panic: %v", r)
				}
			}()
			if err := fn(ctx); err != nil {
				errCh <- err
			}
		}(entry.fn)
	}
	wg.Wait()
	safeclose.Safe(&errChOnce, errCh)

	var errs []string
	for err := range errCh {
		errs = append(errs, err.Error())
		log.Printf("[stage] %s handler error: %v", stage, err)
	}
	if len(errs) > 0 {
		ctx.Lock()
		ctx.Errors = append(ctx.Errors, errs...)
		ctx.Unlock()
	}
}

func (h *StageHost) ToolDefCleaner(name string) func(string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, def := range h.toolDefs {
		if def.Name == name {
			return def.Cleaner
		}
	}
	return nil
}

func (h *StageHost) NoMemoryToolNames() map[string]bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	set := make(map[string]bool, len(h.toolDefs))
	for _, def := range h.toolDefs {
		if def.NoMemory {
			set[def.Name] = true
		}
	}
	return set
}

func (h *StageHost) ToolCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.tools)
}
