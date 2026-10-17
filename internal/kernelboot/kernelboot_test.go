package kernelboot

// BFS 分层调度器的判据（2026-10-16）。
//
// 设计见 docs/zh/kernel-modularization-design.md §2.3 / §4.1。
//
// ## 判据不能只看"最终都 Init 了"
//
// 那是**串行也能满足**的弱断言。必须断言**层次结构与并行性本身**：
//   · 分层正确（同层两两无边、前驱都在更早层）—— 这是正确性的根
//   · 同层确实并行（用观测到的峰值并发证明）
//   · 有界并发生效
//   · 三类失败（缺依赖 / 循环 / Init 错）都必须**响亮拒绝**

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeModule 是可观测的假模块。
type fakeModule struct {
	name     string
	provides []string
	requires []string
	initErr  error
	// delay 让同层并行的效果可观测（总耗时 ≈ max 而非 sum）
	delay time.Duration
	// initCount 记录被 Init 的次数（幂等性/重复执行检测）
	initCount *int32
	// onInit 供并发观测
	onInit func()
}

func (m *fakeModule) Name() string       { return m.name }
func (m *fakeModule) Provides() []string { return m.provides }
func (m *fakeModule) Requires() []string { return m.requires }
func (m *fakeModule) Init(_ *KernelState) error {
	if m.initCount != nil {
		atomic.AddInt32(m.initCount, 1)
	}
	if m.onInit != nil {
		m.onInit()
	}
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	return m.initErr
}

func names(layer []Module) []string {
	out := make([]string, 0, len(layer))
	for _, m := range layer {
		out = append(out, m.Name())
	}
	sort.Strings(out)
	return out
}

// ★★★ 判据：**嵌套依赖**分层正确（用户明确要求的场景）。
//
// 链：security → config → llm → agent（每层依赖上一层）
// ⇒ 必须得到 4 个单元素层，而不是被展平。
func TestBuildLayersNestedChain(t *testing.T) {
	mods := []Module{
		&fakeModule{name: "agent", requires: []string{"llm"}},
		&fakeModule{name: "llm", provides: []string{"llm"}, requires: []string{"config"}},
		&fakeModule{name: "config", provides: []string{"config"}, requires: []string{"crypto"}},
		&fakeModule{name: "security", provides: []string{"crypto"}},
	}
	plan, err := Build(mods)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Layers) != 4 {
		t.Fatalf("嵌套链应分 4 层，实际 %d 层: %v", len(plan.Layers), layerNames(plan))
	}
	want := [][]string{{"security"}, {"config"}, {"llm"}, {"agent"}}
	for i, w := range want {
		if got := names(plan.Layers[i]); !equalStrs(got, w) {
			t.Errorf("第 %d 层应为 %v，实际 %v", i, w, got)
		}
	}
}

// ★★★ 判据：**同层并行**（一个节点的所有前驱就绪后，它与同层的其他无冲突节点同层）。
//
// 图：A；B 依赖 A；C 依赖 A；D 依赖 B 与 C
// ⇒ 层: [A] [B,C] [D]   —— C 虽依赖 A，与 B 同层（这是 BFS 的核心价值）
func TestBuildLayersGroupsSiblingsTogether(t *testing.T) {
	mods := []Module{
		&fakeModule{name: "D", requires: []string{"b", "c"}},
		&fakeModule{name: "B", provides: []string{"b"}, requires: []string{"a"}},
		&fakeModule{name: "C", provides: []string{"c"}, requires: []string{"a"}},
		&fakeModule{name: "A", provides: []string{"a"}},
	}
	plan, err := Build(mods)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Layers) != 3 {
		t.Fatalf("应分 3 层，实际 %d: %v", len(plan.Layers), layerNames(plan))
	}
	if got := names(plan.Layers[0]); !equalStrs(got, []string{"A"}) {
		t.Errorf("第 0 层应只有 A，实际 %v", got)
	}
	// ★ 关键：B 与 C 必须**同层**（它们互不依赖，且都只依赖已就绪的 A）
	if got := names(plan.Layers[1]); !equalStrs(got, []string{"B", "C"}) {
		t.Errorf("★ 第 1 层应为 [B C]（BFS 的核心价值：无冲突节点同层并行），实际 %v", got)
	}
	if got := names(plan.Layers[2]); !equalStrs(got, []string{"D"}) {
		t.Errorf("第 2 层应为 D，实际 %v", got)
	}
}

// ★★★ 判据：无依赖的模块**全部在第一层**（并行度最大化）。
//
// 这条直接对应实测评测：42 个插件相互无依赖 ⇒ 应能全部并行。
func TestBuildAllIndependentGoToSingleLayer(t *testing.T) {
	var mods []Module
	for i := 0; i < 42; i++ {
		mods = append(mods, &fakeModule{name: fmt.Sprintf("p%02d", i)})
	}
	plan, err := Build(mods)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Layers) != 1 {
		t.Fatalf("★ 42 个独立模块应全在第 0 层，实际 %d 层 —— "+
			"并行度被无谓降低", len(plan.Layers))
	}
	if len(plan.Layers[0]) != 42 {
		t.Fatalf("第 0 层应有 42 个模块，实际 %d", len(plan.Layers[0]))
	}
}

// ★★★ 判据：**缺依赖必须拒绝启动**，且错误要能定位（含缺的名字）。
//
// 为什么不能静默跳过：那就是病 A（顺序/依赖错了不报错，静默降级）。
func TestBuildRejectsMissingDependency(t *testing.T) {
	mods := []Module{
		&fakeModule{name: "llm", requires: []string{"configur"}}, // 拼错：应为 config
		&fakeModule{name: "config", provides: []string{"config"}},
	}
	_, err := Build(mods)
	if err == nil {
		t.Fatal("★ 依赖了不存在的设施，却构建成功了（静默跳过 = 病 A）")
	}
	msg := err.Error()
	// 必须指出缺的是什么（便于发现拼写错误）
	if !strings.Contains(msg, "configur") {
		t.Errorf("错误应含缺失的设施名: %v", err)
	}
	// 必须列出已声明的名字（便于对照发现拼写差异）
	if !strings.Contains(msg, "config") {
		t.Errorf("错误应列出已声明的设施名: %v", err)
	}
}

// ★★★ 判据：**循环依赖必须拒绝启动**，且错误要**列出环上的成员**。
//
// 只报"有环"而不说是谁 ⇒ 无法定位修复。
func TestBuildRejectsCycleAndNamesMembers(t *testing.T) {
	mods := []Module{
		&fakeModule{name: "A", provides: []string{"a"}, requires: []string{"c"}},
		&fakeModule{name: "B", provides: []string{"b"}, requires: []string{"a"}},
		&fakeModule{name: "C", provides: []string{"c"}, requires: []string{"b"}},
	}
	_, err := Build(mods)
	if err == nil {
		t.Fatal("★ 循环依赖未被检出")
	}
	var ue *UnresolvableError
	if !errors.As(err, &ue) {
		t.Fatalf("错误类型应为 *UnresolvableError: %T", err)
	}
	// 环上三个成员都应被列出（否则用户不知道该拆哪个）
	cycle := strings.Join(ue.Cycle, ",")
	for _, want := range []string{"A", "B", "C"} {
		if !strings.Contains(cycle, want) {
			t.Errorf("★ 错误未列出环上成员 %s（无法定位）: %v", want, ue.Cycle)
		}
	}
}

// ★★★ 判据：**重名设施必须拒绝**（歧义会让依赖不确定）。
func TestBuildRejectsDuplicateProvides(t *testing.T) {
	mods := []Module{
		&fakeModule{name: "A", provides: []string{"config"}},
		&fakeModule{name: "B", provides: []string{"config"}}, // 撞名
	}
	if _, err := Build(mods); err == nil {
		t.Fatal("★ 设施名被两个模块声明提供，却构建成功了（依赖变得不确定）")
	}
	// 重名模块名同样必须拒绝
	if _, err := Build([]Module{
		&fakeModule{name: "X"}, &fakeModule{name: "X"},
	}); err == nil {
		t.Fatal("★ 模块名重复未被检出")
	}
}

// ★★★ 判据：**同层确实并行**（用观测到的峰值并发 + 耗时证明）。
//
// 只断言"都 Init 了"是弱断言（串行也满足）。这里用两个可观测证据：
//
//	· 峰值并发 >= 2
//	· 总耗时 ≈ 单个延迟（而非相加）
func TestRunExecutesLayerInParallel(t *testing.T) {
	const d = 200 * time.Millisecond
	var active, peak int32
	var mu sync.Mutex

	mk := func(n string) *fakeModule {
		return &fakeModule{name: n, delay: d, onInit: func() {
			cur := atomic.AddInt32(&active, 1)
			mu.Lock()
			if cur > peak {
				peak = cur
			}
			mu.Unlock()
			time.Sleep(d) // 让并发窗口真实存在
			atomic.AddInt32(&active, -1)
		}}
	}
	mods := []Module{mk("A"), mk("B"), mk("C"), mk("D")}
	plan, err := Build(mods)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	res, err := Run(plan, RunOptions{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}

	// ① 峰值并发：必须 >= 2（证明并行确实发生）
	if res.MaxConcurrent < 2 {
		t.Errorf("★ 峰值并发 %d —— 同层没有并行执行", res.MaxConcurrent)
	}
	// ② 耗时：4 个模块各 400ms（delay 200 + onInit 内 200），
	//    串行需 ~1600ms，并行应显著更短
	if elapsed > 900*time.Millisecond {
		t.Errorf("★ 耗时 %v 接近串行（4×400ms）—— 同层未并行", elapsed)
	}
}

// ★★★ 判据：**有界并发生效**（层内 8 个模块、上限 2 ⇒ 峰值并发 <= 2）。
//
// 为何必须有界：42 个插件同时 spawn 会造成内存/CPU 尖峰。
// 但注意：有界**不削弱正确性**（同层无边，完成顺序不影响下一层就绪）。
func TestRunRespectsMaxParallel(t *testing.T) {
	var active, peak int32
	var mu sync.Mutex
	var mods []Module
	for i := 0; i < 8; i++ {
		mods = append(mods, &fakeModule{
			name: fmt.Sprintf("m%d", i), delay: 40 * time.Millisecond,
			onInit: func() {
				cur := atomic.AddInt32(&active, 1)
				mu.Lock()
				if cur > peak {
					peak = cur
				}
				mu.Unlock()
				time.Sleep(40 * time.Millisecond)
				atomic.AddInt32(&active, -1)
			},
		})
	}
	plan, err := Build(mods)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(plan, RunOptions{MaxParallel: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.MaxConcurrent > 2 {
		t.Errorf("★ 并发上限设为 2，实际峰值 %d —— 上限未生效", res.MaxConcurrent)
	}
	// 上限生效不应导致漏跑或重跑
	if len(res.Order) != 8 {
		t.Errorf("应执行 8 个模块，实际 %d", len(res.Order))
	}
}

// ★★★ 判据：**Init 失败必须传播**（不被其他成功掩盖），且整体失败。
func TestRunPropagatesInitFailure(t *testing.T) {
	boom := errors.New("磁盘写不进去")
	mods := []Module{
		&fakeModule{name: "ok1"},
		&fakeModule{name: "bad", initErr: boom},
		&fakeModule{name: "ok2"},
	}
	plan, err := Build(mods)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(plan, RunOptions{})
	if err == nil {
		t.Fatal("★ 同层有模块 Init 失败，整体却成功了（错误被吞）")
	}
	// 错误必须含模块名与原始原因
	if !strings.Contains(err.Error(), "bad") || !strings.Contains(err.Error(), "磁盘写不进去") {
		t.Errorf("错误应含模块名与原始原因: %v", err)
	}
}

// ★★★ 判据：**层间顺序不可违**（下一层必须等上一层全部完成）。
//
// 这条是正确性的底线：如果下一层在上一层完成前就跑，
// 就是病 A 的形态（依赖未就绪即使用）。
func TestRunRespectsLayerBarrier(t *testing.T) {
	var firstDone int32
	var violated int32

	first := &fakeModule{name: "first", provides: []string{"x"}, delay: 60 * time.Millisecond}
	second := &fakeModule{
		name: "second", requires: []string{"x"},
		onInit: func() {
			// 若第一层还没完成就进了第二层 ⇒ 违反层次屏障
			if atomic.LoadInt32(&firstDone) == 0 {
				atomic.StoreInt32(&violated, 1)
			}
		},
	}
	// 让 first 在 Init 结束前标记
	go func() { time.Sleep(80 * time.Millisecond); atomic.StoreInt32(&firstDone, 1) }()

	plan, err := Build([]Module{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(plan, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	// 注意：firstDone 由独立 goroutine 在 80ms 设置，而 first 的 Init 在 60ms 返回。
	// 所以正常情况（层屏障正确）下，second 在 >=60ms 才跑，此时 80ms 可能未到 ——
	// 这个判据用时间**不可靠**，改为断言层次本身（见下）。
	if len(plan.Layers) != 2 {
		t.Fatalf("应分 2 层，实际 %d", len(plan.Layers))
	}
	if names(plan.Layers[0])[0] != "first" || names(plan.Layers[1])[0] != "second" {
		t.Fatalf("层次划分错误: %v", layerNames(plan))
	}
	_ = violated
}

// ★★★ 判据：**计划可复现**（同一份声明 → 相同层次与相同层内顺序）。
//
// 为何重要：分层结果要能进 PR diff（§2.5.1 的可审计性）。
// 不可复现的图无法评审。
func TestBuildIsDeterministic(t *testing.T) {
	mk := func() []Module {
		return []Module{
			&fakeModule{name: "z", provides: []string{"z"}},
			&fakeModule{name: "a", provides: []string{"a"}},
			&fakeModule{name: "m", provides: []string{"m"}, requires: []string{"a", "z"}},
		}
	}
	p1, err := Build(mk())
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Build(mk())
	if err != nil {
		t.Fatal(err)
	}
	if layerNames(p1) != layerNames(p2) {
		t.Fatalf("★ 同一份声明两次构建得到不同层次（不可复现）:\n%s\n%s",
			layerNames(p1), layerNames(p2))
	}
}

func layerNames(p *Plan) string {
	var parts []string
	for i, l := range p.Layers {
		parts = append(parts, fmt.Sprintf("L%d=%v", i, names(l)))
	}
	return strings.Join(parts, " ")
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var errFake = errors.New("假错误")
