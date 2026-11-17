package test

// internal/security 的**基线**判据：受保护路径的写前留档 + L1 还原，
// 以及路径沙箱判定。用真实文件系统输入 → 可观察文件内容输出。
//
// ★ 这一块 2026-11-16 从 `internal/system` 迁入本包：
// 它们与 `Policy.CheckPath` 是**同一条策略的三段**（能写吗 / 写了留档 / 坏了还原），
// 散在两个包里正是「留档做了、还原没接」的结构原因。
//
// 数据目录的路径按 sandbox 的语义显式注入：SetProtectedPaths 是
// 部署适配入口（不同发行版受保护前缀不同），不属于被测逻辑。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// TestArchiveAndRestoreRoundTrip 覆盖基线的完整职责：
// 受保护文件被覆盖前留档 → 覆盖后可恢复成原内容。
func TestArchiveAndRestoreRoundTrip(t *testing.T) {
	base := t.TempDir()
	protected := filepath.Join(base, "etc")
	security.SetProtectedPaths([]string{protected + string(os.PathSeparator)})
	t.Cleanup(func() { security.SetProtectedPaths(nil) })

	if err := os.MkdirAll(protected, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(protected, "config.txt")
	const original = "original-content"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	archived, err := security.ArchiveBeforeWrite(base, target)
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

	restored, err := security.RestoreFileFromBaseline(base, target)
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
	security.SetProtectedPaths([]string{protected + string(os.PathSeparator)})
	t.Cleanup(func() { security.SetProtectedPaths(nil) })
	for _, d := range []string{protected, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	unprotected := filepath.Join(other, "file.txt")
	if err := os.WriteFile(unprotected, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if archived, err := security.ArchiveBeforeWrite(base, unprotected); err != nil || archived {
		t.Fatalf("unprotected file archived=%v err=%v", archived, err)
	}

	newFile := filepath.Join(protected, "does-not-exist.txt")
	if archived, err := security.ArchiveBeforeWrite(base, newFile); err != nil || archived {
		t.Fatalf("new file archived=%v err=%v", archived, err)
	}
	if _, err := os.Stat(filepath.Join(security.FileBaselineDir(base), "etc", "does-not-exist.txt")); err == nil {
		t.Fatal("baseline written for a file that did not exist")
	}
}

// TestArchiveIsIdempotentForUnchangedContent：内容未变时重复留档不再产生新档案。
func TestArchiveIsIdempotentForUnchangedContent(t *testing.T) {
	base := t.TempDir()
	protected := filepath.Join(base, "etc")
	security.SetProtectedPaths([]string{protected + string(os.PathSeparator)})
	t.Cleanup(func() { security.SetProtectedPaths(nil) })
	if err := os.MkdirAll(protected, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(protected, "same.txt")
	if err := os.WriteFile(target, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	if archived, err := security.ArchiveBeforeWrite(base, target); err != nil || !archived {
		t.Fatalf("first archive: archived=%v err=%v", archived, err)
	}
	if archived, err := security.ArchiveBeforeWrite(base, target); err != nil || archived {
		t.Fatalf("second archive on unchanged content: archived=%v err=%v", archived, err)
	}
}

// TestSandboxPathJudgement：路径沙箱按方向判读写根，且 `..` 不能逃逸。
//
// 这是 `Policy.CheckPath` 的职责面 —— 在 2026-11-16 之前它**零生产调用**
// （配置读进了 Policy，却没有任何模块拿它判路径）。现在由内核在工具执行前
// 按 `ToolDef.PathArgs` 调用（见 agent/core/toolguard.go）。
func TestSandboxPathJudgement(t *testing.T) {
	allowRead := t.TempDir()
	allowWrite := t.TempDir()
	outside := t.TempDir()

	rt := security.NewDisabledRuntime()
	rt.SetPolicy(security.Policy{
		Sandbox: security.SandboxPolicy{
			Enabled:    true,
			ReadRoots:  []string{allowRead},
			WriteRoots: []string{allowWrite},
		},
	})

	inside := filepath.Join(allowRead, "a.txt")
	if d := rt.CheckPath("read", inside); !d.Allowed {
		t.Fatalf("★ 读根内被拒: %s", d.Reason)
	}
	if d := rt.CheckPath("read", filepath.Join(outside, "a.txt")); d.Allowed {
		t.Fatal("★ 读根外被放行")
	}
	// 同一个路径在写方向按 write_roots 判：读根内的那个不是写根 ⇒ 应被拒。
	if d := rt.CheckPath("write", inside); d.Allowed {
		t.Fatal("★ 读根内、但不在写根的路径，写方向却放行了")
	}
	if d := rt.CheckPath("write", filepath.Join(allowWrite, "b.txt")); !d.Allowed {
		t.Fatalf("★ 写根内被拒: %s", d.Reason)
	}
	// `..` 逃逸：clean 之后仍必须落在根外。
	if d := rt.CheckPath("read", filepath.Join(allowRead, "..", filepath.Base(outside), "x")); d.Allowed {
		t.Fatal("★ `..` 逃逸未被拦住")
	}
}

// TestSandboxDisabledAllowsEverything：沙箱未启用时一切放行（存量行为不变）。
func TestSandboxDisabledAllowsEverything(t *testing.T) {
	rt := security.NewDisabledRuntime()
	rt.SetPolicy(security.Policy{}) // 零值 = 不限制
	if d := rt.CheckPath("write", "/etc/passwd"); !d.Allowed {
		t.Fatalf("★ 未启用沙箱却拦了写: %s", d.Reason)
	}
}
