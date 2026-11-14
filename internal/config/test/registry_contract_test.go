package test

// internal/config 的黑盒判据：只用 NewConfigRegistry / PluginConfig 的导出方法，
// 输入 → 输出。不读源码、不碰 registry 内部结构。

import (
	"os"
	"path/filepath"
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

// TestSecretSurvivesRestartAndStaysEncrypted 覆盖配置模块的完整职责：
// 敏感项写入 → 磁盘上无明文 → 关闭并重新打开同一数据库后仍可正确解密读回。
// 只用公开 API 观察，不查询内部表结构。
func TestSecretSurvivesRestartAndStaysEncrypted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")

	first := config.NewConfigRegistry(path)
	first.SetCodec(config.NewTestCodec(t))
	p := first.PluginConfig("alpha")
	p.RegisterDef(config.ConfigDef{Key: "api_key", Secret: true})
	const secret = "PLAINTEXT-SECRET-123"
	if err := p.Set("api_key", secret); err != nil {
		first.Close()
		t.Fatalf("Set: %v", err)
	}
	if got, err := p.GetSecret("api_key"); err != nil || got != secret {
		first.Close()
		t.Fatalf("GetSecret before restart = %q / %v", got, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 落盘文件里不得出现明文。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == secret || containsBytes(raw, []byte(secret)) {
		t.Fatalf("secret written in plaintext on disk")
	}

	second := config.NewConfigRegistry(path)
	defer second.Close()
	second.SetCodec(config.NewTestCodec(t))
	q := second.PluginConfig("alpha")
	if got, err := q.GetSecret("api_key"); err != nil || got != secret {
		t.Fatalf("GetSecret after restart = %q / %v", got, err)
	}
}

func containsBytes(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
