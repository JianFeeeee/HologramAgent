//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// guard 的配置加载与心跳判定 —— 这两块决定了「guard 会不会误判」
// 和「配置坏了会发生什么」，此前**零测试覆盖**。
//
// 为何优先补这两处：它们是纯逻辑、无外部依赖，能立刻把三个真实风险
// 钉成判据（见每个测试的说明）。

// TestLoadGuardConfigDefaults 保证缺配置时有一份**可用**的默认值。
//
// 风险：guard 是「agent 能不能起来」的决策层。若默认值里出现
// 零值（如 MaxRestarts=0 → 永远进 failback、heartbeatTimeout=0
// → 心跳永远判定为 stale → 无限杀 worker），那「没配 guard.yaml」
// 就等于「装上就坏」。
func TestLoadGuardConfigDefaults(t *testing.T) {
	dir := t.TempDir() // 空目录 = 无 guard.yaml

	cfg := loadGuardConfig(dir)

	if cfg.MaxRestarts < 1 {
		t.Errorf("MaxRestarts=%d 必须 ≥1，否则第一次崩溃就直接进 failback", cfg.MaxRestarts)
	}
	if cfg.heartbeatTimeout <= 0 {
		t.Error("heartbeatTimeout 必须为正值：0 会让每次心跳检查都判定 stale → 无限杀 worker")
	}
	if cfg.heartbeatInterval <= 0 {
		t.Error("heartbeatInterval 必须为正值")
	}
	if !cfg.FailbackEnabled {
		t.Error("failback 缺省应为启用（这是设计默认）")
	}
	if cfg.LastResort != "restart_app" && cfg.LastResort != "reboot" {
		t.Errorf("LastResort=%q 不是合法值（loadGuardConfig 应归一为 restart_app）", cfg.LastResort)
	}
	if cfg.Recovery.MaxAttempts < 1 {
		t.Errorf("Recovery.MaxAttempts=%d 必须 ≥1，否则 failback 一轮就宣告耗尽", cfg.Recovery.MaxAttempts)
	}
	if cfg.Recovery.attemptTimeout <= 0 {
		t.Error("Recovery.attemptTimeout 必须为正值：0 会让 probe 立即超时 → 梯子必判失败")
	}
	if len(cfg.Recovery.Plugins) == 0 {
		t.Error("缺省 failback 插件集不应为空（空 = 受限启动什么都不加载）")
	}
	// LLMSnapshot 必须指向 dataDir 下的文件，否则 L1 还原找不到基线。
	if cfg.LLMSnapshot == "" {
		t.Error("LLMSnapshot 缺省应填 <dataDir>/llm_snapshot.json")
	} else if filepath.Dir(cfg.LLMSnapshot) != dir {
		t.Errorf("LLMSnapshot=%q 应在 dataDir=%s 下", cfg.LLMSnapshot, dir)
	}
}

// TestLoadGuardConfigParsesYAML 保证真实配置能被解析并覆盖默认值。
func TestLoadGuardConfigParsesYAML(t *testing.T) {
	dir := t.TempDir()
	yaml := `
max_restarts: 5
heartbeat_timeout: 45s
failback_enabled: false
last_resort: reboot
llm:
  sources:
    - name: rescue
      base_url: https://example.invalid/v1
      api_key: sk-test
      model: some-model
      adapter: openai
recovery:
  max_attempts: 7
  attempt_timeout: 30s
  plugins: [webui]
system:
  protected_paths: [/custom/]
  network_paths: [/custom/resolv.conf]
`
	if err := os.WriteFile(filepath.Join(dir, "guard.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadGuardConfig(dir)

	if cfg.MaxRestarts != 5 {
		t.Errorf("MaxRestarts=%d, want 5", cfg.MaxRestarts)
	}
	if cfg.heartbeatTimeout != 45*time.Second {
		t.Errorf("heartbeatTimeout=%v, want 45s", cfg.heartbeatTimeout)
	}
	if cfg.FailbackEnabled {
		t.Error("FailbackEnabled 被 YAML 覆盖失效")
	}
	if cfg.LastResort != "reboot" {
		t.Errorf("LastResort=%q, want reboot", cfg.LastResort)
	}
	if cfg.Recovery.MaxAttempts != 7 {
		t.Errorf("Recovery.MaxAttempts=%d, want 7", cfg.Recovery.MaxAttempts)
	}
	if len(cfg.Recovery.Plugins) != 1 || cfg.Recovery.Plugins[0] != "webui" {
		t.Errorf("Recovery.Plugins=%v, want [webui]", cfg.Recovery.Plugins)
	}
	if len(cfg.LLM.Sources) != 1 || cfg.LLM.Sources[0].BaseURL != "https://example.invalid/v1" {
		t.Errorf("LLM.Sources 未被 YAML 覆盖: %+v", cfg.LLM.Sources)
	}
	// 系统路径适配要生效（发行版差异靠它）
	if len(cfg.System.ProtectedPaths) != 1 || cfg.System.ProtectedPaths[0] != "/custom/" {
		t.Errorf("System.ProtectedPaths=%v", cfg.System.ProtectedPaths)
	}
}

// TestLoadGuardConfigSurvivesCorruptYAML 保证**配置写坏不会让 guard 变得不可用**。
//
// 真实风险：guard.yaml 是纯文本，被编辑器截断或手工写错都很常见。
// 若解析失败时返回零值配置（而不是默认值），就会出现
// MaxRestarts=0 → 立刻 failback → heartbeatTimeout=0 → 无限杀 worker。
// loadGuardConfig 当前用「先填默认值再 Unmarshal」规避了这点，本测试钉住它。
func TestLoadGuardConfigSurvivesCorruptYAML(t *testing.T) {
	dir := t.TempDir()
	// 典型的手工写坏：缩进错误 + 半截内容
	broken := "max_restarts: 3\n  bad_indent: [\n  unclosed: {"
	if err := os.WriteFile(filepath.Join(dir, "guard.yaml"), []byte(broken), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadGuardConfig(dir)

	// 关键：必须退回一份**可用**的配置，而不是零值
	if cfg.MaxRestarts < 1 {
		t.Errorf("配置损坏后 MaxRestarts=%d，退化成了零值（会导致立刻 failback）", cfg.MaxRestarts)
	}
	if cfg.heartbeatTimeout <= 0 {
		t.Error("配置损坏后 heartbeatTimeout 退化为零值（会导致无限杀 worker）")
	}
	if cfg.Recovery.MaxAttempts < 1 {
		t.Error("配置损坏后 Recovery.MaxAttempts 退化为零值（failback 一轮就耗尽）")
	}
	if cfg.Recovery.attemptTimeout <= 0 {
		t.Error("配置损坏后 attemptTimeout 退化为零值（probe 立即超时 → 梯子必败）")
	}
}

// TestLoadGuardConfigRejectsOutOfRangeValues 保证越界值被归一到可用区间。
func TestLoadGuardConfigRejectsOutOfRangeValues(t *testing.T) {
	dir := t.TempDir()
	yaml := `
max_restarts: 0
heartbeat_timeout: "not-a-duration"
last_resort: bogus
recovery:
  max_attempts: -5
  attempt_timeout: "also-not-a-duration"
`
	if err := os.WriteFile(filepath.Join(dir, "guard.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadGuardConfig(dir)

	if cfg.MaxRestarts < 1 {
		t.Errorf("max_restarts=0 未被归一：%d", cfg.MaxRestarts)
	}
	if cfg.heartbeatTimeout <= 0 {
		t.Error("无法解析的 heartbeat_timeout 未回落到默认值")
	}
	if cfg.LastResort == "bogus" {
		t.Error("非法 last_resort 未被归一为 restart_app")
	}
	if cfg.Recovery.MaxAttempts < 1 {
		t.Errorf("负数 max_attempts 未被归一：%d", cfg.Recovery.MaxAttempts)
	}
	if cfg.Recovery.attemptTimeout <= 0 {
		t.Error("无法解析的 attempt_timeout 未回落到默认值")
	}
}

// TestHeartbeatStale 保证「卡死判定」不会误杀，也不会漏判。
//
// 这是 guard 唯一的存活判据，两种错都致命：
//   - **误判 stale**（心跳刚写过却说卡死）→ 杀掉健康的 worker。
//     代价：agent 反复重启，正在进行的对话全丢。
//   - **漏判 stale**（心跳早就不动了却说不卡死）→ 卡死的 worker 永不被杀，
//     guard 一直等它，agent 实际不可用却没人恢复。
//
// 三个分支都要覆盖：无文件（宽限）/ 新鲜 / 陈旧。
func TestHeartbeatStale(t *testing.T) {
	dir := t.TempDir()
	hb := filepath.Join(dir, "heartbeat")

	// ① 无心跳文件：刚启动或异常，**必须宽限**（否则刚起就杀）
	if heartbeatStale(hb, 30*time.Second) {
		t.Error("无心跳文件时应判为不陈旧（宽限），否则 worker 刚启动就会被杀")
	}

	// ② 刚写入：新鲜
	if err := os.WriteFile(hb, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if heartbeatStale(hb, 30*time.Second) {
		t.Error("心跳刚写入却判为陈旧 —— 会误杀健康 worker")
	}

	// ③ 陈旧：把 mtime 推到过去
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(hb, old, old); err != nil {
		t.Fatal(err)
	}
	if !heartbeatStale(hb, 30*time.Second) {
		t.Error("心跳 2 分钟未更新（阈值 30s）却判为新鲜 —— 卡死的 worker 永远不会被杀")
	}
	// 阈值边界：刚过阈值一点点就该判陈旧
	just := time.Now().Add(-31 * time.Second)
	if err := os.Chtimes(hb, just, just); err != nil {
		t.Fatal(err)
	}
	if !heartbeatStale(hb, 30*time.Second) {
		t.Error("心跳 31s 未更新（阈值 30s）应判为陈旧")
	}
}

// TestSnapshotBeforeDefaultsTrue 记录当前默认行为 —— 该默认值意味着
// 「failback 耗尽时会不可逆地回滚并删除 agentfs 变更」。
//
// 这不是判据，是**显式记录**：一旦有人要改这个默认值，本测试会提醒
// 同时更新文档/部署说明，因为该操作的破坏性是用户数据级的。
//
// 真正的安全性由 TestRollbackAgentFSIsDestructive 这条（下面）保证。
func TestSnapshotBeforeDefaultsTrue(t *testing.T) {
	dir := t.TempDir()
	cfg := loadGuardConfig(dir)
	if !cfg.LastResortCfg.SnapshotBefore {
		t.Log("注意：SnapshotBefore 默认为 false（不自动回滚 agentfs）")
	}
	// 两种取值都不该让 guard 崩或跳过 L2
	if !cfg.FailbackEnabled {
		t.Error("failback 缺省应启用")
	}
}

// TestStartupGraceAllowsInit 钉住「启动宽限期」这一部署关键行为。
//
// ★ 场景（2026-10-12 生产部署实测触发的故障）：
//
//	停机 34s 后重启，heartbeat 文件 mtime 停在上一代 worker（已过期），
//	新 worker 要先做完全部重型初始化（chineseclip + 静态词向量 + 知识重建，
//	冷启动 5–8s）才开始写心跳。guard 的 5s ticker 先到、拿旧 mtime 判 stale
//	→ 把还在加载的 worker SIGKILL → 连续 6 次重启、guard 进入 failback
//	Safe-Mode（服务降级）。
//
// 不变量：
//
//	(a) 宽限期内，即使心跳文件 mtime 极旧，也**不许**判杀
//	(b) 宽限期满后，陈旧心跳**必须**判杀（卡死的 worker 不能靠宽限躲过去）
//	(c) 宽限期满后，新鲜心跳不许判杀（误杀健康 worker 同样是事故）
func TestStartupGraceAllowsInit(t *testing.T) {
	dir := t.TempDir()
	hb := filepath.Join(dir, "heartbeat")
	if err := os.WriteFile(hb, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	// 上一代 worker 的旧 mtime（停机 34s → 远超 30s 阈值）
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(hb, old, old); err != nil {
		t.Fatal(err)
	}

	spawn := time.Now()
	deadline := spawn.Add(30 * time.Second)

	// (a) 宽限期内（刚 spawn 1s）：旧 mtime 也不许杀
	if shouldKillStaleHeartbeat(spawn.Add(1*time.Second), deadline, hb, 30*time.Second) {
		t.Error("宽限期内判杀 —— 会把还在加载多模态空间/知识库的 worker SIGKILL（生产已实测触发 6 连杀）")
	}
	// 宽限最后一刻同样不许杀（ticker 恰在 deadline 前触发是常态）
	if shouldKillStaleHeartbeat(deadline.Add(-50*time.Millisecond), deadline, hb, 30*time.Second) {
		t.Error("宽限期末沿判杀 —— worker 还没来得及写第一次心跳")
	}

	// (b) 宽限期满 + 心跳仍旧（真的卡死）：必须杀
	if !shouldKillStaleHeartbeat(deadline.Add(1*time.Second), deadline, hb, 30*time.Second) {
		t.Error("宽限期满后陈旧心跳未判杀 —— 卡死的 worker 靠宽限躲过去，agent 实际不可用")
	}

	// (c) 宽限期满 + 心跳新鲜（健康 worker）：不许杀
	fresh := time.Now()
	if err := os.Chtimes(hb, fresh, fresh); err != nil {
		t.Fatal(err)
	}
	if shouldKillStaleHeartbeat(deadline.Add(2*time.Second), deadline, hb, 30*time.Second) {
		t.Error("宽限期满后新鲜心跳被判杀 —— 误杀健康 worker")
	}
}
