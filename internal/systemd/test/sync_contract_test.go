package test

// internal/systemd 的黑盒判据：State/NewMigratePlan 的导出方法，输入 → 输出。
// 不读单元文件、不执行脚本、不碰系统状态。

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/systemd"
)

// TestStateInSync：相同路径（含 ./ 与尾部斜杠）判一致；不同判不一致。
func TestStateInSync(t *testing.T) {
	cases := []struct {
		name string
		r, c string
		want bool
	}{
		{"完全相同", "/data/home", "/data/home", true},
		{"路径等价", "/data/home/", "/data/home/./", true},
		{"空运行期", "", "/data/home", true},
		{"空配置", "/data/home", "", true},
		{"真不一致", "/data/a", "/data/b", false},
	}
	for _, c := range cases {
		s := systemd.State{RuntimeDataDir: c.r, ConfigDataDir: c.c}
		if got := s.InSync(); got != c.want {
			t.Errorf("★ %s: InSync=%v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestStateDescribe：一致时诊断为空；不一致时给出两个路径与后果。
func TestStateDescribe(t *testing.T) {
	ok := systemd.State{RuntimeDataDir: "/d", ConfigDataDir: "/d"}
	if d := ok.Describe(); d != "" {
		t.Fatalf("★ 一致时 Describe 应为空，实际 %q", d)
	}
	bad := systemd.State{RuntimeDataDir: "/data/a", ConfigDataDir: "/data/b"}
	d := bad.Describe()
	if !strings.Contains(d, "/data/a") || !strings.Contains(d, "/data/b") {
		t.Fatalf("★ Describe 缺两个路径：%q", d)
	}
	if !strings.Contains(d, "数据分裂") {
		t.Fatalf("★ Describe 没说明后果：%q", d)
	}
}

// TestSyncCommand：不一致时给出可执行的同步命令。
func TestSyncCommand(t *testing.T) {
	s := systemd.State{RuntimeDataDir: "/data/a", ConfigDataDir: "/data/b"}
	cmd := s.SyncCommand("/data/a")
	if cmd == "" {
		t.Fatal("★ 不一致时 SyncCommand 为空")
	}
	if !strings.Contains(cmd, "/data/a") {
		t.Fatalf("★ SyncCommand 不含目标路径：%q", cmd)
	}
}

// TestNewMigratePlanNormalizesPaths：构造计划时路径被 Clean，脚本含新旧路径。
func TestNewMigratePlanNormalizesPaths(t *testing.T) {
	old := "/data/old/./"
	newDir := "/data/new//"
	p := systemd.NewMigratePlan(old, newDir)
	if p.OldDir != filepath.Clean(old) || p.NewDir != filepath.Clean(newDir) {
		t.Fatalf("★ 路径未 Clean：old=%q new=%q", p.OldDir, p.NewDir)
	}
	script := p.Script()
	if !strings.Contains(script, p.OldDir) || !strings.Contains(script, p.NewDir) {
		t.Fatal("★ 迁移脚本没内联新旧路径")
	}
	if !strings.HasPrefix(script, "#!/usr/bin/env bash") {
		t.Fatal("★ 脚本缺 shebang —— 不能直接执行")
	}
}

// TestRestartCommand：重启命令必须包含给定地址。
func TestRestartCommand(t *testing.T) {
	cmd := systemd.RestartCommand("127.0.0.1:8080", "test-key")
	if !strings.Contains(cmd, "127.0.0.1:8080") {
		t.Fatalf("★ RestartCommand 不含地址：%q", cmd)
	}
}
