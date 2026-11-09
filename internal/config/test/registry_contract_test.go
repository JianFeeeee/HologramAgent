package test

// internal/config 的黑盒判据：只用 NewConfigRegistry / PluginConfig 的导出方法，
// 输入 → 输出。不读源码、不碰 registry 内部结构。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/config"
)

func newRegistry(t *testing.T) *config.ConfigRegistry {
	t.Helper()
	reg := config.NewConfigRegistry("")
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// TestCoreSetGetDelete：核心命名空间的写入→读取→删除。
func TestCoreSetGetDelete(t *testing.T) {
	reg := newRegistry(t)
	if err := reg.Set("core.llm.model", "deepseek-chat"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := reg.Get("core.llm.model")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "deepseek-chat" {
		t.Fatalf("★ Get = %v，期望 deepseek-chat", got)
	}
	if err := reg.Delete("core.llm.model"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, _ = reg.Get("core.llm.model")
	if got != nil {
		t.Fatalf("★ 删除后 Get = %v，期望 nil", got)
	}
}

// TestPluginNamespaceIsIsolated：两个插件同名 key 互不串台；核心表看不到插件键。
func TestPluginNamespaceIsIsolated(t *testing.T) {
	reg := newRegistry(t)
	a := reg.PluginConfig("alpha")
	b := reg.PluginConfig("beta")
	// 契约：插件表由 RegisterDef 创建，读写不隐式建表。
	a.RegisterDef(config.ConfigDef{Key: "token"})
	b.RegisterDef(config.ConfigDef{Key: "token"})
	if err := a.Set("token", "A-TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := b.Set("token", "B-TOKEN"); err != nil {
		t.Fatal(err)
	}
	va, _ := a.Get("token")
	vb, _ := b.Get("token")
	if va != "A-TOKEN" || vb != "B-TOKEN" {
		t.Fatalf("★ 命名空间串台：alpha=%v beta=%v", va, vb)
	}
	if v, _ := reg.Get("plugin.alpha.token"); v != nil {
		t.Fatalf("★ 插件键泄漏进核心表：%v", v)
	}
}

// TestPluginListByPrefix：List 只返回匹配前缀的键。
func TestPluginListByPrefix(t *testing.T) {
	reg := newRegistry(t)
	p := reg.PluginConfig("alpha")
	for _, k := range []string{"db.host", "db.port", "api.key", "api.secret"} {
		p.RegisterDef(config.ConfigDef{Key: k})
		if err := p.Set(k, "v"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := p.List("db.")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("★ List(\"db.\") = %v，期望 2 个键", got)
	}
}

// TestDefRoundTripPreservesSecret：RegisterDef → ListDefs 必须保住 Secret 标记。
//
// 这是真实缺陷形态：SDK 的 ConfigDef 一直有 Secret，但内核侧转换漏过它，
// 导致敏感项被当普通文本框明文回显。
func TestDefRoundTripPreservesSecret(t *testing.T) {
	reg := newRegistry(t)
	p := reg.PluginConfig("alpha")
	p.RegisterDef(config.ConfigDef{Key: "by_flag", Type: "string", Secret: true})
	p.RegisterDef(config.ConfigDef{Key: "by_type", Type: "password"})
	defs := p.ListDefs("")
	if len(defs) != 2 {
		t.Fatalf("★ ListDefs = %d 条，期望 2", len(defs))
	}
	byKey := map[string]*config.ConfigDef{}
	for _, d := range defs {
		byKey[d.Key] = d
	}
	if !byKey["by_flag"].Secret {
		t.Fatal("★ Secret:true 在 RegisterDef → ListDefs 后丢失")
	}
	if byKey["by_type"].Type != "password" {
		t.Fatalf("★ Type 丢失：%q", byKey["by_type"].Type)
	}
}

// TestSecretValueIsNotPlaintextOnDisk：SetSecret → GetSecret 往返，且 DB 里无明文。
func TestSecretValueIsNotPlaintextOnDisk(t *testing.T) {
	reg := newRegistry(t)
	reg.SetCodec(config.NewTestCodec(t))
	p := reg.PluginConfig("alpha")
	p.RegisterDef(config.ConfigDef{Key: "api_key", Secret: true})
	if err := p.Set("api_key", "PLAINTEXT-SECRET-123"); err != nil {
		t.Fatal(err)
	}
	got, err := p.GetSecret("api_key")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if got != "PLAINTEXT-SECRET-123" {
		t.Fatalf("★ 解密后 = %q，期望原文", got)
	}
	var raw string
	if err := reg.DB().QueryRow(
		"SELECT value FROM config_alpha WHERE key='api_key'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "PLAINTEXT-SECRET-123" {
		t.Fatal("★ 磁盘上是明文 —— 敏感项没有加密")
	}
}
