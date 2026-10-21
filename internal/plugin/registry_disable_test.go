package plugin

import (
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// TestDisablePluginNotInstalled 验证 DisablePlugin 对未安装插件的拒绝：
// 修复前会把任意名字写进 disabled_plugins 表（脏数据），修复后应直接报错。
func TestDisablePluginNotInstalled(t *testing.T) {
	r := &Registry{
		plugins:       make(map[string]sdk.Plugin),
		factories:     make(map[string]NativeFactory),
		knownDisabled: make(map[string]bool),
		plgDir:        t.TempDir(),
	}
	err := r.DisablePlugin("nonexistent_test_xyz", "test")
	if err == nil {
		t.Fatal("DisablePlugin should reject a plugin that is not installed")
	}
	if err.Error() != "plugin nonexistent_test_xyz not installed" {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.knownDisabled["nonexistent_test_xyz"] {
		t.Fatal("knownDisabled must not contain a non-installed plugin")
	}
}

// TestPluginInstalledFactory 内置插件（工厂注册，无物理目录）应视为已安装。
func TestPluginInstalledFactory(t *testing.T) {
	r := &Registry{
		plugins:       make(map[string]sdk.Plugin),
		factories:     map[string]NativeFactory{"builtin_demo": nil},
		knownDisabled: make(map[string]bool),
		plgDir:        t.TempDir(),
	}
	if !r.pluginInstalled("builtin_demo") {
		t.Fatal("factory-registered builtin plugin should be considered installed")
	}
	if r.pluginInstalled("no_such_plugin") {
		t.Fatal("unknown plugin should not be considered installed")
	}
}

// TestPluginInstalledDir 外部插件按 plgDir/<name> 目录存在判定已安装。
func TestPluginInstalledDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "external_demo"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	r := &Registry{
		plugins:       make(map[string]sdk.Plugin),
		factories:     make(map[string]NativeFactory),
		knownDisabled: make(map[string]bool),
		plgDir:        dir,
	}
	if !r.pluginInstalled("external_demo") {
		t.Fatal("plugin with an existing directory should be considered installed")
	}
	// 同名普通文件（非目录）不算已安装
	if err := os.WriteFile(filepath.Join(dir, "just_a_file"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if r.pluginInstalled("just_a_file") {
		t.Fatal("a regular file must not count as an installed plugin")
	}
	// 空名兜底
	if r.pluginInstalled("") {
		t.Fatal("empty plugin name must not be considered installed")
	}
}
