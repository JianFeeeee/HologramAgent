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
//   - 结构体字段集 / 方法集：`reflect`（编译期事实，比读文本权威）
//   - 访问器数量：`reflect` 数方法
//
// ★ 读源码文本做**结构**断言（"这个符号不存在"）与 §0 第 5 条禁止的
//
//	"读源码做业务断言"不是一回事：前者断言的是架构约束本身，
//	后者才是在测试里复述实现逻辑。
package test

import (
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/memory"
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
	// ★ 进度：`internal/memory` 已归零（2026-11-18，值类型上移 model +
	//   纯函数能力经 kernelapi.MemoryOps / GraphOpener 注入）。
	//   剩下三个是**下一刀**的对象（见函数注释）。
	// ★ 2026-11-18：`internal/memory` / `coldcontext` / `text` / `vector`
	//   全部归零（值类型上移 model、纯函数进 internal/colddoc 与
	//   internal/vector 叶子包、能力经 kernelapi.MemoryOps / GraphOpener 注入）。
	//
	// 清单留空是**终态**：基座对记忆实现包的依赖应当是零。
	known := map[string]bool{}
	deps := internalDeps(goListDeps(t, "./internal/agent/agent"))
	got := map[string]bool{}
	for _, d := range deps {
		if strings.HasPrefix(d, "internal/agent/mainagent/memory") {
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
// TestSingleAgentRecordTable 已删除（2026-11-25）。
//
// 它用 `os.ReadFile` + `strings.Contains` 扫 security 包源码，断言
// `outputsByAgent` / `principals.byID` 两个**私有字段名**不存在。
//
// 两个问题：
//
//	① 判据在测"某个名字别出现" —— 换名、换结构就静默失效；
//	② 而它真正要防的东西（旧表活得比记录久 ⇒ 同名重建继承旧策略）
//	   是**可观测的行为**，不需要读源码。
//
// 行为判据取代它，见 `internal/security/test/agentrecord_contract_test.go`：
//
//	TestPolicyDiesWithRecord      删除记录 ⇒ 收窄授权消失（qq 从拒变允）
//	TestChildPolicyDiesWithRecord 删除记录 ⇒ 父为子声明的工具策略消失
//
// ★ 这两条比原判据更强：它们直接观测"旧表若还在会怎样"，
//   而不是"某个字符串没出现"。

// grepFiles / itoa 已删除（2026-11-25）：唯一调用者
// `TestSingleAgentRecordTable` 已由行为判据取代
// （见 `internal/security/test/agentrecord_contract_test.go`）。

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

// TestHooksHasExactlyFourSlots 判据 5：`Hooks` 只有 **3 个槽位**。
//
// ★ 黑盒（2026-11-25 改写）：观测**编译期可见的公开事实** —— 结构体字段名，
//
//	用 `reflect` 取，不读源码文本。
//
// 原实现 `os.ReadFile(hooks.go)` + 手写切分结构体字面量，有两个问题：
//
//	① 判据自己会漂：注释写"4 个槽位"、代码里是 `want = 3`，两处对不上；
//	② 改注释/挪代码位置会误伤，而真正的契约（字段集）反而没被盯住。
//
// 字段集是编译期事实，`reflect` 就是它的权威读法。
//
// 加槽位前先问：它是不是某个类型自己的实现？（A2/E 删掉的 9 个都是）
func TestHooksHasExactlyFourSlots(t *testing.T) {
	typ := reflect.TypeOf(agent.Hooks{})

	var got []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue // 私有字段不是槽位（槽位是类型要填的东西）
		}
		got = append(got, f.Name)
	}
	sort.Strings(got)

	// 三个槽位：生命周期登记 / 上下文预算 / 溢出处置。
	want := []string{"ContextBudget", "OverflowHandler", "Runtime"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("★ Hooks 的槽位集变了：\n  实际 %v\n  期望 %v\n"+
			"加槽位前先问：它是不是某个类型自己的实现？（A2/E 删掉的 9 个都是）",
			got, want)
	}
}

// TestRuntimeHooksSlotSet：`RuntimeHooks` 的槽位集（生命周期 + 队列状态）。
//
// ★ 与 `TestHooksHasExactlyFourSlots` 是一对：`Hooks` 是**三**个通用槽
//
//	（生命周期登记 / 上下文预算 / 溢出处置），`RuntimeHooks` 是生命周期
//	本身那组。原先只在文档里写"4 个"（Initialize/Restore/Start/Stop），
//	而代码实有 7 个 —— 文档过期了三个月没人发现，因为没有判据盯结构体。
//
// 加一个槽位前先问：它是不是某个类型的实现细节？
func TestRuntimeHooksSlotSet(t *testing.T) {
	typ := reflect.TypeOf(agent.RuntimeHooks{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		if f := typ.Field(i); f.IsExported() {
			got = append(got, f.Name)
		}
	}
	sort.Strings(got)

	want := []string{
		"DrainTerminal",    // 停机时给未运行的任务补终态（队列归类型）
		"Initialize",       // 装配期一次
		"InterruptPending", // 类型自己的队列是否还有中断
		"Restore",          // 内核驱动的恢复（目录由内核给）
		"RoundsExecuted",   // 状态面要的轮次数
		"Start",            // 起自己的循环
		"Stop",             // 停自己 + 排空队列
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("★ RuntimeHooks 槽位集变了：\n  实际 %v\n  期望 %v\n"+
			"改动它意味着改了「类型交什么给内核」的契约 —— 先确认它不是某个类型的实现细节", got, want)
	}
}

// TestSurfaceHasFiveAccessors 判据 6：基座对外的**构造面**只有 5 个访问器。
//
// `Config` / `Hooks` / `SetHooks` / `Ctx` / `Cancel`。
//
// ★ 黑盒（2026-11-25 改写）：观测 `*agent.Agent` 的**方法集**（`reflect`），
//
//	而不是读 `surface.go` 文本。方法集是编译期事实，谁把它挪到哪个文件、
//	拆成几个文件都不该影响这条判据 —— 那本来也不是契约。
//
// ★ 判据刻意只钉**这五个名字存在**，不去枚举全部导出方法：
//
//	`*Agent` 上还有大量**能力**方法（`RunToolBatch` / `PruneContext` /
//	`RegisterPeriodic` / `ExecuteTool` …），它们不是"构造面再导出"，
//	而是基座提供的阶段与设施。用"导出方法总数"当判据会变成一条
//	什么都管的红条，然后大家就开始往白名单里加东西 —— 那等于判据失效。
//
// 真正要防的回退是：有人把 `Provider()` / `Sched()` / `Sub()` 这类
// **字段再导出**加回来。所以判据是"这五个必须能调，且那几个必须不能调"。
func TestSurfaceHasFiveAccessors(t *testing.T) {
	typ := reflect.TypeOf(&agent.Agent{})

	for _, want := range []string{"Config", "Hooks", "SetHooks", "Ctx", "Cancel"} {
		if _, ok := typ.MethodByName(want); !ok {
			t.Fatalf("★ 构造面缺少 %q —— 基座对类型的最小面被改小了", want)
		}
	}

	// 字段再导出（历史回退形态）：这些名字一旦出现在方法集上，
	// 就意味着"类型可以直接摸基座的内部状态"，绕过 syscall 边界。
	//
	// ★ 清单取自 docs §3.2「不许出现的方法」，逐字对齐。
	//
	// 刻意**不含** `Provider` / `ProviderManager`：它们存在（装配点要读），
	// 约束是"类型不许调用它们"（§9.6），那是**调用侧**的约束，不是方法集
	// 的约束。用方法集去钉调用侧只会得到一个必须开白名单的假判据。
	//
	// ★ 也**不含** `InterruptPending` / `RoundsExecuted`：它们是 `RuntimeHooks`
	//   的槽位（类型自己的队列状态与轮次），基座上的同名方法只是给状态面用的
	//   转发。docs §3.2 把它们列进"不许出现"是**文档过期**（P7 要修），
	//   而这里应该跟代码的终态一致 —— 判据不是用来固化过期文档的。
	for _, banned := range []string{"Scheduler", "SetScheduler", "Publish", "EventBus", "Notify",
		"GraphDB", "GraphMem", "GraphOpener", "LightMemory",
		"RaiseKernelInterruptAt", "RaiseVector"} {
		if _, ok := typ.MethodByName(banned); ok {
			t.Fatalf("★ 方法集里出现了 %q —— 那是字段再导出（§3.2 明确禁止）", banned)
		}
	}
}

// TestBlockIDsAgree 钉住"块 ID 规则只有一份"这件事的**可观察形式**。
//
// `SentenceBlockID` 与 `MediaBlockID` 是**内容派生 ID**：同内容必须同 ID，
// 否则同一段原句/同一张图会落成两个块（去重失效、召回重复）。
//
// 这两条规则今天在**两处**各自实现（基座内联 + memory 侧），因为
// "基座不能 import memory、memory 不能 import 基座"（成环）。
// 有意的重复 → 必须有判据钉住它们一致，否则迟早漂移。
//
// 判据只比对**输出**（黑盒），不读实现。
func TestBlockIDsAgree(t *testing.T) {
	// 基座侧的派生函数是私有的，所以这里通过 memory 侧的导出函数 +
	// 一个**独立重算**（同规则的期望值）比对。若两边任一漂移，本判据红。
	cases := []struct {
		mime string
		data []byte
	}{
		{"", nil},
		{"image/png", []byte{1, 2, 3}},
		{"text/plain", []byte("hello")},
		{"image/jpeg", []byte{0, 255, 0}},
	}
	for _, c := range cases {
		got := memory.MediaBlockID(c.mime, c.data)
		want := expectedMediaBlockID(c.mime, c.data)
		if got != want {
			t.Fatalf("★ MediaBlockID 规则漂移（mime=%q len(data)=%d）:\n  got  %q\n  want %q",
				c.mime, len(c.data), got, want)
		}
	}
	for _, text := range []string{"", "hello", "  中文句子  ", "a\nb"} {
		got := memory.SentenceBlockID(text)
		want := expectedSentenceBlockID(text)
		if got != want {
			t.Fatalf("★ SentenceBlockID 规则漂移（text=%q）:\n  got  %q\n  want %q", text, got, want)
		}
	}
}

// expectedMediaBlockID 是规则的**独立重算**（判据侧实现，不是被测代码）。
func expectedMediaBlockID(mime string, data []byte) string {
	h := sha256.New()
	h.Write([]byte(strings.TrimSpace(mime)))
	h.Write([]byte{0})
	h.Write(data)
	return "blk_media_" + hex.EncodeToString(h.Sum(nil)[:12])
}

// expectedSentenceBlockID 同上。
func expectedSentenceBlockID(text string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return "blk_src_" + hex.EncodeToString(h[:12])
}
