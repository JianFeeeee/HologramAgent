package test

// internal/system 的模块级黑盒判据：受保护路径的写前留档与恢复，
// 用真实文件系统输入 → 可观察文件内容输出。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/system"
)

// TestArchiveAndRestoreRoundTrip 覆盖 guard 的完整职责：
// 受保护文件被覆盖前留档 → 覆盖后可恢复成原内容。
func TestArchiveAndRestoreRoundTrip(t *testing.T) {
	base := t.TempDir()
	protected := filepath.Join(base, "etc")
	system.SetProtectedPaths([]string{protected + string(os.PathSeparator)})
	t.Cleanup(func() { system.SetProtectedPaths(nil) })

	if err := os.MkdirAll(protected, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(protected, "config.txt")
	const original = "original-content"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	archived, err := system.ArchiveBeforeWrite(base, target)
	if err != nil {
		t.Fatalf("ArchiveBeforeWrite: %v", err)
	}
	if !archived {
		t.Fatal("existing protected file was not archived")
	}

	// 调用方随后覆盖文件（模拟 L0 写入）。
	if err := os.WriteFile(target, []byte("broken-update"), 0o644); err != nil {
		t.Fatal(err)
	}

	restored, err := system.RestoreFileFromBaseline(base, target)
	if err != nil {
		t.Fatalf("RestoreFileFromBaseline: %v", err)
	}
	if !restored {
		t.Fatal("restore reported no baseline")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("restored content = %q, want %q", got, original)
	}
}

// TestArchiveSkipsUnprotectedAndNewFiles：非受保护路径与新建文件都不留档。
func TestArchiveSkipsUnprotectedAndNewFiles(t *testing.T) {
	base := t.TempDir()
	protected := filepath.Join(base, "etc")
	other := filepath.Join(base, "home")
	system.SetProtectedPaths([]string{protected + string(os.PathSeparator)})
	t.Cleanup(func() { system.SetProtectedPaths(nil) })
	for _, d := range []string{protected, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	unprotected := filepath.Join(other, "file.txt")
	if err := os.WriteFile(unprotected, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if archived, err := system.ArchiveBeforeWrite(base, unprotected); err != nil || archived {
		t.Fatalf("unprotected file archived=%v err=%v", archived, err)
	}

	newFile := filepath.Join(protected, "does-not-exist.txt")
	if archived, err := system.ArchiveBeforeWrite(base, newFile); err != nil || archived {
		t.Fatalf("new file archived=%v err=%v", archived, err)
	}
	if _, err := os.Stat(filepath.Join(system.FileBaselineDir(base), "etc", "does-not-exist.txt")); err == nil {
		t.Fatal("baseline written for a file that did not exist")
	}
}

// TestArchiveIsIdempotentForUnchangedContent：内容未变时重复留档不再产生新档案。
func TestArchiveIsIdempotentForUnchangedContent(t *testing.T) {
	base := t.TempDir()
	protected := filepath.Join(base, "etc")
	system.SetProtectedPaths([]string{protected + string(os.PathSeparator)})
	t.Cleanup(func() { system.SetProtectedPaths(nil) })
	if err := os.MkdirAll(protected, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(protected, "same.txt")
	if err := os.WriteFile(target, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	if archived, err := system.ArchiveBeforeWrite(base, target); err != nil || !archived {
		t.Fatalf("first archive: archived=%v err=%v", archived, err)
	}
	if archived, err := system.ArchiveBeforeWrite(base, target); err != nil || archived {
		t.Fatalf("second archive on unchanged content: archived=%v err=%v", archived, err)
	}
}
