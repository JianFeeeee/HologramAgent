package pluginhost

// plugin-host（插件宿主）的判据（2026-10-16）。
//
// 设计见 docs/zh/kernel-modularization-design.md §2.6 + 用户方案 A。
//
// ## 这组判据守什么
//
//  ① **三形态分流正确**（用户明确要求：自动启动 / 启用但不自动启动 / 禁用）
//  ② **元数据缓存**（用户指出的设计：扫一次、供调度与展示共用）
//  ③ **内置插件不被漏掉** ← 这是我实际踩过的回归（19 个全丢）
//  ④ 关键顺序：failback 白名单必须在插件拉起前生效

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/JianFeeeee/HomeAgent/internal/kernelboot"
)

// makePluginDir 造一个带 plugin.json 的插件目录。
func makePluginDir(t *testing.T, root, name, manifest string) {
	t.Helper()
	d := filepath.Join(root, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ★★★ 判据：**三形态分流正确**（用户设计要求的三种形态）。
func TestClassifyThreeForms(t *testing.T) {
	root := t.TempDir()
	// ① 自动启动 + 声明依赖
	makePluginDir(t, root, "auto_declared",
		`{"name":"auto_declared","version":"1","entry":"plugin.bin","requires_facilities":["config"]}`)
	// ① 自动启动 + 未声明
	makePluginDir(t, root, "auto_plain",
		`{"name":"auto_plain","version":"1","entry":"plugin.bin"}`)
	// ② 启用但不自动启动
	makePluginDir(t, root, "manual",
		`{"name":"manual","version":"1","entry":"plugin.bin","requires_facilities":["config"],"auto_start":false}`)
	// ③ 禁用
	makePluginDir(t, root, "banned",
		`{"name":"banned","version":"1","entry":"plugin.bin"}`)

	cat, _ := scanCatalog(root, func(n string) bool { return n == "banned" })
	g := classify(cat)

	eq := func(got, want []string, what string) {
		t.Helper()
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("★ %s: 期望 %v 实际 %v", what, want, got)
		}
	}
	eq(g.autoStart, []string{"auto_declared", "auto_plain"}, "① 自动启动")
	eq(g.declared, []string{"auto_declared"}, "① 中声明了依赖的")
	eq(g.manual, []string{"manual"}, "② 启用但不自动启动")
	eq(g.disabled, []string{"banned"}, "③ 持久禁用")

	// ★ 关键互斥：三形态必须互不重叠（一个插件只能属于一种）
	inBoth := func(a, b []string, an, bn string) {
		t.Helper()
		m := map[string]bool{}
		for _, x := range a {
			m[x] = true
		}
		for _, x := range b {
			if m[x] {
				t.Errorf("★ %s 与 %s 同时包含 %s（三形态必须互斥）", an, bn, x)
			}
		}
	}
	inBoth(g.autoStart, g.manual, "autoStart", "manual")
	inBoth(g.autoStart, g.disabled, "autoStart", "disabled")
	inBoth(g.manual, g.disabled, "manual", "disabled")
}

// ★★★ 判据：**元数据缓存内容正确**（用户设计：读列表→写缓存）。
func TestCatalogCachesMetadata(t *testing.T) {
	root := t.TempDir()
	makePluginDir(t, root, "p1",
		`{"name":"p1","name_zh":"一","version":"2.3.4","entry":"plugin.bin",
		  "description":"演示","requires_facilities":["config","llm"],"tags":["a","b"]}`)

	cat, noskip := scanCatalog(root, nil)
	if len(noskip) != 0 {
		t.Errorf("不该有跳过的目录: %v", noskip)
	}
	if cat.Size() != 1 {
		t.Fatalf("缓存应有 1 条，实际 %d", cat.Size())
	}
	m, ok := cat.Get("p1")
	if !ok {
		t.Fatal("取不到 p1 的元数据")
	}
	// 每个字段都要缓存对（调度靠 RequiresFacilities，展示靠其余）
	if len(m.RequiresFacilities) != 2 || m.RequiresFacilities[0] != "config" {
		t.Errorf("RequiresFacilities 未正确缓存: %v", m.RequiresFacilities)
	}
	if !m.AutoStart {
		t.Error("缺省 auto_start 应为 true")
	}
	if m.Version != "2.3.4" || m.NameZh != "一" || m.Description != "演示" {
		t.Errorf("展示字段未正确缓存: %+v", m)
	}
	if len(m.Tags) != 2 {
		t.Errorf("Tags 未正确缓存: %v", m.Tags)
	}
	if m.ManifestPath == "" || m.ScannedAt.IsZero() {
		t.Error("诊断字段（路径/扫描时刻）应被记录")
	}
	// 缓存必须能列出（pluginmgr 的列表用它）
	if names := cat.Names(); len(names) != 1 || names[0] != "p1" {
		t.Errorf("Names 不对: %v", names)
	}
}

// ★★★ 判据：**无 plugin.json 的目录被记为"无 manifest"而不是"错误"**。
//
// ★ 这条守的是我实际踩过的回归：19 个**内置插件**目录没有 plugin.json
// （它们是编译期工厂），我第一版把它们记成"跳过 19 个非插件目录"并
// **完全没有加载它们**（webui/pluginmgr/cli/files/timer… 全丢）。
//
// 判据分两层：
//
//	· 本层：scanCatalog 必须**如实**报告哪些目录没 manifest（便于诊断）
//	· 装配层：LoadSkipping 必须补上内置插件（见 TestBuiltinsNotDropped）
func TestScanReportsNoManifestDirs(t *testing.T) {
	root := t.TempDir()
	makePluginDir(t, root, "has_manifest", `{"name":"has_manifest","version":"1","entry":"plugin.bin"}`)
	// 模拟内置插件目录：只有可执行文件，没有 plugin.json
	builtin := filepath.Join(root, "builtin_plugin")
	os.MkdirAll(builtin, 0o755)
	os.WriteFile(filepath.Join(builtin, "plugin.bin"), []byte("x"), 0o755)

	cat, noManifest := scanCatalog(root, nil)
	if cat.Size() != 1 {
		t.Errorf("缓存应只含带 manifest 的（1 条），实际 %d", cat.Size())
	}
	if len(noManifest) != 1 || noManifest[0] != "builtin_plugin" {
		t.Fatalf("★ 无 manifest 的目录必须被如实报告（便于区分「内置插件」与「坏插件」）: %v",
			noManifest)
	}
}

// ★★★ 判据：**内置插件不会被漏掉**（行为判据，回归守）。
//
// ## 为什么这条必须存在
//
// 我第一版让 plugin-host 只处理"有 plugin.json 的外部插件"，
// 结果 **19 个内置插件全部没加载** —— webui（人机界面）、pluginmgr
// （插件管理）、cli、files、cmd、timer、mcp… 全丢，而**日志看起来正常**
// （只有一条"跳过 19 个非插件目录"的提示，很容易被当成噪音略过）。
//
// ## ★ 为什么必须是**行为**判据（我第一版写成源码检查 ⇒ 变异不咬）
//
// 初版检查"源码里有 `LoadSkipping(`"。但删掉**调用**后，
// 注释里仍有 `LoadSkipping` 字样 ⇒ 判据照样绿
// （**测写法不测行为** —— 本仓反复踩的同一类坑）。
//
// ⇒ 改为跑**生产同一个函数** `buildSkipSet`，断言：
//
//	① 内置插件（无 manifest）**不在** skip 里 ⇒ 会走全量加载
//	② 三形态的插件**在** skip 里（防重复加载 / 防误加载）
func TestBuiltinsAreNotSkipped(t *testing.T) {
	root := t.TempDir()
	makePluginDir(t, root, "ext_auto",
		`{"name":"ext_auto","version":"1","entry":"plugin.bin","requires_facilities":["config"]}`)
	makePluginDir(t, root, "ext_manual",
		`{"name":"ext_manual","version":"1","entry":"plugin.bin","auto_start":false}`)

	cat, _ := scanCatalog(root, nil)
	g := classify(cat)
	skip := buildSkipSet(cat, g)

	// ★ 核心：内置插件（不在 catalog 里 = 没有 plugin.json）**不得**被跳过。
	//   若它在 skip 里 ⇒ LoadSkipping 会跳过它 ⇒ 它永远不会被加载。
	const builtinName = "builtin_only"
	if skip[builtinName] {
		t.Fatalf("★ 内置插件 %s 在 skip 集合里 ⇒ 不会被加载"+
			"（webui/pluginmgr/cli/files/timer/mcp 全是这类，会静默消失）", builtinName)
	}
	if !skip["ext_auto"] {
		t.Error("① 声明依赖的插件应在 skip 里（plugin-host 自己拉起，避免重复）")
	}
	if !skip["ext_manual"] {
		t.Fatal("★ auto_start=false 的插件必须在 skip 里 —— 否则会被全量加载" +
			"（与「不自动启动」完全相反）")
	}
}

// ★★★ 判据：**meta 层不谎报"已接线"** + 接线本身由 e2e 守。
//
// ## ★ 这里我连续踩了三次同一个坑，值得写下来
//
// 初版判据：`strings.Contains(code, "LoadSkipping(")` ⇒ 变异不咬，
//
//	因为**注释里也有这个字样**。
//
// 二版判据：`strings.Contains(code, "LoadSkipping(dir, skip)")` ⇒
//
//	变异成 `LoadSkipping(dir, nil)` 后**仍然绿**，因为 mod.go 的
//	**注释第 116 行正好写着 `` `LoadSkipping(dir, skip)` ``**。
//
// **教训（第 N+1 次）：源码字符串检查测的是"文本"，不是"行为"。**
// 只要文档/注释里出现过同一串，判据就永远绿 —— 而本仓的规范恰恰是
// "每个关键决定都要在注释里讲清楚"，所以注释里几乎必然出现这些串。
// ⇒ **源码检查只能用于"是否存在某个符号定义"，绝不能用于断言接线。**
//
// ## 那接线靠什么守？
//
// 行为层：真机 e2e 已验证（20 个插件加载、manual_probe **未**加载、
// 内置插件全部在列）。这条 e2e 是接线的真正判据。
//
// 本判据只做一件**不依赖注释**的事：确认 `buildSkipSet` 有**真实调用点**
// （AST 层面：调用表达式，不是注释里的文本）。
func TestSkipSetHasRealCallSite(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "mod.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := ce.Fun.(*ast.Ident); ok && id.Name == "buildSkipSet" {
			calls++
		}
		return true
	})
	if calls == 0 {
		t.Fatal("★ buildSkipSet 必须被**真实调用**（AST 层面）" +
			"—— 定义而不用等于没接。\n" +
			"   （注意：不能用字符串 Contains 判 —— 注释里会命中，实测骗过我三次）")
	}
	// LoadSkipping 的**调用**（AST 层面）必须存在
	ls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := ce.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "LoadSkipping" {
			ls++
			// ★ 且第二个实参必须**不是** nil（nil = skip 失效）
			if len(ce.Args) >= 2 {
				if id, ok := ce.Args[1].(*ast.Ident); ok && id.Name == "nil" {
					t.Error("★ LoadSkipping 的 skip 实参是 nil ⇒ " +
						"三形态跳过逻辑全部失效（内置插件会被误跳过、" +
						"manual 会被误加载）")
				}
			}
		}
		return true
	})
	if ls == 0 {
		t.Fatal("★ 必须真实调用 LoadSkipping（AST 层面，非注释）")
	}
}

// ★★★ 判据：**缓存并发安全**（pluginmgr 运行期读、装配期写）。
func TestCatalogConcurrentRead(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		makePluginDir(t, root, "p"+string(rune('a'+i)),
			`{"name":"x","version":"1","entry":"plugin.bin"}`)
	}
	cat, _ := scanCatalog(root, nil)

	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				_ = cat.Names()
				_ = cat.All()
				_ = cat.Size()
				_, _ = cat.Get("pa")
			}
		}()
	}
	for i := 0; i < 16; i++ {
		<-done
	}
}

// ★★★ 判据：**SyncBus 为 nil 时不得 panic**（typed-nil 陷阱，实测踩到）。
//
// ## 为什么必须有这条
//
// `var nb *SyncBus; var i SyncState = nb` ⇒ **i != nil**（经典陷阱）。
// 于是 `NewScheduler(i, …)` 内部 `i.IsReady(...)` 解引用空指针 ⇒ **panic**。
//
// 实测：`ks.SyncBus == nil`（非生产装配路径 / 单测）时 plugin-host 会崩。
// 而 panic 发生在**插件拉起路径**上 ⇒ 整个内核启动失败。
//
// ⇒ 判据：把 typed-nil 传给 Scheduler 前必须已被拦截。
// 这里直接验证**拦截逻辑本身**（不依赖 produciton 装配）。
func TestNilSyncBusDoesNotPanic(t *testing.T) {
	var nb *kernelboot.SyncBus // typed-nil
	var iface kernelboot.SyncState = nb
	if iface == nil {
		t.Skip("接口就是 nil（不会有陷阱）—— 与本判据的前提不符")
	}
	// ★ 确认陷阱真实存在（否则这条判据是空的）
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("★ 前提不成立：typed-nil bus 竟然没 panic —— " +
					"那就不需要那条 nil 拦截了，请一并删掉测试与拦截")
			}
		}()
		_ = kernelboot.NewScheduler(iface, func(kernelboot.PluginSpec) error { return nil })
	}()
	// ★ 而生产代码里的拦截：mod.go 必须**显式判 nil** 后再传
	src, err := os.ReadFile("mod.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	if !strings.Contains(code, "ks.SyncBus != nil") {
		t.Fatal("★ mod.go 必须显式判 `ks.SyncBus != nil` 再构造 Scheduler —— " +
			"否则 typed-nil 会在 IsReady 处 panic（实测）")
	}
	if strings.Contains(code, "NewScheduler(ks.SyncBus,") {
		t.Fatal("★ 不得把 ks.SyncBus **直接**传给 NewScheduler（typed-nil 陷阱）")
	}
}

// ★★★★ 判据：**未声明依赖的插件必须等内核完全初始化**（§2.6.5 硬约束）。
//
// ## 这是我实际违反过、且有实测后果的约束 —— 必须写成判据
//
// 我第一版把插件按"本模块依赖是否覆盖插件声明"分成 launchNow / waitLater，
// 并在 Init 里**直接**加载 launchNow。而未声明依赖的插件 `RequiresFacilities`
// 是**空** ⇒ "覆盖空集"恒真 ⇒ 它们被归入 launchNow ⇒ **第 4 层就被加载**。
//
// 而 §2.6.5 的硬约束是「未声明 ⇒ 启动时机**逐字不变**（内核**完全**初始化后）」。
// 原文警告："静默改变启动时机 = 静默引入竞态"。
//
// ## ★ 后果不是理论上的 —— 实测丢了一个工具
//
// 内置插件 `healthcheck` 在 `Start` 里做：
//
//	if s.Status() != nil { s.RegisterTool("healthcheck_kernel", …) }
//
// 第 4 层时 agent 还没建好（它与 plugin-host **并行**）⇒ `Status()` 为 nil
// ⇒ **静默少注册一个工具**。实测：86 个工具（应为 87），**无任何报错**。
// （nginx 式的 nil 保护把"时序错了"降级成了"少个功能"，最难发现。）
//
// ⇒ 判据：未声明的插件**不得**在 `KernelComplete()` 之前被拉起。
// 用**真的 Scheduler + 真的 SyncBus** 验证（跑生产同一路径）。
func TestUndeclaredPluginsWaitForKernelComplete(t *testing.T) {
	root := t.TempDir()
	makePluginDir(t, root, "undeclared",
		`{"name":"undeclared","version":"1","entry":"plugin.bin"}`)
	makePluginDir(t, root, "declared",
		`{"name":"declared","version":"1","entry":"plugin.bin","requires_facilities":["config"]}`)

	cat, _ := scanCatalog(root, nil)
	g := classify(cat)
	specs := buildSpecs(cat, g)
	if len(specs) != 2 {
		t.Fatalf("两个自动启动插件都应进调度器（含**未声明**的），实际 %d 个", len(specs))
	}

	bus := kernelboot.NewSyncBus()
	bus.MarkDeclared([]string{"config", "agent"})
	bus.MarkReady("config")

	launched := map[string]bool{}
	sched := kernelboot.NewScheduler(bus, func(s kernelboot.PluginSpec) error {
		launched[s.Name] = true
		return nil
	})
	got := sched.Submit(specs)
	for _, n := range got {
		launched[n] = true
	}

	// ① 声明的：依赖（config）已就绪 ⇒ **应当**立即拉起
	if !launched["declared"] {
		t.Error("声明了 config 且 config 已就绪的插件应当被拉起（这是本步的核心收益）")
	}
	// ② ★ 未声明的：内核**未**完成 ⇒ **不得**拉起
	if launched["undeclared"] {
		t.Fatal("★ 未声明依赖的插件在内核完全初始化**之前**被拉起了 ⇒ 违反 §2.6.5。\n" +
			"   实测后果：healthcheck 的 s.Status() 为 nil ⇒ 静默少注册一个工具" +
			"（86 vs 87，无任何报错）。")
	}
	// ③ 内核完成后 ⇒ 未声明的才应拉起
	bus.MarkKernelComplete()
	if !launched["undeclared"] {
		t.Fatal("★ 内核完成后未声明的插件仍未被拉起 ⇒ 它永远起不来")
	}
}

// ★★★ 判据：**未声明的插件必须在 skip 集合【外面】**（否则会被重复加载）。
//
// 调度器会拉起它，LoadSkipping 也会碰到它 ⇒ 两条路都加载 = 重复。
// （实测未复现重复，是因为时序上 LoadSkipping 晚于调度器且 registry 有门，
//
//	但**不应依赖巧合** —— 判据把语义钉住。）
func TestUndeclaredNotDoubleLoaded(t *testing.T) {
	root := t.TempDir()
	makePluginDir(t, root, "undeclared",
		`{"name":"undeclared","version":"1","entry":"plugin.bin"}`)
	cat, _ := scanCatalog(root, nil)
	g := classify(cat)
	skip := buildSkipSet(cat, g)

	// 未声明的插件在 autoStart 里（调度器会管）
	if len(g.autoStart) != 1 || g.autoStart[0] != "undeclared" {
		t.Fatalf("前提不成立: %v", g.autoStart)
	}
	// ★ 修法后：catalog 里登记的（含未声明的）**一律**不在 LoadSkipping 的
	// 管辖范围 ⇒ 必须在 skip 里。这样"调度器"与"LoadSkipping"两条路
	// **按构造不相交**，不可能双加载。
	//
	// （我第一版只 skip declared/manual/disabled ⇒ 未声明的外部插件会被
	//  两条路各加载一次。实测变异：healthcheck loaded 2 次、工具 87→161。）
	if !skip["undeclared"] {
		t.Fatal("★ catalog 里登记的插件都必须在 skip 里 —— " +
			"否则未声明的外部插件会被调度器与 LoadSkipping **各加载一次**" +
			"（实测：工具数翻倍）")
	}
	// ★ 反向钉住：LoadSkipping 仍必须能加载**内置插件**（不在 catalog 里）
	if skip["some_builtin_without_manifest"] {
		t.Fatal("★ 内置插件（无 plugin.json，不在 catalog）不得被 skip —— " +
			"它是 LoadSkipping 唯一能加载的对象")
	}
}
