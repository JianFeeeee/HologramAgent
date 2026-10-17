package security

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func rotationKey(fill byte) []byte { return bytes.Repeat([]byte{fill}, 32) }

// ★ E-1：轮换后既有别名**不变**、旧值仍可回填。
func TestRotateKeyPreservesAliasesAndReadsOldEntries(t *testing.T) {
	dir := t.TempDir()
	oldKey, newKey := rotationKey(0x11), rotationKey(0x22)
	s := New(Options{Path: dir + "/variables.json", KeyFn: func() ([]byte, error) { return oldKey, nil }, Enabled: true})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	alias, err := s.Submit("SUPER_SECRET", "rotation")
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.Fill("@{" + alias + "}")
	if before != "SUPER_SECRET" {
		t.Fatalf("轮换前回填失败: %q", before)
	}
	if err := s.RotateKey(newKey); err != nil {
		t.Fatal(err)
	}
	after, _, _ := s.Fill("@{" + alias + "}")
	if after != "SUPER_SECRET" {
		t.Fatalf("★ 轮换后旧别名/旧值不可用: %q", after)
	}
	alias2, err := s.Alias("SUPER_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	if alias2 != alias {
		t.Fatalf("★ 轮换改变了别名: before=%s after=%s", alias, alias2)
	}
	if !strings.Contains(string(mustRead(t, dir+"/variables.json")), alias) {
		t.Fatal("轮换落盘缺少旧别名")
	}
}

// ★ 轮换落盘后重启：只注入 new + old ring，也必须能回填旧条目。
func TestRotatedStoreRestartsWithOldKeyRing(t *testing.T) {
	dir := t.TempDir() + "/variables.json"
	oldKey, newKey := rotationKey(0x31), rotationKey(0x32)
	s := New(Options{Path: dir, KeyFn: func() ([]byte, error) { return oldKey, nil }, Enabled: true})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	alias, err := s.Submit("ROTATE_ME", "restart")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RotateKey(newKey); err != nil {
		t.Fatal(err)
	}
	// 新进程：current=new，history=old。
	s2 := New(Options{Path: dir, KeyFn: func() ([]byte, error) { return newKey, nil }, OldKeys: [][]byte{oldKey}, Enabled: true})
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s2.Fill("@{" + alias + "}")
	if got != "ROTATE_ME" {
		t.Fatalf("重启后历史密钥环无法解密: %q", got)
	}
}

// ★ 没有旧钥环时换钥必须响亮失败，而不是把库当成空库重建。
func TestRotateKeyRefusesWrongCurrentKey(t *testing.T) {
	dir := t.TempDir() + "/variables.json"
	oldKey := rotationKey(0x41)
	s := New(Options{Path: dir, KeyFn: func() ([]byte, error) { return oldKey, nil }, Enabled: true})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit("DO_NOT_LOSE", "wrong-key"); err != nil {
		t.Fatal(err)
	}
	wrong := New(Options{Path: dir, KeyFn: func() ([]byte, error) { return rotationKey(0x42), nil }, Enabled: true})
	if err := wrong.Load(); err != nil {
		t.Fatal(err)
	}
	if err := wrong.RotateKey(rotationKey(0x43)); err == nil {
		t.Fatal("★ 错钥轮换却成功 —— 可能静默丢数据")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ★ keyring 文件：轮换后重启必须能自动装回历史钥（否则 E-1 只是"能轮换一次"）。
func TestKeyringPersistAndRestart(t *testing.T) {
	dir := t.TempDir()
	oldKey, newKey := rotationKey(0x51), rotationKey(0x52)
	if err := RotateKeyFile(dir, oldKey, newKey); err != nil {
		t.Fatal(err)
	}
	history, err := LoadKeyring(dir, newKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || !sameKey(history[0], oldKey) {
		t.Fatalf("历史钥未装回: %v", history)
	}
	// 与 security.key 不一致时必须拒绝（否则会拿错钥去解，静默丢数据）
	if _, err := LoadKeyring(dir, rotationKey(0x53)); err == nil {
		t.Fatal("★ keyring 与 security.key 不一致却未被拒绝")
	}
}

// ★ 端到端：轮换 → 重启（只给 new + keyring history）→ 旧条目仍可回填。
func TestRotationSurvivesRestartViaKeyring(t *testing.T) {
	dir := t.TempDir()
	varPath := dir + "/variables.json"
	oldKey, newKey := rotationKey(0x61), rotationKey(0x62)
	s := New(Options{Path: varPath, KeyFn: func() ([]byte, error) { return oldKey, nil }, Enabled: true})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	alias, err := s.Submit("RESTART_SECRET", "e2e")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RotateKey(newKey); err != nil {
		t.Fatal(err)
	}
	if err := RotateKeyFile(dir, oldKey, newKey); err != nil {
		t.Fatal(err)
	}
	history, err := LoadKeyring(dir, newKey)
	if err != nil {
		t.Fatal(err)
	}
	s2 := New(Options{Path: varPath, KeyFn: func() ([]byte, error) { return newKey, nil }, OldKeys: history, Enabled: true})
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s2.Fill("@{" + alias + "}")
	if got != "RESTART_SECRET" {
		t.Fatalf("重启后无法回填: %q", got)
	}
}

// ★★ 轮换必须**真的重加密并落盘**：重启时只给新钥（不带任何历史钥）也必须能读。
//
// 为何这条不可省：只把旧钥留在环里能让旧密文"看起来还能用"，但磁盘上
// 仍是旧钥密文 —— 那意味着旧钥永远退不掉，轮换等于没做。
// 这条判据正是用来咬住"内存换了、磁盘没换"的半轮换。
func TestRotationPersistsReEncryptionToDisk(t *testing.T) {
	dir := t.TempDir() + "/variables.json"
	oldKey, newKey := rotationKey(0x91), rotationKey(0x92)
	s := New(Options{Path: dir, KeyFn: func() ([]byte, error) { return oldKey, nil }, Enabled: true})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	alias, err := s.Submit("RE_ENCRYPT_ME", "persist")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RotateKey(newKey); err != nil {
		t.Fatal(err)
	}

	// 只有新钥，没有历史钥 —— 能读出来才说明磁盘上已是新钥密文。
	fresh := New(Options{Path: dir, KeyFn: func() ([]byte, error) { return newKey, nil }, Enabled: true})
	if err := fresh.Load(); err != nil {
		t.Fatal(err)
	}
	got, _, _ := fresh.Fill("@" + "{" + alias + "}")
	if got != "RE_ENCRYPT_ME" {
		t.Fatalf("★ 只用新钥读不出来 ⇒ 轮换没有真正重加密落盘（半轮换）: %q", got)
	}
}
