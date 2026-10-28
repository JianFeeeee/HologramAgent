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
