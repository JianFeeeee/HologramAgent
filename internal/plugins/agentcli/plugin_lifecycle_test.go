//go:build linux

package agentcli

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 超时终端必须真正释放：杀掉整个进程组（包括 sh 的子进程如 sleep）
// 且回收子进程（不留 <defunct>）。
//
// 旧实现三个缺陷叠加：
//  1. readLoop 的 IsExpired 分支只 delete(sessions) 后 return，不 Kill 不 Close；
//  2. Kill 只杀直接子进程 sh，Setsid 后真正的命令（sleep）是孙进程，成为孤儿；
//  3. 只 Start 从不 Wait，退出的子进程无人回收，积成僵尸。
func TestExpiredTerminalReleasesProcessGroup(t *testing.T) {
	p := New("agentcli")
	sdkInst := sdk.New("agentcli", sdk.SDKConfig{
		RegTool:  func(string, sdk.ToolDef, sdk.ToolHandler) error { return nil },
		RegStage: func(sdk.Stage, sdk.StageHandler) {},
		RegAPI:   func(string) error { return nil },
		Settings: sdk.NewSettings("agentcli", nil),
	})
	sdkInst.SetIOInjector(&injectCapture{})
	if err := p.Start(sdkInst); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	// 命令设计要点（缺一不可）：
	//   1) 让 sh 保留为父进程、另起孙进程（`... & wait`）——单个 `sleep 300`
	//      会被 sh 直接 exec 掉，只有一个进程，测不到「孙进程逃逸」；
	//   2) 孙进程显式忽略 SIGHUP——否则关 PTY master 时内核发的 SIGHUP 会
	//      顺手把它带走，于是「只杀 leader」也能通过，测不出进程组 Kill 的必要性。
	// 两个条件合起来，只有给整个进程组发 SIGKILL 才能清干净。
	marker := `(trap "" HUP; sleep 300) & wait`
	res, err := p.handleCreate(sdkInst, map[string]interface{}{
		"command": marker,
		"timeout": "1s",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]interface{})
	if m["error"] != nil {
		t.Skipf("PTY unavailable: %v", m["error"])
	}

	p.mu.Lock()
	ts := p.sessions[m["id"].(string)]
	p.mu.Unlock()
	if ts == nil {
		t.Fatal("terminal not registered")
	}
	pid := ts.cmd.Process.Pid

	// 等超时被 readLoop 处理（含 Kill 进程组 + Wait 回收）
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if !procAlive(pid) && !procGroupAlive(pid) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("expired terminal not released: leader pid=%d alive=%v groupAlive=%v",
		pid, procAlive(pid), procGroupAlive(pid))
}

func procAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// procGroupAlive 检查「原进程组」里是否还有活着的成员（含被 init 收养的孙进程）。
//
// 不能用 kill(-pgid, 0)：组领头进程一死，内核就可能回收该 pgid，
// 即便组里还有被 reparent 的成员，这个探测也会失败。
// 改为直接遍历 /proc 查 pgid 匹配的活进程。
func procGroupAlive(pgid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			if readPgid(pid) == pgid {
				return true
			}
		}
	}
	return false
}

// readPgid 从 /proc/<pid>/stat 读进程组 id（第 5 个字段）。
// stat 的 comm 字段可能含空格/括号，所以从最后一个 ')' 之后再切分。
func readPgid(pid int) int {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return -1
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return -1
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 3 {
		return -1
	}
	// fields[0]=state, [1]=ppid, [2]=pgrp
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return -1
	}
	return pgid
}
