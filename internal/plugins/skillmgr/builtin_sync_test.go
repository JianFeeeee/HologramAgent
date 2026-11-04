package skillmgr

// 判据：内置 skill 的**更新语义**。
//
// ## 为什么必须成组断言（2026-11-04 实测缺陷）
//
// 原规则是「目标目录已存在就跳过」，本意是保住用户改动。但它把两件事混成一件：
//
//	用户改过的版本     ⇒ 应该保留
//	内核自己写过的旧版 ⇒ 应该随内核更新
//
// 两者都表现为「目录已存在」，于是内核改名/改文档后，内置 skill
// **永远更新不到已部署机器**。
//
// 实测后果：b850fea 把 `memory_recall` 改成 `stablemem_recall`，
// 生产机的 kernel-memory 一直停在旧名 —— 模型照着旧名调工具必然失败，
// 而日志里一个字都没有。
//
// ## 所以这里必须四条一起断言
//
//	① 首次安装：写入 + 写 stamp
//	② 未被改动      ⇒ 覆盖成新版（这是修掉的那个缺陷）
//	③ 用户改过      ⇒ **不**覆盖（保住原设计意图）
//	④ 没有 stamp    ⇒ 不覆盖（保守：老机器上的目录可能是人的）
//
// 只断言 ② 会把 ③ 破坏掉（那就成了"内核覆盖用户修改"）。
// 只断言 ③ 则缺陷依旧（那就是原来的行为）。

import (
	"os"
	"path/filepath"
	"testing"
)

// writeBuiltinFixture 在临时目录里造一个 skillsDir。
func writeBuiltinFixture(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// putSkill 写一份 SKILL.md + 可选 stamp。
func putSkill(t *testing.T, dir, content string, withStamp bool) {
	t.Helper()
	skillDir := filepath.Join(dir, "kernel-memory")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, skillFileName), []byte(content), 0644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	if withStamp {
		writeBuiltinStamp(skillDir, []byte(content))
	}
}

// TestMaterializeBuiltinSkills_FirstInstall ①：首次安装写入并留 stamp。
func TestMaterializeBuiltinSkills_FirstInstall(t *testing.T) {
	dir := writeBuiltinFixture(t)
	n := materializeBuiltinSkills(dir)
	if n == 0 {
		t.Fatal("首次安装应当写入内嵌 skill，实际写入 0 个")
	}
	stamp := filepath.Join(dir, "kernel-memory", builtinStampName)
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("首次安装后没有 stamp 文件（%v）—— 下次就无法判断'有没有人改过'", err)
	}
	if !builtinSkillUnmodified(filepath.Join(dir, "kernel-memory")) {
		t.Fatal("刚写入的 skill 却被判定为'已被改动'")
	}
}

// TestMaterializeBuiltinSkills_OverwritesUnmodified ②：**核心判据**。
//
// 变红条件：把覆盖逻辑改回"目录已存在就 continue" ⇒ 红。
func TestMaterializeBuiltinSkills_OverwritesUnmodified(t *testing.T) {
	dir := writeBuiltinFixture(t)
	// 先装一次（拿到真 stamp），再把内容改成"旧版"并同步 stamp，
	// 模拟"内核上次写的是旧内容，但用户没动过"。
	if n := materializeBuiltinSkills(dir); n == 0 {
		t.Fatal("前置：首次安装失败")
	}
	skillDir := filepath.Join(dir, "kernel-memory")
	const stale = "# 旧版内容\n旧工具名 memory_recall\n"
	if err := os.WriteFile(filepath.Join(skillDir, skillFileName), []byte(stale), 0644); err != nil {
		t.Fatalf("写旧版: %v", err)
	}
	writeBuiltinStamp(skillDir, []byte(stale)) // 用户没动过 ⇒ stamp 与内容一致

	n := materializeBuiltinSkills(dir)
	if n == 0 {
		t.Fatal("★ 内核自己写的旧版没有被更新 —— 内置 skill 的修复永远到不了已部署机器" +
			"（这正是 memory_recall → stablemem_recall 那次改名失联的原因）")
	}
	got, err := os.ReadFile(filepath.Join(skillDir, skillFileName))
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if string(got) == stale {
		t.Fatal("★ 返回了'已更新'但内容还是旧的")
	}
	// 更新后 stamp 必须跟着变，否则下次会被误判成"用户改过"。
	if !builtinSkillUnmodified(skillDir) {
		t.Fatal("更新后 stamp 没同步 —— 下次启动会认为这是用户版本，从此不再更新")
	}
}

// TestMaterializeBuiltinSkills_KeepsUserEdits ③：用户改过的不覆盖。
//
// 这是原设计意图，不能被 ② 的修复破坏。
func TestMaterializeBuiltinSkills_KeepsUserEdits(t *testing.T) {
	dir := writeBuiltinFixture(t)
	if n := materializeBuiltinSkills(dir); n == 0 {
		t.Fatal("前置：首次安装失败")
	}
	skillDir := filepath.Join(dir, "kernel-memory")
	const userEdit = "# 我自己改的\n这一行是用户加的\n"
	if err := os.WriteFile(filepath.Join(skillDir, skillFileName), []byte(userEdit), 0644); err != nil {
		t.Fatalf("写用户版: %v", err)
	}
	// ★ 不动 stamp：模拟"用户手改了文件，但没更新 .builtin-sha256"。

	materializeBuiltinSkills(dir)
	got, err := os.ReadFile(filepath.Join(skillDir, skillFileName))
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if string(got) != userEdit {
		t.Fatalf("★ 用户的修改被内核覆盖了（现在内容 %q）—— "+
			"内置 skill 的更新必须以'不覆盖人的改动'为前提", string(got))
	}
}

// TestMaterializeBuiltinSkills_NoStampIsConservative ④：没有 stamp 视为用户版本。
//
// 老机器上（功能上线前装的目录）没有 stamp。此时代码无从判断，
// 必须选保守侧：不覆盖。代价是那些机器需要手动重装一次。
func TestMaterializeBuiltinSkills_NoStampIsConservative(t *testing.T) {
	dir := writeBuiltinFixture(t)
	const unknown = "# 来历不明的旧内容\n"
	putSkill(t, dir, unknown, false) // 有内容、无 stamp

	// ★ 不能断言返回值为 0：内置 skill 有多个，其余几个目录不存在会被正常写入。
	// 第一版这么断言，判据红了但代码是对的 —— 这个坑记在这里：
	// 计数是"本次写入总数"，不是"是否覆盖了这一个"。
	materializeBuiltinSkills(dir)

	got, err := os.ReadFile(filepath.Join(dir, "kernel-memory", skillFileName))
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if string(got) != unknown {
		t.Fatalf("没有 stamp 的 kernel-memory 被覆盖了（现在 %q）—— "+
			"无法确认那份是不是人改的，必须保守", string(got))
	}
}

// TestMaterializeBuiltinSkills_Idempotent 内容最新时不重写（保住 mtime）。
func TestMaterializeBuiltinSkills_Idempotent(t *testing.T) {
	dir := writeBuiltinFixture(t)
	if n := materializeBuiltinSkills(dir); n == 0 {
		t.Fatal("前置：首次安装失败")
	}
	before, err := os.Stat(filepath.Join(dir, "kernel-memory", skillFileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// 同 ④：返回值是"本次写入总数"，此时全部已存在且最新 ⇒ 应为 0。
	if n := materializeBuiltinSkills(dir); n != 0 {
		t.Fatalf("内容已最新时不该再写（写了 %d 个）—— 会无谓改动 mtime，干扰运维判断", n)
	}
	after, err := os.Stat(filepath.Join(dir, "kernel-memory", skillFileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("内容未变却重写了文件（mtime 变了）")
	}
}
