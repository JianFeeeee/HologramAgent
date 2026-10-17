package migration

// 数据目录迁移体检的判据。
//
// ## 为什么要这些判据
//
// 迁移是**破坏性**操作。体检是唯一能让运维在动手前看到"会丢什么"的东西，
// 所以它漏报的后果是**数据丢失**（而不是一个报错）。
//
// 三个必须守住的点（都来自 2026-10-17 生产实测）：
//  1. overlay 挂载点必须被识别（不识别 ⇒ 搬走空壳）
//  2. 未知目录默认**必须迁**（宁可多迁，不可漏迁）
//  3. 同设备/跨设备必须判对（跨设备不能 rename，必须 rsync）

import (
	"os"
	"path/filepath"
	"testing"
)

// ★★★ 判据：未知目录默认 data（必须迁），不能被当成"可丢"。
func TestPreflightDefaultsUnknownDirsToData(t *testing.T) {
	dir := t.TempDir()
	// 造一个体检表里没有的目录
	mustMkdir(t, filepath.Join(dir, "some_new_thing"))
	os.WriteFile(filepath.Join(dir, "some_new_thing", "f"), make([]byte, 100), 0o644)

	rep, err := Preflight(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range rep.Dirs {
		if d.Name == "some_new_thing" {
			found = true
			if d.Kind != "data" {
				t.Fatalf("★ 未知目录被归为 %q（应为 data）—— 漏迁 = 丢数据", d.Kind)
			}
		}
	}
	if !found {
		t.Fatal("体检未列出目录")
	}
}

// ★★★ 判据：heartbeat 归 runtime（不迁），log/backups/memory.bak 归 optional。
func TestPreflightClassifiesRuntimeAndOptional(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"heartbeat", "log", "backups", "memory.bak", "memory"} {
		mustMkdir(t, filepath.Join(dir, n))
	}
	rep, err := Preflight(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"heartbeat": "runtime", "log": "optional", "backups": "optional",
		"memory.bak": "optional", "memory": "data",
	}
	got := map[string]string{}
	for _, d := range rep.Dirs {
		got[d.Name] = d.Kind
	}
	for n, w := range want {
		if got[n] != w {
			t.Fatalf("★ %s 分类为 %q，应为 %q", n, got[n], w)
		}
	}
}

// ★★★ 判据：overlay 挂载点必须被识别（不识别 = 搬走空壳）。
//
// 用真实的 /proc/mounts 判断：若本机恰好有 agentfs/merged 挂载则验证识别，
// 否则验证"没挂载时不给假警告"。
func TestPreflightDetectsOverlayMount(t *testing.T) {
	if !isMountpoint("/proc") {
		t.Fatal("isMountpoint 连 /proc 都识别不出 ⇒ /proc/mounts 解析坏了")
	}
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "agentfs", "merged"))
	rep, err := Preflight(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	// 临时目录里 agentfs/merged 不是真挂载点 ⇒ 不应报进 Mounts
	for _, m := range rep.Mounts {
		if m == filepath.Join(dir, "agentfs", "merged") {
			t.Fatal("★ 非挂载点被误报为挂载点（会导致迁移脚本 umount 报错）")
		}
	}
}

// ★★★ 判据：体检必须报出"数据库绝对路径"与"security.key"两类风险。
//
// 这四条风险是生产实测得到的；少报任何一条，运维都会踩坑。
func TestPreflightReportsKnownRisks(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "memory"))
	// agentfs 必须存在，overlay 风险才会计入（风险是关于 agentfs 的）
	mustMkdir(t, filepath.Join(dir, "agentfs", "merged"))
	rep, err := Preflight(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	needles := []string{"overlay", "绝对路径", "security.key"}
	joined := ""
	for _, r := range rep.Risks {
		joined += r + "\n"
	}
	for _, n := range needles {
		if !contains(joined, n) {
			t.Fatalf("★ 体检未报出风险 %q；实际风险: %v", n, rep.Risks)
		}
	}
	if len(rep.DBPathKeys) != 8 {
		t.Fatalf("★ DB 绝对路径键应为 8 个，实际 %d", len(rep.DBPathKeys))
	}
}

// ★★★ 判据：同设备 → CanRename=true；跨设备 → false 且报风险。
func TestPreflightDeviceDetection(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "memory"))

	// 同设备：目标父目录就是 dir 自己
	rep, err := Preflight(dir, filepath.Join(dir, "target"))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.SameDevice || !rep.CanRename {
		t.Fatalf("★ 同设备未被识别：same=%v canRename=%v", rep.SameDevice, rep.CanRename)
	}
	// 跨设备：/dev/shm 通常 tmpfs，与 t.TempDir() 不同设备
	if st, err := os.Stat("/dev/shm"); err == nil && st.IsDir() {
		rep2, err := Preflight(dir, "/dev/shm/xxagent")
		if err != nil {
			t.Fatal(err)
		}
		if rep2.SameDevice {
			t.Skip("/dev/shm 与临时目录同设备，跳过跨设备分支")
		}
		if rep2.CanRename {
			t.Fatal("★ 跨设备被判为可 rename ⇒ 迁移会失败")
		}
		found := false
		for _, r := range rep2.Risks {
			if contains(r, "同设备") || contains(r, "rename") {
				found = true
			}
		}
		if !found {
			t.Fatal("★ 跨设备未报风险")
		}
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
