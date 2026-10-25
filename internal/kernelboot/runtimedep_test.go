package kernelboot

// 执行期依赖校验（verifyLayerDependencies）的纯逻辑判据（2026-10-24，ID:14）。
//
// ## 为什么与 Build 的判据分开
//
// Build 的判据（TestBuildRejectsMissingDependency / …Cycle …）验的是**声明**：
// 一个模块 Requires 了没人 Provides 的设施，或成环 ⇒ Build 就得拒绝。
//
// 本文件的判据假设**声明完全合法**（Build 能算出图），只把算好的 Plan 的
// 层顺序改坏 —— 这才是 Build 绿而 Run 崩的那类故障（实测 SIGSEGV 见
// internal/kernelmod/all/runtime_dep_test.go）。
//
// ## 覆盖的四类形状
//
//	① 正序（正确开销）           ⇒ 不误报
//	② 提供者排到消费者之后        ⇒ 指名模块名 + 缺的设施
//	③ 提供者整层缺失              ⇒ 第 0 层就拒绝，一个 Init 都不跑
//	④ 层内多模块各缺不同依赖      ⇒ 逐个模块报（不是只报第一个）
//
// 另有一条"M2 型变异"的自检：如果 produced 从 plan 预填（拿声明当事实），
// ② ③ 会变成**永不触发**的假绿 —— 判据必须能把它抓出来。

import (
	"errors"
	"strings"
	"testing"
)

// chainPlan 造一个三层的合法计划：c → b → a（c 无依赖，a 最上层）。
func chainPlan(t *testing.T) *Plan {
	t.Helper()
	mods := []Module{
		&fakeModule{name: "a", provides: []string{"fa"}, requires: []string{"fb"}},
		&fakeModule{name: "b", provides: []string{"fb"}, requires: []string{"fc"}},
		&fakeModule{name: "c", provides: []string{"fc"}},
	}
	plan, err := Build(mods)
	if err != nil {
		t.Fatalf("前置：声明必须合法（本文件只测顺序被改坏）: %v", err)
	}
	return plan
}

// ★★★ ① 正序不误报。
func TestRunGuardAcceptsCorrectOrder(t *testing.T) {
	if _, err := Run(chainPlan(t), RunOptions{}); err != nil {
		t.Fatalf("★ 合法顺序被判成缺依赖（误报）: %v", err)
	}
}

// ★★★ ② 提供者排到消费者之后 ⇒ 指名道姓报错，且 Init 一个都没跑。
func TestRunGuardRejectsSwappedLayers(t *testing.T) {
	plan := chainPlan(t)
	// 把 b（提供 fb）与其消费者 a 交换：a 先跑，它要的 fb 还没产出。
	swapped := &Plan{Layers: [][]Module{
		plan.Layers[0], // c
		plan.Layers[2], // a  ← 原本在最后
		plan.Layers[1], // b
	}}

	var inited []string
	for _, l := range swapped.Layers {
		for _, m := range l {
			mm := m.(*fakeModule)
			inner := mm.onInit
			mm.onInit = func() {
				inited = append(inited, mm.name)
				if inner != nil {
					inner()
				}
			}
		}
	}

	_, err := Run(swapped, RunOptions{})
	if err == nil {
		t.Fatal("★ 顺序被改坏却装配成功 —— 校验没生效（模块会拿到 nil 依赖）")
	}
	var dnr *DependencyNotReadyError
	if !errors.As(err, &dnr) {
		t.Fatalf("★ 期望 *DependencyNotReadyError，实际 %T: %v", err, err)
	}
	if got, want := strings.Join(dnr.Missing["a"], ","), "fb"; got != want {
		t.Errorf("★ 缺依赖报错不准：期望 a 缺 fb，实际 %v", dnr.Missing)
	}
	if len(inited) != 1 || inited[0] != "c" {
		t.Errorf("★ 违规层不该有任何模块被 Init（只应跑过 c 层），实际跑了 %v", inited)
	}
	for _, sub := range []string{"第 1 层", "模块 a", "fb"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("★ 报错文本缺少 %q（原文: %s）", sub, err.Error())
		}
	}
}

// ★★★ ③ 提供者整层缺失 ⇒ 第 0 层就拒绝，produced 为空。
func TestRunGuardRejectsMissingProviderLayer(t *testing.T) {
	plan := chainPlan(t)
	cut := &Plan{Layers: [][]Module{plan.Layers[1], plan.Layers[2]}}

	_, err := Run(cut, RunOptions{})
	if err == nil {
		t.Fatal("★ 提供者整层缺失却装配成功")
	}
	var dnr *DependencyNotReadyError
	if !errors.As(err, &dnr) {
		t.Fatalf("★ 期望 *DependencyNotReadyError，实际 %T: %v", err, err)
	}
	if dnr.Layer != 0 {
		t.Errorf("★ 应在第 0 层拒绝，实际第 %d 层", dnr.Layer)
	}
	if len(dnr.Produced) != 0 {
		t.Errorf("★ 第 0 层之前不该有产出，实际 %v", dnr.Produced)
	}
}

// ★★★ ④ 同层多模块各缺不同依赖 ⇒ 逐个报（不是只报第一个，否则要修 N 轮）。
func TestRunGuardReportsEveryMissingModuleInLayer(t *testing.T) {
	// 把 a 与 b 放进同一层，且都不提供任何东西 ⇒ 两者都缺依赖。
	merged := &Plan{Layers: [][]Module{
		{&fakeModule{name: "a", requires: []string{"fb"}},
			&fakeModule{name: "b", requires: []string{"fc"}}},
	}}
	_, err := Run(merged, RunOptions{})
	if err == nil {
		t.Fatal("★ 缺依赖却装配成功")
	}
	var dnr *DependencyNotReadyError
	if !errors.As(err, &dnr) {
		t.Fatalf("★ 期望 *DependencyNotReadyError，实际 %T: %v", err, err)
	}
	if len(dnr.Missing) != 2 {
		t.Fatalf("★ 只报了 %d 个模块缺依赖（应 2 个，否则要修 N 轮才看得全）: %v",
			len(dnr.Missing), dnr.Missing)
	}
	if strings.Join(dnr.Missing["a"], ",") != "fb" || strings.Join(dnr.Missing["b"], ",") != "fc" {
		t.Errorf("★ 报错内容不准: %v", dnr.Missing)
	}
}

// ★★★ 自检（防 M2 型变异）：把 produced 预填成"全部声明" ⇒ 上面 ②③ 必须红。
//
// 这条不是测被测代码，而是测**判据具不具备判别力**：
// 若有人把执行期校验改成"从 plan 预填 produced"（等于相信声明），
// 它就退化成 Build 的重复实现，抓不到顺序被改坏 —— 那时本文件的 ②③
// 会变绿（假绿）。所以这里直接把这种实现形态跑一遍，要求它**确实**放行违规，
// 从而证明 ②③ 的红不是"任何实现都会红"。
func TestGuardPrefilledFromPlanIsDetectablyWeaker(t *testing.T) {
	plan := chainPlan(t)
	prefilled := map[string]bool{}
	for _, l := range plan.Layers {
		for _, m := range l {
			for _, p := range m.Provides() {
				prefilled[p] = true
			}
		}
	}
	// 违规层：a 在第 0 层，但它要的 fb 由第 2 层的 b 提供。
	layer := []Module{plan.Layers[2][0]}
	if err := verifyLayerDependencies(0, layer, prefilled); err != nil {
		t.Fatalf("★ 预填 produced（拿声明当事实）竟报错 —— 说明本判据的选择无法区分两种实现，需重设: %v", err)
	}
	// 同一层、同一顺序，用**空** produced（真事实）必须报错 ⇒ 判据有判别力。
	if err := verifyLayerDependencies(0, layer, map[string]bool{}); err == nil {
		t.Fatal("★ 空 produced 也不报错 —— verifyLayerDependencies 没有判别力")
	}
}
