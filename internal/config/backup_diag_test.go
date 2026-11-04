package config

import (
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// ★★ E-2：备份来自**另一台机器**（密钥不同）时，审计必须报出“不可解”，
// 而不是静默当明文/空值，也不是等到某个功能坏了才发现。
//
// 这是备份恢复场景的核心判据：db 能打开、表能读、但密文解不开 ——
// 那种状态最容易被误判成“配置丢了”。
func TestAuditSecretsDetectsForeignKeyBackup(t *testing.T) {
	// 本包的 codec 是**进程级全局**，测试必须还原现场，
	// 否则会把密钥泄漏给同包其它判据（实测：导致 GetInt 读到密文）。
	dir := t.TempDir()
	keyA := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	keyB := []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	// 机器 A：写入一条加密配置
	cA, cerr := security.NewConfigCodec(keyA, nil)
	if cerr != nil {
		t.Fatal(cerr)
	}
	regA := NewConfigRegistry(filepath.Join(dir, "config.db"))
	regA.SetCodec(cA)
	regA.RegisterDef(cfgDef("core.llm.api_key"))
	if err := regA.Set("core.llm.api_key", "sk-machine-a"); err != nil {
		t.Fatal(err)
	}
	ok, bad, err := regA.AuditSecrets()
	if err != nil || ok != 1 || bad != 0 {
		t.Fatalf("machine A 自审应为 ok=1 bad=0，实际 ok=%d bad=%d err=%v", ok, bad, err)
	}
	regA.Close()

	// 机器 B：同一份 db，但密钥不同
	cB, cerr := security.NewConfigCodec(keyB, nil)
	if cerr != nil {
		t.Fatal(cerr)
	}
	regB := NewConfigRegistry(filepath.Join(dir, "config.db"))
	regB.SetCodec(cB)
	defer regB.Close()
	ok2, bad2, err := regB.AuditSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if bad2 == 0 {
		t.Fatal("★ 外来密钥的备份未被识别 —— 会被误判为「配置丢了」而非「密钥不匹配」")
	}
	if ok2 != 0 {
		t.Errorf("外来密钥下可解条数应为 0，实际 %d", ok2)
	}
	// 读取该键必须**响亮报错**，不能把密文当明文交出去
	if v, err := regB.Get("core.llm.api_key"); err == nil {
		t.Fatalf("★ 密文解不开却返回了值: %v", v)
	}
}

// ★ 同一密钥下重启（正常恢复）必须全部可解 —— 不能把正常备份误报成坏。
func TestAuditSecretsCleanOnSameKey(t *testing.T) {
	dir := t.TempDir()
	key := []byte("cccccccccccccccccccccccccccccccc")
	c, cerr := security.NewConfigCodec(key, nil)
	if cerr != nil {
		t.Fatal(cerr)
	}
	db := filepath.Join(dir, "config.db")
	reg := NewConfigRegistry(db)
	reg.SetCodec(c)
	reg.RegisterDef(cfgDef("core.llm.api_key"))
	if err := reg.Set("core.llm.api_key", "sk-1"); err != nil {
		t.Fatal(err)
	}
	reg.Close()

	cy, cerr := security.NewConfigCodec(key, nil)
	if cerr != nil {
		t.Fatal(cerr)
	}
	reg2 := NewConfigRegistry(db)
	reg2.SetCodec(cy)
	defer reg2.Close()
	ok, bad, err := reg2.AuditSecrets()
	if err != nil || bad != 0 || ok != 1 {
		t.Fatalf("同密钥重启应为 ok=1 bad=0，实际 ok=%d bad=%d err=%v", ok, bad, err)
	}
}

// cfgDef 造一个最小 secret 键定义。
func cfgDef(key string) ConfigDef {
	return ConfigDef{Key: key, Type: "string", Secret: true}
}
