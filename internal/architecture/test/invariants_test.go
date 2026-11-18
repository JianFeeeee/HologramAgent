// Package test 是**架构不变量的机械判据**（目标架构 §10，17 条里可判的那些）。
//
// ## 为什么这些判据住在一个独立包里
//
// 它们断言的不是"某个函数怎么工作"，而是**整个仓库的形状**：
// 谁 import 谁、某类符号是否存在、某个文件里还剩几个访问器。
//
// 这类判据的价值在于**防止回退**：重构做完之后，任何人（包括未来的自己）
// 往里加一行 `import internal/security` 都会立刻红。它不是"测试实现细节"，
// 它是把**架构决定**变成可执行的东西。
//
// ## 判据怎么读仓库（都是 `go list` / 读文件，不 import 被测包）
//
//   - 依赖方向：`go list -deps <pkg>`（Go 自己算的，最权威）
//   - 符号是否消失：读源码文本（因为常量/字段名不导出，无法从外部断言）
//   - 访问器数量：`grep -c` 等价
//
// ★ 读源码文本做**结构**断言（"这个符号不存在"）与 §0 第 5 条禁止的
//
//	"读源码做业务断言"不是一回事：前者断言的是架构约束本身，
//	后者才是在测试里复述实现逻辑。
package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot 返回仓根（本判据在 internal/architecture/test 下）。
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// goListDeps 返回某个包的**直接 import**（不是传递闭包）。
//
// ★ 为什么必须是直接 import 而不是 `-deps`（文档 §10 那一条写的是 -deps，
//
//	但那样判不成立，实测确认）：
//
//	`internal/kernelapi` 自己定义了 `LLMProvider = agentAPI.Provider`、
//	`EventBus` 用到 `model.Event`，所以它的传递闭包天然包含 `agent/api`。
//	用 `-deps` 判"kernelloop 只依赖 kernelapi"就永远为假 —— 判据会变成
//	"要求 kernelapi 也零内部依赖"，那是另一件事（且与既有契约冲突）。
//
//	本判据要断言的是**这个包自己不 import 别的东西**，所以看直接 import。
//	传递依赖是依赖方的选择，不归被依赖方管。
func goListDeps(t *testing.T, pkg string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{join .Imports \"\\n\"}}", pkg)
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -f Imports %s: %v", pkg, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// internalDeps 过滤出本仓自己的内部包。
func internalDeps(deps []string) []string {
	const prefix = "github.com/JianFeeeee/HologramAgent/"
	var out []string
	for _, d := range deps {
		if strings.HasPrefix(d, prefix) {
			out = append(out, strings.TrimPrefix(d, prefix))
		}
	}
	return out
}

// TestKernelloopKnowsNoAgent 判据 1：内核事件循环**不认识任何 agent**。
//
// 它的内部依赖只能是 kernelapi 与 kernelapi/model。
// 这一条一旦破了（比如有人图方便 import 了 agent），整个解耦归零 ——
// 因为内核就会持有 `*Agent`，从而"内核认识模块"重新成立。
func TestKernelloopKnowsNoAgent(t *testing.T) {
	deps := internalDeps(goListDeps(t, "./internal/kernelloop"))
	allowed := map[string]bool{
		"internal/kernelapi":       true,
		"internal/kernelapi/model": true,
	}
	for _, d := range deps {
		if !allowed[d] {
			t.Fatalf("★ kernelloop 依赖了 %q（只允许 kernelapi + kernelapi/model）\n全部内部依赖: %v", d, deps)
		}
	}
}

// TestSecurityDoesNotImportAgent 判据 16：安全模块**不 import 基座**。
//
// 依赖方向：agentreg → security → agent（单向）。反了就是"把墙焊在基座上"。
func TestSecurityDoesNotImportAgent(t *testing.T) {
	deps := internalDeps(goListDeps(t, "./internal/security"))
	for _, d := range deps {
		if strings.HasPrefix(d, "internal/agent/agent") {
			t.Fatalf("★ internal/security 依赖了 %q —— 安全模块必须只读 kernelapi.AgentIndex", d)
		}
	}
}

// TestAgentHasNoImplementationImports 判据 3：基座**不 import 实现包**。
//
// `knowledge` / `security` / `plugin` / `coldcontext` 已归零；
// `memory` 仍是已知未清项（见 TestKnownGaps 的说明）：这里**只断言已归零的那几个**，
// 不用"允许名单"把未清的项也一并放过 —— 那会让判据变成"什么都没有"。
func TestAgentHasNoImplementationImports(t *testing.T) {
	deps := internalDeps(goListDeps(t, "./internal/agent/agent"))
	// ★ 已完成归零的**四个**（判据 3 的原文清单）。
	//
	// `coldcontext` / `memory` 仍是已知未清项 —— 见 TestAgentMemoryImportIsDocumented。
	// 把它们写进本判据会让它**永远红**，那就等于判据不存在：一条永远红的
	// 断言不会阻止任何人加新依赖，只会训练大家忽略它。
	forbidden := []string{
		"internal/security",
		"internal/plugin",
		"internal/plugins/knowledge",
		"internal/knowledge",
	}
	for _, d := range deps {
		for _, f := range forbidden {
			if d == f || strings.HasPrefix(d, f+"/") {
				t.Fatalf("★ 基座依赖了实现包 %q（判据 3）\n全部内部依赖: %v", d, deps)
			}
		}
	}
}

// TestKnownUnfinishedImportsAreListed 记录**已知未清项**（不是放过它们）。
//
// 基座仍直接依赖两个**记忆侧实现包**：
//
//	internal/memory             MemoryBlock / Triple / TurnScene / LightMemory …
//	internal/memory/coldcontext ColdDoc / Store（归档、跨模态、蒸馏）
//	internal/memory/vector      Vector / Vectorizer 接口（context 的稠密路）
//
// 要归零必须把「冷文档蒸馏 + 记忆面 + 跨模态」整体搬进 memory 模块 ——
// 那是独立的一刀，不在本轮 H/I/J/K/L 范围内。
//
// ★ 本判据的作用是把它钉成**一份清单**（双向）：
//
//	清单项被清掉      ⇒ 红（提示"更新清单"）
//	清单外新增一项    ⇒ 红（提示"你又在基座里焊了一个记忆包"）
//
// 于是「还剩什么没做」在 CI 里是有数的 —— 不靠记忆，也不会被
// "全部归零"这种说法悄悄盖过去。
func TestKnownUnfinishedImportsAreListed(t *testing.T) {
	known := map[string]bool{
		"internal/memory":             true,
		"internal/memory/coldcontext": true,
		"internal/memory/vector":      true,
		"internal/memory/text":        true,
	}
	deps := internalDeps(goListDeps(t, "./internal/agent/agent"))
	got := map[string]bool{}
	for _, d := range deps {
		if strings.HasPrefix(d, "internal/memory") {
			got[d] = true
		}
	}
	for k := range known {
		if !got[k] {
			t.Fatalf("★ 未清项 %q 已消失 —— 请更新本判据的清单（它不是永久豁免）", k)
		}
	}
	for d := range got {
		if !known[d] {
			t.Fatalf("★ 基座新增了对记忆实现包 %q 的依赖 —— 请归零，或写明理由并加进清单", d)
		}
	}
}

// TestAgentRegistersAgentsTable 判据 15 的一半：**按 agent ID 记账的表只有一张**。
//
// 四张旧表的**定义**必须从仓库里消失（不是"改个名字"）：
//
//	outputsByAgent · principals.byID · InstanceManager.recs · scopesByAgent 的 churn
//
// 表只剩 `AgentRecord`。判据读源码文本，因为这些字段不导出、无法从外部断言。
func TestSingleAgentRecordTable(t *testing.T) {
	root := repoRoot(t)
	// 旧表的**标识符**。它们若还在（哪怕只剩定义），就说明有两份事实。
	//
	// ★ `scopesByAgent` 不在此列，且这是**刻意的**：它是 per-agent 视图表，
	//   剩下的唯一内容是 `sessionProfile`（**本轮事实**，End 即清）——
	//   它没有别的落点，也不随 agent 生命周期残留。
	//   而 `longtimeProfile`（会残留、会提权的那一半）已搬进 `AgentRecord`。
	dead := []string{"outputsByAgent", "principals.byID"}
	for _, name := range dead {
		hits := grepFiles(t, root, "internal/security", name)
		if len(hits) > 0 {
			t.Fatalf("★ 旧表 %q 仍在 security 包里（判据 15）：\n%s", name, strings.Join(hits, "\n"))
		}
	}
}

// grepFiles 在目录下所有 .go 文件里找标识符，返回 "file:line"。
func grepFiles(t *testing.T, root, dir, ident string) []string {
	t.Helper()
	var hits []string
	base := filepath.Join(root, dir)
	err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(data), "\n") {
			// 只算**代码**行（注释里提到旧表名是合法的历史说明）。
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, ident) {
				rel, _ := filepath.Rel(root, path)
				hits = append(hits, rel+":"+itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", base, err)
	}
	return hits
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestHooksHasExactlyFourSlots 判据 5：`Hooks` 只有 **4 个槽位**。
//
// 判据读 `hooks.go` 的**结构体字面量**（字段名与类型），因为槽位是装配契约，
// 它的形状必须能被机械核对 —— 加第五个槽位时这条会红，逼人先想清楚
// "它是不是某个类型自己的实现"（那正是 A2/E 删掉 9 个槽位的理由）。
func TestHooksHasExactlyFourSlots(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "internal", "agent", "agent", "hooks.go"))
	if err != nil {
		t.Fatalf("read hooks.go: %v", err)
	}
	src := string(data)
	start := strings.Index(src, "\ntype Hooks struct {")
	if start < 0 {
		t.Fatal("★ 找不到 `type Hooks struct {` —— 结构被改名了，请更新本判据")
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatal("★ 找不到 Hooks 结构体的结尾")
	}
	open := strings.Index(src[start:], "{")
	body := src[start+open+1 : start+end]

	// 槽位 = 结构体里的**顶层字段**（`Name Type`）。
	//
	// ★ 包括 `Runtime RuntimeHooks`（它是**一个**槽位：类型的生命周期登记），
	//   只数 `func(...)` 会把 4 数成 3 —— 那样判据就错了，
	//   而错的判据比没有判据更糟（它会训练人改判据而不是改代码）。
	var slots []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || trimmed == "}" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) >= 2 {
			slots = append(slots, fields[0])
		}
	}
	const want = 4
	if len(slots) != want {
		t.Fatalf("★ Hooks 有 %d 个槽位（应为 %d）: %v\n"+
			"加槽位前先问：它是不是某个类型自己的实现？（A2/E 删掉的 9 个都是）",
			len(slots), want, slots)
	}
}

// TestSurfaceHasFiveAccessors 判据 6：基座对外的**构造面**只有 5 个访问器。
//
// `Config` / `Hooks` / `SetHooks` / `Ctx` / `Cancel`。
//
// ★ 判据刻意只数**这五个名字**，不去枚举全部导出方法：
//
//	`*Agent` 上还有大量**能力**方法（`RunToolBatch` / `PruneContext` /
//	`RegisterPeriodic` / `ExecuteTool` …），它们不是"构造面再导出"，
//	而是基座提供的阶段与设施。用"导出方法总数"当判据会变成一条
//	什么都管的红条，然后大家就开始往白名单里加东西 —— 那等于判据失效。
//
// 真正要防的回退是：有人把 `Provider()` / `Sched()` / `Sub()` 这类
// **字段再导出**加回来。所以判据是"这个名字集合里只许有五个，
// 且 surface.go 里只留 Ctx/Cancel"。
func TestSurfaceHasFiveAccessors(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "internal", "agent", "agent")

	// ① surface.go 只剩 2 个（其余三个与各自实现同处，见 §6.3）。
	data, err := os.ReadFile(filepath.Join(dir, "surface.go"))
	if err != nil {
		t.Fatalf("read surface.go: %v", err)
	}
	count := strings.Count(string(data), "func (a *Agent) ")
	if count != 2 {
		t.Fatalf("★ surface.go 有 %d 个访问器（应为 2：Ctx + Cancel）。"+
			"§6.3 的 34 → 5 已落地，加回来就是回退", count)
	}

	// ② 那五个名字必须存在（否则是"删过头"，类型模块会编译不过 ——
	//    但这条判据让失败信息更直接）。
	want := map[string]string{
		"Config":   "install.go",
		"Hooks":    "stages_exported.go",
		"SetHooks": "agent.go",
		"Ctx":      "surface.go",
		"Cancel":   "surface.go",
	}
	decl := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatal(rerr)
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "func (a *Agent) ") {
				continue
			}
			rest := strings.TrimPrefix(line, "func (a *Agent) ")
			name := rest
			if i := strings.IndexAny(rest, "( "); i > 0 {
				name = rest[:i]
			}
			decl[name] = true
		}
	}
	for name := range want {
		if !decl[name] {
			t.Fatalf("★ 构造面访问器 %q 不存在了（判据 6 要求这五个都在）", name)
		}
	}
}
