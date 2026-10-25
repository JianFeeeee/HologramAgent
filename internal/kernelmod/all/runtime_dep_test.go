package all

// ID:14 · Run 的**执行期**依赖校验判据（2026-10-24）。
//
// ## 与构建期校验的分工（别把这条当成 Build 的重复实现）
//
//   - 构建期（kernelboot.Build → diagnoseUnresolvable）：看**声明**。
//     Requires 有没有人 Provides、有没有环。声明写对了就绿。
//   - 执行期（kernelboot.Run → verifyLayerDependencies，本文件测的）：看**事实**。
//     按这份 plan 真的跑完的层里产出了什么。
//
// 两者不可互相替代。**声明全对、图也自洽**时，plan 的层顺序仍可能被改坏
// （手改 Layers / 图与代码漂移 / 有人把某层提前），此时 Build 依然绿，
// 而模块 Init 拿到的是 nil 依赖 ⇒ 崩在业务代码里。
//
// 本判据的复现形状就是实测撞到的那个：让 **llm 排到 lua 之前**，
// llm.buildProviderManager 收到 0x0 的 LuaVM，在
// `luaVM.ConfigureConcurrency(...)`（internal/lua/vm.go:212 取 v.mu）里 SIGSEGV。
//
// ## 为什么判据必须用真实登记表
//
// 用假模块自造一份错误的层顺序，只能证明"这段校验逻辑会报错"，
// 不能证明"真实 17 个模块的依赖形状被它覆盖"。这里直接拿
// kernelmod.Registered() 算出的真实 plan 做变异（删除首层 / 交换两层），
// 报错里的模块名与设施名都是真名字。

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// realPlan 用真实登记表算分层计划（与生产 dev 路径同一个入口）。
func realPlan(t *testing.T) *kernelboot.Plan {
	t.Helper()
	plan, err := kernelboot.Build(kernelmod.Registered())
	if err != nil {
		t.Fatalf("真实登记表算不出启动图: %v", err)
	}
	if len(plan.Layers) < 4 {
		t.Fatalf("★ 真实计划只有 %d 层 —— 这条判据的变异前提不成立（层数太少无法制造顺序破坏）",
			len(plan.Layers))
	}
	return plan
}

// layerNames 返回某层的模块名（升序，便于断言消息）。
func layerNames(layer []kernelboot.Module) []string {
	out := make([]string, 0, len(layer))
	for _, m := range layer {
		out = append(out, m.Name())
	}
	return out
}

// emptyState 造一个最小可用的 KernelState：只填 Group 0 的产物。
//
// ★ 为什么必须填 Cfg：`config` 模块的 Init 会显式拒绝 nil 的 ks.Cfg
// （它自己就有前置检查）。不填的话"正确的 L0 也跑不起来"，
// 那样就分不清红是因为顺序变了还是因为夹具没准备好。
//
// ★ 为什么连 Daemon.DataDir 一起填：万一有人在本判据上做"临时摸掉守卫"的
// 变异，模块会真的跑到写盘那一行。不填的话 text-memory 会往**仓内**
// 当前目录下的 memory/text 写（实测产生过 internal/kernelmod/all/memory/）。
// 夹具不能往仓库里漏文件。
func emptyState(t *testing.T) *kernelboot.KernelState {
	t.Helper()
	dir := t.TempDir()
	ks := kernelboot.NewKernelState()
	ks.Cfg = &types.Config{}
	ks.Cfg.Daemon.DataDir = dir
	ks.Cfg.Plugin.Dir = filepath.Join(dir, "plugins")
	ks.DataDir = dir
	ks.AgentWorkDir = filepath.Join(dir, "agentfs")
	ks.HTTPAddr = "127.0.0.1:0"
	return ks
}

// ★★★ 判据 A（正向基线）：**正确的层顺序下，执行期校验不误报**。
//
// 取真实 plan 的**第一层**单独跑：模块（config / kernel-api）本来就绪条件
// 为空，若能通过 ⇒ 校验没把"合法的第 0 层"当成缺依赖。
//
// （整机正序不误报由 internal/kernelmod/all 的整机判据覆盖（bootgraph_test.go）：
// 它用真实 plan 跑完全部 5 层，装配成功后资产/关闭/泄漏全部断言。
// 这里只做最小正向基线，避免重复一份整机夹具。）
func TestRunRuntimeGuardDoesNotFireOnCorrectPrefix(t *testing.T) {
	plan := realPlan(t)
	prefix := &kernelboot.Plan{Layers: [][]kernelboot.Module{plan.Layers[0]}}

	if _, err := kernelboot.Run(prefix, kernelboot.RunOptions{State: emptyState(t)}); err != nil {
		t.Fatalf("★ 合法前缀（第 0 层 %v）被判成缺依赖 —— 校验误报: %v",
			layerNames(plan.Layers[0]), err)
	}
}

// ★★★ 判据 B（**复现原始崩溃条件**）：把提供者排到消费者之后 ⇒ 必须
// 返回可读的 DependencyNotReadyError，**不是** SIGSEGV。
//
// 做法：交换真实 plan 的第 1 层与第 2 层。
// 交换后 lua/memory/onnx-parser/knowledge-migration 都排到了 llm/text-memory/
// knowledge-store 之后 —— 这正是"改层顺序"的最小形态，也是实测崩溃的场景
// （llm 拿 0x0 LuaVM）。
//
// 断言三件事：
//  1. 错误类型是 *DependencyNotReadyError（不是 panic、不是 Init 透传的错）
//  2. 报错**指名**哪个模块缺哪个设施（可读、可定位）
//  3. 报错发生在**装配之前**：此前各层实际产出的设施只有 L0 那两个，
//     且 llm 依赖的 LuaVM 从未被创建（ks.LuaVM == nil）
func TestRunRejectsLayerOrderViolation(t *testing.T) {
	plan := realPlan(t)

	// 前置：真实 plan 的第 1 层确实提供 lua/memory/onnx，第 2 层确实消费它们。
	// 若前提变了（有人调整了模块依赖），下面交换就不再"制造出违规" ⇒ 判据变空转。
	providesIn := func(layer []kernelboot.Module, fac string) bool {
		for _, m := range layer {
			for _, p := range m.Provides() {
				if p == fac {
					return true
				}
			}
		}
		return false
	}
	for _, fac := range []string{kernelmod.FacLua, kernelmod.FacMemory, kernelmod.FacONNX} {
		if !providesIn(plan.Layers[1], fac) {
			t.Fatalf("★ 前提不成立：第 1 层不再提供 %s —— 本判据的变异会变成空转（不会制造违规）", fac)
		}
	}

	swapped := &kernelboot.Plan{Layers: [][]kernelboot.Module{
		plan.Layers[0],
		plan.Layers[2], // ← 原本在第 3 位：knowledge-store / llm / text-memory
		plan.Layers[1], // ← 原本在第 2 位：lua / memory / onnx-parser / …
	}}
	// 只保留到违规发生处（第 1 层）就够：守卫在 Init 之前就失败。
	swapped.Layers = swapped.Layers[:2]

	ks := emptyState(t)
	_, err := kernelboot.Run(swapped, kernelboot.RunOptions{State: ks})
	if err == nil {
		t.Fatalf("★ 层顺序被改坏（%v 排到了 %v 之前）却装配成功 —— 校验没生效",
			layerNames(plan.Layers[2]), layerNames(plan.Layers[1]))
	}
	var dnr *kernelboot.DependencyNotReadyError
	if !errors.As(err, &dnr) {
		t.Fatalf("★ 期望 *DependencyNotReadyError（可读报错），实际是 %T: %v",
			err, err)
	}

	// ② 指名道姓：llm 缺 lua、text-memory 缺 memory/onnx、knowledge-store 缺迁移标记。
	want := map[string][]string{
		"llm":             {kernelmod.FacLua},
		"text-memory":     {kernelmod.FacMemory, kernelmod.FacONNX},
		"knowledge-store": {kernelmod.FacMigrated},
	}
	for mod, facs := range want {
		got, ok := dnr.Missing[mod]
		if !ok {
			t.Fatalf("★ 报错没指名模块 %s 缺依赖。实际缺失表: %v", mod, dnr.Missing)
		}
		if strings.Join(got, ",") != strings.Join(facs, ",") {
			t.Errorf("★ 模块 %s 缺失设施报错不准：期望 %v，实际 %v", mod, facs, got)
		}
	}
	// 报错文本必须能直接照着修（模块名 + 设施名 + 层号都在里面）。
	for _, wantSub := range []string{"第 1 层", "llm", kernelmod.FacLua, "缺设施"} {
		if !strings.Contains(err.Error(), wantSub) {
			t.Errorf("★ 报错文本缺少 %q，无法据此定位（原文: %s）", wantSub, err.Error())
		}
	}

	// ③ 违规发生在装配之前：只跑完了 L0。
	//
	// ★ L0 现在是三个：config / kernel-api / update。update 模块 Requires
	//   为空（它肩负"启动失败回滚上一次升级"，必须早于所有模块），所以
	//   天然落在 L0。这里断言**集合**而非顺序 —— 注册表本就是乱序的。
	got := dnr.Produced
	wantL0 := map[string]bool{
		kernelmod.FacConfig:   true,
		kernelmod.FacKernelIO: true,
		kernelmod.FacVersion:  true,
	}
	if len(got) != len(wantL0) {
		t.Errorf("★ 守卫失败时 L0 应产出 %d 个设施，实际 %v", len(wantL0), dnr.Produced)
	}
	for _, f := range got {
		if !wantL0[f] {
			t.Errorf("★ L0 出现了意外的设施 %q（实际产出 %v）", f, dnr.Produced)
		}
	}
	if ks.LuaVM != nil {
		t.Error("★ 守卫失败后 ks.LuaVM 非 nil —— 说明 lua 模块被跑过了，与\"装配前校验\"矛盾")
	}
	if dnr.Layer != 1 {
		t.Errorf("★ 报错层号应为 1，实际 %d", dnr.Layer)
	}
}

// ★★★ 判据 C（变异证明：删掉首层 = 所有消费者都缺依赖）：
//
// 与判据 B 不同的问题形态 —— 不是"顺序颠倒"，而是"提供者整层消失"
// （旧图/新代码混装、或有人误删一层）。此时第 0 层全部模块缺 config/kernel-io，
// 且**一个 Init 都不该跑**。
func TestRunRejectsMissingProvidingLayer(t *testing.T) {
	plan := realPlan(t)

	// 删掉第 0 层，其余层原样前移。
	cut := &kernelboot.Plan{Layers: append([][]kernelboot.Module{}, plan.Layers[1:]...)}

	ks := emptyState(t)
	_, err := kernelboot.Run(cut, kernelboot.RunOptions{State: ks})
	if err == nil {
		t.Fatal("★ 提供者整层消失却装配成功")
	}
	var dnr *kernelboot.DependencyNotReadyError
	if !errors.As(err, &dnr) {
		t.Fatalf("★ 期望 *DependencyNotReadyError，实际 %T: %v", err, err)
	}
	if dnr.Layer != 0 {
		t.Errorf("★ 应在第 0 层就拒绝（该层没有任何前置产出），实际报第 %d 层", dnr.Layer)
	}
	if len(dnr.Produced) != 0 {
		t.Errorf("★ 第 0 层之前不该有任何产出，实际 %v", dnr.Produced)
	}
	// 原第 1 层的模块全部缺 config 或 kernel-io（每个模块至少要一个）。
	if len(dnr.Missing) < 5 {
		t.Fatalf("★ 只报出 %d 个模块缺依赖（原第 1 层有 %d 个）—— 校验漏报",
			len(dnr.Missing), len(plan.Layers[1]))
	}
	for mod, facs := range dnr.Missing {
		ok := false
		for _, f := range facs {
			if f == kernelmod.FacConfig || f == kernelmod.FacKernelIO {
				ok = true
			}
		}
		if !ok {
			t.Errorf("★ 模块 %s 被报缺 %v，但它缺的应是 config/kernel-io（整层消失）", mod, facs)
		}
	}
	// 一个模块都不该跑过：IO / EventBus 是 kernel-api 的产物，应仍为 nil。
	if ks.IO != nil || ks.EventBus != nil {
		t.Error("★ 第 0 层被判缺依赖后仍有模块被执行（IO/EventBus 非 nil）")
	}
}
