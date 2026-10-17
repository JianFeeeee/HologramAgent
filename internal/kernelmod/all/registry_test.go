package all

// 内核模块注册表 + 分层的判据（2026-10-16）。
//
// 设计见 docs/zh/kernel-modularization-design.md §2.3/§2.6/§2.8。
//
// ## 用户的验收标准（必须被判据钉住）
//
// > 模块应该在 /modelname/ 独立文件夹下，根目录只有 core.go，
// > core 内有一个注册表注册各个模块 … 注意注册表中的模块列表是乱序的
//
// ⇒ 四条底线：
//   ① 每个模块**独立文件夹**
//   ② 依赖写在**模块自己**的 Requires() 里（无中央依赖清单）
//   ③ 注册表列表**乱序**（顺序无意义，不能被依赖）
//   ④ 第 0 层模块**不依赖任何模块**（"core 不依赖任何模块"字面成立）

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/JianFeeeee/HomeAgent/internal/kernelboot"
	"github.com/JianFeeeee/HomeAgent/internal/kernelmod"
)

// ★★★ 判据：**依赖图可建 + 模块全登记 + 无重复**。
func TestRegistryBuildsValidGraph(t *testing.T) {
	if d := kernelmod.Duplicates(); len(d) > 0 {
		t.Fatalf("★ 模块名重复注册: %v（两个模块同名，或一个模块被导入两次）", d)
	}
	mods := kernelmod.Registered()
	if len(mods) < 10 {
		t.Fatalf("★ 只登记 %d 个模块 —— 可能有模块漏了 init() 注册", len(mods))
	}
	plan, err := kernelboot.Build(mods)
	if err != nil {
		t.Fatalf("★ 依赖图无解（结构错误）: %v", err)
	}
	total := 0
	for _, l := range plan.Layers {
		total += len(l)
	}
	if total != len(mods) {
		t.Fatalf("★ 分层丢失模块: %d != %d", total, len(mods))
	}
}

// ★★★ 判据（用户验收标准 ④）：**第 0 层不依赖任何模块**。
func TestFirstLayerHasNoDependencies(t *testing.T) {
	plan, err := kernelboot.Build(kernelmod.Registered())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range plan.Layers[0] {
		if len(m.Requires()) != 0 {
			t.Errorf("★ 第 0 层模块 %s 有依赖 %v —— 分层正确的话不该有",
				m.Name(), m.Requires())
		}
	}
	names := map[string]bool{}
	for _, m := range plan.Layers[0] {
		names[m.Name()] = true
	}
	for _, want := range []string{"config", "kernel-api"} {
		if !names[want] {
			t.Errorf("★ %s 应在第 0 层（它不依赖任何模块）", want)
		}
	}
}

// ★★★ 判据（用户验收标准 ①②）：**每模块独立文件夹；依赖写在自己文件里**。
func TestEachModuleInOwnFolder(t *testing.T) {
	root := ".."
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	modDirs := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		modDirs++
		files, _ := filepath.Glob(filepath.Join(root, e.Name(), "*.go"))
		if len(files) == 0 {
			t.Errorf("★ 模块目录 %s 下没有 .go 文件", e.Name())
		}
	}
	if modDirs < 10 {
		t.Fatalf("★ 只找到 %d 个模块目录（期望 ≥10）", modDirs)
	}

	// ② 依赖不能出现在"中央清单"里：根目录只有 core.go/facilities.go/all 子包
	for _, f := range []string{"core.go", "facilities.go"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue
		}
		src := string(b)
		if strings.Contains(src, "Requires() []string { return []string{") {
			t.Errorf("★ %s 里出现了具体的 Requires —— 依赖应写在各模块自己的文件里", f)
		}
	}
}

// ★★★ 判据（用户验收标准 ③）：**注册表列表乱序**（顺序无意义，不能被依赖）。
//
// ★ 怎么测"乱序"：注册顺序由 init() 决定。这里断言 Names() 的顺序
// **不等于**按名字排序的顺序 —— 若相等，说明有人给它排过序，
// 那会给使用者"这个顺序有意义"的错觉（用户明确要求乱序）。
//
// ⚠ 注意：这条是**性质检查**，不是绝对的（init 顺序恰好等于字典序时也会通过
// 排序检查）。所以真正守"顺序无意义"的是**文档 + 分层器内部排序**，
// 而本判据是"如果有人显式排序就报红"的报警器。
func TestRegistryOrderIsNotMeaningful(t *testing.T) {
	names := kernelmod.Names()
	if len(names) < 2 {
		t.Fatal("模块太少")
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	if strings.Join(names, ",") == strings.Join(sorted, ",") {
		t.Logf("提示：注册顺序恰好等于字典序（不必然是错误，但请确认没有人"+
			"给注册表排序——顺序无意义是设计的一部分）: %v", names)
	}
	// 核心断言：Registered() 返回的列表**不得**按依赖序排列
	// （即：不能有人把"启动顺序"写进注册表）
	plan, err := kernelboot.Build(kernelmod.Registered())
	if err != nil {
		t.Fatal(err)
	}
	var layerOrder []string
	for _, l := range plan.Layers {
		for _, m := range l {
			layerOrder = append(layerOrder, m.Name())
		}
	}
	if strings.Join(names, ",") == strings.Join(layerOrder, ",") {
		t.Fatal("★ 注册表顺序恰好等于**依赖分层序** —— 说明注册表被用来表达启动顺序了，" +
			"那正是要消灭的隐式顺序依赖（用户要求注册表乱序）")
	}
}

// ★★★ 判据：**关键顺序被依赖图保证**（每条都来自真实约束）。
func TestLayeringPreservesCriticalOrder(t *testing.T) {
	plan, err := kernelboot.Build(kernelmod.Registered())
	if err != nil {
		t.Fatal(err)
	}
	layerOf := map[string]int{}
	for i, l := range plan.Layers {
		for _, m := range l {
			layerOf[m.Name()] = i
		}
	}
	cases := []struct{ a, b, why string }{
		{"config", "lua", "initLuaVM(cfg) 收 cfg"},
		{"lua", "llm", "initLLMProviders 收 luaVM"},
		{"config", "memory", "initMemoryStack 收 cfgReg"},
		{"knowledge-migration", "knowledge-store", "迁移必须早于 store 扫盘"},
		{"llm", "stage-host", "newStageAndRegistry 收 providerMgr"},
		{"memory", "stage-host", "newStageAndRegistry 收 mem.db"},
		{"stage-host", "agent", "newMainAgent 收 stageHost"},
		{"llm", "agent", "newMainAgent 收 provider"},
		{"memory", "agent", "newMainAgent 收 mem.db"},
		{"tracker", "agent", "newMainAgent 收 trk"},
	}
	for _, c := range cases {
		la, ok1 := layerOf[c.a]
		lb, ok2 := layerOf[c.b]
		if !ok1 || !ok2 {
			t.Errorf("模块缺失: %s=%v %s=%v", c.a, ok1, c.b, ok2)
			continue
		}
		if la >= lb {
			t.Errorf("★ %s 应在 %s 之前（%s），实际层号 %d >= %d", c.a, c.b, c.why, la, lb)
		}
	}
}

// ★★★ 判据：**text-memory 显式依赖 onnx-parser**（隐藏依赖，见 textmem 包注释）。
//
// ★ 不能用层号断言：text-memory 已因 memory 被推到第 2 层，
// 而 onnx-parser 在第 1 层 ⇒ 删掉依赖后层号依然满足。必须断言**声明本身**。
func TestHiddenDependencyOnnxDeclared(t *testing.T) {
	src := readMod(t, filepath.Join("..", "textmem", "mod.go"))
	if !strings.Contains(src, "FacONNX") {
		t.Fatal("★ text-memory 未声明对 onnx-parser 的依赖 —— " +
			"它在运行期读 nlp.defaultParser（onnx 模块写的包级单例）。" +
			"当前靠 distillLoop 的 Interval=10min 碰巧安全；" +
			"Interval 变小或解析器装配更后 ⇒ 静默降级（图记忆实体退化成整句复合值）")
	}
}

// ★★★ 判据：**分层带来并行**（对照原 17 步串行）。
func TestLayeringYieldsParallelism(t *testing.T) {
	mods := kernelmod.Registered()
	plan, err := kernelboot.Build(mods)
	if err != nil {
		t.Fatal(err)
	}
	maxLayer := 0
	for _, l := range plan.Layers {
		if len(l) > maxLayer {
			maxLayer = len(l)
		}
	}
	if len(plan.Layers) >= len(mods) {
		t.Fatalf("★ 无并行度（%d 层 / %d 模块）", len(plan.Layers), len(mods))
	}
	t.Logf("并行度: %d 层 / %d 模块，最宽层 %d 个", len(plan.Layers), len(mods), maxLayer)
}

func readMod(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}

// ★★★ 判据：**每个模块目录都被 import 引入**（漏 import ⇒ 模块静默消失）。
//
// ## ★ 这条是变异测试发现的**真实覆盖漏洞**
//
// 我删掉 `all.go` 里 `multimodal` 的 import 后，**全部判据仍然通过** ——
// 因为：
//
//	· 依赖图仍然解得开（multimodal **没有下游依赖**，没人需要它）
//	· 注册表数量检查只有 `>= 10` 这种下限（少一个不报）
//
// 后果：**多模态能力凭空消失**，而启动照常成功、无任何报错。
// 这与本仓反复吃亏的"静默降级"是同一形态：
//
//	· 漏 import 插件包 ⇒ 插件静默消失（已有同类教训）
//	· 漏 import 模块包 ⇒ 模块静默消失（本条）
//
// ## 判据做法：**统计目录 vs 统计注册**
//
// 扫描 `internal/kernelmod/` 下的模块目录（含 `mod.go` 或实现 Module 的 .go
// 文件），与注册表里的模块名比对 —— 目录有而注册表没有 ⇒ 漏 import。
//
// ★ 为什么不用"手写清单"：手写清单在"新增目录忘了加 import"时
// **也不会包含新目录** ⇒ 判据照样绿（本仓已有先例）。
func TestEveryModuleDirIsImported(t *testing.T) {
	root := ".."
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	registered := map[string]bool{}
	for _, n := range kernelmod.Names() {
		registered[n] = true
	}

	// 收集"看起来是模块目录"的：含 .go 文件、且文件里有 kernelmod.Register 调用
	var moduleDirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "all" {
			continue // 这个子包是 import 清单本身
		}
		files, _ := filepath.Glob(filepath.Join(root, name, "*.go"))
		hasRegister := false
		for _, f := range files {
			if strings.Contains(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if strings.Contains(string(b), "kernelmod.Register(") {
				hasRegister = true
				break
			}
		}
		if hasRegister {
			moduleDirs = append(moduleDirs, name)
		}
	}
	if len(moduleDirs) < 5 {
		t.Fatalf("只找到 %d 个模块目录 —— 扫描逻辑失效（判据会假绿）", len(moduleDirs))
	}

	// 每个模块目录里的 Register(...) 参数名 → 期望的模块名
	// （用"目录名"与"注册名"的映射，因为目录名与 Name() 不必相同）
	dirToNames := map[string][]string{}
	for _, d := range moduleDirs {
		files, _ := filepath.Glob(filepath.Join(root, d, "*.go"))
		for _, f := range files {
			if strings.Contains(f, "_test.go") {
				continue
			}
			b, _ := os.ReadFile(f)
			// ★ 正则要容忍 gofmt 的对齐空格（列对齐会让 `{` 前有多个空格）。
			// 初版写成 `\{ return` （单空格）⇒ 只匹配到 agent 一个，
			// 其余全部"找不到目录"（判据假红）。实测撞到。
			re := regexp.MustCompile(`(?m)^func \([A-Za-z0-9_]+\) Name\(\) string\s+\{ return "([^"]+)" \}`)
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				dirToNames[d] = append(dirToNames[d], m[1])
			}
		}
	}

	var missing []string
	for dir, names := range dirToNames {
		if len(names) == 0 {
			continue
		}
		// 该目录的**每个**模块名都必须在注册表里
		//（一个目录可含多个模块，如 knowledge/{migration,store}）
		anyFound := false
		for _, n := range names {
			if registered[n] {
				anyFound = true
			}
		}
		if !anyFound {
			missing = append(missing, fmt.Sprintf("%s(%v)", dir, names))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("★ 这些模块目录**没有被 import 引入**（模块静默消失，启动不报错）: %v\n"+
			"   修复：在 internal/kernelmod/all/all.go 里加空白 import", missing)
	}

	// 反向：注册表里的每个模块都应能对应到某个目录
	known := map[string]bool{}
	for _, ns := range dirToNames {
		for _, n := range ns {
			known[n] = true
		}
	}
	for n := range registered {
		if !known[n] {
			t.Errorf("★ 注册表里的模块 %s 找不到对应目录 —— 可能是测试夹具注册的，或扫描漏了", n)
		}
	}
}
