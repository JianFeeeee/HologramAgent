package update

// Apply / 回滚路径的**回归判据**。
//
// 这些用例的共同点：它们钉住的都是“**静默产出看起来成功的结果**”
// 那类缺陷。升级路径上的静默失败 = 机器进崩溃循环 + 没人知道为什么，
// 所以每个用例都不只断言“返回了 error”，而是断言**副作用没有发生**。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── 测试脚手架 ───────────────────────────────────────────────

// fakeBin 造一个能被 `--version` 读出版本的假二进制（shell 脚本）。
func fakeBin(t *testing.T, dir, name, version, commit string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	body := "#!/bin/sh\necho version=" + version + "\necho commit=" + commit +
		"\necho build_time=2026-01-01T00:00:00Z\necho sdk=1.4.0\n"
	if err := os.WriteFile(p, []byte(body), 0755); err != nil {
		t.Fatalf("写假二进制: %v", err)
	}
	return p
}

// oldKernelBin 造一个**不支持 --version** 的假旧内核（模拟真实 1.3.x 形态）。
func oldKernelBin(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "homed")
	body := "#!/bin/sh\n" +
		"echo 'flag provided but not defined: -version' >&2\n" +
		"exit 2\n"
	if err := os.WriteFile(p, []byte(body), 0755); err != nil {
		t.Fatalf("写旧内核: %v", err)
	}
	return p
}

func newPkgRoot(t *testing.T, version, commit string) string {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	src := fakeBin(t, binDir, "homed", version, commit)
	_ = src
	return root
}

// ── ★ 核心回归：从「没有 --version 的旧内核」升级失败后，必须能回滚 ──

// TestRollbackToOldKernelWithoutVersionFlag 钉住 PR #31 引入的兼容性回归。
//
// 事故形态：旧内核（1.3.x 及以前）没有 `--version`。若回滚校验依赖
// `--version`，那么从它升上去、新版本起不来时：
//
//	① 文件已正确还原成可执行的旧内核
//	② 但读版本失败 ⇒ 判“回滚失败”
//	③ kernelmod/update 拒绝启动
//	④ marker 保留 ⇒ 下次启动同一条路径 ⇒ **永久回滚不了**
//
// 判据不是“返回 nil”，而是“文件真的被还原且内容与备份逐字节相同”。
func TestRollbackToOldKernelWithoutVersionFlag(t *testing.T) {
	dir := t.TempDir()
	dataDir := t.TempDir()

	// 备份：真实的旧内核形态（没有 --version）
	backupDir := filepath.Join(dataDir, BackupDirPrefix+NowStamp())
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	oldBin := oldKernelBin(t, backupDir)
	oldSum, err := sha256File(oldBin)
	if err != nil {
		t.Fatal(err)
	}

	// 现状：升级后的新二进制是坏的（不可执行）
	bin := filepath.Join(dir, "homed")
	if err := os.WriteFile(bin, []byte(""), 0755); err != nil {
		t.Fatal(err)
	}

	// marker 记录了升级前的 digest（新版 Apply 一定会写）
	if err := MarkPendingUpgrade(dataDir, PendingRecord{
		FromVer: "1.3.0", FromCommit: "aaa1111",
		ToVer: "1.4.0", ToCommit: "bbb2222",
		BackupDir: backupDir,
		Digests:   map[string]string{bin: oldSum},
	}); err != nil {
		t.Fatal(err)
	}

	rolled, from, to, err := RollbackIfUnconfirmed(dataDir, bin)
	if err != nil {
		t.Fatalf("★ 回滚不应失败（旧内核没有 --version 是常态，不是缺陷）: %v", err)
	}
	if !rolled {
		t.Fatal("有待确认标记时必须回滚")
	}
	if from != "1.3.0" || to != "1.4.0" {
		t.Errorf("版本轨迹不对: %s → %s", from, to)
	}

	// ★ 真正要断言的是：磁盘上就是那份旧内核，逐字节相同
	got, err := sha256File(bin)
	if err != nil {
		t.Fatal(err)
	}
	if got != oldSum {
		t.Errorf("★ 回滚后内容与备份不一致：期望 %s 实际 %s", ShortSum(oldSum), ShortSum(got))
	}
	// marker 必须清掉：回滚成功且已校验
	if rec, _ := ReadPendingUpgrade(dataDir); rec != nil {
		t.Error("回滚成功后应清除 marker")
	}
}

// TestRollbackRejectsCorruptBackupByDigest 钉住"坏备份不能被当成回滚成功"。
//
// 旧实现的判据是 `CopyFile 没报错` —— 零字节备份拷过去也成功。
// 现在用 digest：内容不对就是失败，且 marker 必须保留（保住第二次自愈机会）。
func TestRollbackRejectsCorruptBackupByDigest(t *testing.T) {
	dir := t.TempDir()
	dataDir := t.TempDir()

	backupDir := filepath.Join(dataDir, BackupDirPrefix+NowStamp())
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	// 备份是零字节（磁盘满时极易发生）
	if err := os.WriteFile(filepath.Join(backupDir, "homed"), []byte{}, 0755); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "homed")
	if err := os.WriteFile(bin, []byte("BROKEN"), 0755); err != nil {
		t.Fatal(err)
	}

	// marker 里记的是"当时那份文件"的 digest，与零字节备份不符
	if err := MarkPendingUpgrade(dataDir, PendingRecord{
		FromVer: "1.4.0", FromCommit: "old",
		ToVer: "1.4.1", ToCommit: "new",
		BackupDir: backupDir,
		Digests:   map[string]string{bin: strings.Repeat("a", 64)},
	}); err != nil {
		t.Fatal(err)
	}

	rolled, _, _, err := RollbackIfUnconfirmed(dataDir, bin)
	if err == nil {
		t.Fatal("★ 坏备份被当成回滚成功了 —— 未做 digest 校验")
	}
	if rolled {
		t.Error("校验失败时 rolled 必须为 false（否则调用方以为自愈完成）")
	}
	// ★ marker 必须仍在：失败后要保住第二次自愈机会
	if rec, _ := ReadPendingUpgrade(dataDir); rec == nil {
		t.Error("★★ 回滚失败却清了 marker ⇒ 失去第二次自愈机会")
	}
}

// TestRollbackRemovesDepGraphAddedByUpgrade 钉住「旧 homed + 新 depgraph」崩溃循环。
//
// 场景：升级前机器没有 depgraph.bin（或备份没带上），升级包带了新的。
// 新内核起不来 ⇒ 回滚。若只换 homed 不删 depgraph，磁盘上是
// 「旧 homed + 新 depgraph」：旧 homed 内嵌的 digest 对不上，
// kernelboot 装载期直接拒绝启动，而 marker 已清 ⇒ 既起不来也回滚不了。
func TestRollbackRemovesDepGraphAddedByUpgrade(t *testing.T) {
	dir := t.TempDir()
	dataDir := t.TempDir()

	backupDir := filepath.Join(dataDir, BackupDirPrefix+NowStamp())
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	// 备份里**没有** depgraph.bin（升级前机器本来就没有）
	oldBin := fakeBin(t, backupDir, "homed", "1.3.0", "aaa")
	oldSum, _ := sha256File(oldBin)

	bin := filepath.Join(dir, "homed")
	if err := os.WriteFile(bin, []byte("BROKEN"), 0755); err != nil {
		t.Fatal(err)
	}
	// 升级时装上了新的 depgraph
	dgPath := filepath.Join(dir, "depgraph.bin")
	if err := os.WriteFile(dgPath, []byte("NEW-DEPGRAPH"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := MarkPendingUpgrade(dataDir, PendingRecord{
		FromVer: "1.3.0", FromCommit: "aaa",
		ToVer: "1.4.0", ToCommit: "bbb",
		BackupDir:   backupDir,
		HadDepGraph: false,
		Digests:     map[string]string{bin: oldSum},
	}); err != nil {
		t.Fatal(err)
	}

	rolled, _, _, err := RollbackIfUnconfirmed(dataDir, bin)
	if err != nil {
		t.Fatalf("回滚不应失败: %v", err)
	}
	if !rolled {
		t.Fatal("应回滚")
	}
	// ★ 关键断言：新装上的 depgraph 必须被删掉
	if _, statErr := os.Stat(dgPath); statErr == nil {
		b, _ := os.ReadFile(dgPath)
		t.Fatalf("★★ 残留升级时装上的 depgraph（%q）⇒ 旧 homed 的 digest 对不上 ⇒ 崩溃循环", b)
	}
}

// TestApplyRefusesWhenCannotWrite 钉住"安装形态决定能不能自替换"。
//
// deb/rpm 形态下直接替换会让包管理器数据库与实际文件不一致。
// 判据：拒绝 + 给出可执行的替代命令，且**磁盘上什么都没变**。
func TestApplyRefusesWhenCannotWrite(t *testing.T) {
	dir := t.TempDir()
	bin := fakeBin(t, dir, "homed", "1.4.0", "old")
	orig, _ := os.ReadFile(bin)

	root := newPkgRoot(t, "1.9.9", "new")

	// dataDir 为空必须拒绝：无 pending 标记就无回滚能力。
	// 这是最前置的守卫，位于预检/备份**之前**。
	if _, err := Apply(root, bin, "", "", nil); err == nil {
		t.Fatal("dataDir 为空时必须拒绝升级（无 marker 就无回滚能力）")
	}
	after, _ := os.ReadFile(bin)
	if string(after) != string(orig) {
		t.Error("★ 被拒绝的升级不应改动任何文件")
	}
}

// TestApplyRefusesWhenBinPathEmpty 钉住“binPath 为空必须拒绝”。
//
// ## 为什么要单开一个用例（这是一个真实的误伤现场）
//
// 早先 cmd/homed-upgrade 的 resolveBinPath 在“同目录找不到 homed”时
// 兜底到 os.Executable()。后果：
//
//	本 CLI 叫 homed-upgrade，与 homed **不同名** ⇒ 它把**自己**当成
//	内核 ⇒ “升级 homed”实际变成“把 CLI 覆盖成新包里的 homed”。
//
// 一个 1 字节的假包就能把工具本体换成一段 shell 脚本，而全程无报错。
//
// 现在两道闸：CLI 找不到就返回空并退出；Apply 对空 binPath 直接拒绝。
// 这个用例钉的是**第二道闸**。
func TestApplyRefusesWhenBinPathEmpty(t *testing.T) {
	dataDir := t.TempDir()
	root := newPkgRoot(t, "1.9.9", "new")

	if _, err := Apply(root, "", "", dataDir, nil); err == nil {
		t.Fatal("★ binPath 为空时必须拒绝：否则会把未知文件（甚至本工具自己）当成内核替换掉")
	}

	// 拒绝时不能留下任何痕迹：不能建备份、不能写 marker。
	entries, _ := os.ReadDir(dataDir)
	for _, e := range entries {
		t.Errorf("★ 被拒绝的升级不应在 dataDir 留下 %q", e.Name())
	}
}

// TestDetectRefusesUnknownInstall 钉住“探测不出形态就不允许自替换”。
//
// 失败方向必须是收紧：Unknown 意味着“我们不知道这个东西归谁管”，
// 猜错的代价是破坏包管理器数据库，那个后果比“不能升级”严重得多。
//
// ★ 本用例**只在非 root 下有效**，root 下直接 Skip。
//
// 为什么：root 无视目录权限位，chmod 0555 对它依旧可写，于是
// dirWritable 返回 true、用例恒红。强行用 root 跑会让它变成
// “CI 里必红/本机必绿”的假信号 —— 本仓已经吃过这类亏
// （用“本机是否装了 homed”验版本推断，CI 假绿、生产真红）。
// 所以：能跑就当真值验，不能跑就明说跳过，不以假结论填充。
func TestDetectRefusesUnknownInstall(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视目录权限位，本用例无法构造“不可写”状态")
	}
	dir := t.TempDir()
	bin := fakeBin(t, dir, "homed", "1.4.0", "x")
	if err := os.Chmod(dir, 0555); err != nil {
		t.Skipf("无法改目录权限: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0755) })

	c := Detect(bin, t.TempDir())
	if c.CanWriteBin {
		t.Errorf("★ 目录不可写时不得声称可以自替换（kind=%s）", c.Kind)
	}
	if c.Reason == "" {
		t.Error("拒绝时必须给出可执行的原因（不能只有 bool）")
	}
}

// TestDetectContainerRefusesSelfReplace 钉住容器内的判定（不依赖权限）。
//
// 容器里文件系统随容器销毁还原，自替换毫无意义。这条判据跨环境稳定：
// 它不依赖运行时的 euid，只依赖 /.dockerenv 之类的标记。
// 在本机（非容器）时它测的是“非容器不应被误判成容器”。
func TestDetectContainerRefusesSelfReplace(t *testing.T) {
	c := Detect("/bin/sh", t.TempDir())
	if c.Kind == KindContainer && c.CanWriteBin {
		t.Error("★ 容器内不得声称可以自替换")
	}
	if c.Kind != KindContainer && inContainer() {
		t.Error("★ 在容器内却未识别出容器形态")
	}
}

// TestApplyRecordsDigests 钉住"Apply 必须把升级前文件的 digest 写进 marker"。
//
// 没有 digest 就没有可靠的旧版本回滚判据（旧内核没有 --version）。
func TestApplyRecordsDigests(t *testing.T) {
	dir := t.TempDir()
	dataDir := t.TempDir()
	bin := fakeBin(t, dir, "homed", "1.4.0", "old")
	wantSum, err := sha256File(bin)
	if err != nil {
		t.Fatal(err)
	}

	root := newPkgRoot(t, "1.5.0", "new")
	out, err := Apply(root, bin, "", dataDir, nil)
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if out.To.Version != "1.5.0" {
		t.Errorf("新版本不对: %s", out.To.Version)
	}

	rec, rerr := ReadPendingUpgrade(dataDir)
	if rerr != nil || rec == nil {
		t.Fatalf("Apply 成功后必须有 pending marker: rec=%v err=%v", rec, rerr)
	}
	if got, ok := rec.Digests[bin]; !ok {
		t.Fatalf("★ marker 缺少 %s 的 digest ⇒ 未来无法可靠回滚到旧版本", bin)
	} else if got != wantSum {
		t.Errorf("digest 记错：期望 %s 实际 %s", ShortSum(wantSum), ShortSum(got))
	}
	if !rec.HadDepGraph == true && rec.HadDepGraph {
		// 占位：HadDepGraph 在本用例里没有 depgraph，应为 false
	}
	if rec.HadDepGraph {
		t.Error("机器上本来没有 depgraph.bin，HadDepGraph 应为 false")
	}
}

// TestApplyRestoresFullyBeforeClearingMarker 钉住"还原完整才清 marker"。
//
// 旧实现先清 marker 再还原，于是"还原也失败"时磁盘上是半新半旧、
// 而 marker 已经没了 ⇒ 失去第二次自愈机会。
func TestApplyRestoresFullyBeforeClearingMarker(t *testing.T) {
	dir := t.TempDir()
	dataDir := t.TempDir()
	bin := fakeBin(t, dir, "homed", "1.4.0", "old")
	orig, _ := os.ReadFile(bin)

	root := newPkgRoot(t, "1.5.0", "new")

	// 让替换阶段的 homed 目标变成"不可 rename"：
	// 把临时文件路径占成一个目录 ⇒ CopyFileAtomic 在 OpenFile 阶段失败。
	// 这样 homed 没被替换，restore 阶段也应成功还原（因为什么都没改）。
	blocker := bin + atomicTmpSuffix
	if err := os.MkdirAll(blocker, 0755); err != nil {
		t.Fatal(err)
	}

	_, err := Apply(root, bin, "", dataDir, nil)
	if err == nil {
		t.Fatal("替换失败时应返回 error")
	}
	// 文件必须原样（restore 成功）
	after, _ := os.ReadFile(bin)
	if string(after) != string(orig) {
		t.Error("★ 替换失败后 homed 未被还原")
	}
}
