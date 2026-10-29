package log

// 日志保留策略的判据（本包此前**零测试**）。
//
// ## 为什么必须补
//
// 2026-10-29 发现 `applyRetention` 的兜底值是 `"forever"`：
//
//	policy := RetentionPolicy(m.cfg.GetString("core.log.retention", "forever"))
//
// 而 `core.log.retention` 用 `regMeta` 登记（只登记、不播种），
// `GetXxx` 对这类键**永远返回调用方兜底值** —— registry 里写的
// Default="n_months" 对实际行为零影响。于是压缩照做、**永不清理**：
// 生产实测 log/ 目录 590MB / 504 个文件。
//
// 这类 bug 的特征是「配置项存在、代码在读、但默认值从另一处生效」，
// 只有判据能拦住。本文件钉三件事：
//   ① 兜底值必须是会清理的策略（不是 forever）
//   ② 未知策略值不静默变成 forever（拼写错误会让清理永久失效）
//   ③ 保留策略的清理函数本身真的删文件

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeCfg 是 ConfigProvider 的测试替身；未设置的键返回调用方兜底值
// （与真实 ConfigRegistry 对 regMeta 键的行为一致）。
type fakeCfg map[string]string

func (f fakeCfg) GetString(key, def string) string {
	if v, ok := f[key]; ok {
		return v
	}
	return def
}

func (f fakeCfg) GetInt(key string, def int) int {
	if v, ok := f[key]; ok {
		var n int
		fmt.Sscanf(v, "%d", &n)
		return n
	}
	return def
}

// ① ★ 未配置时的兜底策略必须真的会清理（回归：曾经是 forever）。
func TestDefaultRetentionIsNotForever(t *testing.T) {
	// 通过 reflect 拿不到局部变量，改为行为断言：
	// 建一个月前的归档，未配置时必须被删掉。
	dir := t.TempDir()
	old := time.Now().AddDate(0, -6, 0) // 6 个月前
	name := fmt.Sprintf("month_%04d-%02d.tar.gz", old.Year(), int(old.Month()))
	writeFile(t, dir, name)

	m := NewManager(dir, fakeCfg{}) // 空配置 ⇒ 全走兜底
	m.applyRetention()

	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		t.Fatalf("★ 未配置保留策略时 %s 仍存在 —— 兜底值又变回 forever 了（压缩生效但不清理）", name)
	}
}

// ② forever 时必须保留（显式要求不删，不能被默认值覆盖）。
func TestForeverKeepsArchives(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().AddDate(0, -6, 0)
	name := fmt.Sprintf("month_%04d-%02d.tar.gz", old.Year(), int(old.Month()))
	writeFile(t, dir, name)

	m := NewManager(dir, fakeCfg{"core.log.retention": "forever"})
	m.applyRetention()

	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatalf("forever 策略下 %s 不应被删除: %v", name, err)
	}
}

// ③ ★ 未知策略值不得静默变成 forever。
//
// 拼写错误（"weekly" / "n_month"）若是落到 forever，清理会永久失效，
// 而运维看着配置里写着"保留 3 个月"以为已经生效 —— 磁盘直到写满都没人发现。
func TestUnknownPolicyFallsBackToCleaningNotForever(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().AddDate(0, -6, 0)
	name := fmt.Sprintf("month_%04d-%02d.tar.gz", old.Year(), int(old.Month()))
	writeFile(t, dir, name)

	m := NewManager(dir, fakeCfg{"core.log.retention": "n_month"}) // 少一个 s
	m.applyRetention()

	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		t.Fatal("★ 未知策略值被当成了 forever —— 拼写错误会导致清理永久失效")
	}
}

// ④ n_months 的月数来自配置，且非法值回退 3。
func TestRetentionMonthsHonoredAndSanitized(t *testing.T) {
	dir := t.TempDir()
	// 5 个月前：months=3 应删；months=12 应留。
	old := time.Now().AddDate(0, -5, 0)
	name := fmt.Sprintf("month_%04d-%02d.tar.gz", old.Year(), int(old.Month()))

	writeFile(t, dir, name)
	NewManager(dir, fakeCfg{
		"core.log.retention":        "n_months",
		"core.log.retention_months": "12",
	}).applyRetention()
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatalf("months=12 时 5 个月前的归档应保留: %v", err)
	}

	// 非法月数（0 / 负数）应回退 3 ⇒ 5 个月前被删。
	NewManager(dir, fakeCfg{
		"core.log.retention":        "n_months",
		"core.log.retention_months": "0",
	}).applyRetention()
	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		t.Fatal("months=0 应回退到 3 ⇒ 5 个月前的归档必须被删")
	}
}

// ⑤ 原始日志（当月的 homed_*.log）不归保留策略管：
//
//	它们要等跨月压缩成 month_*.tar.gz 之后才可能被清。
//
//	这条是**刻意的语义边界**，钉住它是为了说明"清归档"不等于"清当场日志"。
func TestRawLogsNotRemovedByRetention(t *testing.T) {
	dir := t.TempDir()
	raw := "homed_2026-01-01_00-00-00.log"
	writeFile(t, dir, raw)

	NewManager(dir, fakeCfg{"core.log.retention": "this_week"}).applyRetention()

	if _, err := os.Stat(filepath.Join(dir, raw)); err != nil {
		t.Fatalf("原始 homed_*.log 不应被保留策略直接删除（它先要跨月压缩）: %v", err)
	}
}

func writeFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatalf("建 %s: %v", name, err)
	}
}
