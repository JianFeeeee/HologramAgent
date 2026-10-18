package core

// 判据：写进日志的工具内容必须**先脱敏**。
//
// ## 为什么日志泄漏比上下文泄漏更严重
//
//	模型上下文  短期——一轮结束就不再发送
//	日志        长期——每天一个文件、从不自动清理，systemd journal 另存一份，
//	                   排查问题/贴 issue 时还会再复制出去
//
// 一次落盘就是永久泄漏。凭据轮换前，任何拿到日志的人都能直接用。
//
// ## 具体缺陷（2026-10-18 审计）
//
//	task.go:1031  log.Printf("[agent] tool %s result: %s", ..., truncateStr(result, 100))
//	task.go:967   log.Printf("[agent] tool %s raw_arguments: %s", ..., truncateStr(tc.RawArguments, 300))
//
// 两处都原样打印。工具输出含服务端回显的凭据时（enc_shell 跑 curl 拿 200），
// 明文直接写进日志文件。
//
// ## 判据方式
//
// 直接调 scrubForLog 验证行为（真 Store，不用 mock），
// 再验证两处调用点确实走了它（接线）。

import (
	"os"
	"strings"
	"testing"
)

// ★★★ 判据：scrubForLog 把明文换成别名。
func TestScrubForLogReplacesPlaintext(t *testing.T) {
	newScrubStore(t)

	const tok = "sk-" + "abcdefghijklmnopqrstuvwxyz0123"
	in := `{"output":"HTTP 200 {` + `"key":"` + tok + `"}"}`

	out := scrubForLog(in, "tool_result:enc_shell")
	if strings.Contains(out, tok) {
		t.Fatalf("★ 日志文本里仍有明文 key：%s", out)
	}
	if !strings.Contains(out, "sec_") {
		t.Fatalf("★ 没有替换成别名（而是被删掉了？）：%s", out)
	}
}

// ★★★ 判据：普通文本不被改动（不误伤）。
func TestScrubForLogLeavesNormalTextAlone(t *testing.T) {
	newScrubStore(t)
	const normal = "工具 cmd_run 执行成功，输出 42 行，无异常"
	if got := scrubForLog(normal, "tool_result:cmd_run"); got != normal {
		t.Fatalf("★ 普通文本被改动了：%q → %q", normal, got)
	}
}

// ★★★ 判据：security.Global() 为 nil 时不 panic（安全模块未启用是常态）。
func TestScrubForLogNilStoreIsSafe(t *testing.T) {
	// 不初始化 store
	got := scrubForLog("文本 sk-abcdefghijklmnopqrstuvwxyz0123", "x")
	if got == "" {
		t.Fatal("nil store 时不该返回空")
	}
}

// ★★★ 判据（接线）：两处日志调用点必须走 scrubForLog。
//
// 只测函数不测接线 = 漏掉原始缺陷（函数写对了但没人调）。
func TestLogCallSitesUseScrub(t *testing.T) {
	b, err := os.ReadFile("task.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	for _, c := range []struct{ name, marker string }{
		{"工具结果日志", `log.Printf("[agent] tool %s result: %s"`},
		{"原始参数日志", `log.Printf("[agent] tool %s raw_arguments: %s"`},
	} {
		i := strings.Index(src, c.marker)
		if i < 0 {
			t.Fatalf("找不到%s（被改名/删除了？）", c.name)
		}
		// 取该语句到结束（下一行 log/语句边界）
		seg := src[i:]
		if j := strings.Index(seg, "\n\n"); j > 0 {
			seg = seg[:j]
		}
		if !strings.Contains(seg, "scrubForLog(") {
			t.Fatalf("★ %s 没有走 scrubForLog —— 凭据会原样写进日志文件（永久泄漏）", c.name)
		}
	}
}
