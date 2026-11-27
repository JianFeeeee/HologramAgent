package kit

// stagehost.go —— **阶段管道**（`StageHostType`）。
//
// ## 它只是阶段管道
//
// 用户裁定 2026-11-25：
//
//	「stagehost 是工具啊？你好好看看 stagehost 到底是什么」
//	「tool 应该是一个独立的模块，与 stagehost 在内核通过 kstate 协作」
//
// 此前本类型同时是三样东西：阶段管道 + 工具登记簿 + 凭证回填。
// 它们唯一的共同点是"插件注册进来的"，而不是同一种能力 ——
// 阶段管道跑的是"某一轮的前置/后置动作"，工具登记是"一个可被模型
// 调用的名字 + schema + 执行体"。
//
// 工具那部分已搬到 `toolreg.go`（`ToolRegistry`）。本文件只管：
//
//	RegisterStage / RegisterStageFor   登记
//	UnregisterPluginStages             摘除（插件卸载/崩溃）
//	RunStage                           并行执行

import (
	"log"
	"sync"

	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// StageHostType 是阶段管道的持有者（每个 agent 一份）。
type StageHostType struct {
	// frozenFn 报告内核是否处于伪关机（迁移中）。nil = 永不冻结。
	// 用函数而不是直接引用 plugin.Registry，避免 agent/core 反向依赖 plugin 包。
	frozenFn func() bool

	mu     sync.RWMutex
	stages map[sdk.Stage][]stageEntry
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

// NewStageHost 构造一个空的阶段管道。
func NewStageHost() *StageHostType {
	return &StageHostType{
		stages: make(map[sdk.Stage][]stageEntry),
	}
}

// RegisterStage 登记一个不属于任何插件的 stage handler（内核自身注册）。
func (h *StageHostType) RegisterStage(stage sdk.Stage, handler sdk.StageHandler) {
	h.RegisterStageFor("", stage, handler)
}

// RegisterStageFor 登记带归属插件名的 stage handler。
// plugin 为空时等同 RegisterStage（内核自身注册的 handler，不参与成组摘除）。
func (h *StageHostType) RegisterStageFor(plugin string, stage sdk.Stage, handler sdk.StageHandler) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stages[stage] = append(h.stages[stage], stageEntry{plugin: plugin, fn: handler})
}

// UnregisterPluginStages 摘除某插件注册的全部 stage handler，返回摘除数量。
//
// 与 `ToolRegistry.UnregisterPluginTools` 成对：卸载/重载/崩溃时两者都得做，
// 否则插件的工具没了但 stage handler 还在，继续打向不存在的插件。
func (h *StageHostType) UnregisterPluginStages(pluginName string) int {
	if h == nil || pluginName == "" {
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

// RunStage 并行调用同阶段所有注册的处理函数。
//
// 各 handler 共享 *StageContext，通过其内置 RWMutex 安全读写：
//   - 只读操作先调用 ctx.RLock() / defer ctx.RUnlock()
//   - 写操作（如设置 ctx.Response）先调用 ctx.Lock() / defer ctx.Unlock()
//
// 如果任意 handler 设置了 Response，后续 handler 可通过 ctx.IsResponded() 判断后提前返回。
// handler 返回的 error 会被收集到 ctx.Errors 中并记录日志，不会中断其他 handler 的执行。
func (h *StageHostType) RunStage(stage sdk.Stage, ctx *sdk.StageContext) {
	if h == nil {
		return
	}
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

// SetFrozenFn 注入"内核是否处于伪关机"的判定（装配期一次）。
func (h *StageHostType) SetFrozenFn(fn func() bool) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.frozenFn = fn
	h.mu.Unlock()
}
