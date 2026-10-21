package main

// 内核模块装配的执行入口（Group 0 之后调用）。

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"

	// ★ 把全部内核模块**引入编译**（Go 要求 import 才会编译进来）。
	// 加一个新模块 = 新建 internal/kernelmod/<name>/mod.go + 在 all.go 加一行。
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/all"
)

// RunKernelModules 装配全部内核模块（必须在 Group 0 串行段之后调用）。
//
// ## 为什么"加模块零中央清单"在这里成立
//
// 各模块在自己的文件夹里写 `func init() { kernelmod.Register(...) }`，
// 由 `kernelmod/all` 统一引入编译。本函数只问注册表要模块，
// **不认识任何具体模块**，也不含任何依赖清单。
//
//	① 注册表（kernelmod）返回全部已注册模块（**乱序**，顺序无意义）
//	② Build：算依赖图层（缺依赖/环/重名 ⇒ 这里就报错）
//	③ Run：分层执行（同层有界并行），层边界更新 syncbus
//
// kernelAssembleHooks 是装配的**阶段回调**（供 main 在正确时机挂接）。
type kernelAssembleHooks struct {
	// BeforeRun 在**执行任何模块之前**调用（此时 syncbus 已创建、
	// 全部设施已 MarkDeclared，但还没有一个就绪）。
	//
	// ★★ 为什么需要它（一个真实的设计缺口）：
	//
	// 插件调度器若在装配**完成之后**才挂到 syncbus 上，那么它提交插件时
	// 全部设施早已就绪 ⇒ 每个声明了依赖的插件都"立即拉起"
	// ⇒ **「声明提前启动」从未真正生效**（永远退化成同步全就绪）。
	//
	// 实测证据（真机，2026-10-16）：装一个声明 requires_facilities=[config] 的
	// 外部插件，它仍在内核**全部 5 层完成**后才被拉起 —— 因为 stage-host
	// 就绪（第 3 层）时插件注册表才存在，而调度器那时还没接上拉起动作。
	//
	// 有了本回调，调度器在 **Run 之前**订阅 syncbus ⇒ 内核每完成一层
	// 都会触发它重新评估 ⇒ 声明了 config 的插件不必等 agent 层。
	BeforeRun func(bus *kernelboot.SyncBus) error

	// OnLayerDone 在每层完成后调用（供 main 在 **stage-host 就绪** 的那一刻
	// 接上插件拉起动作）。
	//
	// ★★ 为什么必须是"每层"而不是"全部完成后"：
	//
	// 插件加载需要 **plugin.Registry**，而它是 `stage-host` 模块的产物
	// ⇒ 插件**不可能**早于 stage-host 被加载（这是硬约束）。
	// 但 stage-host 在第 3 层，而 agent 在第 4 层 —— 而 agent 层实测要
	// **35 秒**（context dense backfill 750 events + document/knowledge 重建索引）。
	//
	// ⇒ 声明了早期设施（config/llm）的插件完全没必要等那 35 秒。
	//   在 stage-host 就绪那一刻接上拉起动作，它们就能立刻起。
	//
	// 实测收益（真机）：early_probe（只声明 config）从"装配全部完成后"
	// 提前到"第 3 层完成时"，省掉 agent 层那 35 秒。
	OnLayerDone func(idx int, layer []kernelboot.Module)
}

func RunKernelModules(ks *kernelboot.KernelState, hooks kernelAssembleHooks) (*kernelboot.SyncBus, error) {
	mods, err := buildModuleList()
	if err != nil {
		return nil, err
	}

	// ★★ 启动顺序来自**编译期产出的图**（用户设计）。
	//
	//	编译期：homed-depgraph 算好分层 → depgraph.bin（含层号）+ digest 注入二进制
	//	运行期：**读**这份图 ⇒ 不自己算顺序
	//
	// 这样"启动顺序"是**编译期制品**，可审计（进 PR diff）、可离线校验。
	//
	// 缺失/损坏/漂移策略：
	//   · dev（EmbeddedGraphHash 为空）：允许无图 ⇒ 回退运行期 Build（便于 go test/go run）
	//   · 生产（有 hash）：必须有图，且 hash 必须匹配，且层号必须与声明一致
	//     —— 任一不符 ⇒ **拒绝启动**（不猜哪边对：都是制品装配错误）
	plan, err := loadStartupPlan(mods)
	if err != nil {
		return nil, err
	}

	bus := kernelboot.NewSyncBus()

	// ★★ 先把**全部模块声明的设施**登记为"内核注册过"，**再**调用 BeforeRun。
	//
	// 时序很关键（我第一版搞反了，实测报了"内核已知的设施名: []"）：
	//
	//	BeforeRun（attach）：插件调度器提交声明 ⇒ 触发**拼错校验**
	//	                      ⇒ 若此刻 known 为空，**所有声明都被误判为拼错**
	//	MarkDeclared       ：把各模块的 Provides 登记进 known
	//
	// ⇒ 顺序必须是 **MarkDeclared → BeforeRun**。
	//   （Run 内部也会 MarkDeclared，那是为了"装配中途失败也成立"，
	//     与本处不冲突 —— MarkDeclared 幂等。）
	var allProvides []string
	for _, layer := range plan.Layers {
		for _, m := range layer {
			allProvides = append(allProvides, m.Provides()...)
		}
	}
	bus.MarkDeclared(allProvides)

	// ★ 在**执行任何模块之前**挂接（插件调度器在这里订阅 syncbus）。
	//   这样内核每完成一层都会触发调度器重新评估 ⇒ 声明提前启动**真的生效**。
	if hooks.BeforeRun != nil {
		if err := hooks.BeforeRun(bus); err != nil {
			return nil, err
		}
	}

	res, err := kernelboot.Run(plan, kernelboot.RunOptions{
		State: ks,
		Bus:   bus,
		// 并发上限：开 DB / 起 VM 很吃 IO，全放开会互相拖慢。
		// 同层最多 8 个模块时 4 路并发实测最快（再高变 IO 争抢）。
		MaxParallel: 4,
		OnLayerDone: func(i int, layer []kernelboot.Module) {
			names := make([]string, 0, len(layer))
			for _, m := range layer {
				names = append(names, m.Name())
			}
			log.Printf("[homed] 内核装配 第 %d 层完成（%d 个）: %s",
				i, len(layer), strings.Join(names, ", "))
			// ★ 把"某层完成"事件交给 main（用于在 stage-host 就绪时
			//   立刻接上插件拉起动作，而不是等 agent 层的 35 秒）。
			if hooks.OnLayerDone != nil {
				hooks.OnLayerDone(i, layer)
			}
		},
	})
	if err != nil {
		return nil, err
	}

	log.Printf("[homed] 内核装配完成: %d 层 / %d 模块（峰值并发 %d，原为 17 步串行）",
		res.LayerCount, len(res.Order), res.MaxConcurrent)
	return bus, nil
}

// loadStartupPlan 取启动计划：优先读**编译期图**，dev 环境回退运行期 Build。
func loadStartupPlan(mods []kernelboot.Module) (*kernelboot.Plan, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("无法确定二进制路径（图与它同目录）: %w", err)
	}
	graphPath := filepath.Join(filepath.Dir(exe), kernelboot.GraphFileName)

	gf, err := kernelboot.LoadAndCheckGraph(graphPath, kernelboot.EmbeddedGraphHash)
	if err != nil {
		return nil, err
	}
	if gf == nil {
		// dev：没有图（也没注入 hash）⇒ 运行期现算。
		// 这条路径**不是**生产路径；它存在只为让 go test / go run 能用。
		log.Printf("[homed] 未找到编译期依赖图（dev 构建），回退运行期分层")
		return kernelboot.Build(mods)
	}

	plan, err := kernelboot.PlanFromGraph(gf, mods)
	if err != nil {
		return nil, err
	}
	log.Printf("[homed] 启动顺序来自编译期依赖图 %s（digest %s）",
		filepath.Base(graphPath), gf.Digest[:16])
	return plan, nil
}

// buildModuleList 从注册表取模块并做结构性检查。
func buildModuleList() ([]kernelboot.Module, error) {
	if d := kernelmod.Duplicates(); len(d) > 0 {
		return nil, fmt.Errorf("内核模块名重复注册: %v（两个模块取了同名，或同一模块被导入两次）", d)
	}
	mods := kernelmod.Registered()
	if len(mods) == 0 {
		return nil, fmt.Errorf("没有任何内核模块被登记 —— " +
			"通常是没有 import internal/kernelmod/all（见 cmd/homed/kmod_runtime.go）")
	}
	return mods, nil
}
