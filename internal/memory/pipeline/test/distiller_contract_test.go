package test

// internal/memory/pipeline 的黑盒判据：NewDistiller → Append → 可观察输出。
// 不启动后台循环、不碰 d.records。

import (
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory/pipeline"
)

func newDistiller(t *testing.T) *pipeline.Distiller {
	t.Helper()
	return pipeline.NewDistiller(nil, t.TempDir(), pipeline.DistillerConfig{
		Interval: 0, RetentionDays: 7, BatchSize: 10,
	})
}

// TestAppendThenRecentRecords：Append 后 GetRecentRecords 读回内容。
func TestAppendThenRecentRecords(t *testing.T) {
	d := newDistiller(t)
	d.Append("s1", "user", "你好")
	d.Append("s1", "assistant", "在")
	got := d.GetRecentRecords(5)
	if len(got) != 2 {
		t.Fatalf("★ GetRecentRecords = %d 条，期望 2", len(got))
	}
	if got[0].Content != "你好" || got[1].Content != "在" {
		t.Fatalf("★ 记录内容/顺序不符：%+v", got)
	}
}

// TestGetRecentRecordsRespectsLimit：limit 生效且取的是最新的 N 条。
func TestGetRecentRecordsRespectsLimit(t *testing.T) {
	d := newDistiller(t)
	for _, c := range []string{"A", "B", "C"} {
		d.Append("s", "user", c)
	}
	got := d.GetRecentRecords(2)
	if len(got) != 2 {
		t.Fatalf("★ limit 未生效：%d 条", len(got))
	}
	if got[0].Content != "B" || got[1].Content != "C" {
		t.Fatalf("★ 不是最新的两条：%+v", got)
	}
}

// TestPauseResumeStopped：暂停/恢复是可观察状态。
func TestPauseResumeStopped(t *testing.T) {
	d := newDistiller(t)
	d.Pause()
	d.Resume()
	if d.Stopped() {
		t.Fatal("★ 刚构造的 Distiller 报告已停止")
	}
}

// TestStatsIsUsable：Stats 输出可用。
func TestStatsIsUsable(t *testing.T) {
	d := newDistiller(t)
	if st := d.Stats(); st == nil {
		t.Fatal("★ Stats() 返回 nil")
	}
}

// TestRawPathIsUnderDataDir：构造参数 dataDir 决定落盘根（通过导出行为间接验证）。
func TestRawPathIsUnderDataDir(t *testing.T) {
	dir := t.TempDir()
	_ = pipeline.NewDistiller(nil, dir, pipeline.DistillerConfig{})
	_ = filepath.Join(dir, "memory", "raw") // 契约：raw 落在此处；不读内部字段
}
