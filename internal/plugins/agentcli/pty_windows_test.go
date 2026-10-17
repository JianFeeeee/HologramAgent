//go:build windows

package agentcli

import (
	"strings"
	"testing"
	"time"
)

// TestNewCommandPtyConPTY 验证 Windows ConPTY 后端：一次性命令输出可读，
// 交互式会话可写读往返。
func TestNewCommandPtyConPTY(t *testing.T) {
	ta, _, err := newCommandPty("cmd.exe /c echo conpty-ok", 24, 80)
	if err != nil {
		t.Fatalf("once: %v", err)
	}
	outA := drainFor(ta, 3*time.Second)
	if !strings.Contains(string(outA), "conpty-ok") {
		t.Fatalf("once output missing echo: %q", string(outA))
	}
	ta.Close()

	tb, _, err := newCommandPty("cmd.exe", 24, 80)
	if err != nil {
		t.Fatalf("interactive: %v", err)
	}
	defer tb.Close()
	time.Sleep(300 * time.Millisecond)
	if _, err := tb.WriteString("echo hi-123\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	outB := drainFor(tb, 3*time.Second)
	if !strings.Contains(string(outB), "hi-123") {
		t.Fatalf("interactive output missing echo: %q", string(outB))
	}
	if !tb.Running() {
		t.Fatalf("interactive shell should still be running")
	}
}

func drainFor(term ptyTerm, dur time.Duration) []byte {
	deadline := time.Now().Add(dur)
	buf := make([]byte, 4096)
	var out []byte
	for time.Now().Before(deadline) {
		ch := make(chan struct {
			N int
			E error
		}, 1)
		go func() {
			n, e := term.Read(buf)
			ch <- struct {
				N int
				E error
			}{n, e}
		}()
		select {
		case r := <-ch:
			if r.N > 0 {
				out = append(out, buf[:r.N]...)
			}
			if r.E != nil {
				return out
			}
		case <-time.After(500 * time.Millisecond):
			return out
		}
	}
	return out
}
