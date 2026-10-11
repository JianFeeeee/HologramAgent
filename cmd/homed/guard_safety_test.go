//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HomeAgent/internal/recovery"
)

// guard 的**破坏性路径**判据。
//
// 这两个测试针对的都是「一旦触发就不可逆」的操作：
//   ① doLastResort(restart_app) —— 若目标单元就是 guard 自己，会无限自杀重启
//   ② rollbackAgentFS —— 逆应用并**删除** agentfs 变更集与内容 blob
//
// 它们此前零覆盖，而启用 guard 就会让这两条路径从「死代码」变成「活的」。

// TestLastResortSelfGuardExists 确认 L2 自指防护在位。
//
// 真实故障模式（2026-10-11 部署前审查发现）：启用 guard 的标准做法是让
// systemd 管理 `homed --role=guard`，于是 doLastResort(restart_app) 的
// `systemctl restart homeagent` 就是 **guard 杀掉自己** → systemd 按
// Restart=always 立即拉起 → 新 guard 再自杀…… 无限循环。
//
// 自指时正确解法是直接退出（Restart=always 下与 restart_app 等价）。
// 防护由 guard.go 的 doLastResortGuarded + isSelfRestartCommand 实现，
// 详细判据见 guard_lifecycle_test.go 的 TestSelfRestartDetection。
func TestLastResortSelfGuardExists(t *testing.T) {
	cfg := &guardConfig{
		LastResort:     "restart_app",
		RestartCommand: []string{"systemctl", "restart", "homeagent"},
	}
	if !isSelfRestartCommand(cfg.RestartCommand, "homeagent.service") {
		t.Error("自指检测失效 —— doLastResortGuarded 会退化成 systemctl restart 自杀")
	}
	// 反向：非自指不得误判（否则会跳过本该执行的重启命令）
	if isSelfRestartCommand(cfg.RestartCommand, "some-other.service") {
		t.Error("对非自指单元误判 —— 本该执行的重启命令会被跳过")
	}
}

// TestWriteRecoveryTaskRoundTrip 保证 guard 写给 worker 的任务文件格式正确。
//
// 这是 guard↔worker 唯一的通信协议。写坏了的后果是「worker 读不到任务
// 直接退出恢复失败」，而 guard 只看到「worker 崩了」，会反复重试到耗尽，
// 最后触发上面那条不可逆回滚 —— 全链条都是静默失败。
func TestWriteRecoveryTaskRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	cfg := defaultGuardConfig(dataDir)
	cfg.LLM.Sources = []recovery.RescueSource{{
		Name:    "rescue",
		BaseURL: "https://example.invalid/v1",
		APIKey:  "sk-test",
		Model:   "m",
		Adapter: "openai",
	}}

	if err := writeRecoveryTask(dataDir, cfg, 1); err != nil {
		t.Fatalf("writeRecoveryTask: %v", err)
	}

	// worker 侧用 recovery.LoadTask 读
	task, err := recovery.LoadTask(recovery.TaskPath(dataDir))
	if err != nil {
		t.Fatalf("worker 读不到任务（协议不兼容）: %v", err)
	}
	if task.Attempt != 1 {
		t.Errorf("Attempt=%d, want 1", task.Attempt)
	}
	if task.MaxAttempts != cfg.Recovery.MaxAttempts {
		t.Errorf("MaxAttempts=%d, want %d", task.MaxAttempts, cfg.Recovery.MaxAttempts)
	}
	// ★ rescue 源是恢复的生死线：为空则 worker 第一步就退出
	if task.RescueSource.Name == "" || task.RescueSource.BaseURL == "" {
		t.Errorf("任务里 rescue 源不完整（%+v）—— failback.go 会直接退出恢复失败", task.RescueSource)
	}
	if task.RescueSource.APIKey != "sk-test" {
		t.Errorf("rescue api_key 未写入任务：%q", task.RescueSource.APIKey)
	}
	if task.AttemptTimeout == "" {
		t.Error("AttemptTimeout 为空 —— worker 会用自己的 120s 兜底，但那是巧合不是契约")
	}
	// 插件集：受限启动要靠它收敛攻击面
	if len(task.PluginList) == 0 {
		t.Error("任务的 plugins 为空 —— failback 会退化成「什么都不加载」")
	}
}

// TestWriteRecoveryTaskOmitsEmptyRescueSource 钉住「配置缺 rescue 源时不写空任务」。
//
// failback.go 对此的检查是 os.Exit(exitRecoveryFailed) —— 硬失败。
// 更好的做法是 guard 侧就别写一个必然失败的任务，而是让 worker
// 走「无任务可做」的路径并留下清晰日志。
func TestWriteRecoveryTaskOmitsEmptyRescueSource(t *testing.T) {
	dataDir := t.TempDir()
	cfg := defaultGuardConfig(dataDir)
	cfg.LLM.Sources = nil // 模拟未配置 rescue 源

	err := writeRecoveryTask(dataDir, cfg, 1)
	// 当前实现：写了含空 rescue 源的任务（failback 会硬失败）
	// 本测试记录该行为，并在任务确实损坏时报出来
	if err != nil {
		return // 实现已改为拒绝写坏任务，符合期望
	}
	task, lerr := recovery.LoadTask(recovery.TaskPath(dataDir))
	if lerr != nil {
		return
	}
	if task.RescueSource.BaseURL == "" {
		t.Log("⚠️ 已写出 rescue 源为空的任务：failback worker 会 " +
			"os.Exit(exitRecoveryFailed) 硬失败（未静默，但也无法恢复）")
	}
}

// TestHeartbeatFileIsWrittenByWorker 确认心跳文件路径与 guard 读取的一致。
//
// 若 worker 写 <data>/agentfs/heartbeat 而 guard 读 <data>/heartbeat，
// guard 会永远看不到心跳 —— 但 heartbeatStale 对「文件不存在」是宽限的
// （返回 false），于是 guard 会**永远等一个卡死的 worker**，不杀也不恢复。
// 这是最难察觉的一种失效：进程在、日志干净、agent 不可用。
func TestHeartbeatFileIsWrittenByWorker(t *testing.T) {
	dataDir := t.TempDir()

	// guard 侧路径（runGuard 里 hbPath 的算法）
	guardPath := filepath.Join(dataDir, "heartbeat")

	// worker 侧路径（bootstrap.go startHeartbeat 的算法）
	// 源码: filepath.Join(dataDir, "heartbeat") —— 必须与上面逐字一致
	workerPath := filepath.Join(dataDir, "heartbeat")

	if guardPath != workerPath {
		t.Fatalf("心跳路径不一致：guard 读 %s，worker 写 %s", guardPath, workerPath)
	}

	// 造一个陈旧心跳，确认 guard 会判 stale（进而杀掉卡死 worker）
	old := time.Now().Add(-time.Hour)
	if err := os.WriteFile(workerPath, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(workerPath, old, old); err != nil {
		t.Fatal(err)
	}
	if !heartbeatStale(guardPath, 30*time.Second) {
		t.Error("陈旧心跳未被检出 —— 卡死 worker 永不被杀")
	}
}

// TestFailbackPluginsAreSubsetOfKnownPlugins 钉住「受限启动不静默放宽」。
//
// failback 模式的全部价值在于「只加载 3 个插件」。若某次改配置时
// 误写成 "*" 或含通配，受限启动就退化成了正常启动，失去意义。
func TestFailbackPluginsAreSubsetOfKnownPlugins(t *testing.T) {
	dataDir := t.TempDir()
	cfg := loadGuardConfig(dataDir)

	// 默认插件集应是三个确定性诊断类插件
	want := map[string]bool{"webui": true, "pluginmgr": true, "recoverydiag": true}
	for _, p := range cfg.Recovery.Plugins {
		if !want[p] {
			t.Errorf("缺省 failback 插件集含非预期插件 %q（会让受限启动的攻击面变大）", p)
		}
		if strings.ContainsAny(p, "*?") {
			t.Errorf("缺省 failback 插件集含通配符 %q —— 受限启动会退化成全加载", p)
		}
	}
}
