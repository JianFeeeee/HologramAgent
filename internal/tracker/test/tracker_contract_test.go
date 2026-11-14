package test

// internal/tracker 的模块级黑盒判据：真实文件变更 → changeset → 回滚恢复文件。
// 使用 upper 目录作为可写工作区的真实输入，不碰 t.changeSets 之外无公开替代的状态。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/tracker"
)

func newTracker(t *testing.T) (*tracker.Tracker, string) {
	t.Helper()
	dir := t.TempDir()
	tr := tracker.NewTracker(filepath.Join(dir, "data"), filepath.Join(dir, "work"))
	if err := tr.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = tr.Stop() })
	return tr, filepath.Join(dir, "work", "upper")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestPostActionRecordsRealFileChange：真实新建文件后 PostAction 必须记录该文件。
func TestPostActionRecordsRealFileChange(t *testing.T) {
	tr, upper := newTracker(t)
	tr.PreAction("write_file")
	writeFile(t, filepath.Join(upper, "new.txt"), "hello")
	cs := tr.PostAction("write_file")
	if len(cs.Files) != 1 || cs.Files[0].Path != "new.txt" || cs.Files[0].Type != tracker.ChangeFileCreated {
		t.Fatalf("changeset = %+v", cs.Files)
	}
	if !tr.HasChanges() {
		t.Fatal("HasChanges false after a tracked change")
	}
}

// TestRollbackLatestRestoresModifiedContent：修改已存在文件的原文被回滚恢复。
func TestRollbackLatestRestoresModifiedContent(t *testing.T) {
	tr, upper := newTracker(t)
	const original = "original"
	const changed = "changed"
	writeFile(t, filepath.Join(upper, "cfg.txt"), original)

	tr.PreAction("edit")
	writeFile(t, filepath.Join(upper, "cfg.txt"), changed)
	tr.PostAction("edit")
	if got := readFile(t, filepath.Join(upper, "cfg.txt")); got != changed {
		t.Fatalf("precondition: %q", got)
	}

	if err := tr.RollbackLatest(); err != nil {
		t.Fatalf("RollbackLatest: %v", err)
	}
	if got := readFile(t, filepath.Join(upper, "cfg.txt")); got != original {
		t.Fatalf("after rollback = %q, want %q", got, original)
	}
}

// TestRollbackLatestRemovesCreatedFile：新建文件被回滚时应当消失。
func TestRollbackLatestRemovesCreatedFile(t *testing.T) {
	tr, upper := newTracker(t)
	tr.PreAction("write_file")
	path := filepath.Join(upper, "temp.txt")
	writeFile(t, path, "scratch")
	tr.PostAction("write_file")
	if err := tr.RollbackLatest(); err != nil {
		t.Fatalf("RollbackLatest: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("created file survived rollback (err=%v)", err)
	}
}

// TestRollbackLatestWithoutChangesReportsError：没有变更时定向回滚必须明确报错。
func TestRollbackLatestWithoutChangesReportsError(t *testing.T) {
	tr, _ := newTracker(t)
	if err := tr.RollbackLatest(); err == nil {
		t.Fatal("RollbackLatest accepted empty history")
	}
}
