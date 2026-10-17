package config

// 配置落盘加密的判据。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HomeAgent/internal/security"
)

func testCodec(t *testing.T) {
	t.Helper()
	if _, err := security.InitConfigCodec([]byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
}

func newSecReg(t *testing.T) *ConfigRegistry {
	t.Helper()
	dir := t.TempDir()
	testCodec(t)
	return NewConfigRegistry(filepath.Join(dir, "config.db"))
}

// 直接在 sqlite 层面读原始值（绕过 ConfigRegistry 的解密）。
func rawValue(t *testing.T, r *ConfigRegistry, key string) string {
	t.Helper()
	var v string
	if err := r.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v); err != nil {
		t.Fatalf("读原始值失败: %v", err)
	}
	return v
}

// ★★ 判据：敏感配置**落盘是密文**，而读接口返回明文。
func TestSecretEncryptedOnDiskPlainOnRead(t *testing.T) {
	r := newSecReg(t)
	const secret = "sk-super-secret-value-1234567890"

	if err := r.Set("core.llm.api_key", secret); err != nil {
		t.Fatal(err)
	}

	// ① 磁盘上是密文
	raw := rawValue(t, r, "core.llm.api_key")
	if raw == secret {
		t.Fatal("敏感项在磁盘上是明文")
	}
	if !security.IsEncrypted(raw) {
		t.Fatalf("应有 enc:v1: 前缀，实际 %q", raw)
	}

	// ② 读接口返回明文（内部可信路径语义不变）
	got, err := r.Get("core.llm.api_key")
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatalf("Get 应返回明文，实际 %v", got)
	}
	// ③ GetString / Dump 同样给明文
	if r.GetString("core.llm.api_key", "") != secret {
		t.Error("GetString 应返回明文")
	}
	if r.Dump()["core.llm.api_key"] != secret {
		t.Error("Dump 应返回明文（ToConfig 依赖它）")
	}
}

// ★★ 判据：**非敏感项不受影响**（不能被误加密）。
func TestNonSecretStaysPlain(t *testing.T) {
	r := newSecReg(t)
	if err := r.Set("core.llm.base_url", "https://api.deepseek.com"); err != nil {
		t.Fatal(err)
	}
	raw := rawValue(t, r, "core.llm.base_url")
	if raw != "https://api.deepseek.com" {
		t.Fatalf("非敏感项不应被加密: %q", raw)
	}
}

// ★ 判据：`Secret: true` 与 `Type: "password"` 两种声明都生效。
func TestBothDeclarationFormsWork(t *testing.T) {
	r := newSecReg(t)
	r.RegisterDef(ConfigDef{Key: "core.a.token_field", Default: "", Secret: true})
	r.RegisterDef(ConfigDef{Key: "core.b.password_field", Default: "", Type: "password"})

	for _, k := range []string{"core.a.token_field", "core.b.password_field"} {
		if err := r.Set(k, "value-1234567890"); err != nil {
			t.Fatal(err)
		}
		if !security.IsEncrypted(rawValue(t, r, k)) {
			t.Errorf("%s 未被加密（Secret 或 password 声明未生效）", k)
		}
	}
}

// ★★ 判据：**插件配置同样加密**（插件无感知）。
func TestPluginSecretEncrypted(t *testing.T) {
	r := newSecReg(t)
	ps := r.PluginConfig("demo")
	ps.RegisterDef(ConfigDef{Key: "token", Default: "", Secret: true})

	const secret = "plugin-token-abcdef123456"
	if err := ps.Set("token", secret); err != nil {
		t.Fatal(err)
	}

	// 磁盘（插件表）上是密文
	var raw string
	if err := r.db.QueryRow(`SELECT value FROM config_demo WHERE key = 'token'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !security.IsEncrypted(raw) {
		t.Fatalf("插件敏感项应加密，实际 %q", raw)
	}
	// 插件读回明文
	got, err := ps.Get("token")
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatalf("插件应读回明文，实际 %v", got)
	}
}

// ★ 判据：无 def 的插件键靠**键名模式**兜底加密。
func TestPluginSecretByKeyPattern(t *testing.T) {
	r := newSecReg(t)
	ps := r.PluginConfig("demo2")
	// 先注册一个**非敏感** def：插件表只在 RegisterDef 时创建，
	// 但 api_key 这个键本身没有 def —— 正好检验键名模式兜底。
	ps.RegisterDef(ConfigDef{Key: "enabled", Default: "true"})
	if err := ps.Set("api_key", "abcdefgh12345678"); err != nil {
		t.Fatal(err)
	}
	var raw string
	r.db.QueryRow(`SELECT value FROM config_demo2 WHERE key = 'api_key'`).Scan(&raw)
	if !security.IsEncrypted(raw) {
		t.Fatalf("无 def 时 api_key 键名应触发加密，实际 %q", raw)
	}
}

// ★★ 判据：存量明文在启动迁移后被加密（幂等）。
func TestMigrateExistingSecrets(t *testing.T) {
	r := newSecReg(t)
	// 模拟升级前的明文存量（直接写库，绕过 Set）
	r.db.Exec(`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`,
		"core.llm.api_key", "legacy-plaintext-key-123")
	r.db.Exec(`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`,
		"webui.listen_addr", ":8080")

	n, err := r.EncryptExistingSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应加密 1 条，实际 %d", n)
	}
	if !security.IsEncrypted(rawValue(t, r, "core.llm.api_key")) {
		t.Error("存量敏感项未被加密")
	}
	// 非敏感项不动
	if rawValue(t, r, "webui.listen_addr") != ":8080" {
		t.Error("非敏感项被误改")
	}
	// 迁移后仍可读明文
	if v, _ := r.Get("core.llm.api_key"); v != "legacy-plaintext-key-123" {
		t.Errorf("迁移后应可读明文，实际 %v", v)
	}
	// 幂等
	if n2, _ := r.EncryptExistingSecrets(); n2 != 0 {
		t.Errorf("迁移应幂等，第二次却加密了 %d 条", n2)
	}
}

// ★★ 判据：**存量明文不需要迁移也能读**（老数据向后兼容）。
func TestLegacyPlaintextReadableWithoutMigration(t *testing.T) {
	r := newSecReg(t)
	r.db.Exec(`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`,
		"core.llm.api_key", "still-plain")
	v, err := r.Get("core.llm.api_key")
	if err != nil {
		t.Fatal(err)
	}
	if v != "still-plain" {
		t.Fatalf("明文存量应可读，实际 %v", v)
	}
}

// ★★ 判据：换密钥后**响亮失败**（不静默返回密文）。
func TestWrongKeyFailsLoudly(t *testing.T) {
	r := newSecReg(t)
	if err := r.Set("core.llm.api_key", "secret-value-1234567890"); err != nil {
		t.Fatal(err)
	}
	// 换一把主密钥
	if _, err := security.InitConfigCodec([]byte("ffffffffffffffffffffffffffffffff")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get("core.llm.api_key"); err == nil {
		t.Fatal("换密钥后必须报错，不能把密文当明文返回")
	}
	// Dump 也不得吐密文
	if v := r.Dump()["core.llm.api_key"]; v != nil {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "enc:") {
			t.Fatal("Dump 把密文交出去了")
		}
	}
}

// ★ 判据：未装配 codec 时退化为明文读写（本地开发/测试不阻塞）。
func TestNoCodecDegradesToPlain(t *testing.T) {
	dir := t.TempDir()
	// 清空全局 codec
	security.ClearConfigCodec()
	r := NewConfigRegistry(filepath.Join(dir, "c.db"))
	if err := r.Set("core.llm.api_key", "plain-dev"); err != nil {
		t.Fatal(err)
	}
	if rawValue(t, r, "core.llm.api_key") != "plain-dev" {
		t.Error("未装配 codec 时应保持明文")
	}
	_ = os.Remove
}

// ★ 判据：域隔离 —— 配置子密钥与别名密钥不可互换。
func TestDomainSeparation(t *testing.T) {
	master := []byte("0123456789abcdef0123456789abcdef")
	cfgCodec, err := security.NewCodec(master, "config-at-rest")
	if err != nil {
		t.Fatal(err)
	}
	otherCodec, err := security.NewCodec(master, "some-other-domain")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := cfgCodec.Seal("hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherCodec.Open(sealed); err == nil {
		t.Fatal("不同域的子密钥不应能解开 —— 域隔离失效")
	}
	if got, _ := cfgCodec.Open(sealed); got != "hello" {
		t.Fatal("同域应可解")
	}
}

// ★★ 判据：**ToConfig 在加密开启后仍能取到真 api_key**。
//
// 这是最容易被加密改坏的一条：ToConfig 靠 Dump() 重建 LLMSource。
// 若 Dump 改返回掩码/密文，LLM 就会拿着垃圾去调上游 —— 而且
// 失败现象是「认证失败」，与配置加密毫无关联，归因极难。
func TestToConfigStillGetsRealAPIKey(t *testing.T) {
	r := newSecReg(t)
	const key = "sk-real-key-for-upstream-123456"
	if err := r.Set("core.llm.api_key", key); err != nil {
		t.Fatal(err)
	}
	if err := r.Set("core.llm.sources.probe.base_url", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	if err := r.Set("core.llm.sources.probe.api_key", key); err != nil {
		t.Fatal(err)
	}
	if err := r.Set("core.llm.sources.probe.model", "m1"); err != nil {
		t.Fatal(err)
	}

	cfg := r.ToConfig()
	if cfg.LLM.APIKey != key {
		t.Fatalf("ToConfig 的 APIKey 不真：%q", cfg.LLM.APIKey)
	}
	var found bool
	for _, s := range cfg.LLM.Sources {
		if s.Name == "probe" {
			found = true
			if s.APIKey != key {
				t.Fatalf("source api_key 不真：%q", s.APIKey)
			}
		}
	}
	if !found {
		t.Fatal("source probe 丢失")
	}
}

// ★★ 判据：**数量类键名不得被误判为敏感**。
//
// 这是真机跑出来的缺陷：初版用 strings.Contains(key,"token")，
// 把 core.llm.max_tokens（输出上限）与
// core.agent.context.max_target_tokens（上下文预算）也加密了。
// 功能上无害，但语义错误、且库变得无法人工检查。
func TestNonSecretTokenCountsNotEncrypted(t *testing.T) {
	r := newSecReg(t)
	benign := []string{
		"core.llm.max_tokens",
		"core.agent.context.max_target_tokens",
		"core.compact.target_tokens",
		"core.keyboard_layout", // 含 "key" 但不是凭据
		"core.monkey_mode",     // 同上
		"webui.listen_addr",
		"core.llm.temperature",
	}
	for _, k := range benign {
		if r.isSecretKey(k) {
			t.Errorf("%q 被误判为敏感（会被无谓加密）", k)
		}
	}
}

// ★★ 判据：真敏感键名必须命中（含无 def 的插件键）。
func TestSecretKeyNamesDetected(t *testing.T) {
	r := newSecReg(t)
	sensitive := []string{
		"core.llm.api_key",
		"core.llm.sources.x.access_token",
		"plugin.qq.token",
		"plugin.demo.password",
		"plugin.a.client_secret",
		"plugin.b.apikey",
		"plugin.c.key",
		"core.memory.multimodal_space.options.api_key",
	}
	for _, k := range sensitive {
		if !r.isSecretKey(k) {
			t.Errorf("%q 应被判定为敏感", k)
		}
	}
}

// ★★ 判据：显式加密 API（SetSecret/GetSecret）**不看键名**也加密。
//
// 存在意义：插件可能用中性键名（blob/payload/conn）存凭据，
// 键名模式猜不到 ⇒ 必须能显式声明意图。
func TestExplicitSetSecretIgnoresKeyName(t *testing.T) {
	r := newSecReg(t)
	ps := r.PluginConfig("demo3")
	ps.RegisterDef(ConfigDef{Key: "enabled", Default: "1"})

	const neutral = "blob" // 中性键名，模式判定为**非敏感**
	if r.isSecretKey("plugin.demo3." + neutral) {
		t.Fatal("前提失效：blob 不该被模式判定为敏感")
	}

	const secret = "creds-payload-with-neutral-key-name"
	if err := ps.SetSecret(neutral, secret); err != nil {
		t.Fatal(err)
	}
	// 磁盘上是密文
	var raw string
	if err := r.db.QueryRow(`SELECT value FROM config_demo3 WHERE key = ?`, neutral).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !security.IsEncrypted(raw) {
		t.Fatalf("SetSecret 应强制加密，实际 %q", raw)
	}
	// GetSecret 读回明文
	got, err := ps.GetSecret(neutral)
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatalf("GetSecret 应返回明文，实际 %q", got)
	}
}

// ★★ 判据：**密钥未装配时 SetSecret 拒绝写入**（不静默存明文）。
//
// 这是最危险的静默降级：插件以为存了密文，实际磁盘上是明文。
func TestSetSecretRefusesWithoutKey(t *testing.T) {
	dir := t.TempDir()
	security.ClearConfigCodec()
	defer testCodec(t) // 复原，避免影响其它判据

	r := NewConfigRegistry(filepath.Join(dir, "c.db"))
	ps := r.PluginConfig("demo4")
	ps.RegisterDef(ConfigDef{Key: "enabled", Default: "1"})

	err := ps.SetSecret("blob", "must-not-be-plain")
	if err == nil {
		t.Fatal("无密钥时 SetSecret 必须报错，不能静默写明文")
	}
	// 且确实没写进去
	var cnt int
	r.db.QueryRow(`SELECT COUNT(*) FROM config_demo4 WHERE key = 'blob'`).Scan(&cnt)
	if cnt != 0 {
		t.Error("拒绝写入后库里不应有该键")
	}
	// 空值是允许的（删除语义）
	if err := ps.SetSecret("blob", ""); err != nil {
		t.Errorf("空值应允许写入: %v", err)
	}
}

// ★ 判据：SetSecret 写入的值能被普通 Get 读到（两种读法互通，不产生孤岛）。
func TestSetSecretReadableByGet(t *testing.T) {
	r := newSecReg(t)
	ps := r.PluginConfig("demo5")
	ps.RegisterDef(ConfigDef{Key: "enabled", Default: "1"})
	if err := ps.SetSecret("blob", "value-xyz-123456"); err != nil {
		t.Fatal(err)
	}
	v, err := ps.Get("blob") // Get 会走 openFromStorage
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(v) != "value-xyz-123456" {
		t.Fatalf("Get 应能读到 SetSecret 写的值，实际 %v", v)
	}
}
