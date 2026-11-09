package test

// internal/tracker 的黑盒判据：NewTracker/PreAction/PostAction → 可观察输出。
// 不碰 t.changeSets、不读内部目录结构（只用公开方法返回的结果）。

import (
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/tracker"
)

func newTracker(t *testing.T) *tracker.Tracker {
	t.Helper()
	dir := t.TempDir()
	tr := tracker.NewTracker(filepath.Join(dir, "data"), filepath.Join(dir, "work"))
	if err := tr.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return tr
}

// TestFreshTrackerHasNoChanges：新 tracker 无 changeset。
func TestFreshTrackerHasNoChanges(t *testing.T) {
	tr := newTracker(t)
	if tr.HasChanges() {
		t.Fatal("★ 新 tracker 报告有变更")
	}
	if got := tr.ChangeSets(); len(got) != 0 {
		t.Fatalf("★ ChangeSets() = %v，期望空", got)
	}
}

// TestNewChangeSetCarriesAction：构造出的 changeset 带 action/ID/时间。
func TestNewChangeSetCarriesAction(t *testing.T) {
	cs := tracker.NewChangeSet("write_file")
	if cs.Action != "write_file" {
		t.Fatalf("★ Action = %q", cs.Action)
	}
	if cs.ID == "" || cs.Timestamp.IsZero() {
		t.Fatalf("★ ID/时间未填：%+v", cs)
	}
}

// TestPostActionWithoutFileChangeIsNoop：没有文件变更时 PostAction
// 不得凭空造出 changeset。
func TestPostActionWithoutFileChangeIsNoop(t *testing.T) {
	tr := newTracker(t)
	_ = tr.PreAction("write_file")
	got := tr.PostAction("write_file")
	if len(got.Files) != 0 {
		t.Fatalf("★ 无文件变更却记录了 %d 条", len(got.Files))
	}
	if tr.HasChanges() {
		t.Fatal("★ 无文件变更却 HasChanges=true")
	}
}

// TestRollbackLatestWithoutChangesReportsError：无 changeset 时定向回滚报错。
func TestRollbackLatestWithoutChangesReportsError(t *testing.T) {
	tr := newTracker(t)
	if err := tr.RollbackLatest(); err == nil {
		t.Fatal("★ 无 changeset 时 RollbackLatest 应报错")
	}
}

// TestRollbackClearsChangeSets：Rollback 后视图清空（无需挂载 overlay）。
func TestRollbackClearsChangeSets(t *testing.T) {
	tr := newTracker(t)
	_ = tr.PreAction("a")
	_ = tr.PostAction("a")
	if err := tr.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if tr.HasChanges() {
		t.Fatal("★ Rollback 后仍有变更")
	}
}
