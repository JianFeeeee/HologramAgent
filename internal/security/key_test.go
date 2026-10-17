package security

// 主密钥管理的判据。

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateKeyCreatesFile0600(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvKeyName, "") // 确保走磁盘路径

	k1, src, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(k1) != 32 {
		t.Fatalf("密钥应 32 字节，实际 %d", len(k1))
	}
	p := filepath.Join(dir, KeyFileName)
	if src != p {
		t.Errorf("来源应为密钥文件，实际 %q", src)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("密钥文件必须 0600，实际 %04o", fi.Mode().Perm())
	}

	// 再次调用必须得到**同一把**密钥（否则每次重启都解不开旧变量）
	k2, _, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(k1) != string(k2) {
		t.Fatal("重启后密钥必须一致")
	}
}

// ★ 环境变量优先（生产用 systemd 注入）。
func TestEnvKeyTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	want := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	t.Setenv(EnvKeyName, want)

	k, src, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if src != EnvKeyName {
		t.Errorf("来源应为环境变量，实际 %q（不应落盘）", src)
	}
	if hexEncode(k) != want {
		t.Errorf("密钥不符: %s", hexEncode(k))
	}
	// 不应因此创建文件
	if _, err := os.Stat(filepath.Join(dir, KeyFileName)); !os.IsNotExist(err) {
		t.Error("用环境变量时不应创建密钥文件")
	}
}

// ★ 判据：密钥文件权限宽松 ⇒ 拒绝启动（响亮失败）。
func TestRejectsLooseKeyFilePerms(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvKeyName, "")
	p := filepath.Join(dir, KeyFileName)
	if err := os.WriteFile(p, []byte("aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateKey(dir); err == nil {
		t.Fatal("0644 的密钥文件必须被拒绝")
	}
}

// ★ 环境变量格式非法 ⇒ 拒绝（不悄悄忽略）。
func TestRejectsMalformedEnvKey(t *testing.T) {
	t.Setenv(EnvKeyName, "tooshort")
	if _, _, err := LoadOrCreateKey(t.TempDir()); err == nil {
		t.Fatal("非法密钥长度必须报错")
	}
}

func TestParseKeyAcceptsHexAndRaw32(t *testing.T) {
	// 64 位 hex → 32 字节
	k, err := parseKey("aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899")
	if err != nil || len(k) != 32 {
		t.Fatalf("hex 解析失败: %v len=%d", err, len(k))
	}
	// 32 字节原文
	k2, err := parseKey("0123456789abcdef0123456789abcdef")
	if err != nil || len(k2) != 32 {
		t.Fatalf("原样 32 字节解析失败: %v", err)
	}
	if string(k2) != "0123456789abcdef0123456789abcdef" {
		t.Error("原样 32 字节应保持原值")
	}
	// 长度非法
	if _, err := parseKey("short"); err == nil {
		t.Error("长度非法必须报错")
	}
}

// ★★ 判据：**用错密钥必须响亮失败**，不能静默重建库。
//
// 这是最危险的静默降级：密钥换了若不报错，看起来一切正常，
// 实际所有凭据都解不开 —— 而 agent 会继续用别名去调用，全线失败。
func TestWrongKeyFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "v.json")

	s1 := New(Options{Path: p, KeyFn: testKey, Enabled: true})
	if err := s1.Load(); err != nil {
		t.Fatal(err)
	}
	alias, err := s1.Submit(testToken, "")
	if err != nil {
		t.Fatal(err)
	}

	// 换一把密钥重新加载（模拟密钥丢失/轮换失误）
	other := func() ([]byte, error) { return []byte("ffffffffffffffffffffffffffffffff"), nil }
	s2 := New(Options{Path: p, KeyFn: other, Enabled: true})
	if err := s2.Load(); err != nil {
		t.Fatalf("加载本身应成功（只是解不开）: %v", err)
	}
	// ★ 回填必须失败（返回原文，不 panic、不吐垃圾）
	out, n, _ := s2.Fill("x=@{" + alias + "}")
	if n != 0 {
		t.Errorf("用错密钥时不应回填成功，实际 n=%d out=%q", n, out)
	}
	if out != "x=@{"+alias+"}" {
		t.Errorf("用错密钥应原样保留引用（可诊断），实际 %q", out)
	}
	// 记录仍在（没被静默清空）
	if s2.Count() != 1 {
		t.Errorf("用错密钥不应清空记录，实际 %d 条", s2.Count())
	}
}

// ★ 判据：库文件损坏必须报错，**不静默重建**（"不静默重建"）。
func TestCorruptStoreFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "v.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Path: p, KeyFn: testKey, Enabled: true})
	if err := s.Load(); err == nil {
		t.Fatal("损坏的变量库必须报错，不能静默重建")
	}
}

// ★ 首次启动（文件不存在）不是错误。
func TestMissingStoreIsNotError(t *testing.T) {
	s := New(Options{Path: filepath.Join(t.TempDir(), "nope.json"), KeyFn: testKey, Enabled: true})
	if err := s.Load(); err != nil {
		t.Fatalf("首次启动不应报错: %v", err)
	}
	if s.Count() != 0 {
		t.Error("新库应为空")
	}
}

// ---------------- E-4：systemd 凭据生产注入 ----------------

// ★ 判据：systemd 凭据目录被设置且凭据存在 ⇒ 用它（且**优先于 env**）。
func TestSystemdCredentialIsPreferred(t *testing.T) {
	dir := t.TempDir()
	cred := make([]byte, 32)
	for i := range cred {
		cred[i] = byte(i + 1)
	}
	want := hex.EncodeToString(cred)
	if err := os.WriteFile(filepath.Join(dir, CredentialName), []byte(want), 0o400); err != nil {
		t.Fatal(err)
	}
	// 同时设一个**不同的** env，验证凭据优先
	t.Setenv(CredentialDirEnv, dir)
	t.Setenv(EnvKeyName, strings.Repeat("ff", 32))

	key, src, err := LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, "systemd") {
		t.Errorf("来源应标明 systemd 凭据，实际 %q", src)
	}
	if !bytes.Equal(key, cred) {
		t.Error("★ 应取 systemd 凭据的值，而不是 env 的值（env 对同用户进程可见）")
	}
}

// ★★ 判据：凭据目录设置但**凭据缺失** ⇒ 响亮失败，**不得**静默回退到磁盘密钥。
//
// 为何重要：静默回退会让「忘记配凭据」表现成「一切正常」，
// 而实际用的是磁盘上的旧密钥 —— 本模块最怕的静默降级。
func TestMissingSystemdCredentialFailsLoudly(t *testing.T) {
	dir := t.TempDir() // 空目录：没有凭据文件
	t.Setenv(CredentialDirEnv, dir)
	t.Setenv(EnvKeyName, "")

	dataDir := t.TempDir()
	// 磁盘上放一把**可用**的密钥 —— 诱惑实现去回退
	diskKey := hex.EncodeToString(bytes.Repeat([]byte{0xAB}, 32))
	if err := os.WriteFile(filepath.Join(dataDir, KeyFileName), []byte(diskKey), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := LoadOrCreateKey(dataDir)
	if err == nil {
		t.Fatal("★ 凭据缺失却成功返回 —— 静默回退到了磁盘密钥")
	}
	if !strings.Contains(err.Error(), "拒绝静默回退") {
		t.Errorf("错误应说明拒绝回退，实际: %v", err)
	}
}

// ★ 判据：凭据内容格式非法 ⇒ 响亮失败（不当作空串）。
func TestBadSystemdCredentialFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, CredentialName), []byte("not-a-valid-key"), 0o400)
	t.Setenv(CredentialDirEnv, dir)
	t.Setenv(EnvKeyName, "")

	if _, _, err := LoadOrCreateKey(t.TempDir()); err == nil {
		t.Fatal("格式非法应报错")
	}
}

// ★ 判据：没设凭据目录时，行为与从前一致（env > 文件）—— 不能破坏开发机路径。
func TestNoCredentialDirFallsBackToEnv(t *testing.T) {
	t.Setenv(CredentialDirEnv, "")
	want := strings.Repeat("cd", 32)
	t.Setenv(EnvKeyName, want)

	key, src, err := LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if src != EnvKeyName {
		t.Errorf("来源应为 %s，实际 %q", EnvKeyName, src)
	}
	if hex.EncodeToString(key) != want {
		t.Error("应取 env 的值")
	}
}
