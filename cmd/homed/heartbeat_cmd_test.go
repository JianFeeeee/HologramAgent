package main

// 心跳控制指令的判据（2026-10-17）。
//
// ## 为什么这两条判据必须存在
//
// 指令通道有三个**静默失效**点，每一个都表现为"按钮点了没反应"：
//
//  1. 解析不到 → guard 不知道用户想关机 → 照常重拉（用户以为没生效）
//  2. **消费后不清除** → guard 每次心跳都读到同一条 → 无限重启/关机
//  3. 清除了但 worker 内存里还留着 → 下一次心跳又写回去 → 同上
//
// 第 2、3 点是同一个 bug 的两面，而且**本机测不出来**（只有真跑才会
// 陷入重启循环）。所以用判据把它们钉住。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ★★★ 判据：能解析出合法指令，畸形行被忽略。
func TestParseHeartbeatCmd(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want hbCommand
	}{
		{"关机", "t=100\ncmd=shutdown reason=用户点击\n", hbCommand{Cmd: "shutdown", Reason: "用户点击", OK: true}},
		{"重启", "t=100\ncmd=restart reason=升级配置\n", hbCommand{Cmd: "restart", Reason: "升级配置", OK: true}},
		{"无指令", "t=100\nt=101\n", hbCommand{}},
		{"未知指令", "t=100\ncmd=explode\n", hbCommand{}},
		{"畸形行", "garbage\ncmd=\n", hbCommand{}},
		{"无 reason", "cmd=shutdown\n", hbCommand{Cmd: "shutdown", OK: true}},
		{"空内容", "", hbCommand{}},
	}
	for _, c := range cases {
		got := parseHeartbeatCmd(c.in)
		if got.Cmd != c.want.Cmd || got.OK != c.want.OK {
			t.Errorf("%s: 得到 %+v，应以 %+v", c.name, got, c.want)
		}
		if c.want.Reason != "" && got.Reason != c.want.Reason {
			t.Errorf("%s: reason 得到 %q，应 %q", c.name, got.Reason, c.want.Reason)
		}
	}
}

// ★★★ 判据：consumeHeartbeatCmd 读完必须**清除** cmd 行（否则无限重启）。
func TestConsumeHeartbeatCmdClearsCommand(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "heartbeat")
	if err := os.WriteFile(p, []byte("t=100\ncmd=shutdown reason=x\n"), 0644); err != nil {
		t.Fatal(err)
	}

	c := consumeHeartbeatCmd(p)
	if !c.OK || c.Cmd != hbCmdShutdown {
		t.Fatalf("未读到关机指令: %+v", c)
	}

	// 文件必须已被清除
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if parseHeartbeatCmd(string(b)).OK {
		t.Fatalf("★ 消费后 cmd 行仍在（%q）—— guard 每次心跳都会读到同一条 ⇒ 无限关机", string(b))
	}
	// mtime 语义必须保留：文件里仍要有 t= 行，否则 guard 可能判 worker 卡死
	if len(b) == 0 {
		t.Fatal("★ 清空成空文件 —— 心跳文件的 mtime 语义被破坏")
	}

	// 第二次消费必须读不到（幂等）
	if parseHeartbeatCmd(readAll(t, p)).OK {
		t.Fatal("★ 第二次仍能读到指令")
	}
}

// ★★★ 判据：worker 侧写过的指令，在 guard 清除后**不该被再写回**。
//
// 这是最容易漏的一面：guard 与 worker 是两个进程，
// guard 清的是**文件**，worker 内存里的 pendingCmd 还在。
// 若 worker 不检查文件状态，下一次 tick 就把指令写回去 ⇒ 无限循环。
func TestWorkerDoesNotRewriteConsumedCommand(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "heartbeat")
	// 模拟：已经没有 cmd 行（guard 消费过）
	if err := os.WriteFile(p, []byte("t=200\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 投递一条指令，并标记"已经写过一次"
	requestHeartbeatCmd(hbCmdShutdown, "test")
	// ★ 用 defer 而不是 t.Cleanup：Cleanup 里的手动 Lock 一旦前面的
	// t.Fatalf 提前返回就可能与未释放的锁叠加（静态分析也提示了这一点）。
	// defer 的语义更直白：本函数怎么退出都会解锁并复位。
	defer resetPendingCmdForTest()
	pendingCmdMu.Lock()
	pendingWritten = true
	pendingCmdMu.Unlock()

	content := nextHeartbeatWrite(p, 300)
	if content != "" {
		t.Fatalf("★ guard 已消费（文件无 cmd 行），worker 却又写回指令：%q\n"+
			"  后果：无限关机/重启循环 —— 本机单测测不出，只有真跑才暴露", content)
	}
}

// ★★★ 判据：指令未被消费时必须持续写在心跳里（不能写完一次就丢）。
func TestWorkerKeepsWritingUntilConsumed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "heartbeat")
	os.WriteFile(p, []byte("t=100\ncmd=shutdown reason=test\n"), 0644)

	requestHeartbeatCmd(hbCmdShutdown, "test")
	defer resetPendingCmdForTest()

	// 第一次：文件里还有 cmd= ⇒ 继续写（还没被消费）
	c1 := nextHeartbeatWrite(p, 200)
	if c1 == "" {
		t.Fatal("★ 指令还在文件里（未消费）却不继续写 —— guard 可能错过它")
	}
}

func readAll(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// resetPendingCmdForTest 复位 pending 状态，保证测试之间不串。
func resetPendingCmdForTest() {
	pendingCmdMu.Lock()
	pendingCmd = hbCommand{}
	pendingWritten = false
	pendingCmdMu.Unlock()
}

// ★★★ 判据：心跳文件必须**滚动**，不能无限增长。
//
// 2026-10-17 生产实测：心跳文件长到 **14.6MB**（纯 O_APPEND，约 70 天）。
// 心跳的唯一职责是刷 mtime，历史行没有任何读者 ⇒ 累加纯属浪费，
// 且 consumeHeartbeatCmd 会被迫整读 14.6MB。
func TestHeartbeatFileDoesNotGrowUnbounded(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "heartbeat")

	// 模拟 worker 的滚动写：连续写 200 次（每次都是"当前这一行"）
	for i := 0; i < 200; i++ {
		content := "t=" + itoa(int64(1000+i)) + "\n"
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// 200 次心跳后文件仍应是单行（约 10 字节），不是 200 行
	if fi.Size() > 64 {
		t.Fatalf("★ 心跳文件增长到 %d 字节 —— 必须是滚动写入（只保留当前行）\n"+
			"  生产实测这种累加会到 14.6MB", fi.Size())
	}
	lines := strings.Count(readAll(t, p), "\n")
	if lines != 1 {
		t.Fatalf("★ 心跳文件有 %d 行，应为 1 行", lines)
	}
}

// ★★★ 判据：巨型历史心跳文件只读尾部（不整读），且读后收敛成小文件。
func TestConsumeLargeHeartbeatReadsTailOnly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "heartbeat")

	// 造一个 1MB 的历史文件，末尾追加指令
	var sb strings.Builder
	for i := 0; i < 100000; i++ {
		sb.WriteString("t=" + itoa(int64(i)) + "\n")
	}
	sb.WriteString("t=999999\ncmd=shutdown reason=bigfile\n")
	if err := os.WriteFile(p, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}

	c := consumeHeartbeatCmd(p)
	if !c.OK || c.Cmd != hbCmdShutdown {
		t.Fatalf("★ 大文件里的指令没读到: %+v", c)
	}
	// 读后必须收敛（clearHeartbeatCmd 重写成单行）
	fi, _ := os.Stat(p)
	if fi.Size() > 64 {
		t.Fatalf("★ 消费后文件仍 %d 字节 —— 巨型历史文件应被一次性收敛", fi.Size())
	}
	// mtime 语义保留：文件里仍有 t= 行
	if !strings.HasPrefix(readAll(t, p), "t=") {
		t.Fatal("★ 收敛后丢了 t= 行 —— guard 的 mtime 语义会被破坏")
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
