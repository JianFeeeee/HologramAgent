package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── 升级确认与回滚的判据 ────────────────────────────────────
//
// 这些钉住"启动失败自动回滚"这个核心承诺。它一旦失效，症状是
// 升级后机器进崩溃循环、只能人工上机器救 —— 而那时用户看不到任何提示。

// 判据 1：写标记 → 能读回，字段不丢。
func TestPendingMarker_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := PendingRecord{
		FromVer: "1.4.0", FromCommit: "aaa111",
		ToVer: "1.4.1", ToCommit: "bbb222",
		BackupDir: "/root/homed-upgrade-backup-x",
	}
	if err := MarkPendingUpgrade(dir, rec); err != nil {
		t.Fatalf("写标记: %v", err)
	}
	got, err := ReadPendingUpgrade(dir)
	if err != nil {
		t.Fatalf("读标记: %v", err)
	}
	if got == nil {
		t.Fatal("应读到标记")
	}
	if got.FromCommit != rec.FromCommit || got.ToCommit != rec.ToCommit || got.BackupDir != rec.BackupDir {
		t.Errorf("字段丢失: %+v", got)
	}
	if got.At.IsZero() {
		t.Error("At 应被自动填上（诊断用）")
	}
}

// 判据 2：没有标记时读回 (nil, nil) —— 不是 error。
//
// 这是绝大多数启动的形态（没有待确认升级）。若这里返回 error，
// 每次启动都会走"回滚失败"分支 ⇒ 内核永远起不来。
func TestPendingMarker_AbsentIsNotError(t *testing.T) {
	got, err := ReadPendingUpgrade(t.TempDir())
	if err != nil {
		t.Fatalf("无标记不应报错: %v", err)
	}
	if got != nil {
		t.Errorf("无标记应返回 nil，实际 %+v", got)
	}
}

// 判据 3：清标记后再读应为 nil。
func TestPendingMarker_Clear(t *testing.T) {
	dir := t.TempDir()
	if err := MarkPendingUpgrade(dir, PendingRecord{FromVer: "a", ToVer: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := ClearPendingUpgrade(dir); err != nil {
		t.Fatalf("清标记: %v", err)
	}
	if got, _ := ReadPendingUpgrade(dir); got != nil {
		t.Errorf("清后应读不到，实际 %+v", got)
	}
	// 幂等：再清一次不报错（update 模块每次启动都调）
	if err := ClearPendingUpgrade(dir); err != nil {
		t.Errorf("重复清除应幂等: %v", err)
	}
}

// 判据 4：RollbackIfUnconfirmed 在无标记时不动二进制。
func TestRollback_NoMarkerNoOp(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "homed")
	os.WriteFile(bin, []byte("current"), 0755)

	rolled, _, _, err := RollbackIfUnconfirmed(dir, bin)
	if err != nil {
		t.Fatalf("无标记不应报错: %v", err)
	}
	if rolled {
		t.Error("无标记不应触发回滚")
	}
	b, _ := os.ReadFile(bin)
	if string(b) != "current" {
		t.Errorf("二进制不应被改动，实际 %q", b)
	}
}

// 判据 5：有标记且备份存在 ⇒ 回滚，二进制回到旧版。
//
// 这是核心路径：上次升级留下的版本没能启动成功。
func TestRollback_RestoresBackup(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "homed")

	// 备份目录里放"旧版"
	backupDir := filepath.Join(dir, "backup")
	os.MkdirAll(backupDir, 0755)
	oldBin := filepath.Join(backupDir, "homed")
	os.WriteFile(oldBin, []byte("OLD-GOOD"), 0755)
	os.WriteFile(filepath.Join(backupDir, "depgraph.bin"), []byte("OLD-DG"), 0644)

	// 当前是"升级后的坏版本"
	os.WriteFile(bin, []byte("NEW-BROKEN"), 0755)
	os.WriteFile(filepath.Join(dir, "depgraph.bin"), []byte("NEW-DG"), 0644)

	MarkPendingUpgrade(dir, PendingRecord{
		FromVer: "1.4.0", FromCommit: "old",
		ToVer: "1.4.1", ToCommit: "new",
		BackupDir: backupDir,
	})

	rolled, from, to, err := RollbackIfUnconfirmed(dir, bin)
	if err != nil {
		t.Fatalf("回滚不应失败: %v", err)
	}
	if !rolled {
		t.Fatal("有待确认标记时必须回滚")
	}
	if from != "1.4.0" || to != "1.4.1" {
		t.Errorf("版本轨迹应记录 from/to，实际 %s → %s", from, to)
	}

	// ① 二进制回到旧版
	b, _ := os.ReadFile(bin)
	if string(b) != "OLD-GOOD" {
		t.Errorf("二进制应回滚到旧版，实际 %q", b)
	}
	// ② depgraph 同批回滚（必须与 homed 一致）
	dg, _ := os.ReadFile(filepath.Join(dir, "depgraph.bin"))
	if string(dg) != "OLD-DG" {
		t.Errorf("depgraph 应同批回滚，实际 %q", dg)
	}
	// ③ 标记已清（否则下次启动又回滚一次）
	if got, _ := ReadPendingUpgrade(dir); got != nil {
		t.Error("回滚后应清除标记")
	}
	// ④ 坏版本被留档（万一回滚目标也有问题，能回到现场）
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if len(e.Name()) > 12 && e.Name()[:12] == "homed-broken" {
			found = true
		}
	}
	if !found {
		t.Error("坏版本应被留档到 homed-broken-*")
	}
}

// 判据 6：备份不存在 ⇒ 回滚失败（返回 error），**不静默继续**。
//
// 静默继续的后果：用一个已知起不来的版本装配 ⇒ 崩溃循环。
// 必须让调用方中止启动，把问题暴露给人。
func TestRollback_MissingBackupIsLoud(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "homed")
	os.WriteFile(bin, []byte("NEW-BROKEN"), 0755)

	MarkPendingUpgrade(dir, PendingRecord{
		FromVer: "1.4.0", BackupDir: filepath.Join(dir, "nonexistent-backup"),
	})

	rolled, _, _, err := RollbackIfUnconfirmed(dir, bin)
	if err == nil {
		t.Fatal("备份不存在时必须报错（静默继续 = 用已知起不来的版本装配）")
	}
	if rolled {
		t.Error("回滚失败时 rolled 应为 false")
	}
	// 错误必须可执行：说出备份路径与标记路径，人能照着修
	if !containsStr2(err.Error(), "nonexistent-backup") {
		t.Errorf("错误应指出备份路径，实际: %v", err)
	}
	if !containsStr2(err.Error(), "kernel-upgrade-pending") {
		t.Errorf("错误应指出标记路径（删它即可放弃回滚），实际: %v", err)
	}
}

// 判据 7：标记写坏（JSON 损坏）⇒ 报错并提示可删。
func TestPendingMarker_CorruptIsActionable(t *testing.T) {
	dir := t.TempDir()
	p := pendingPath(dir)
	os.WriteFile(p, []byte("{ not json"), 0644)

	_, err := ReadPendingUpgrade(dir)
	if err == nil {
		t.Fatal("损坏的标记必须报错")
	}
	if !containsStr2(err.Error(), "删掉") {
		t.Errorf("错误应说明可删掉该文件，实际: %v", err)
	}
}

func containsStr2(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// 判据 8：State.Summary 必须带回滚信息（healthcheck 要展示）。
func TestStateSummary_CarriesRollback(t *testing.T) {
	st := &State{
		Version: "1.4.0", Commit: "abc123", SDKVersion: "1.3.0",
		RolledBack: true, RolledFrom: "1.4.0", RolledTo: "1.4.1",
	}
	m := st.Summary()
	if m["rolled_back"] != true {
		t.Error("Summary 应带回滚标记")
	}
	if m["rolled_from"] != "1.4.0" || m["rolled_to"] != "1.4.1" {
		t.Errorf("Summary 应带回滚轨迹，实际 %v", m)
	}
	// ShortCommit 只对**超过 8 位**的 commit 截短；6 位的原样返回。
	// 用长 commit 才能验到截短行为。
	st2 := &State{Version: "1.4.0", Commit: "abcdef1234567890"}
	if m2 := st2.Summary(); m2["commit"] != "abcdef12" {
		t.Errorf("长 commit 应被截到 8 位，实际 %v", m2["commit"])
	}
}

// 判据 9：nil State 的 Summary 不 panic（healthcheck 可能在模块未就绪时调用）。
func TestStateSummary_NilSafe(t *testing.T) {
	var st *State
	if got := st.Summary(); got != nil {
		t.Errorf("nil State 应返回 nil，实际 %v", got)
	}
}

// 判据 10：标记里的时间会被填上（诊断"这次升级是什么时候试的"）。
func TestPendingMarker_TimestampSet(t *testing.T) {
	dir := t.TempDir()
	before := time.Now().Add(-time.Second)
	MarkPendingUpgrade(dir, PendingRecord{FromVer: "a", ToVer: "b"})
	got, _ := ReadPendingUpgrade(dir)
	if got.At.Before(before) {
		t.Errorf("At 应接近当前时间，实际 %v", got.At)
	}
}

// ★★★ 判据：升级后的**第一次启动**必须确认，不能回滚。
//
// 真机实测（2026-10-26）踩到的缺陷：标记在替换**之前**写，所以升级后第一次
// 启动必然看到它。原实现"见标记就回滚"⇒ 每次升级都被立刻撤销：
//
//	-apply 报"✅ 升级完成"
//	重启后日志：ROLLED BACK kernel 1.4.0dev → 1.4.0dev（上一次升级未能确认启动）
//
// 升级因此永远不可能生效。正确语义是比对版本：
// 当前跑的 == 标记里的 To ⇒ 升级已生效 ⇒ 确认并清标记。
func TestRollback_ConfirmsWhenRunningTargetVersion(t *testing.T) {
	dir := t.TempDir()
	bin := writeVersionScript(t, dir, "homed", "1.4.1", "newcommit")

	backupDir := filepath.Join(dir, "backup")
	os.MkdirAll(backupDir, 0755)
	os.WriteFile(filepath.Join(backupDir, "homed"), []byte("OLD-GOOD"), 0755)

	MarkPendingUpgrade(dir, PendingRecord{
		FromVer: "1.4.0", FromCommit: "old",
		ToVer: "1.4.1", ToCommit: "newcommit",
		BackupDir: backupDir,
	})

	rolled, _, _, err := RollbackIfUnconfirmed(dir, bin)
	if err != nil {
		t.Fatalf("确认不应失败: %v", err)
	}
	if rolled {
		t.Fatal("★★★ 已在跑升级后的版本，却被回滚了 —— 升级永远无法生效")
	}
	// 标记必须被清掉，否则下次启动又判一次
	if reced, _ := ReadPendingUpgrade(dir); reced != nil {
		t.Error("确认成功后应清除 pending 标记")
	}
	// 二进制不能被改动
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "1.4.1") {
		t.Error("确认路径不应改写二进制")
	}
}

// ★★★ 判据：当前版本**不是**升级目标 ⇒ 必须回滚（升级没生效/被回退）。
func TestRollback_RollsBackWhenRunningOldVersion(t *testing.T) {
	dir := t.TempDir()
	bin := writeVersionScript(t, dir, "homed", "1.4.0", "oldcommit")

	backupDir := filepath.Join(dir, "backup")
	os.MkdirAll(backupDir, 0755)
	os.WriteFile(filepath.Join(backupDir, "homed"), []byte("OLD-GOOD"), 0755)

	MarkPendingUpgrade(dir, PendingRecord{
		FromVer: "1.4.0", FromCommit: "oldcommit",
		ToVer: "1.4.1", ToCommit: "newcommit",
		BackupDir: backupDir,
	})

	rolled, from, to, err := RollbackIfUnconfirmed(dir, bin)
	if err != nil {
		t.Fatalf("回滚不应失败: %v", err)
	}
	if !rolled {
		t.Fatal("跑的还是旧版本，说明升级没生效，必须回滚")
	}
	if from != "1.4.0" || to != "1.4.1" {
		t.Errorf("版本轨迹不对: %s → %s", from, to)
	}
}

// writeVersionScript 造一个能被 ReadBinVersion 读出版本的假二进制。
//
// 为什么用脚本而不是随便写字节：ReadBinVersion 靠**执行** `<bin> --version`
// 取版本，写死字节的假文件根本跑不起来（现有用例走的就是"读不到 ⇒ 回滚"
// 分支，所以那些用例测不到"确认"这条路径）。
func writeVersionScript(t *testing.T, dir, name, ver, commit string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	body := "#!/bin/sh\nprintf 'version=" + ver + "\\ncommit=" + commit + "\\n'\n"
	if err := os.WriteFile(p, []byte(body), 0755); err != nil {
		t.Fatalf("写假二进制: %v", err)
	}
	return p
}
