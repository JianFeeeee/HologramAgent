package test

// internal/log 的黑盒判据：NewManager → Start/Stop 不 panic；
// RetentionPolicy 常量是对外契约。不读日志文件内容、不碰 m.done。

import (
	"context"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/log"
)

type stubCfg struct{}

func (c *stubCfg) GetString(key, def string) string { return def }
func (c *stubCfg) GetInt(key string, def int) int   { return def }

// TestManagerStartStopIsSafe：构造 + 启动 + 停止不得 panic/挂住。
func TestManagerStartStopIsSafe(t *testing.T) {
	m := log.NewManager(t.TempDir(), &stubCfg{})
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	cancel()
	done := make(chan struct{})
	go func() { m.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("★ Stop 挂住")
	}
}

// TestManagerStopIsIdempotent：连停两次不得 panic（close 纪律）。
func TestManagerStopIsIdempotent(t *testing.T) {
	m := log.NewManager(t.TempDir(), &stubCfg{})
	m.Stop()
	m.Stop()
}

// TestRetentionPoliciesAreDistinct：四个策略常量互不相同且非空。
func TestRetentionPoliciesAreDistinct(t *testing.T) {
	all := []log.RetentionPolicy{
		log.RetentionThisWeek, log.RetentionThisMonth,
		log.RetentionNMonths, log.RetentionForever,
	}
	seen := map[log.RetentionPolicy]bool{}
	for _, p := range all {
		if p == "" || seen[p] {
			t.Fatalf("★ 策略常量为空或重复：%q", p)
		}
		seen[p] = true
	}
}
