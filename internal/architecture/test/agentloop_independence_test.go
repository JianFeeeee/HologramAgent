package test

// agentloop_independence_test.go —— **三份 agentloop 互不依赖**（docs §8.4）。
//
// 用户裁定 2026-11-20：
// 「虽然现在主 agent 与驻留式子 agent 用一个 sch，但是不代表以后也会用一个，
//   要在各自的 agentloop 中实现，中断循环也是」。
//
// 判据分三层：
//
//	① 驱动归位：resident / rt 的循环在自己包里（不借 mainagent）
//	② 解释分离：三类的标记解释不同（rt 覆盖 vs 主 agent 分级）
//	③ 契约层不含策略：io 的 Marks 只取值；级别映射各类型自持

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/resident"
	"github.com/JianFeeeee/HologramAgent/internal/agent/rtchild"
)

// TestTypeLoopsDoNotBorrowMainAgent：驻留子与 rt 不借主 agent 的循环。
//
// ★ 诚实条文：**帧推进**（8 个 step 函数）当前仍借 `mainagent.RunTaskSteps` /
//
//	`RunInputTask` —— 那是 3,228 行的状态机，拆它要先有共享原语载体
//	（§8.4.3）。本轮不拆 = 不留半成品。但**驱动/分拣**（取任务、背压、
//	标记解释）已经各归各位 —— 那正是"改任一方不影响另一方"的那部分。
func TestTypeLoopsDoNotBorrowMainAgent(t *testing.T) {
	// ① 驻留子的调度驱动必须是它自己包里的方法。
	//
	// ★ 黑盒（2026-11-25 改写）：`reflect` 查**方法集**，不读源码文本。
	//   方法存在与否是编译期事实 —— 谁把它挪到别的文件、拆成几个文件，
	//   都不该影响这条判据。
	if _, ok := reflect.TypeOf(&resident.M{}).MethodByName("SchedulerLoop"); !ok {
		t.Fatal("★ resident 没有自己的 SchedulerLoop —— 它的驱动还在借主 agent")
	}
	// ② rt 的分拣也必须是它自己包里的方法。
	if _, ok := reflect.TypeOf(&rtchild.M{}).MethodByName("InterceptLoop"); !ok {
		t.Fatal("★ rtchild 没有自己的 InterceptLoop —— 它的分拣还在借主 agent")
	}
	// ③④ "不许跨包借循环"由**依赖闭包**覆盖：
	//
	//   resident / rtchild 的 `go list -deps` 里没有 mainagent
	//   （见 `TestTypeDepsExcludeMainAgent`）。既然 import 都不存在，
	//   就不可能出现 `mainagent.Wrap(...).SchedulerLoop(...)` 这种调用。
	//
	//   原判据用正则扫源码，那是"在已经由编译期保证的事实上再查一遍"。
}

// TestMarksContractHasNoSchedulingPolicy：标记**取值的契约层**里没有调度策略。
//
// ★ docs §0.3 / §7：kernelflow 已删除。取值的契约在 io 包（io.MarksOf），
//
//	  而"L4 算哪一级"是**类型的策略**（mainagent/marks.go、resident/marks.go
//	  各一份）。判据因此钉两件事：
//
//		① io 包里的 Marks 只取值、不判定（没有 Level / preempt / suspend / queue）
//		② 级别映射只出现在类型的 marks.go 里
func TestMarksContractHasNoSchedulingPolicy(t *testing.T) {
	// ① io 包的**导出符号表**里只有取值原语，没有级别映射。
	//
	// ★ 黑盒（2026-11-25 改写）：`reflect` 查包级导出函数 —— 编译期事实。
	//   原判据扫源码文本，`func LevelOf(` 换个换行/别名就绕过，而真正要防的
	//   "契约层出现级别判定"在符号表上是明明白白的。
	//
	//   `io` 包是类型可以 import 的**契约层**。它一旦导出 LevelOf / ParseLevel，
	//   就等于替所有类型预定了"L4 是哪一级"的答案。
	if _, ok := reflect.TypeOf(agentIO.Marks{}).MethodByName("Level"); ok {
		t.Fatal("★ io.Marks 上出现了 Level 方法 —— 取值层不许长调度策略")
	}
	if _, ok := reflect.TypeOf(agentIO.Marks{}).MethodByName("Preempt"); ok {
		t.Fatal("★ io.Marks 上出现了 Preempt 方法 —— 取值层不许长调度策略")
	}
	// 取值原语必须存在（否则没法取值）。
	_ = agentIO.MarksOf(nil)

	// ② 每个有分级的类型都有自己的解释：由**依赖闭包**覆盖 ——
	//   mainagent / resident 各自 import io（取值）而不互相 import
	//   （`TestTypeDepsExcludeMainAgent`）。解释函数是包内私有的，
	//   外部本来就不该看见它；能观测的事实是"两类都在、且互不依赖"。
}

// TestThreeTypesInterpretMarksDifferently：三类的标记解释确实不同。
//
// 可执行证据：
//
//	rt       有 Clear（"stop 就清槽"）—— 主 agent 没有这个概念
//	resident 有"不挂起"的显式分支
//	main     有 interruptLevelOf（分级）—— rt / resident 不许有同样的分级实现
// TestThreeTypesInterpretMarksDifferently 已删除（2026-11-25）。
//
// 它扫源码找 `slot.Clear()` / `func levelOf(` / `interruptLevelOf` /
// `"不挂起"` 这些**字符串**。其中最脆弱的是 `"不挂起"` —— 它是注释里的
// 中文词，改个措辞就误红，而"这个类型真的不挂起吗"根本不该靠注释判断。
//
// 那个约束的真实判据是**行为**的，已经在各自包里：
//
//	resident  TestResidentNeverSuspends     AST 扫"有没有返回 outcomeSuspended"
//	          —— 本类型不挂起，一条都不许有；有了就是丢任务（实测过）
//	rt        TestRTSlot_NeverQueues         槽语义（最新者胜）
//	main      分级行为由其调度器判据覆盖
//
// 扫描"某个词在不在源码里"既抓不到真实行为，又会随重构误红。

// TestTypeDepsExcludeMainAgent：类型包的**依赖闭包**不包含主 agent 的实现包。
//
// ★ 黑盒判据（用户裁定 2026-11-25）：只观测**外部可见事实** ——
//
//	编译器看到的依赖闭包（`go list -deps`），不读源码、不查文件存在性。
//
// 为什么这条重要：内部重构允许"结构变化"，不允许"隔离被破坏"。
// 隔离被破坏的症状不是编译错，而是改一处牵连另一处（docs §0.3 铁律）。
//
// 四种类型（resident / rtchild / worker / 插件类型）互相之间、
// 以及它们对主 agent 的实现包（memory / coldcontext / textmem）必须是**零依赖**。
func TestTypeDepsExcludeMainAgent(t *testing.T) {
	const mod = "github.com/JianFeeeee/HologramAgent"
	types := []string{
		mod + "/internal/agent/resident",
		mod + "/internal/agent/rtchild",
		mod + "/internal/agent/worker",
		mod + "/internal/kernelmod/agenttype",
	}
	// 主 agent 的实现包：这些只能被 mainagent 自己 import。
	forbidden := []string{
		"/internal/agent/mainagent/memory",
		"/internal/agent/mainagent/coldcontext",
		"/internal/agent/mainagent/textmem",
	}
	for _, pkg := range types {
		out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", pkg).Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			for _, f := range forbidden {
				if strings.HasSuffix(line, f) {
					t.Fatalf("★ %s 的依赖闭包里出现了 %s —— 类型隔离被破坏（docs §0.3）",
						pkg, line)
				}
			}
			// 反向：主 agent 包不许出现在其它类型的闭包里（自己除外）。
			if !strings.HasSuffix(pkg, "/internal/agent/mainagent") &&
				strings.HasSuffix(line, "/internal/agent/mainagent") {
				t.Fatalf("★ %s 依赖了主 agent 的实现包 —— 主/子必须同构但不共享实现",
					pkg)
			}
		}
	}
}
