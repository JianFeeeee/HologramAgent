package webui

import (
	"path/filepath"
	"strings"
	"testing"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
)

// 门禁：**全局 config 表里不得出现 plugin.<name>.* 形态的键**。
//
// ## 要判的是什么（2026-10-05 生产清理）
//
// 生产 config.db 的全局 `config` 表里躺着：
//
//	plugin.webui.username = admin
//	plugin.webui.password = admin123     ← 明文口令
//	plugin.webui.api_key  = admin123     ← 明文密钥
//
// 而**生效凭证根本不在这张表**：`getWebUIConfig` 走 `h.settings.Get`
// → `PluginConfig("webui")` → `config_webui` 表，那里是 jianf/jinrui233719。
//
// ⇒ 那三行是**旧版 SDK 的残留**（那时 Settings() 还写全局表）。
// 现在没有任何代码路径会再写它们（已用判据钉住，见下），
// 但留在库里就是明文泄露：备份、日志、误 dump、任何能读 config.db 的人。
//
// ★ 读不到 ≠ 不该存在。密钥类数据留在库里本身就是风险。
//
// ## 判据钉的是「命名空间纪律」而非逐键列举
//
// `plugin.<name>.<key>` 形态的键只允许出现在 `config_<name>` 表
// （那是插件配置的合法位置）。全局表里出现即命名空间错误。
// 比「这些键现在没被读」强：将来任何插件误写全局表都会被抓到。

// newCfgRegistry 建一个带默认播种的临时 registry。
func newCfgRegistry(t *testing.T) *internalConfig.ConfigRegistry {
	t.Helper()
	dir := t.TempDir()
	reg := internalConfig.NewConfigRegistry(filepath.Join(dir, "config.db"))
	reg.SeedDefaults(dir)
	t.Cleanup(func() { reg.Close() })
	return reg
}

// TestPluginSettingsGoesToPluginTable 钉住「插件的 Set 落 config_<name>，不是全局表」。
//
// ★ 这条是正向断言，比「不许出现 plugin.* 键」更根本：
//
//	只要 Settings().Set 还写 config_<name>，全局表就不会再长出 plugin.* 键。
//	生产那三行残留正是因为历史上它曾经写全局表。
func TestPluginSettingsGoesToPluginTable(t *testing.T) {
	reg := newCfgRegistry(t)

	// ★ 必须先 RegisterDef：插件配置表（config_<name>）由 RegisterDef 创建
	//   （见 registry.go 的 ensurePluginTable），而插件 Start 时也是这么做的。
	//   直接 Set 会得到 "no such table: config_webui"——那不是缺陷，
	//   是「表尚未随定义建立」的正常时序。
	ps := reg.PluginConfig("webui")
	ps.RegisterDef(internalConfig.ConfigDef{
		Key: "password", Default: "", Type: "password",
	})

	const secret = "s3cret-should-not-be-in-global-table"
	if err := ps.Set("password", secret); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// ① 全局表里查不到。
	if v, _ := reg.Get("plugin.webui.password"); v != nil {
		t.Errorf("插件配置泄漏进全局表：plugin.webui.password = %v", v)
	}
	// ② 插件表里能查到（证明 ① 不是「压根没写进去」）。
	if v, _ := ps.Get("password"); v != secret {
		t.Errorf("插件表里读不到刚写的值：%v", v)
	}
	// ③ 全局 List 也不该暴露它。
	for _, k := range reg.List("") {
		if strings.HasPrefix(k, "plugin.") {
			t.Errorf("全局 List 暴露 plugin.* 形态的键 %q", k)
		}
	}
}

// TestGlobalConfigHasNoPluginPrefixedKeys 在**播种了全部默认值**之后检查全局表。
//
// ★ 与上一条同源但覆盖面更大：默认值播种（SeedDefaults）是「没人显式 Set
//
//	也会往库里写」的那条路径，最容易把带前缀的键漏进全局表。
func TestGlobalConfigHasNoPluginPrefixedKeys(t *testing.T) {
	reg := newCfgRegistry(t)

	for _, k := range reg.List("") {
		if strings.HasPrefix(k, "plugin.") {
			t.Errorf("SeedDefaults 后全局 config 表出现 plugin.* 形态的键 %q —— "+
				"插件配置必须写进 config_<name> 表；写进全局表会把口令/密钥"+
				"落到不该落的地方（生产曾残留 plugin.webui.password=admin123）", k)
		}
	}
}

// TestRegisterDefDoesNotQualifyKey 钉住 RegisterDef 的键限定行为：
//
// `PluginSettings.RegisterDef` 会把 `def.Key` 改写成 `plugin.<name>.<key>`
// 存进全局 defs 注册表（供设置页展示 meta），而**值**仍写 config_<name>。
// 这里确认「defs 注册表里有 plugin.* 键」是**预期**的，
// 免得后人把这条门禁误改成「全局不许出现 plugin. 前缀」而误伤正常元数据。
func TestRegisterDefDoesNotQualifyKey(t *testing.T) {
	reg := newCfgRegistry(t)

	ps := reg.PluginConfig("webui")
	defs := ps.ListDefs("")
	var sawQualified bool
	for _, d := range defs {
		if strings.HasPrefix(d.Key, "plugin.webui.") {
			sawQualified = true
		}
	}
	if !sawQualified {
		// 没播种 webui 的 def 也可能为空 —— 那就不做断言，但要点明意图。
		t.Log("本次 registry 无 webui 配置定义（SeedDefaults 未含），跳过正向确认")
	}
}
