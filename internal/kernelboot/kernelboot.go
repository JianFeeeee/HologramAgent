// Package kernelboot 是内核模块的**声明式装配调度器**。
//
// 设计权威：docs/zh/kernel-modularization-design.md §2.3。
//
// ## 要解决的问题（实测，2026-10-16）
//
// `cmd/homed/main.go` 里 277 行装配代码、11 处「必须早于 / 必须在之前」注释。
// 这些**顺序依赖只写在注释里，没有任何机器校验** —— 错了不报错，
// 而是静默降级，症状出现在很远的地方。
//
// 现场故障：安全模块装配晚于读 `core.llm.*` 的操作 ⇒ 密文解不开 ⇒
// key 变空 ⇒ 所有 LLM 调用 401，而看起来像"key 配错了"。
//
// ## 本包提供的性质（用户指出的核心洞察）
//
// > 依赖图是一个由依赖关系生成的图，通过 BFS 遍历：当前遍历到的节点初始化后，
// > 下一层 BFS 就可以安全地并行初始化 —— 这是依赖图的性质。
//
// 形式化：设边 `a → b` 表示 b 依赖 a。BFS 从入度 0 的节点开始分层。
// 第 k 层任意节点的**全部前驱都在 <k 层**，且**第 k 层内部两两无边**
// （否则其中一个会在第 ≥k+1 层）。
//
// ⇒ 只要第 <k 层已全部就绪，第 k 层的**任意子集**都可以并发 Init。
//
//	分层本身已经保证了并行安全，无需任何额外判断。
//
// 这就是为什么用 BFS 而不是「一次拓扑排序 + 逐个执行」：
// 拓扑序给的是**一个**合法顺序（全串行），BFS 分层给的是**最大化并行的顺序集合**。
//
// ## 本包只做纯逻辑
//
// 不依赖任何内核子系统，因此可以用极硬的判据覆盖：
// 分层正确性、同层并行、有界并发、三类失败（缺依赖/循环/Init 错）。
// 真正的模块定义与接线在 Step 4 的后续增量里做（见文档 §6）。
package kernelboot

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Module 是一个可装配的内核模块。
//
// ★ 注意 Requires 的语义：**不是"我要在谁之后"，而是"我需要谁已就绪"**。
// 前者是顺序（要人肉维护），后者是依赖（可由图推导）——
// 本设计的全部价值就在于把前者换成后者。
type Module interface {
	// Name 是模块的唯一标识（依赖图节点名）。
	Name() string
	// Requires 声明依赖的**设施名**（即其他模块 Provides 的名字）。
	Requires() []string
	// Provides 声明本模块提供的设施名（供他人 Requires 引用）。
	//
	// 允许一个模块提供多个设施名（如"配置库"同时提供"配置读"与"配置写"）。
	// 返回空表示"不提供任何可被依赖的设施"（纯副作用模块）。
	Provides() []string
	// Init 执行装配。ks 是内核**唯一的公共数据结构体**：
	// 模块从中**读**已就绪的依赖，把自己产出的公共数据**写**回它的字段。
	//
	// 返回错误表示装配失败（**必须传播，不得吞掉**）。
	Init(ks *KernelState) error
}

// Plan 是一次依赖解析的结果：分好层的调度计划。
type Plan struct {
	// Layers 是 BFS 分层结果。Layers[0] 无依赖，Layers[k+1] 依赖 Layers[0..k]。
	Layers [][]Module
}

// TotalModules 返回计划里的模块总数。
func (p *Plan) TotalModules() int {
	n := 0
	for _, l := range p.Layers {
		n += len(l)
	}
	return n
}

// UnresolvableError 是"依赖图无法解析"的错误。
//
// 它必须携带**足够定位问题的信息**（本仓纪律：错误要能指向修复动作）：
//   - Missing：被依赖但从没被任何模块 Provides 的设施名
//   - Cycle：还在等待中的模块名（在无解时，它们构成环或环的下游）
//   - Declared：已声明提供的设施名（便于发现拼写差异，如 "configur" vs "config"）
type UnresolvableError struct {
	Missing  []string
	Cycle    []string
	Declared []string
}

func (e *UnresolvableError) Error() string {
	var b strings.Builder
	b.WriteString("kernelboot: 依赖图无法解析")
	if len(e.Missing) > 0 {
		fmt.Fprintf(&b, "；依赖了不存在的设施: %v（已声明可提供的: %v）",
			e.Missing, e.Declared)
	}
	if len(e.Cycle) > 0 {
		fmt.Fprintf(&b, "；循环依赖或等待环上的模块: %v", e.Cycle)
	}
	return b.String()
}

// Build 解析依赖并分层。
//
// 算法（文档 §2.3.2）：
//
//	ready := {}                      // 已就绪的设施名
//	for 还有未装配的模块 {
//	    layer := 所有 Requires ⊆ ready 的模块
//	    if layer 为空 => 无解（缺依赖或环）=> 报错
//	    ready ∪= 所有 layer 成员的 Provides
//	}
//
// 返回的 Plan 可直接按层执行（层内可并行）。
func Build(modules []Module) (*Plan, error) {
	// ① 先收集所有被声明的设施名，用于区分"缺依赖"与"有环"两类错误。
	declared := map[string]bool{}
	for _, m := range modules {
		for _, p := range m.Provides() {
			declared[p] = true
		}
	}

	// ② 检查重名（两个模块 Provides 同名设施 ⇒ 歧义，必须报错）
	//    为何必须拦：重名会让"依赖了谁"变得不确定，
	//    而依赖图的价值恰恰在于确定性。
	owner := map[string]string{} // 设施名 -> 提供者模块名
	var dupes []string
	for _, m := range modules {
		for _, p := range m.Provides() {
			if prev, ok := owner[p]; ok && prev != m.Name() {
				dupes = append(dupes, fmt.Sprintf("%s（%s 与 %s 都声明提供）", p, prev, m.Name()))
				continue
			}
			owner[p] = m.Name()
		}
	}
	if len(dupes) > 0 {
		sort.Strings(dupes)
		return nil, fmt.Errorf("kernelboot: 设施名被重复声明: %v", dupes)
	}

	// ③ 模块名不得重复
	seen := map[string]bool{}
	for _, m := range modules {
		if seen[m.Name()] {
			return nil, fmt.Errorf("kernelboot: 模块名重复: %s", m.Name())
		}
		seen[m.Name()] = true
	}

	// ④ BFS 分层
	ready := map[string]bool{}
	pending := make([]Module, len(modules))
	copy(pending, modules)
	var layers [][]Module

	for len(pending) > 0 {
		var layer []Module
		var rest []Module
		for _, m := range pending {
			if satisfied(m.Requires(), ready) {
				layer = append(layer, m)
			} else {
				rest = append(rest, m)
			}
		}
		if len(layer) == 0 {
			// 无解：区分"缺依赖"与"循环"
			return nil, diagnoseUnresolvable(rest, declared, owner, ready)
		}
		// 同层内按名字排序：让分层结果**稳定可复现**（便于判据断言与日志对照）。
		// 并行执行顺序不影响正确性（同层无边），但稳定的输出让"层次变了"这类
		// 隐式变更容易被发现。
		sort.Slice(layer, func(i, j int) bool { return layer[i].Name() < layer[j].Name() })
		layers = append(layers, layer)

		for _, m := range layer {
			for _, p := range m.Provides() {
				ready[p] = true
			}
		}
		pending = rest
	}

	return &Plan{Layers: layers}, nil
}

// satisfied 报告 requires 是否全部已在 ready 里。
func satisfied(requires []string, ready map[string]bool) bool {
	for _, r := range requires {
		if !ready[r] {
			return false
		}
	}
	return true
}

// diagnoseUnresolvable 区分两类无解原因，并给出可定位的信息。
//
// 为何要区分：两者的**修复动作完全不同**——
//
//	· 缺依赖 ⇒ 去补 Provides（常是拼写错误）
//	· 有环   ⇒ 去拆环（设计问题）
//
// 只报"有环"会让人去错误的方向查。
func diagnoseUnresolvable(pending []Module, declared map[string]bool, owner map[string]string, ready map[string]bool) error {
	var missing, cycle []string
	for _, m := range pending {
		cycle = append(cycle, m.Name())
		for _, r := range m.Requires() {
			// 该依赖既没被任何模块声明提供、也不在 ready ⇒ 真正的"不存在"
			if !declared[r] && !ready[r] {
				missing = append(missing, fmt.Sprintf("%s（%s 需要）", r, m.Name()))
			}
		}
	}
	sort.Strings(missing)
	sort.Strings(cycle)

	declaredList := make([]string, 0, len(declared))
	for d := range declared {
		declaredList = append(declaredList, d)
	}
	sort.Strings(declaredList)

	return &UnresolvableError{Missing: missing, Cycle: cycle, Declared: declaredList}
}

// RunOptions 控制执行。
type RunOptions struct {
	// MaxParallel 是**层内**的并发上限（<=0 表示不限制）。
	//
	// ★ 为何必须有界：同层无边保证**安全**，但不保证**无代价**——
	// 42 个插件同时 spawn 会造成内存/CPU 尖峰（每个 ~11MB 二进制加载 + 握手）。
	// 并发度只影响**速度**，不影响**顺序**（同层节点两两无边，
	// 无论以什么顺序完成，下一层的就绪条件都相同）。
	MaxParallel int

	// OnLayerDone 在每层完成后回调（层号从 0 起）。可为 nil。
	OnLayerDone func(layerIndex int, modules []Module)

	// State 是内核**唯一的公共数据结构体**（用户设计）：
	// 模块从中读依赖、往它写自己的产物。
	//
	// 为何由调用方传入而不是 Run 内部自建：产物在 Run 返回后**仍然要用**
	// （如 LuaVM、Memory 要一直活着），所以它必须比 Run 活得久。
	State *KernelState

	// Bus 是状态同步总线。非 nil 时，Run 会在**每个层边界**把该层模块贡献的
	// 设施标记为就绪（设计 §2.6.2）。
	//
	// ★ 为何由 Run 自动写而不是各模块自己写：
	// 若模块自己 MarkReady，顺序取决于**层内并发的完成顺序** ⇒ 不可复现；
	// 而且模块可能"忘记标记"（静默漏掉一个设施 ⇒ 插件永远等不到它就绪）。
	// 由 Run 在层边界统一写，既确定又不可能漏。
	Bus *SyncBus
}

// RunResult 是执行结果。
type RunResult struct {
	// LayerCount 是实际执行的层数。
	LayerCount int
	// MaxConcurrent 是观测到的**峰值并发**（用于判据证明并行确实发生）。
	MaxConcurrent int
	// Order 是按完成顺序记录的模块名（仅用于诊断/日志，**不作正确性依据**）。
	Order []string
}

// Run 按计划执行：逐层 Init，层内有界并行。
//
// 失败策略（文档 §2.3.3）：**任何 Init 失败都令整体失败**。
// 不静默跳过、不"带着半初始化状态继续"——后者正是病 A 的形态。
//
// 同层内若**多个**模块失败，返回第一个（按名字序，稳定）；
// 但**不等同于只跑一个**：同层其余模块仍会被执行完（避免半途中断造成更乱的状态）。
func Run(plan *Plan, opts RunOptions) (*RunResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("kernelboot: 计划为 nil")
	}
	res := &RunResult{}
	var orderMu sync.Mutex

	ks := opts.State
	if ks == nil {
		ks = NewKernelState()
	}

	// ★ 执行前把**全部**模块声明的设施登记为"内核注册过"。
	//
	// 这样插件的 `requires_facilities` 校验（防拼错 ⇒ 静默永不启动）
	// 在"装配结果之外"也成立：即使某层失败、后面的模块没跑，
	// "这个设施名是模块声明过的"依然为真（语义与"是否已就绪"解耦）。
	if opts.Bus != nil {
		var all []string
		for _, layer := range plan.Layers {
			for _, m := range layer {
				all = append(all, m.Provides()...)
			}
		}
		opts.Bus.MarkDeclared(all)
		// ★ 把 bus 放进 KernelState：模块的 Init 只能看到 ks，
		// 而 plugin-host 需要订阅状态变化（挂"等 agent 就绪"的回调）。
		// 不放这里 ⇒ 模块拿不到 bus（实测撞到）。
		ks.SyncBus = opts.Bus
	}

	for li, layer := range plan.Layers {
		if len(layer) == 0 {
			continue
		}
		limit := opts.MaxParallel
		if limit <= 0 || limit > len(layer) {
			limit = len(layer)
		}

		sem := make(chan struct{}, limit)
		var wg sync.WaitGroup
		errs := make([]error, len(layer))

		// 峰值并发观测（判据用它证明"并行确实发生了"）
		var active, peak int
		var peakMu sync.Mutex

		for i, m := range layer {
			wg.Add(1)
			go func(i int, m Module) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				peakMu.Lock()
				active++
				if active > peak {
					peak = active
				}
				peakMu.Unlock()

				err := m.Init(ks)

				peakMu.Lock()
				active--
				peakMu.Unlock()

				orderMu.Lock()
				res.Order = append(res.Order, m.Name())
				orderMu.Unlock()

				if err != nil {
					errs[i] = fmt.Errorf("模块 %s 装配失败: %w", m.Name(), err)
				}
			}(i, m)
		}
		wg.Wait()

		peakMu.Lock()
		if peak > res.MaxConcurrent {
			res.MaxConcurrent = peak
		}
		peakMu.Unlock()

		// 层内失败：收集后**一次性**报告（不要 fail-fast 丢掉其他失败信息）
		var failed []string
		for _, e := range errs {
			if e != nil {
				failed = append(failed, e.Error())
			}
		}
		if len(failed) > 0 {
			sort.Strings(failed)
			return res, fmt.Errorf("kernelboot: 第 %d 层装配失败: %s",
				li, strings.Join(failed, "; "))
		}

		res.LayerCount++
		// ★ 层边界：把本层贡献的设施标记就绪（设计 §2.6.2）。
		//   在层内并发**全部完成之后**统一标记 ⇒ 就绪顺序确定、可复现。
		if opts.Bus != nil {
			var provides []string
			for _, m := range layer {
				provides = append(provides, m.Provides()...)
			}
			opts.Bus.MarkLayerReady(provides)
		}
		if opts.OnLayerDone != nil {
			opts.OnLayerDone(li, layer)
		}
	}

	// ★ 全部层完成 ⇒ 内核完全初始化。
	//
	// 这是"未声明依赖的插件"的启动信号（默认路径 = 现行为，设计 §2.6.5）。
	// 它在**所有层都成功之后**才发 —— 中途失败会提前 return，不发这个信号，
	// 于是那些插件**不会被拉起**（这是对的：内核没装好就拉插件 = 病 A）。
	if opts.Bus != nil {
		opts.Bus.MarkKernelComplete()
	}
	return res, nil
}
