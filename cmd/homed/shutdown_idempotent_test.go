package main

// 退出路径的**幂等性**判据（2026-10-17）。
//
// ## 为什么必须有这条
//
// 实测 panic（真机 SIGTERM）：
//
//	panic: close of closed channel
//	goroutine 1 [running]:
//	main.startHeartbeat.func3()  bootstrap.go:259
//	main.main()                  main.go:293
//
// 根因：`startHeartbeat` 返回的 stop 函数是裸的 `close(hbStop)`，
// 而它有**两个**调用点：
//   - main.go 的 `defer stopHeartbeatEarly()`
//   - waitForShutdown 里显式 `stopHeartbeat()`
//
// ⇒ 第二次 close 必然 panic。
//
// ## 为什么这条缺陷很值得守
//
// 它发生在**退出路径的最后一步**：进程已经把所有清理做完了，
// 却在收尾时崩溃 ⇒ 退出码从 0 变成 2（panic）⇒
// systemd/guard 把它判成"异常退出"而不是"正常停止"，
// 进而触发**不必要的重启/告警**。而功能上一切正常，
// 排查方向会被引向"是不是哪里崩了"。
//
// ## 判据形态
//
// 断言「close(停止通道) 必须被 sync.Once（或等价）保护」。
// 这是"模式存在性"，源码扫描恰当；行为验证由真机 SIGTERM 做
// （已实测：修前 panic、修后 `[homed] stopped` 且 0 panic）。

import (
	"os"
	"strings"
	"testing"
)

// ★★★ 判据：startHeartbeat 的 stop 函数必须幂等。
func TestStartHeartbeatStopIsIdempotent(t *testing.T) {
	src, err := os.ReadFile("bootstrap.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)

	// 找 startHeartbeat 函数体
	i := strings.Index(code, "func startHeartbeat(")
	if i < 0 {
		t.Skip("startHeartbeat 已改名/搬走，请同步更新判据")
	}
	end := strings.Index(code[i:], "\n}\n")
	if end < 0 {
		t.Fatal("找不到 startHeartbeat 结尾")
	}
	body := code[i : i+end]

	// 必须返回 stop 函数
	if !strings.Contains(body, "return func()") {
		t.Skip("stop 函数的形态已变，请同步更新判据")
	}
	// ★ 核心：裸 close 是缺陷
	if strings.Contains(body, "return func() { close(") {
		t.Fatal("★ startHeartbeat 的 stop 函数是**裸的 close()** —— " +
			"而它有两个调用点（main 的 defer + waitForShutdown 的显式调用）\n" +
			"   ⇒ 第二次 close 必然 panic: close of closed channel\n" +
			"   ⇒ 退出码从 0 变 2 ⇒ systemd/guard 判成异常退出并重启。\n" +
			"   修法：用 sync.Once 保护。")
	}
	// 正向：必须有 sync.Once（或至少 Once.Do）
	if !strings.Contains(body, "sync.Once") && !strings.Contains(body, ".Do(") {
		t.Error("★ startHeartbeat 的 stop 函数没有幂等保护（sync.Once）")
	}
}

// ★★★ 判据：**所有**返回 stop 函数的启动函数都必须幂等。
//
// 一般化：本仓的 stop 函数习惯上既被 defer 调、又被显式调，
// 裸 close 是通用的坑。
func TestNoBareChannelCloseInStopFunctions(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	bad := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
			strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		for idx, ln := range lines {
			trim := strings.TrimSpace(ln)
			// 只查"返回 stop 函数且其中裸 close"的形态
			if !strings.Contains(trim, "return func()") {
				continue
			}
			// 同一行内联形态：return func() { close(x) }
			if strings.Contains(trim, "close(") && !strings.Contains(trim, "Do(") {
				t.Errorf("★ %s:%d 返回的 stop 函数是裸 close（调用两次即 panic）: %s",
					e.Name(), idx+1, trim)
				bad++
			}
		}
	}
	_ = bad
}
