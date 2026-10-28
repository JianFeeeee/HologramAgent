package update

// 内核 Apply 的四条高危缺陷回归测试（H1–H4）。
//
// 这四条的共同点：**失败时不是报错，而是静默产出"看起来成功了"的升级**。
// 而升级路径上的静默失败 = 机器进崩溃循环 + 没人知道为什么。所以每个用例
// 都刻意去断言"副作用没有发生"，而不只是断言"返回了 error"。

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/events"
)

// ── 测试辅助 ────────────────────────────────────────────────

// fakePackageBin 造一个"能被 ReadBinVersion 读出版本"的假二进制。
//
// ReadBinVersion 会真的去 exec 它并解析 `key=value` 行，所以这里写一个
// 最小可执行的 shell 脚本：预检（H2 之后的备份路径检查）能照常通过。
func fakePackageBin(t *testing.T, version, commit string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "homed")
	script := "#!/bin/sh\n" +
		"echo version=" + version + "\n" +
		"echo commit=" + commit + "\n" +
		"echo build_time=2026-01-01T00:00:00Z\n" +
		"echo sdk=1.4.0\n"
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatalf("写假二进制: %v", err)
	}
	return p
}

// newPkgRoot 造一个"解包后"的包目录（root 下含 bin/homed）。
//
// Apply 用 FindFile(root, "homed") 找新二进制，所以目录结构要一致。
func newPkgRoot(t *testing.T, version, commit string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	bin := fakePackageBin(t, version, commit)
	if err := CopyFile(bin, filepath.Join(root, "bin", "homed")); err != nil {
		t.Fatal(err)
	}
	return root
}

// installFakeKernel 在临时目录里造一个"现役内核"（bin/homed + depgraph.bin）。
func installFakeKernel(t *testing.T) (binPath, dgPath string) {
	t.Helper()
	dir := t.TempDir()
	binPath = filepath.Join(dir, "homed")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho version=1.4.0\necho commit=old\n"), 0755); err != nil {
		t.Fatal(err)
	}
	dgPath = filepath.Join(dir, "depgraph.bin")
	if err := os.WriteFile(dgPath, []byte("OLD-DEPGRAPH"), 0644); err != nil {
		t.Fatal(err)
	}
	return binPath, dgPath
}

// collectResults 订阅 update_result，返回取事件的函数。
func collectResults(t *testing.T, bus *events.Bus) func() []*events.Event {
	t.Helper()
	var got []*events.Event
	bus.Subscribe(events.EventUpdateResult, func(e *events.Event) {
		got = append(got, e)
	})
	return func() []*events.Event { return got }
}

// ── H1：dataDir 为空必须拒绝升级 ────────────────────────────

// TestApplyRefusesEmptyDataDir 钉住"无标记就无回滚 ⇒ 不许升级"。
//
// ★ 断言的重点不是 error 存在，而是**磁盘上什么都没变**：
// 旧代码在 dataDir=="" 时只是跳过写 pending，后面照样替换二进制，
// 于是产出一个"升级成功、但新版本起不来时不会回滚"的状态 ——
// 这正是崩溃循环的成因。所以这里必须逐字节比对替换前后的内容。
func TestApplyRefusesEmptyDataDir(t *testing.T) {
	binPath, dgPath := installFakeKernel(t)
	root := newPkgRoot(t, "1.9.9", "newcommit")

	origBin, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	origDG, err := os.ReadFile(dgPath)
	if err != nil {
		t.Fatal(err)
	}

	// 包内带 depgraph，确保"如果代码继续往下走"它确实会被替换 ——
	// 否则这个用例测不出 depgraph 被改动。
	newDG := filepath.Join(root, "bin", "depgraph.bin")
	if err := os.WriteFile(newDG, []byte("NEW-DEPGRAPH"), 0644); err != nil {
		t.Fatal(err)
	}

	// ★★★ 关键：在**临时目录**里跑，并拍下跑之前的快照。
	//
	// 为什么必须切 CWD：守卫万一被挪到后面（先预检/先备份才判空），
	// filepath.Join("", prefix) 会得到一个**相对路径**，于是备份目录被
	// 建到当前目录下。旧用例只查了"目标二进制没变"，这种 "什么都没写"
	// 的违规完全看不到（变异实测：把守卫挪到预检之后，旧用例依旧绿）。
	workDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(oldWD) })

	out, err := Apply(root, binPath, newDG, "", nil)
	if err == nil {
		t.Fatalf("dataDir 为空时必须拒绝升级，实际返回 out=%+v, err=nil", out)
	}
	if out != nil {
		t.Errorf("拒绝升级时不应返回 Outcome，实际 %+v", out)
	}
	if !strings.Contains(err.Error(), "dataDir 为空") {
		t.Errorf("错误文案应明确指出 dataDir 为空，实际: %v", err)
	}

	// ① 目标二进制**逐字节未变**
	gotBin, _ := os.ReadFile(binPath)
	if string(gotBin) != string(origBin) {
		t.Errorf("拒绝升级却改动了二进制:\n  期望 %q\n  实际 %q", origBin, gotBin)
	}
	// ② depgraph 也未被替换（同批替换的另一半）
	gotDG, _ := os.ReadFile(dgPath)
	if string(gotDG) != string(origDG) {
		t.Errorf("拒绝升级却改动了 depgraph.bin:\n  期望 %q\n  实际 %q", origDG, gotDG)
	}
	// ③ 连临时文件都没留下（CopyFile 的 .tmp-upgrade）
	if FileExists(binPath + ".tmp-upgrade") {
		t.Error("拒绝升级却留下了 .tmp-upgrade 临时文件")
	}
	// ④ ★ 根本没有写任何东西：备份目录/pending 标记都不许出现。
	// "不写任何文件"是 H1 的原始承诺（拒绝要在**预检之前**发生）。
	// 备份目录会因为 filepath.Join("", …) 而变成相对路径 —— 那种情况下
	// 它会在 CWD 里，所以这里必须盯住 CWD。
	entries, derr := os.ReadDir(workDir)
	if derr != nil {
		t.Fatal(derr)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("拒绝升级却写了文件（dataDir 为空时的守卫必须发生在动任何文件之前）: %v", names)
	}
}

// ── H2：备份目录必须落在 dataDir 下，不能是 /root ────────────

// TestBackupDirUnderDataDir 钉住备份落点。
//
// 旧代码写死 /root/homed-upgrade-backup-<ts>：服务以
// ProtectSystem=strict + ReadWritePaths=/var/lib/hologram 运行，
// 非 root 身份对 /root 没有写权限 ⇒ MkdirAll 必 EACCES ⇒ **首次升级必挂**。
//
// 所以本条既断言"前缀是 dataDir"，也断言 dataDir 下真的出现了备份目录
// 且里面有两个文件。同时反证：/root 下不该被创建（当 dataDir != /root 时）。
func TestBackupDirUnderDataDir(t *testing.T) {
	binPath, _ := installFakeKernel(t)
	root := newPkgRoot(t, "1.9.9", "newcommit")
	dataDir := t.TempDir()

	out, err := Apply(root, binPath, "", dataDir, nil)
	if err != nil {
		t.Fatalf("Apply 应成功: %v", err)
	}
	if out == nil || out.BackupDir == "" {
		t.Fatal("Outcome.BackupDir 不应为空")
	}

	// ① 路径前缀必须是 dataDir，而不是 /root
	if !strings.HasPrefix(out.BackupDir, dataDir+string(os.PathSeparator)) {
		t.Errorf("备份目录应在 dataDir 下: dataDir=%s, BackupDir=%s", dataDir, out.BackupDir)
	}
	if strings.HasPrefix(out.BackupDir, "/root") {
		t.Errorf("备份目录不能落在 /root（服务无写权限，首次升级必挂）: %s", out.BackupDir)
	}
	// ② 名字前缀不变（GC 按前缀识别，见 gc.go）
	if !strings.HasPrefix(filepath.Base(out.BackupDir), BackupDirPrefix) {
		t.Errorf("备份目录名应以 %q 开头，实际 %s", BackupDirPrefix, filepath.Base(out.BackupDir))
	}
	// ③ 备份真的写进去了（homed + depgraph.bin 都存在）
	if !FileExists(filepath.Join(out.BackupDir, "homed")) {
		t.Error("备份目录里应有 homed")
	}
	if !FileExists(filepath.Join(out.BackupDir, "depgraph.bin")) {
		t.Error("备份目录里应有 depgraph.bin（与 homed 同批）")
	}
	// ④ pending 标记落在 dataDir 下（备份路径与标记同源，才能被回滚读到）
	if !FileExists(pendingPath(dataDir)) {
		t.Errorf("pending 标记应在 dataDir 下: %s", pendingPath(dataDir))
	}
	rec, _ := ReadPendingUpgrade(dataDir)
	if rec == nil {
		t.Fatal("应能读回 pending 标记")
	}
	if rec.BackupDir != out.BackupDir {
		t.Errorf("标记里的 BackupDir 应指向本次备份: 标记=%s, Outcome=%s", rec.BackupDir, out.BackupDir)
	}
}

// ── H3：备份目录要有 GC ────────────────────────────────────

// TestPruneOldBackups 钉住保留策略：upgrade 备 3 个，broken 备 1 个。
//
// ★ "剩下的是最新的"这一条同样重要：若 GC 删错方向（留下最旧的），
// 回滚源就会指向早已被替换过多次的版本，回滚等于装回一个更老的版本。
//
// 排序依据是目录名里的定长时间戳（NowStamp 的 20060102-150405），
// 所以这里不用 os.Chtimes 造 mtime，而是直接按名字造 —— 与实现约定一致，
// 也避免文件系统时间戳精度/时区差异让测试变成 flaky。
func TestPruneOldBackups(t *testing.T) {
	dataDir := t.TempDir()

	// 5 个备份（升序时间戳：第 i 个越新）
	backupTS := []string{
		"20260101-010101", "20260102-010101", "20260103-010101",
		"20260104-010101", "20260105-010101",
	}
	for _, ts := range backupTS {
		d := filepath.Join(dataDir, BackupDirPrefix+ts)
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "homed"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// 2 个坏二进制现场
	brokenTS := []string{"20260101-020202", "20260102-020202"}
	for _, ts := range brokenTS {
		if err := os.MkdirAll(BrokenDirPath(dataDir, ts), 0755); err != nil {
			t.Fatal(err)
		}
	}
	// 干扰项：不该被删（pending 标记）与不该被匹配（前缀相近但不是）
	marker := pendingPath(dataDir)
	if err := os.WriteFile(marker, []byte(`{"from_version":"1.4.0"}`), 0644); err != nil {
		t.Fatal(err)
	}
	notOurs := filepath.Join(dataDir, BackupDirPrefix+"not-a-timestamp")
	if err := os.MkdirAll(notOurs, 0755); err != nil {
		t.Fatal(err)
	}
	// 一个普通文件（不是目录）：GC 只处理目录
	strayFile := filepath.Join(dataDir, BackupDirPrefix+"20260109-010101")
	if err := os.WriteFile(strayFile, []byte("i am a file"), 0644); err != nil {
		t.Fatal(err)
	}

	PruneOldBackups(dataDir)

	// ① 备份剩 3 个，且是最新的三个
	gotBackups := listStampDirs(t, dataDir, BackupDirPrefix)
	wantBackups := []string{
		BackupDirPrefix + "20260103-010101",
		BackupDirPrefix + "20260104-010101",
		BackupDirPrefix + "20260105-010101",
	}
	sort.Strings(gotBackups)
	sort.Strings(wantBackups)
	if len(gotBackups) != 3 {
		t.Fatalf("应保留 3 个备份，实际 %d 个: %v", len(gotBackups), gotBackups)
	}
	for i := range wantBackups {
		if gotBackups[i] != wantBackups[i] {
			t.Errorf("保留的应是最新的 3 个备份\n  期望 %v\n  实际 %v", wantBackups, gotBackups)
			break
		}
	}

	// ② broken 剩 1 个，且是最新的那个
	gotBroken := listStampDirs(t, dataDir, BrokenDirPrefix)
	if len(gotBroken) != 1 {
		t.Fatalf("应保留 1 个 homed-broken-*，实际 %d 个: %v", len(gotBroken), gotBroken)
	}
	if gotBroken[0] != BrokenDirPrefix+"20260102-020202" {
		t.Errorf("保留的应是最新的 broken 目录，实际 %s", gotBroken[0])
	}

	// ③ 没被误删：pending 标记、前缀相近的目录、以及那个同名普通文件
	if !FileExists(marker) {
		t.Error("GC 不能删 pending 标记（删了就等于丢掉回滚能力）")
	}
	if !FileExists(notOurs) {
		t.Error("GC 不能删名字前缀相近但不是我们自己造的目录")
	}
	if !FileExists(strayFile) {
		t.Error("GC 只处理目录，不该删同名文件")
	}

	// ④ 幂等：再跑一次不应有变化
	PruneOldBackups(dataDir)
	if n := len(listStampDirs(t, dataDir, BackupDirPrefix)); n != 3 {
		t.Errorf("GC 应幂等，第二次跑后剩余 %d 个", n)
	}

	// ⑤ dataDir 为空时**什么都不做**（尤其不能把空串当 CWD 去删）
	if before := len(listStampDirs(t, dataDir, BackupDirPrefix)); before != 3 {
		t.Fatalf("前置条件被破坏: %d", before)
	}
	PruneOldBackups("")
	if after := len(listStampDirs(t, dataDir, BackupDirPrefix)); after != 3 {
		t.Errorf("dataDir 为空时不应改动任何东西（%d → %d）", 3, after)
	}
}

// listStampDirs 列出 dir 下名字为 "<prefix><ts>" 形态的子目录。
//
// ★ 必须按时间戳形态过滤，不能用"前缀匹配"代替：GC 本身也按形态
// 过滤（不删名字碰巧带同样前缀的东西），用前缀匹配的辅助函数会把
// 那些干扰项算进数量，测出来的就不是真实保留个数。
func listStampDirs(t *testing.T, dir, prefix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if isBackupStamp(e.Name()[len(prefix):]) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// ── H4：结果事件必须带 error，成功也必须发 result ────────────

// TestPublishResultCarriesError 钉住"失败原因必须能被订阅方读到"。
//
// 旧代码把 errMsg 收进 `if errMsg != "" { _ = errMsg }` 直接丢掉，
// 于是订阅方只看到 ok=false，**看不到为什么失败** —— 而失败原因正是
// 用户唯一想知道的东西（"磁盘满"和"二进制不兼容"要采取的动作完全不同）。
func TestPublishResultCarriesError(t *testing.T) {
	bus := events.NewBus()
	results := collectResults(t, bus)

	publishResult(bus, "kernel", "", false, "1.4.0", "1.4.1", true, "替换 homed: 权限不足")

	got := results()
	if len(got) != 1 {
		t.Fatalf("应发出 1 个 update_result 事件，实际 %d 个", len(got))
	}
	payload := got[0].Payload
	if payload["ok"] != false {
		t.Errorf("ok 应为 false，实际 %v", payload["ok"])
	}
	errVal, exists := payload["error"]
	if !exists {
		t.Fatal("payload 必须带 error 字段（旧代码在这里把它丢掉了）")
	}
	msg, _ := errVal.(string)
	if strings.TrimSpace(msg) == "" {
		t.Fatal("失败时 payload[error] 不能为空 —— 订阅方靠它决定怎么处置")
	}
	if !strings.Contains(msg, "权限不足") {
		t.Errorf("payload[error] 应保留原始失败原因，实际 %q", msg)
	}
	if payload["rolled_back"] != true {
		t.Errorf("rolled_back 应为 true，实际 %v", payload["rolled_back"])
	}
}

// TestApplySuccessEmitsResult 钉住"成功路径也发 result"。
//
// 旧代码只有失败路径调 publishResult，成功路径什么都不发 ⇒ 订阅方只能靠
// "进度停在最后一步且没看到 failed"来猜成功，猜错的方向是"以为还在升级"，
// 而紧接着进程就重启消失了 —— 用户永远等不到那个结果。
func TestApplySuccessEmitsResult(t *testing.T) {
	binPath, _ := installFakeKernel(t)
	root := newPkgRoot(t, "1.9.9", "newcommit")
	dataDir := t.TempDir()

	bus := events.NewBus()
	results := collectResults(t, bus)

	out, err := Apply(root, binPath, "", dataDir, bus)
	if err != nil {
		t.Fatalf("Apply 应成功: %v", err)
	}

	got := results()
	if len(got) != 1 {
		t.Fatalf("成功路径必须发出 1 个 update_result 事件，实际 %d 个", len(got))
	}
	payload := got[0].Payload
	if payload["ok"] != true {
		t.Errorf("ok 应为 true，实际 %v", payload["ok"])
	}
	if payload["rolled_back"] != false {
		t.Errorf("成功时 rolled_back 应为 false，实际 %v", payload["rolled_back"])
	}
	if payload["to"] != out.To.Version {
		t.Errorf("payload[to] 应为新版本 %s，实际 %v", out.To.Version, payload["to"])
	}
	// 成功时 error 字段存在且为空：订阅方无需做"字段有没有"的兼容分支
	if v, exists := payload["error"]; exists {
		if s, _ := v.(string); s != "" {
			t.Errorf("成功时 error 应为空串，实际 %q", s)
		}
	}
}

// ── M6：Apply 必须**真的调用** PruneOldBackups（行为级，不是"函数能跑"）──

// TestApplyPrunesOldBackups 钉住"Apply 成功路径真的执行了 GC"。
//
// ★ 这一条是补 M6 真空洞的。此前只有 TestPruneOldBackups，它直接调
// PruneOldBackups —— 把 Apply 里那一行 `PruneOldBackups(dataDir)` 整个删掉，
// 它照样绿。也就是说旧判据只证明了"函数能跑"，**没证明 Apply 调了它**，
// 而"没人调用"恰恰是 H3 的原始缺陷形态（注释承诺了 GC，代码里没有）。
//
// 做法：预置超量备份（含一个很旧的），跑一次真实 Apply，断言旧的被清掉。
// 这条路只有 Apply 内部真调了 GC 才会绿。
func TestApplyPrunesOldBackups(t *testing.T) {
	binPath, _ := installFakeKernel(t)
	root := newPkgRoot(t, "1.9.9", "newcommit")
	dataDir := t.TempDir()

	// 预置 5 个"历史备份"，最旧的那个应该在本次 Apply 后被 GC 掉
	oldTS := []string{
		"20200101-010101", "20200102-010101", "20200103-010101",
		"20200104-010101", "20200105-010101",
	}
	for _, ts := range oldTS {
		if err := os.MkdirAll(filepath.Join(dataDir, BackupDirPrefix+ts), 0755); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Apply(root, binPath, "", dataDir, nil); err != nil {
		t.Fatalf("Apply 应成功: %v", err)
	}

	// 5 个旧的 + 本次新增 1 个 = 6 个；保留最近 3 个 ⇒ 最旧的 3 个必须没了。
	remaining := listStampDirs(t, dataDir, BackupDirPrefix)
	if len(remaining) != 3 {
		t.Fatalf("Apply 后应只剩 3 个备份（GC 未被调用？），实际 %d 个: %v", len(remaining), remaining)
	}
	for _, ts := range oldTS[:3] {
		if FileExists(filepath.Join(dataDir, BackupDirPrefix+ts)) {
			t.Errorf("Apply 后最旧的备份 %s 应已被 GC 清除", ts)
		}
	}
}

// ── B1：回滚必须回读校验，校验不过不得清 marker ─────────────

// TestRollback_RefusesCorruptBackup 钉住"拷贝成功 ≠ 回滚成功"。
//
// ★ 这是已复现过的事故形态：备份是**零字节**文件时，CopyFile 会成功
// （字节确实搬完了），旧代码于是清掉 marker、返回 rolled=true, err=nil，
// 日志还打 "rolled back to 1.4.0"。而磁盘上躺着的是一个不可执行的
// 二进制 ⇒ 机器带坏二进制继续启动，且 marker 已清 ⇒ **失去第二次自愈机会**。
//
// 正确行为：回读 ReadBinVersion 失败 ⇒ 返回 error、rolled=false、
// **marker 必须还在**（下一次启动还能再试 / 人能介入）。
func TestRollback_RefusesCorruptBackup(t *testing.T) {
	dir := t.TempDir()
	// 当前跑的是 1.4.0，与标记的 ToVer(1.4.1) 不同 ⇒ 进入回滚分支
	bin := writeVersionScript(t, dir, "homed", "1.4.0", "oldcommit")
	orig, _ := os.ReadFile(bin)

	backupDir := filepath.Join(dir, "backup")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	// 零字节的坏备份：CopyFile 会"成功"
	if err := os.WriteFile(filepath.Join(backupDir, "homed"), []byte{}, 0755); err != nil {
		t.Fatal(err)
	}

	if err := MarkPendingUpgrade(dir, PendingRecord{
		FromVer: "1.4.0", FromCommit: "oldcommit",
		ToVer: "1.4.1", ToCommit: "newcommit",
		BackupDir: backupDir,
	}); err != nil {
		t.Fatal(err)
	}

	rolled, _, _, err := RollbackIfUnconfirmed(dir, bin)
	if err == nil {
		t.Fatal("★★ 拿不可执行的备份\"回滚成功\"了 —— 未做回读校验")
	}
	if rolled {
		t.Error("回滚校验失败时 rolled 必须为 false（否则调用方以为自愈完成）")
	}
	// ★ marker 必须仍在：失败后要保住第二次自愈机会
	if rec, _ := ReadPendingUpgrade(dir); rec == nil {
		t.Error("★★ 回滚失败却清了 marker ⇒ 失去第二次自愈机会")
	}
	// 错误必须可执行：指出是回读失败 + 让人去换二进制
	msg := err.Error()
	if !strings.Contains(msg, "回读") && !strings.Contains(msg, "不可执行") {
		t.Errorf("错误应说明回读校验失败，实际: %v", msg)
	}
	_ = orig
}

// TestRollback_RefusesVersionMismatch 钉住"版本非得是标记里的 FromVer"。
//
// 备份存在且可执行，但版本不是当初记录的 FromVer（备份与 marker 不匹配，
// 例如备份目录被别的升级覆盖过）。这种情况不算回滚成功 —— 装回去的是
// 一个谁也不认识的版本，而 marker 里承诺的是 FromVer。
func TestRollback_RefusesVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	bin := writeVersionScript(t, dir, "homed", "1.4.0", "oldcommit")

	backupDir := filepath.Join(dir, "backup")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	// 备份是**可执行**的，但版本是 1.3.0（≠ marker.FromVer 1.4.0）
	src := writeVersionScript(t, t.TempDir(), "homed", "1.3.0", "weird")
	if err := CopyFile(src, filepath.Join(backupDir, "homed")); err != nil {
		t.Fatal(err)
	}

	if err := MarkPendingUpgrade(dir, PendingRecord{
		FromVer: "1.4.0", FromCommit: "oldcommit",
		ToVer: "1.4.1", ToCommit: "newcommit",
		BackupDir: backupDir,
	}); err != nil {
		t.Fatal(err)
	}

	rolled, _, _, err := RollbackIfUnconfirmed(dir, bin)
	if err == nil {
		t.Fatal("版本与标记的 FromVer 不符时必须报错")
	}
	if rolled {
		t.Error("版本不符时 rolled 必须为 false")
	}
	if !strings.Contains(err.Error(), "1.3.0") {
		t.Errorf("错误应报出实际版本，实际: %v", err)
	}
	if rec, _ := ReadPendingUpgrade(dir); rec == nil {
		t.Error("版本不符时 marker 必须保留")
	}
}

// TestRollback_CorruptMarkerDoesNotBlockBoot 钉住 B2 的第二半：
// marker 损坏（掉电截断）**不得**让内核拒绝启动。
//
// ★ 已复现的事故形态：marker 写成 `{"from_version":"1.4.0","to_` 时，
// ReadPendingUpgrade 返回 error，旧代码原样上抛，mod.go `return rerr`
// ⇒ 内核拒启动，而且走不到回滚分支 ⇒ **既起不来也回滚不了**。
//
// 正确行为：返回 (false, "", "", nil) —— 当作"无待确认升级"继续启动，
// 同时把坏 marker 改名留存（不删，保留现场）。
func TestRollback_CorruptMarkerDoesNotBlockBoot(t *testing.T) {
	dir := t.TempDir()
	bin := writeVersionScript(t, dir, "homed", "1.4.0", "oldcommit")

	p := pendingPath(dir)
	bad := []byte(`{"from_version":"1.4.0","to_`)
	if err := os.WriteFile(p, bad, 0644); err != nil {
		t.Fatal(err)
	}

	rolled, from, to, err := RollbackIfUnconfirmed(dir, bin)
	if err != nil {
		t.Fatalf("★★ marker 损坏不得让内核拒启动（既起不来也回滚不了）: %v", err)
	}
	if rolled {
		t.Error("marker 读不懂时无法判定回滚目标，rolled 应为 false")
	}
	if from != "" || to != "" {
		t.Errorf("无回滚时版本轨迹应为空，实际 %s → %s", from, to)
	}
	// 坏 marker 应被改名留存（保留现场），而不是原样留着（否则下次启动又报一次）
	if _, err := os.Stat(p); err == nil {
		t.Error("损坏的 marker 应被隔离改名，否则每次启动都会再撞一次")
	}
	entries, _ := os.ReadDir(dir)
	foundQuarantine := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			foundQuarantine = true
		}
	}
	if !foundQuarantine {
		t.Error("损坏的 marker 应被改名留存（排查\"上次升级发生了什么\"的唯一线索）")
	}
}

// TestMarkPendingUpgrade_IsAtomic 钉住 B2 的第一半：写 marker 不留半截。
//
// 掉电时最坏的结果必须是"完整的旧版"或"完整的新版"，不能是半截 JSON。
// 判据：写完不留下 .tmp 残留，且内容可被完整解析。
// （真正的掉电无法在单测里模拟，这里钉住原子写带来的可观测后果。）
func TestMarkPendingUpgrade_IsAtomic(t *testing.T) {
	dir := t.TempDir()
	rec := PendingRecord{FromVer: "1.4.0", FromCommit: "a", ToVer: "1.4.1", ToCommit: "b"}
	if err := MarkPendingUpgrade(dir, rec); err != nil {
		t.Fatalf("写标记: %v", err)
	}
	// ① 不留 .tmp 残留（rename 成功）
	if FileExists(pendingPath(dir) + ".tmp") {
		t.Error("写完后不应残留 .tmp 临时文件")
	}
	// ② 内容完整可解析
	got, err := ReadPendingUpgrade(dir)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if got == nil || got.FromVer != "1.4.0" || got.ToVer != "1.4.1" {
		t.Errorf("读回内容不对: %+v", got)
	}
	// ③ 覆写已有 marker 也要成功（旧版 WriteFile 截断写，这里同样不能坏）
	if err := MarkPendingUpgrade(dir, PendingRecord{FromVer: "2.0.0", ToVer: "2.1.0"}); err != nil {
		t.Fatalf("覆写标记: %v", err)
	}
	got2, err := ReadPendingUpgrade(dir)
	if err != nil {
		t.Fatalf("覆写后读回: %v", err)
	}
	if got2.FromVer != "2.0.0" {
		t.Errorf("覆写未生效: %+v", got2)
	}
}

// TestMarkPendingUpgrade_PreservesOldMarkerOnFailure 钉住 B2 的第二半：
// **写失败时不得破坏已有的旧 marker**。
//
// ★ 为什么 IsAtomic 不够（这是实测出来的缺口）：
// TestMarkPendingUpgrade_IsAtomic 断言的是「最终状态对」——不留 .tmp、
// 内容可解析、可覆写。把 MarkPendingUpgrade 整个退回 os.WriteFile
// 这三条**全都满足**，于是那条判据对「原子写」这个真 bug 是哑的。
//
// 真正能区分两者的可观测差异只有一个：**失败路径上旧文件还在不在**。
//
//	· 原子写：新内容写到 .tmp 崩掉 ⇒ 目标文件仍是**完整旧版本**
//	· 原地覆写（os.WriteFile / tmp := p）：O_TRUNC 先清空再写，
//	  中途失败 ⇒ 目标文件被**截断**，旧 marker 永久丢失
//
// 而丢掉旧 marker 的后果正是 B2 要防的那个事故：不知道上次升了什么，
// 也失去回滚依据。所以这里用「把 tmp 路径占成目录」逼 OpenFile 必失败
// （EISDIR，稳定可复现，不依赖权限/磁盘/平台），再断言旧 marker 原样还在。
//
// ★ 变异纪律：把 rollback.go 的 `tmp := p + ".tmp"` 改成 `tmp := p`
// （即退回原地覆写），本用例必红 —— 那时 OpenFile 打开的是 marker 本身，
// O_TRUNC 立刻把它清空，FromVer 变成新值。
func TestMarkPendingUpgrade_PreservesOldMarkerOnFailure(t *testing.T) {
	dir := t.TempDir()

	// ① 先写一个完好的旧 marker（模拟「上次升级已有待确认记录」）。
	old := PendingRecord{FromVer: "1.0.0", FromCommit: "oldcommit", ToVer: "1.1.0", ToCommit: "newok"}
	if err := MarkPendingUpgrade(dir, old); err != nil {
		t.Fatalf("预置旧 marker: %v", err)
	}

	// ② 把临时文件路径占成目录 ⇒ OpenFile(O_CREATE|O_WRONLY) 必 EISDIR。
	// 这样「写新 marker」一定在中途失败，且失败点与磁盘满/掉电同类
	// （已打开目标、还没写完全部内容）。
	tmpPath := pendingPath(dir) + ".tmp"
	if err := os.Mkdir(tmpPath, 0755); err != nil {
		t.Fatalf("占用 tmp 路径: %v", err)
	}

	// ③ 写新的 marker 必须**报错**（不能假装写成功）。
	// 用 Error 而非 Fatal：让下面的 ④ 也跑完，把「旧 marker 被破坏」
	// 这个真正的事故形态一并报出来（两个症状都属于同一个病）。
	if err := MarkPendingUpgrade(dir, PendingRecord{FromVer: "2.0.0", ToVer: "2.1.0"}); err == nil {
		t.Error("tmp 路径不可写时 MarkPendingUpgrade 必须返回 error（否则调用方以为标记已就绪）")
	}

	// ④ 旧 marker 必须**原样保留**（这是 B2 的核心承诺）。
	got, rerr := ReadPendingUpgrade(dir)
	if rerr != nil {
		t.Fatalf("写失败后旧 marker 应仍可解析（不允许留下半截 JSON）: %v", rerr)
	}
	if got == nil {
		t.Fatal("写失败后旧 marker 丢了：没有回滚依据，且不知道上次升了什么")
	}
	if got.FromVer != "1.0.0" {
		t.Errorf("旧 marker 被破坏了：FromVer 应为 1.0.0（旧值），实际 %q —— "+
			"说明写新 marker 时原地覆写了目标文件（原子性丢失）", got.FromVer)
	}
	if got.ToVer != "1.1.0" {
		t.Errorf("旧 marker 被破坏了：ToVer 应为 1.1.0（旧值），实际 %q", got.ToVer)
	}
}

// ── 确认分支不得使用推断值（B1 的同一个病，转移到了 confirmed）──

// TestRollback_ConfirmedBranchIgnoresInferredVersion 钉住"确认新版已跑起来"
// 只能靠**binPath 自己**报版本，不能用推断回退借来的值。
//
// ★ 已实证的事故形态（环境相关，最难查的那类）：
//   - binPath 写成不可执行的残 file
//   - marker.ToVer 恰等于本机可推断到的版本（如 /usr/local/bin/waiter 是 1.4.0）
//   - ReadBinVersion 失败后走 InferVersionFromRunning 回退，从 waiter 借到
//     1.4.0 ⇒ 与 ToVer 相等 ⇒ confirmed=true ⇒ 清 marker、不回滚
//   - 实测输出：rolled=false err=nil，marker 被清除
//     ⇒ 机器带着跑不起来的二进制继续启动，且失去自愈机会。
//
// ★★ 判据自身必须**不依赖本机装没装 homed/waiter**，否则 CI 假绿、生产真红。
// 探针路径（inferWaiterPath/inferHomedPath）是包级变量，这里临时指到
// t.TempDir() 下自己造的假 waiter，跑完还原。这样判据在任何机器上都
// **确实**会走到"能借到版本"的成立条件，而不是靠环境碰运气：
//   - confirmed 用 ReadBinVersion        ⇒ 借到 9.9.9 ⇒ 判据 RED
//   - confirmed 用 readBinVersionStrict  ⇒ 借不到   ⇒ 走回滚 ⇒ 判据 GREEN
func TestRollback_ConfirmedBranchIgnoresInferredVersion(t *testing.T) {
	// 把推断逻辑可读的两个探针都指到可写临时目录（并断言前提成立）。
	savedHomed, savedWaiter := inferHomedPath, inferWaiterPath
	t.Cleanup(func() { inferHomedPath, inferWaiterPath = savedHomed, savedWaiter })

	probe := t.TempDir()
	inferHomedPath = filepath.Join(probe, "homed")   // 不存在：手段 1 必然失效
	inferWaiterPath = filepath.Join(probe, "waiter") // 下面种一个能报版本的
	body := "#!/bin/sh\nprintf 'waiter 9.9.9 (commit deadbeef, built 2026-01-01T00:00:00Z)\\n'\n"
	if err := os.WriteFile(inferWaiterPath, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}

	// ① 前提校验：推断逻辑必须**真的**能从它借到版本。
	// 借不到就说明这条判据无法区分两种实现（会假绿）—— 直接 Fatal 而不是 Skip：
	// 现在探针路径是我们自己控制的，借不到就是判据自身坏了。
	if v := InferVersionFromRunning(); v == nil || v.Version != "9.9.9" {
		t.Fatalf("判据前提不成立：推断逻辑未从假 waiter 借到 9.9.9（得到 %+v）", v)
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "homed")
	// 不可执行的残 file：strict 读出版本必然失败
	if err := os.WriteFile(bin, []byte("THIS-IS-NOT-A-BINARY"), 0755); err != nil {
		t.Fatal(err)
	}

	// 可用的备份（版本 = FromVer），保证"走回滚"这条路能成功，
	// 这样 confirmed 判错的后果只体现在"该不该回滚"上，不被别的失败掩盖。
	backupDir := filepath.Join(dir, "backup")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeVersionScript(t, backupDir, "homed", "1.4.0", "oldcommit")

	// marker.ToVer 恰好等于推断逻辑能借到的版本 9.9.9
	if err := MarkPendingUpgrade(dir, PendingRecord{
		FromVer: "1.4.0", FromCommit: "oldcommit",
		ToVer: "9.9.9", ToCommit: "deadbeef",
		BackupDir: backupDir,
	}); err != nil {
		t.Fatal(err)
	}

	rolled, from, to, err := RollbackIfUnconfirmed(dir, bin)
	if err != nil {
		t.Fatalf("备份可用时回滚不应失败: %v", err)
	}
	// ★ 核心断言：binPath 报不出版本 ⇒ 必须回滚，绝不能因为"借来的版本"而确认
	if !rolled {
		t.Fatalf("★★★ binPath 不可执行，却被判为\"新版已跑起来\"（confirmed 用了推断值）⇒ 机器带坏二进制继续启动")
	}
	if from != "1.4.0" || to != "9.9.9" {
		t.Errorf("版本轨迹不对: %s → %s", from, to)
	}
	// ② 回读（严格）证明 binPath 真的被换成了可执行的 1.4.0
	if v, verr := readBinVersionStrict(bin); verr != nil {
		t.Errorf("回滚后 binPath 应可执行: %v", verr)
	} else if v.Version != "1.4.0" {
		t.Errorf("应回滚到 1.4.0，实际 %s", v.Version)
	}
}

// TestConfirmedBranch_StrictProbeBitesOnUnparseable 是上一条的"无反依赖"版本：
// 直接把探针指向一个**报不出版本**的假 waiter，并断言 confirmed 仍为 false。
//
// 为什么还要单独一条：上一条依赖"能借到版本"这个前提成立。这一条不依赖
// 任何前提，专门钉住"探针都失效时也不能确认"。
func TestConfirmedBranch_StrictProbeBitesOnUnparseable(t *testing.T) {
	savedHomed, savedWaiter := inferHomedPath, inferWaiterPath
	t.Cleanup(func() { inferHomedPath, inferWaiterPath = savedHomed, savedWaiter })

	probe := t.TempDir()
	inferHomedPath = filepath.Join(probe, "homed")
	inferWaiterPath = filepath.Join(probe, "waiter")
	// 能跑但输出里没有语义化版本 ⇒ versionFromWaiter 返回 nil
	if err := os.WriteFile(inferWaiterPath, []byte("#!/bin/sh\necho no-version-here\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if v := InferVersionFromRunning(); v != nil {
		t.Fatalf("判据前提不成立：应推断不出，实际 %+v", v)
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "homed")
	if err := os.WriteFile(bin, []byte("THIS-IS-NOT-A-BINARY"), 0755); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(dir, "backup")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeVersionScript(t, backupDir, "homed", "1.4.0", "oldcommit")
	if err := MarkPendingUpgrade(dir, PendingRecord{
		FromVer: "1.4.0", FromCommit: "oldcommit",
		ToVer: "1.4.0", ToCommit: "oldcommit", // ToVer 恰等于备份版本（最易误判的形态）
		BackupDir: backupDir,
	}); err != nil {
		t.Fatal(err)
	}

	rolled, _, _, err := RollbackIfUnconfirmed(dir, bin)
	if err != nil {
		t.Fatalf("回滚不应失败: %v", err)
	}
	if !rolled {
		t.Error("★★ binPath 不可执行时必须回滚（不能因 ToVer 与备份版本相同就确认）")
	}
}
