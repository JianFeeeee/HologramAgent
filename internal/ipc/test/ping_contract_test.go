package test

// internal/ipc 的黑盒判据：NewServer/Start → NewClient/Ping → Status 输出。
// 用真实临时目录下的 socket，不 mock 传输层。

import (
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/ipc"
)

// TestPingReturnsServerStatus：Server 提供的状态必须原样到达 Client。
func TestPingReturnsServerStatus(t *testing.T) {
	dataDir := t.TempDir()
	pid := 4242
	srv := ipc.NewServer(dataDir, func() *ipc.Status {
		return &ipc.Status{PID: pid, Boot: "normal", UptimeSec: 7, Tools: 12}
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("Server.Start: %v", err)
	}
	defer srv.Stop()

	c := ipc.NewClient(dataDir)
	defer c.Close()
	st, err := c.Ping(3 * time.Second)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if st.PID != pid || st.Boot != "normal" || st.Tools != 12 {
		t.Fatalf("★ Ping 拿到的状态不符：%+v", st)
	}
}

// TestPingWithoutServerReportsError：没有服务端时 Ping 必须报错，不静默给空状态。
func TestPingWithoutServerReportsError(t *testing.T) {
	c := ipc.NewClient(t.TempDir())
	defer c.Close()
	if _, err := c.Ping(300 * time.Millisecond); err == nil {
		t.Fatal("★ 没有服务端却 Ping 成功")
	}
}
