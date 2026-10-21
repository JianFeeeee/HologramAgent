package core

import (
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

func (h *StageHost) ExecuteTool(name string, args map[string]interface{}) (ret interface{}, err error) {
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
	return handler(args)
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
