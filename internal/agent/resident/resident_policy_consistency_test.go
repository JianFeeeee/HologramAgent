package resident

// resident_policy_consistency_test.go —— **策略声明与驱动行为必须一致**。
//
// ## 事故（2026-11-25，真内核实测）
//
// 本类型的策略声明是「高优先级入队 + **不挂起**」（driver.go 的注释、
// marks.go 的标记解释）。但 `RunTaskSteps` 里曾有一段从主 agent 抄来的
// 让位判断：
//
//	RearmPending()
//	if !isCriticalChannel(...) && sched.PreemptGrantedFor() && sched.CanSuspend() {
//	    return outcomeSuspended
//	}
//
// 而 driver 的主循环**不处理** NextSuspended：
//
//	case sched.NextSuspended:
//	    Logf("unexpected suspended selection (this type has no suspend)")
//	    sched.Done(t)          // ← 任务被丢弃
//
// 于是只要有任何中断获得抢占授权，本帧就让位、驱动把它当"不该发生"丢掉：
// **回复永远发不出去**。现场表现是子 agent 建好、`state=running`、
// `tool call loop start, 1 events` 之后一片安静 —— 而它的 LLM 调用其实成功了
// （诊断确认 resp 已拿到、StagePostAction 也跑完了）。
//
// ## 判据为什么是"扫让位分支"而不是"跑一次抢占"
//
// 端到端复现一次抢占需要：造两条输入、掐准时序、让一条拿到抢占授权 ——
// 这种判据在 CI 上必然不稳（时序敏感），而它要防的东西是**结构性的**：
// "本类型有没有一条会返回 outcomeSuspended 的路径"。
//
// 本类型不该有任何一条。这条约束是可机械检验的：`RunTaskSteps` 与
// `RunInputTask` 的函数体里不许出现 `outcomeSuspended` 的**返回**。
//
// ★ 这不是读源码"复述实现"：它断言的是**策略与实现的矛盾不存在**，
//   而那个矛盾此前真实存在并且丢掉了用户的消息。观测对象只有一个词
//   在函数体里是否出现，语义单一、改名会编译错（常量名固定）。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestResidentNeverSuspends：驻留子的任务推进路径**不许返回** outcomeSuspended。
//
// 一条都不许：本类型没有"让位后由谁恢复"的机制（driver 遇到 suspended
// 直接丢弃），所以任何 Suspended 返回都是一条**丢任务**的路径。
func TestResidentNeverSuspends(t *testing.T) {
	files := []string{"task.go", "driver.go"}
	for _, name := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s: %v", name, err)
		}
		// 只看任务推进的两个函数：其它地方（比如调度器内部）可以用这个值。
		for _, fn := range []string{"RunTaskSteps", "RunInputTask"} {
			body := funcBody(file, fn)
			if body == nil {
				continue // 不在这个文件里
			}
			if hits := suspendedReturns(body, fset); len(hits) > 0 {
				t.Fatalf("★ %s 的 %s 里出现了返回 outcomeSuspended：\n%s\n\n"+
					"本类型声明「不挂起」（driver.go:10），而 driver 遇到 NextSuspended 会直接\n"+
					"`sched.Done(t)` 丢弃任务 —— 这条路径会让用户的回复永远发不出去。\n"+
					"驻留子的正确语义：新输入入队，等当前任务自然结束再处理。",
					name, fn, strings.Join(hits, "\n"))
			}
		}
	}
}

// funcBody 找指定名字的函数声明（含方法）并返回其 Body。
func funcBody(file *ast.File, name string) *ast.BlockStmt {
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != name {
			continue
		}
		return fn.Body
	}
	return nil
}

// suspendedReturns 收集函数体里所有 return 语句中引用 outcomeSuspended 的位置。
func suspendedReturns(body *ast.BlockStmt, fset *token.FileSet) []string {
	var hits []string
	ast.Inspect(body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			return true
		}
		for _, r := range ret.Results {
			ident, ok := r.(*ast.Ident)
			if !ok || ident.Name != "outcomeSuspended" {
				continue
			}
			p := fset.Position(ident.Pos())
			hits = append(hits, p.String())
		}
		return true
	})
	return hits
}
