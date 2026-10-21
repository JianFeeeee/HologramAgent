package safeclose_test

// **close 纪律**判据（2026-10-17）。
//
// ## 纪律
//
// 全仓**除本包之外**，不得出现裸 `close(ch)`（chan 类型）。
// 一律走 `safeclose.Safe(&once, ch)` 或 `safeclose.SafeFunc(ch)`。
//
// ## 为什么这条必须存在（用户要求 + 实测事故）
//
// 实测崩溃（真机 SIGTERM）：
//
//	panic: close of closed channel
//	main.startHeartbeat.func3()  bootstrap.go:259
//
// 根因是有两个调用点。用户明确指出要求：
//
//	「只要调 close 就需要前置检查，这是为了避免未来更新的意外修改」
//
// 这个要求的关键在**"未来"**：今天只在退出时调一次的代码，明天加一条
// 错误/超时/重连路径就变成两次 —— 而那次改动看起来与 close 无关，
// 代码评审不会注意。所以不能靠"逐处判断当前是否安全"，必须**统一入口**。
//
// ## 为什么是 AST 判据而不是字符串搜索
//
// 字符串 `close(` 会命中注释、字符串字面量、以及 `f.Close()` /
// `conn.Close()` 这类完全不同的调用。
// 本仓反复吃过"源码字符串检查骗人"的亏（见
// docs/zh/kernel-modularization-design.md §8.6）。AST 能精确判定：
//   · 调用名恰为 `close`
//   · 实参的类型是 channel（用类型信息或至少排除 selector 形式）
// 并**自动跳过注释**。

import (
	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"

	"regexp"

	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowlist 是**明确允许**裸 close 的位置，每条都要写清理由。
//
// ★ 加白名单的门槛：必须论证"该 channel 在语言层面不可能被关两次"，
//
//	而不是"目前只调一次"（后者正是未来会被改坏的那种）。
var allowlist = map[string]string{
	// 本包自身：Safe 的实现就是那个唯一的 close。
	"internal/safeclose/safeclose.go": "本包是统一的关闭入口，close 就在这里",
}

// ★★★ 判据：全仓不得有裸 close(chan)。
func TestNoBareCloseOutsideSafeclose(t *testing.T) {
	root := filepath.Join("..", "..")
	var violations []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := info.Name()
			// 跳过依赖目录与产物目录
			switch base {
			case ".git", "node_modules", "dist", "build", "third_party", "vendor", ".go":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if _, ok := allowlist[rel]; ok {
			return nil
		}
		// 只查本仓模块内（排除 module cache 之类）
		if strings.Contains(rel, "/.go/") {
			return nil
		}

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0) // 不带 ParseComments ⇒ 注释天然被跳过
		if perr != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// 必须是内建 close：Fun 是 Ident 且名为 close
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "close" {
				return true
			}
			pos := fset.Position(call.Pos())
			violations = append(violations, rel+":"+itoa(pos.Line))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("遍历失败: %v", err)
	}

	if len(violations) > 0 {
		t.Fatalf("★ 发现 %d 处**裸 close(chan)** —— 违反 close 纪律：\n  %s\n\n"+
			"为什么必须改（用户明确要求）：\n"+
			"  今天只调一次的 close，明天加一条错误/超时/重连路径就变成两次，\n"+
			"  而那次改动看起来与 close 无关，评审不会注意 ⇒ 退出路径 panic\n"+
			"  （实测：panic: close of closed channel ⇒ 退出码 0 变 2 ⇒\n"+
			"   systemd 判成异常退出并重启，而功能本身正常）。\n\n"+
			"修法：\n"+
			"  var once sync.Once                       // 局部 channel\n"+
			"  return func() { safeclose.Safe(&once, ch) }\n\n"+
			"  // 或结构体字段\n"+
			"  type T struct { done chan struct{}; doneOnce sync.Once }\n"+
			"  func (t *T) Close() { safeclose.Safe(&t.doneOnce, t.done) }\n\n"+
			"  // 或直接交出幂等 stop 函数\n"+
			"  return safeclose.SafeFunc(ch)\n",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// ★★★ 判据：`Safe` 自身必须真的幂等（行为判据，守核心语义）。
func TestSafeIsIdempotent(t *testing.T) {
	// 不能 import 自身包再测（会循环）；这里用 SafeFunc 走公开 API
	ch := make(chan struct{})
	stop := safeclose.SafeFunc(ch)

	// 连关 5 次，不得 panic
	for i := 0; i < 5; i++ {
		stop()
	}
	// 已关闭：读应立刻返回
	select {
	case <-ch:
	default:
		t.Fatal("channel 应已关闭")
	}
}

// ★★★ 判据：**并发**关闭安全（select 写法在这一条上会挂）。
//
// 这条正是"为什么不能用 select 前置检查"的证明：
// 两个 goroutine 同时落到 default ⇒ 都 close ⇒ panic。
func TestSafeConcurrentClose(t *testing.T) {
	for round := 0; round < 200; round++ {
		ch := make(chan struct{})
		once := safeclose.NewOnceForTest()
		done := make(chan struct{}, 8)
		for i := 0; i < 8; i++ {
			go func() {
				safeclose.Safe(once, ch)
				done <- struct{}{}
			}()
		}
		for i := 0; i < 8; i++ {
			<-done
		}
	}
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

// reSelfOnce 匹配 `xxx.Do(func()` 形态（取 once 变量名）。
// reSelfOnce 匹配 `recv.onceName.Do(func()` 并取出**完整**接收者表达式
// （含点号）—— 这是上一版判据的 bug：`\w+` 不含 `.`，`p.stopOnce.Do` 只捕到
// `p`，于是与内层的 `Safe(&p.stopOnce` 比对永远不命中 ⇒ 判据假绿
// （变异改回自锁后仍然 PASS，实测发现）。
var reSelfOnce = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\.Do\(func\(\)`)

// ★★★★ 判据：**禁止自锁的 Once 嵌套**（`X.Do` 内部再 `Safe(&X)`）。
//
// ## 为什么必须有这条（我方实测事故）
//
// 我把 37 处裸 close 改成 safeclose 时，有两处写成：
//
//	p.stopOnce.Do(func() {
//		safeclose.Safe(&p.stopOnce, p.stopCh)   // ← 同一把 Once
//		...
//	})
//
// `sync.Once` **不可重入**：外层 Do 已持标记/互斥，内层 Do 等同一把锁
// ⇒ **自锁死锁**。实测后果极隐蔽：
//
//	· `go test` 不报错，只是**挂住**（600s 超时，3 个包同时超时）
//	· 栈里只看到 `sync.(*Once).Do` 嵌套 —— 不指向"哪里写错了"
//	· 而单跑那个测试才勉强看得出（还是超时，没有明确信息）
//
// 正确做法：close 的守卫用**独立的** once 字段（如 `stopChOnce`），
// 或干脆不要在 Once 里再调 Safe（裸 close 即可，因为外层 Once 已保护）。
//
// 判据形态：AST/文本混合扫描 —— 找 `X.Do(func()` 之后 30 行内出现
// `Safe(&X`。这是"危险模式存在性"，源码扫描恰当（不是接线断言）。
func TestNoNestedSelfOnceDeadlock(t *testing.T) {
	root := filepath.Join("..", "..")
	var bad []string

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "dist", "build", "third_party", "vendor", ".go":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if strings.Contains(filepath.ToSlash(rel), "/.go/") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		lines := strings.Split(string(b), "\n")
		for i, ln := range lines {
			trim := strings.TrimSpace(ln)
			if strings.HasPrefix(trim, "//") {
				continue
			}
			m := reSelfOnce.MatchString(trim)
			if !m {
				continue
			}
			once := reSelfOnce.FindStringSubmatch(trim)[1]
			for j := i; j < len(lines) && j < i+30; j++ {
				if strings.HasPrefix(strings.TrimSpace(lines[j]), "//") {
					continue
				}
				if strings.Contains(lines[j], "safeclose.Safe(&"+once) {
					bad = append(bad, filepath.ToSlash(rel)+":"+itoa(i+1)+
						"  ("+once+".Do 内部 Safe(&"+once+"))")
					break
				}
			}
		}
		return nil
	})

	if len(bad) > 0 {
		t.Fatalf("★ 发现 %d 处 **Once 自锁死锁**：\n  %s\n\n"+
			"sync.Once 不可重入 ⇒ 外层 Do 持锁、内层 Do 等同一把锁 ⇒ 永久挂住\n"+
			"（实测：go test 600s 超时，栈只显示 sync.(*Once).Do 嵌套，难定位）。\n\n"+
			"修法：close 的守卫用**独立**的 once：\n"+
			"  p.stopOnce.Do(func() { safeclose.Safe(&p.stopChOnce, p.stopCh) })\n",
			len(bad), strings.Join(bad, "\n  "))
	}
}
