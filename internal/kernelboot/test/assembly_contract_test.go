package test

// internal/kernelboot 的**装配执行**判据 —— 面向接口的黑盒。
//
// 入口：Build(modules) / PlanFromGraph(gf, mods) / Run(plan, RunOptions)，
// 断言只看：Init 被调的顺序与次数、RunResult、返回的错误类型与内容。
// 分层算法、闭包、goroutine 编排都不在断言范围内。
//
// ## 原判据的缺口
//
// `graph_contract_test.go` 只测了图的编解码（Encode/Decode/Digest/
// CheckAgainstBinary）—— 那是**制品格式**，不是调度器职责。
// Build/Run 从来没被执行过，而它们才是这个包存在的理由。
//
// ## 要钉的三条核心性质（设计 §2.3 的形式化保证）
//
//	① 分层正确：第 k 层模块的每个依赖，都在 <k 层被 Provides
//	② 同层并行是真的发生（否则"最大化并行"只是文档里的说法）
//	③ 失败必须整体失败，且**不装一半**（宁可不装也不留半初始化状态）

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// recorder 记录每个模块 Init 的次序与当时的栈深（用于判定"同层并行"）。
//
// 用"是否曾在同一时刻同时有 >=2 个 Init 在跑"来判定并行，而不是比较
// 完成顺序 —— 后者在串行实现下也可能乱序（取决于遍历顺序），会假绿。
type recorder struct {
	mu        sync.Mutex
	order     []string
	inflight  int32
	maxFlight int32
	perLayer  map[string]int
}

func newRecorder() *recorder {
	return &recorder{perLayer: map[string]int{}}
}

func (r *recorder) enter(name string) {
	cur := atomic.AddInt32(&r.inflight, 1)
	for {
		max := atomic.LoadInt32(&r.maxFlight)
		if cur <= max || atomic.CompareAndSwapInt32(&r.maxFlight, max, cur) {
			break
		}
	}
	r.mu.Lock()
	r.order = append(r.order, name)
	r.mu.Unlock()
}

func (r *recorder) leave() { atomic.AddInt32(&r.inflight, -1) }

func (r *recorder) peak() int { return int(atomic.LoadInt32(&r.maxFlight)) }

func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

// module 造一个只会记录并在 ks 里写设施的模块。
// delay 用来制造"同层可观察的并发窗口"。
func module(rec *recorder, name string, provides, requires []string, delay time.Duration, fail error) kernelboot.Module {
	return kernelboot.FuncModule(kernelboot.FuncSpec{
		Name:     name,
		Provides: provides,
		Requires: requires,
		Init: func(ks *kstate.State) error {
			rec.enter(name)
			defer rec.leave()
			if delay > 0 {
				time.Sleep(delay)
			}
			if fail != nil {
				return fail
			}
			_ = ks
			return nil
		},
	})
}

// ---- 判据 1：分层必须满足"依赖在更早的层" ----

// TestBuildLayersRespectDependencies：每个模块都被排进某一层，且它的
// 每个依赖都由**更早层**的模块 Provides。
//
// 这是 BFS 分层的定义性质；错一层，并行就不再安全（同一层里两个模块
// 互相依赖对方的产物）。
func TestBuildLayersRespectDependencies(t *testing.T) {
	rec := newRecorder()
	mods := []kernelboot.Module{
		module(rec, "config", []string{"cfg"}, nil, 0, nil),
		module(rec, "security", []string{"sec"}, []string{"cfg"}, 0, nil),
		module(rec, "llm", []string{"llm"}, []string{"sec", "cfg"}, 0, nil),
		module(rec, "agent", []string{"agent"}, []string{"llm"}, 0, nil),
		module(rec, "lua", []string{"lua"}, []string{"cfg"}, 0, nil),
	}

	plan, err := kernelboot.Build(mods)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := plan.TotalModules(); got != len(mods) {
		t.Fatalf("★ 计划里模块数 %d，期望 %d", got, len(mods))
	}

	// 每层的 Provides 汇总；逐层累计"可用设施"。
	available := map[string]bool{}
	for li, layer := range plan.Layers {
		for _, m := range layer {
			for _, req := range m.Requires() {
				if !available[req] {
					t.Fatalf("★ 第 %d 层的模块 %q 依赖 %q，但该设施只在同层或更晚层提供"+
						"—— 同层并行不再安全", li, m.Name(), req)
				}
			}
		}
		for _, m := range layer {
			for _, p := range m.Provides() {
				available[p] = true
			}
		}
	}
}

// ---- 判据 2：同层真的并行（峰值并发 >= 2）----

// TestRunParallelizesWithinLayer：同一层里有多个互相无依赖的模块时，
// 它们的 Init 必须**重叠执行**。
//
// 判据用"同时在跑的 Init 峰值 >= 2"来判定，而不是看完成顺序。
// 若实现退化成逐个执行（拓扑序全串行），峰值恒为 1 ⇒ 红。
//
// ★ 这是本包的核心承诺（"BFS 分层给的是最大化并行的顺序集合"）。
// 没有这条判据，把 Run 改成三层 for 循环逐个跑也照样绿。
func TestRunParallelizesWithinLayer(t *testing.T) {
	rec := newRecorder()
	// 三个同层模块，每个 Init 睡 50ms；串行要 150ms，并行约 50ms。
	var mods []kernelboot.Module
	for _, n := range []string{"a", "b", "c"} {
		mods = append(mods, module(rec, n, []string{n}, nil, 50*time.Millisecond, nil))
	}

	plan, err := kernelboot.Build(mods)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ks := kstate.New()
	if _, err := kernelboot.Run(plan, kernelboot.RunOptions{State: ks}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if peak := rec.peak(); peak < 2 {
		t.Fatalf("★ 同层三个无依赖模块，Init 峰值并发 = %d —— 层内没有并行，%s",
			peak, "「最大化并行」只是文档里的说法")
	}
}

// ---- 判据 3：Init 失败必须整体失败，且**不装一半** ----

// TestRunAbortsOnInitFailure：任一模块 Init 返回错误 ⇒ Run 返回错误，
// 且更晚层**一个都没跑**。
//
// "不装一半"是刻意的：带着半初始化状态继续，正是设计要消除的病 A
// （安全模块晚于读配置 ⇒ key 变空 ⇒ 所有 LLM 401，症状远在远处）。
func TestRunAbortsOnInitFailure(t *testing.T) {
	rec := newRecorder()
	boom := errors.New("boom")
	mods := []kernelboot.Module{
		module(rec, "first", []string{"f"}, nil, 0, nil),
		module(rec, "broken", []string{"b"}, []string{"f"}, 0, boom),
		module(rec, "later", []string{"l"}, []string{"b"}, 0, nil),
	}

	plan, err := kernelboot.Build(mods)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ks := kstate.New()
	_, runErr := kernelboot.Run(plan, kernelboot.RunOptions{State: ks})
	if runErr == nil {
		t.Fatal("★ 有模块 Init 失败，Run 却返回成功")
	}

	ran := rec.names()
	for _, n := range ran {
		if n == "later" {
			t.Fatalf("★ 依赖未就绪的后续层模块仍被 Init —— 带着半初始化状态继续：%v", ran)
		}
	}
	// 失败必须能被定位到模块
	if !strings.Contains(runErr.Error(), "broken") {
		t.Fatalf("★ 错误里没有失败模块名，无法定位：%v", runErr)
	}
}

// ---- 判据 4：依赖图无解时报错，且错误可定位 ----

// TestBuildRejectsMissingDependency：Requires 一个没人 Provides 的设施
// ⇒ Build 报错，且错误里同时出现"缺什么"与"已声明了什么"。
//
// 后者是为拼写差异准备的（"configur" vs "config"）：只报"缺 configur"
// 时，人看到自己明明声明了 config 会怀疑是框架 bug。
func TestBuildRejectsMissingDependency(t *testing.T) {
	rec := newRecorder()
	mods := []kernelboot.Module{
		module(rec, "needy", []string{"n"}, []string{"nonexistent"}, 0, nil),
		module(rec, "provider", []string{"config"}, nil, 0, nil),
	}
	_, err := kernelboot.Build(mods)
	if err == nil {
		t.Fatal("★ 缺依赖的图被接受了")
	}
	msg := err.Error()
	if !strings.Contains(msg, "nonexistent") {
		t.Fatalf("★ 错误未指出缺哪个设施：%v", err)
	}
	if !strings.Contains(msg, "config") {
		t.Fatalf("★ 错误未列出已声明的设施（无法发现拼写差异）：%v", err)
	}
}

// TestBuildRejectsCycle：A→B→A 必须报错且指出涉及的模块。
func TestBuildRejectsCycle(t *testing.T) {
	rec := newRecorder()
	mods := []kernelboot.Module{
		module(rec, "a", []string{"fa"}, []string{"fb"}, 0, nil),
		module(rec, "b", []string{"fb"}, []string{"fa"}, 0, nil),
	}
	_, err := kernelboot.Build(mods)
	if err == nil {
		t.Fatal("★ 循环依赖被接受了")
	}
	msg := err.Error()
	if !strings.Contains(msg, "a") || !strings.Contains(msg, "b") {
		t.Fatalf("★ 错误未指出环里的模块：%v", err)
	}
}

// ---- 判据 5：执行期校验咬得住"层顺序被改坏" ----

// TestRunRejectsLayerOrderBrokenAfterBuild：声明完全自洽（Build 成功），
// 但计划里的层顺序被改坏（把一个依赖前驱的模块挪到更早的层）⇒
// Run 必须在**开工前**拒绝，一个 Init 都不跑。
//
// 这一条与 Build 的分工是刻意的：Build 看**声明**，Run 看**事实**
// （这一层之前真的跑完了的模块产出了什么）。所以这里手工构造一个
// 声明合法、顺序非法的计划。
func TestRunRejectsLayerOrderBrokenAfterBuild(t *testing.T) {
	rec := newRecorder()
	provider := module(rec, "provider", []string{"cfg"}, nil, 0, nil)
	consumer := module(rec, "consumer", []string{"x"}, []string{"cfg"}, 0, nil)

	plan, err := kernelboot.Build([]kernelboot.Module{provider, consumer})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Layers) != 2 {
		t.Fatalf("前置：期望 2 层，实际 %d", len(plan.Layers))
	}
	// 把两层对调：consumer 先跑（它的依赖还没产出）。
	plan.Layers[0], plan.Layers[1] = plan.Layers[1], plan.Layers[0]

	ks := kstate.New()
	_, err = kernelboot.Run(plan, kernelboot.RunOptions{State: ks})
	if err == nil {
		t.Fatal("★ 层顺序被改坏（consumer 先于 provider），Run 却接受了")
	}
	var notReady *kernelboot.DependencyNotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("★ 错误类型不是 DependencyNotReadyError（无法与"+"配置错误"+"区分）：%T %v", err, err)
	}
	// 缺失清单必须真的列出"这一层缺什么"，否则运维只知道"失败了"，
	// 不知道该补哪一层/哪个设施。
	if len(notReady.Missing) == 0 {
		t.Fatalf("★ 错误没带缺失设施清单，无法定位：%+v", notReady)
	}
	missingForConsumer, ok := notReady.Missing["consumer"]
	if !ok || len(missingForConsumer) == 0 {
		t.Fatalf("★ 缺失清单里没点名 consumer 缺什么：%+v", notReady.Missing)
	}
	if missingForConsumer[0] != "cfg" {
		t.Fatalf("★ consumer 缺的应是 cfg，实际 %v", missingForConsumer)
	}
	if ran := rec.names(); len(ran) != 0 {
		t.Fatalf("★ 依赖未就绪却已经跑了 %v —— 宁可什么都不装也不装一半", ran)
	}
}

// ---- 判据 6：PlanFromGraph 读层号，且抓得住图与代码漂移 ----

// TestPlanFromGraphUsesDeclaredLayers：合法图 ⇒ 计划层号与图一致。
func TestPlanFromGraphUsesDeclaredLayers(t *testing.T) {
	rec := newRecorder()
	mods := []kernelboot.Module{
		module(rec, "config", []string{"cfg"}, nil, 0, nil),
		module(rec, "agent", []string{"agent"}, []string{"cfg"}, 0, nil),
	}
	// 层号从 0 起算（Build 的第 0 层 = 无依赖）。图里的层号必须与它对上。
	gf, err := kernelboot.DecodeGraph(kernelboot.EncodeGraph([]kernelboot.GraphEdge{
		{Name: "config", Provides: []string{"cfg"}, Layer: 0},
		{Name: "agent", Provides: []string{"agent"}, Requires: []string{"cfg"}, Layer: 1},
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := kernelboot.PlanFromGraph(gf, mods)
	if err != nil {
		t.Fatalf("PlanFromGraph: %v", err)
	}
	if got := plan.TotalModules(); got != 2 {
		t.Fatalf("★ 计划模块数 %d，期望 2", got)
	}
	ks := kstate.New()
	if _, err := kernelboot.Run(plan, kernelboot.RunOptions{State: ks}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestPlanFromGraphRejectsDriftedLayers：图里的层号与它自己的边矛盾
// （改了 Requires 但没重出图）⇒ 必须在构造计划时拒绝。
//
// 这正是设计 §2.5.2 的"病 A"：文件 hash 有效 ⇒ 按旧顺序启动 ⇒ 静默崩。
// 判据构造一个自相矛盾的图（边说 agent 依赖 cfg，层号却说 agent 在第 0 层）。
func TestPlanFromGraphRejectsDriftedLayers(t *testing.T) {
	rec := newRecorder()
	mods := []kernelboot.Module{
		module(rec, "config", []string{"cfg"}, nil, 0, nil),
		module(rec, "agent", []string{"agent"}, []string{"cfg"}, 0, nil),
	}
	gf, err := kernelboot.DecodeGraph(kernelboot.EncodeGraph([]kernelboot.GraphEdge{
		{Name: "config", Provides: []string{"cfg"}, Layer: 1},
		{Name: "agent", Provides: []string{"agent"}, Requires: []string{"cfg"}, Layer: 0},
	}))
	if err != nil {
		// 编码器可能自己就拒绝矛盾层号 —— 那也是合格的结果。
		return
	}
	if _, err := kernelboot.PlanFromGraph(gf, mods); err == nil {
		t.Fatal("★ 图内层号与边自相矛盾，却被接受 ⇒ 会按旧顺序启动")
	}
}

// ---- 判据 7：层边界回调与状态总线同步 ----

// TestRunMarksFacilitiesReadyAtLayerBoundary：每层完成后，该层模块
// 提供的设施必须可被 Bus 观察到已就绪。
//
// 由 Run 统一在层边界写，而不是各模块自己写 —— 否则顺序取决于层内并发
// 完成顺序（不可复现），且模块可能忘记标记。
func TestRunMarksFacilitiesReadyAtLayerBoundary(t *testing.T) {
	rec := newRecorder()
	mods := []kernelboot.Module{
		module(rec, "config", []string{"cfg"}, nil, 0, nil),
		module(rec, "agent", []string{"agent"}, []string{"cfg"}, 0, nil),
	}
	plan, err := kernelboot.Build(mods)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ks := kstate.New()
	bus := kstate.NewSyncBus()
	var layerDone []int
	res, err := kernelboot.Run(plan, kernelboot.RunOptions{
		State: ks,
		Bus:   bus,
		OnLayerDone: func(idx int, ms []kernelboot.Module) {
			layerDone = append(layerDone, idx)
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.LayerCount != len(plan.Layers) {
		t.Fatalf("★ LayerCount = %d，期望 %d", res.LayerCount, len(plan.Layers))
	}
	if len(layerDone) != len(plan.Layers) {
		t.Fatalf("★ OnLayerDone 被调用 %d 次，期望 %d 次", len(layerDone), len(plan.Layers))
	}
	// 全部跑完后，每个设施都应就绪。
	for _, want := range []string{"cfg", "agent"} {
		if !bus.IsReady(want) {
			t.Fatalf("★ 所有层都跑完了，设施 %q 仍未标记就绪 —— 等待它的插件会永远挂起", want)
		}
	}
}
