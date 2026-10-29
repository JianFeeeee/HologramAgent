package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegistryBasic(t *testing.T) {
	r := NewConfigRegistry("")
	r.Register("core.llm.model", "deepseek-v4-flash")
	r.Register("plugin.qq.access_token", "abc123")

	val, err := r.Get("core.llm.model")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if v, ok := val.(string); !ok || v != "deepseek-v4-flash" {
		t.Fatalf("expected deepseek-v4-flash, got %v", val)
	}

	keys := r.List("core")
	if len(keys) != 1 || keys[0] != "core.llm.model" {
		t.Fatalf("expected [core.llm.model], got %v", keys)
	}

	r.Set("core.llm.model", "gpt-4")
	val, _ = r.Get("core.llm.model")
	if v, _ := val.(string); v != "gpt-4" {
		t.Fatalf("expected gpt-4, got %v", val)
	}
}

func TestRegistryPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")

	r := NewConfigRegistry(path)
	r.Register("core.log_level", "debug")
	r.Set("plugin.test.key", "42")
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	r.Close()

	r2 := NewConfigRegistry(path)
	val, err := r2.Get("plugin.test.key")
	if err != nil {
		t.Fatalf("Get after reload: %v", err)
	}
	if v, _ := val.(string); v != "42" {
		t.Fatalf("expected 42, got %v", val)
	}
	r2.Close()
}

func TestRegistryDelete(t *testing.T) {
	r := NewConfigRegistry("")
	r.Register("a.b", "1")
	r.Register("a.c", "2")
	r.Delete("a.b")
	keys := r.List("a")
	if len(keys) != 1 || keys[0] != "a.c" {
		t.Fatalf("expected [a.c], got %v", keys)
	}
}

func TestRegistryDump(t *testing.T) {
	r := NewConfigRegistry("")
	r.Register("x", "1")
	r.Register("y", "two")
	dump := r.Dump()
	if len(dump) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(dump))
	}
}

func TestRegistryUnknownKey(t *testing.T) {
	r := NewConfigRegistry("")
	_, err := r.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestRegistryFlush(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")
	r := NewConfigRegistry(path)
	r.Set("k", "v")
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	r.Close()

	// Reopen and verify persistence
	r2 := NewConfigRegistry(path)
	val, err := r2.Get("k")
	if err != nil {
		t.Fatalf("Get after flush: %v", err)
	}
	if v, _ := val.(string); v != "v" {
		t.Fatalf("expected v, got %v", val)
	}
	r2.Close()
}

func TestRegistryFlushIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")
	r := NewConfigRegistry(path)
	r.Set("k", "v")
	r.Flush()
	r.Flush() // second flush should not error
	r.Close()
}

func TestPluginConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")
	r := NewConfigRegistry(path)

	ps := r.PluginConfig("test_deepseek")
	ps.RegisterDef(ConfigDef{Key: "api_key", Default: "sk-test123"})
	if err := ps.Set("api_key", "sk-test123"); err != nil {
		t.Fatalf("PluginSettings.Set: %v", err)
	}

	val, err := ps.Get("api_key")
	if err != nil {
		t.Fatalf("PluginSettings.Get: %v", err)
	}
	if v, _ := val.(string); v != "sk-test123" {
		t.Fatalf("expected sk-test123, got %v", val)
	}

	keys, err := ps.List("")
	if err != nil {
		t.Fatalf("PluginSettings.List: %v", err)
	}
	if len(keys) != 1 || keys[0] != "api_key" {
		t.Fatalf("expected [api_key], got %v", keys)
	}

	// Core table should not contain plugin data
	coreKeys := r.List("")
	for _, k := range coreKeys {
		if k == "api_key" {
			t.Fatal("plugin key leaked into core config table")
		}
	}

	r.Close()
}

func TestSeedDefaultsToConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")

	r := NewConfigRegistry(path)
	r.SeedDefaults(dir)

	// Verify DB was seeded with expected number of keys
	keys := r.List("")
	if len(keys) == 0 {
		t.Fatal("SeedDefaults produced empty DB")
	}

	// Reconstruct config from DB
	cfg2 := r.ToConfig()

	if v := r.GetString("webui.listen_addr", ""); v != ":8080" {
		t.Fatalf("expected :8080, got %s", v)
	}
	if cfg2.LLM.Provider != "deepseek" {
		t.Fatalf("expected deepseek, got %s", cfg2.LLM.Provider)
	}
	if len(cfg2.LLM.Sources) == 0 {
		t.Fatal("expected at least 1 LLM source")
	}

	// Second SeedDefaults should be no-op (DB already has data)
	r.SeedDefaults(dir)
	if len(r.List("")) != len(keys) {
		t.Fatal("second SeedDefaults changed DB count")
	}

	r.Close()
}

// 发行包全新安装：postinst 先跑 setup.sh → initconfig，而 initconfig 只写
// webui.listen_addr。于是 config 表已经非空，旧实现据此判定“已有配置”并整体
// 跳过播种——装完没有 core.plugin.dir（0 个插件）、也没有随包模型对应的
// 多模态 provider（754MB 产物 + 24MB 运行库全成死重量）。
func TestSeedDefaultsAfterInitconfigPrepopulate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")

	r := NewConfigRegistry(path)
	// 精确复现 initconfig 的唯一一笔写入
	if _, err := r.db.Exec(`INSERT INTO config (key, value) VALUES ('webui.listen_addr', ':8080')`); err != nil {
		t.Fatalf("预置 initconfig 行: %v", err)
	}

	r.SeedDefaults(dir)

	for _, k := range []string{"core.daemon.data_dir", "core.plugin.dir", "core.memory.multimodal_space.provider"} {
		if r.GetString(k, "") == "" {
			t.Fatalf("全新安装（initconfig 已写 webui.listen_addr）后 %s 仍为空：默认值播种被跳过", k)
		}
	}
	if got := r.GetString("core.memory.multimodal_space.provider", ""); got != "chineseclip" {
		t.Fatalf("随包默认 provider 应为 chineseclip，实为 %q", got)
	}
	r.Close()
}

// 老安装升级：绝不能因为新版本加了默认值就把它注进现有 DB——那会让升级即
// 静默加载一个 1.8GB 的模型。判据是 core.daemon.data_dir 在场（老安装由播种
// 写入）而 seed 标记缺失。
func TestSeedDefaultsDoesNotInjectIntoLegacyInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")

	r := NewConfigRegistry(path)
	if _, err := r.db.Exec(`INSERT INTO config (key, value) VALUES ('core.daemon.data_dir', ?)`, dir); err != nil {
		t.Fatalf("预置老安装行: %v", err)
	}

	r.SeedDefaults(dir)

	if got := r.GetString("core.memory.multimodal_space.provider", ""); got != "" {
		t.Fatalf("老安装升级被注入新默认值 provider=%q（升级后会静默加载大模型）", got)
	}
	if got := r.GetString("core.plugin.dir", ""); got != "" {
		t.Fatalf("老安装升级被注入新默认值 core.plugin.dir=%q", got)
	}

	// 但标记必须补上，否则每次启动都会重走判断
	var n int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM config WHERE key = 'core.internal.seed_version'`).Scan(&n); err != nil {
		t.Fatalf("查 seed 标记: %v", err)
	}
	if n != 1 {
		t.Fatalf("老安装应补上 seed 标记，实际 count=%d", n)
	}
	r.Close()
}

func TestGetHelpers(t *testing.T) {
	r := NewConfigRegistry("")
	r.Set("str_key", "hello")
	r.Set("int_key", "42")
	r.Set("dur_key", "5m")
	r.Set("bool_key", "true")

	if got := r.GetString("str_key", ""); got != "hello" {
		t.Fatalf("GetString: expected hello, got %s", got)
	}
	if got := r.GetString("nonexistent", "fallback"); got != "fallback" {
		t.Fatalf("GetString fallback: expected fallback, got %s", got)
	}
	if got := r.GetInt("int_key", 0); got != 42 {
		t.Fatalf("GetInt: expected 42, got %d", got)
	}
	if got := r.GetInt("nonexistent", 99); got != 99 {
		t.Fatalf("GetInt fallback: expected 99, got %d", got)
	}
	if got := r.GetDuration("dur_key", 0); got != 5*time.Minute {
		t.Fatalf("GetDuration: expected 5m, got %v", got)
	}
	r.Set("dur_key_days", "2d")
	if got := r.GetDuration("dur_key_days", 0); got != 48*time.Hour {
		t.Fatalf("GetDuration d-unit: expected 48h, got %v", got)
	}
	r.Set("dur_key_weeks", "1w")
	if got := r.GetDuration("dur_key_weeks", 0); got != 168*time.Hour {
		t.Fatalf("GetDuration w-unit: expected 168h, got %v", got)
	}
	if got := r.GetDuration("nonexistent", 30*time.Second); got != 30*time.Second {
		t.Fatalf("GetDuration fallback: expected 30s, got %v", got)
	}
	if got := r.GetBool("bool_key", false); got != true {
		t.Fatalf("GetBool: expected true, got %v", got)
	}
	if got := r.GetBool("nonexistent", true); got != true {
		t.Fatalf("GetBool fallback: expected true, got %v", got)
	}
}

func TestParseDurationExtended(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"2d", 48 * time.Hour, false},
		{"1w", 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"2d12h", 60 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"500ms", 500 * time.Millisecond, false},
		{"1h30m", 90 * time.Minute, false},
		{" 3d ", 72 * time.Hour, false},
		{"2w", 336 * time.Hour, false},
		{"", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := parseDurationExtended(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: expected %v, got %v", c.in, c.want, got)
		}
	}
}

func TestSnapshotRestoreCoreLLM(t *testing.T) {
	r := NewConfigRegistry("")
	defer r.Close()

	r.Set("core.llm.sources.main.base_url", "https://a")
	r.Set("core.llm.sources.main.model", "m1")
	r.Set("core.llm.sources.main.api_key", "k1")

	snap := r.SnapshotCoreLLM()
	if len(snap) != 3 {
		t.Fatalf("expected 3 keys, got %d: %v", len(snap), snap)
	}

	// 模拟写坏
	r.Set("core.llm.sources.main.base_url", "https://broken")
	r.Set("core.llm.sources.main.api_key", "hacked")
	r.Set("core.llm.sources.extra.model", "intruder")

	if err := r.RestoreCoreLLM(snap); err != nil {
		t.Fatalf("RestoreCoreLLM: %v", err)
	}
	got := r.SnapshotCoreLLM()
	if len(got) != 3 {
		t.Fatalf("after restore expected 3 keys, got %d: %v", len(got), got)
	}
	for k, v := range snap {
		if got[k] != v {
			t.Errorf("key %s: want %q got %q", k, v, got[k])
		}
	}
}

func TestLLMSnapshotFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llm_snapshot.json")
	snap := map[string]string{"core.llm.sources.main.base_url": "https://a", "core.llm.sources.main.model": "m1"}
	if err := SaveLLMSnapshot(path, snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadLLMSnapshot(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got["core.llm.sources.main.base_url"] != "https://a" || got["core.llm.sources.main.model"] != "m1" {
		t.Fatalf("round-trip mismatch: %v", got)
	}
}

func TestSetLLMSnapshotFile(t *testing.T) {
	r := NewConfigRegistry("")
	defer r.Close()
	r.Set("core.llm.sources.main.base_url", "https://orig")
	r.Set("core.llm.sources.main.model", "m0")

	path := filepath.Join(t.TempDir(), "llm_pre.json")
	r.SetLLMSnapshotFile(path)
	// 再次写入：写前自动留档应记录当前值 orig/m0，随后才被覆盖
	r.Set("core.llm.sources.main.base_url", "https://broken")

	got, err := LoadLLMSnapshot(path)
	if err != nil {
		t.Fatalf("Load snapshot: %v", err)
	}
	if got["core.llm.sources.main.base_url"] != "https://orig" {
		t.Fatalf("write-ahead snapshot should record pre-write value, got %q", got["core.llm.sources.main.base_url"])
	}
	if got["core.llm.sources.main.model"] != "m0" {
		t.Fatalf("snapshot missing untouched key model: %v", got)
	}
}

// TestPluginDefsAreNamespaced 锁住两件曾经一起坏掉的事：
//
//  1. PluginConfig(name).ListDefs 必须只返回**该插件**的 def，且 Key 是插件内
//     局部键。修复前它把 prefix 直接透传给全局 ListDefs，于是返回全仓所有 def
//     （WebUI 设置页因此把每个 def 复制进每个插件命名空间，5208 条里 96% 是重复）。
//  2. ListCoreDefs 必须排除 "plugin.<name>." 命名空间，否则核心列表里会混进
//     插件 def 的副本。
func TestPluginDefsAreNamespaced(t *testing.T) {
	r := NewConfigRegistry("")
	r.RegisterDef(ConfigDef{Key: "core.agent.max_tool_turns", Default: "10"})
	r.RegisterDef(ConfigDef{Key: "core.llm.model", Default: "m"})

	a := r.PluginConfig("a")
	a.RegisterDef(ConfigDef{Key: "addr", Default: ":1"})
	a.RegisterDef(ConfigDef{Key: "token", Default: ""})
	b := r.PluginConfig("b")
	b.RegisterDef(ConfigDef{Key: "secret", Default: ""})

	da := a.ListDefs("")
	if len(da) != 2 {
		t.Fatalf("插件 a 应只有 2 个自己的 def，实际 %d：%v", len(da), keysOf(da))
	}
	for _, d := range da {
		if d.Key != "addr" && d.Key != "token" {
			t.Fatalf("插件 a 看到了不属于自己的 def：%q", d.Key)
		}
	}
	// 局部键前缀过滤（"a" 只匹配插件内以 a 开头的键 → addr）
	if got := a.ListDefs("a"); len(got) != 1 || got[0].Key != "addr" {
		t.Fatalf("插件内前缀过滤失效：%v", keysOf(got))
	}
	db := b.ListDefs("")
	if len(db) != 1 || db[0].Key != "secret" {
		t.Fatalf("插件 b 应只有 secret，实际 %v", keysOf(db))
	}

	core := r.ListCoreDefs("")
	if len(core) != 2 {
		t.Fatalf("核心 def 应为 2 个，实际 %d：%v", len(core), keysOf(core))
	}
	for _, d := range core {
		if strings.HasPrefix(d.Key, "plugin.") {
			t.Fatalf("核心列表混入了插件 def：%q", d.Key)
		}
	}
	if got := r.ListCoreDefs("core.llm"); len(got) != 1 || got[0].Key != "core.llm.model" {
		t.Fatalf("核心前缀过滤失效：%v", keysOf(got))
	}
}

func keysOf(defs []*ConfigDef) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.Key
	}
	return out
}

// TestMaybeCompactReclaimsFreePages 钉住「删除大值后文件要真的缩回去」。
// SQLite 的 DELETE 只把页标空闲，文件体积不变；v1.3.12 之前那条 5MB 的
// plugin.webui.chathistory 就是这样让 config.db 一直占着几 MB。
func TestMaybeCompactReclaimsFreePages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")
	r := NewConfigRegistry(path)
	r.RegisterDef(ConfigDef{Key: "plugin.webui.chathistory", Default: ""})

	big := strings.Repeat("x", 512*1024)
	r.Set("plugin.webui.chathistory", big)
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 阈值远比这次写入小 → 搬走大值后应触发压缩
	if err := r.Delete("plugin.webui.chathistory"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	done, err := r.MaybeCompact(1, 0.1)
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if !done {
		t.Fatal("空闲页远大于阈值时应执行 VACUUM")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() >= before.Size() {
		t.Fatalf("压缩后文件应变小：%d -> %d", before.Size(), after.Size())
	}
	// 数据仍在（压缩不能破坏内容）
	r.Set("core.llm.model", "m1")
	if v, err := r.Get("core.llm.model"); err != nil || v != "m1" {
		t.Fatalf("压缩后读写异常: %v %v", v, err)
	}
	// 没有空闲页时不应白做功
	if done2, err := r.MaybeCompact(1<<30, 0.9); err != nil || done2 {
		t.Fatalf("阈值很高时不应压缩，得到 done=%v err=%v", done2, err)
	}
	r.Close()
}

// regMeta 的**真实契约**：可见，但**不提供**默认值。
//
// ## 这条判据是怎么来的（2026-10-29 记录一次真实的自我纠错）
//
// 我最初在这里断言「regMeta 的键在 GetXxx 上能读到 ConfigDef.Default」，
// 然后它红了。查 `defDefault`：
//
//	if !r.seeded[key] { return "", false }   // ← 只对 registerDefLocked 播种过的键回退
//
// 而 `regMeta` 的实现是 `r.defs[d.Key] = &d`，**从不写 seeded**。
// 于是 regMeta 的键（不只是老安装，**全新安装也一样**）GetXxx 会直接返回
// 调用方传入的兜底值，Default 字段只是给 WebUI 展示用的。
//
// ## 为什么这个区别必须被钉住
//
// 如果装配代码（如 PolicyFromConfig）以为"配置有 Default 就够了"，
// 它写 `GetBool("core.security.sandbox.enabled", false)` 恰好对，
// 但若有人写 `GetBool(..., true)`（照抄别的键的习惯），
// 就会在**所有机器上**读到一个与 ConfigDef.Default 不同的值 —— 而
// `TestPolicyConfigDefaultsMatchRegistry` 存在的意义就是把这两处焊死。
func TestRegMetaIsVisibleButProvidesNoDefault(t *testing.T) {
	dir := t.TempDir()
	r := NewConfigRegistry(filepath.Join(dir, "config.db"))
	r.SeedDefaults(dir) // 用全新安装：证明这不是"老安装才这样"
	defer r.Close()

	// ① 可见性成立：WebUI 设置页靠 ListDefs 列出配置项。
	var seen bool
	for _, d := range r.ListDefs("core.security.") {
		if d.Key == "core.security.tool_deny" {
			seen = true
			if d.Default != "" {
				t.Fatalf("core.security.tool_deny 的 Default 应为空串，实际 %q", d.Default)
			}
		}
	}
	if !seen {
		t.Fatal("core.security.tool_deny 未出现在 ListDefs —— WebUI 里看不到这个配置项")
	}

	// ② ★ 关键：GetXxx 返回的是**调用方兜底值**，不是 Def.Default。
	//    这里传一个故意与 Default 相反的值，证明读到的就是它。
	if got := r.GetBool("core.security.sandbox.enabled", true); !got {
		t.Fatal("regMeta 的键应返回调用方兜底值 true —— 若这里读到 false，" +
			"说明 defDefault 行为变了，PolicyFromConfig 的兜底假设必须重审")
	}
	if got := r.GetString("core.log.retention", "forever"); got != "forever" {
		t.Fatalf("regMeta 的键应返回调用方兜底值 forever，实际 %q", got)
	}

	// ③ 显式写入后必须正常可读（regMeta 只是不提供默认，不是不可写）。
	if err := r.Set("core.security.tool_deny", "cmd_*"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if got := r.GetString("core.security.tool_deny", ""); got != "cmd_*" {
		t.Fatalf("写入后应读到 cmd_*，实际 %q", got)
	}
}
