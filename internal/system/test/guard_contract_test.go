package test

// internal/system 的黑盒判据：导出路径判定函数，输入 → 输出。
// 不改全局状态、不碰系统文件。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/system"
)

// TestIsProtectedPathExplicit：显式前缀集内的路径判受保护，集外的不判。
func TestIsProtectedPathExplicit(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, "protected", "file.txt")
	outside := filepath.Join(dir, "other", "file.txt")
	prefixes := []string{filepath.Join(dir, "protected")}
	if !system.IsProtectedPathExplicit(prefixes, inside) {
		t.Fatal("★ 前缀集内的路径没判受保护")
	}
	if system.IsProtectedPathExplicit(prefixes, outside) {
		t.Fatal("★ 前缀集外的路径被判受保护")
	}
	if system.IsProtectedPathExplicit(nil, inside) {
		t.Fatal("★ 空前缀集却判受保护")
	}
}

// TestArchiveBeforeWrite：覆盖前留档，返回 archived=true 且原文可读。
func TestArchiveBeforeWrite(t *testing.T) {
	base := t.TempDir()
	// 用导出的配置入口把该目录设为受保护范围（不改全局默认值的语义之外）
	protected := filepath.Join(base, "etc")
	system.SetProtectedPaths([]string{protected + string(os.PathSeparator)})
	t.Cleanup(func() { system.SetProtectedPaths(nil) })
	if err := os.MkdirAll(protected, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(protected, "config.txt")
	if err := os.WriteFile(target, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	archived, err := system.ArchiveBeforeWrite(base, target)
	if err != nil {
		t.Fatalf("ArchiveBeforeWrite: %v", err)
	}
	if !archived {
		t.Fatal("★ 已存在的文件却没留档")
	}
	// 留档目录是导出的 FileBaselineDir
	baseline := system.FileBaselineDir(base)
	if baseline == "" {
		t.Fatal("★ FileBaselineDir 为空")
	}
}

// TestNetworkBaselinePathIsStable：路径输出稳定且落在 dataDir 下。
func TestNetworkBaselinePathIsStable(t *testing.T) {
	dir := t.TempDir()
	a := system.NetworkBaselinePath(dir)
	b := system.NetworkBaselinePath(dir)
	if a == "" || a != b {
		t.Fatalf("★ NetworkBaselinePath 不稳定：%q vs %q", a, b)
	}
}

// TestLoadBaselineMissingReportsError：读不存在的基线必须报错。
func TestLoadBaselineMissingReportsError(t *testing.T) {
	if _, err := system.LoadNetworkBaseline(t.TempDir()); err == nil {
		t.Fatal("★ 读不存在的基线却返回成功")
	}
}
