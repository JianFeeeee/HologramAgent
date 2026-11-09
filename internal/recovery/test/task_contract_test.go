package test

// internal/recovery 的黑盒判据：SaveTask/LoadTask/SaveResult/LoadResult，
// 输入 → 输出（文件往返）。不读实现、不碰内部字段。

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/recovery"
)

// TestTaskRoundTrip：SaveTask → LoadTask 得回全部字段。
func TestTaskRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "task.json")
	in := &recovery.Task{
		Attempt: 3, MaxAttempts: 5, AttemptTimeout: "120s",
		TriggerPrompt: "检查网络", PluginList: []string{"cmd", "files"},
	}
	if err := recovery.SaveTask(path, in); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}
	out, err := recovery.LoadTask(path)
	if err != nil {
		t.Fatalf("LoadTask: %v", err)
	}
	if out.Attempt != 3 || out.MaxAttempts != 5 || out.AttemptTimeout != "120s" {
		t.Fatalf("★ 往返后字段丢失：%+v", out)
	}
	if len(out.PluginList) != 2 || out.PluginList[0] != "cmd" {
		t.Fatalf("★ PluginList 丢失：%v", out.PluginList)
	}
	if got := out.Plugins(); len(got) != 2 {
		t.Fatalf("★ Plugins() = %v", got)
	}
}

// TestTaskPathsAreStable：TaskPath/ResultPath 由 dataDir 决定且互不相同。
func TestTaskPathsAreStable(t *testing.T) {
	dir := "/data/contract"
	a := recovery.TaskPath(dir)
	b := recovery.TaskPath(dir)
	if a != b || a == "" {
		t.Fatalf("★ TaskPath 不稳定：%q vs %q", a, b)
	}
	if recovery.ResultPath(dir) == a {
		t.Fatal("★ TaskPath 与 ResultPath 相同")
	}
}

// TestResultRoundTrip：SaveResult → LoadResult 得回结论字段。
func TestResultRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	in := &recovery.Result{
		Attempt: 2, Success: true, Cause: "network", NetworkOK: true,
		RestoredNet: []string{"/etc/resolv.conf"}, ConfigRestored: true,
		QuickChatOK: true, Steps: []string{"check", "restore"},
		Message: "已恢复", Timestamp: time.Now(),
	}
	if err := recovery.SaveResult(path, in); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}
	out, err := recovery.LoadResult(path)
	if err != nil {
		t.Fatalf("LoadResult: %v", err)
	}
	if !out.Success || out.Cause != "network" || !out.ConfigRestored || !out.QuickChatOK {
		t.Fatalf("★ 往返后结论丢失：%+v", out)
	}
}

// TestLoadTaskMissingFileReportsError：读不存在的文件必须报错，不静默给空值。
func TestLoadTaskMissingFileReportsError(t *testing.T) {
	if _, err := recovery.LoadTask(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("★ 读不存在的任务文件却返回成功")
	}
}

// TestSortSteps：步骤排序输出可用（不钉死具体顺序以外的内部结构）。
func TestSortSteps(t *testing.T) {
	in := []string{"c", "a", "b"}
	out := recovery.SortSteps(in)
	if len(out) != 3 {
		t.Fatalf("★ SortSteps = %v", out)
	}
}
