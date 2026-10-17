package sdk

import (
	"fmt"
	"strings"

	internalConfig "github.com/JianFeeeee/HomeAgent/internal/config"
)

type settingsImpl struct {
	pluginName string
	reg        *internalConfig.ConfigRegistry
	dataDir    string
}

func NewSettings(name string, reg *internalConfig.ConfigRegistry) SettingsAPI {
	return &settingsImpl{pluginName: name, reg: reg}
}

// SetDataDir 注入本插件的数据目录（内核装配时调用）。
func (s *settingsImpl) SetDataDir(dir string) { s.dataDir = dir }

// DataDir 返回插件专属数据目录 <data>/plugin_data/<name>，保证目录存在。
func (s *settingsImpl) DataDir() string { return s.dataDir }

func (s *settingsImpl) Get(key string) (interface{}, error) {
	if s.reg == nil {
		return nil, nil
	}
	return s.reg.PluginConfig(s.pluginName).Get(key)
}
func (s *settingsImpl) Set(key string, value interface{}) error {
	if s.reg == nil {
		return nil
	}
	return s.reg.PluginConfig(s.pluginName).Set(key, value)
}

// SetSecret 强制加密写入（不看键名）。
//
// 未装配密钥时拒绝写入：插件以为存了密文而实际是明文，是最危险的静默降级。
func (s *settingsImpl) SetSecret(key string, value interface{}) error {
	if s.reg == nil {
		return nil
	}
	return s.reg.PluginConfig(s.pluginName).SetSecret(key, value)
}

// GetSecret 读取 SetSecret 写入的值。
func (s *settingsImpl) GetSecret(key string) (string, error) {
	if s.reg == nil {
		return "", nil
	}
	return s.reg.PluginConfig(s.pluginName).GetSecret(key)
}

func (s *settingsImpl) Remove(key string) error {
	if s.reg == nil {
		return nil
	}
	return s.reg.PluginConfig(s.pluginName).Remove(key)
}
func (s *settingsImpl) RemoveCore(key string) error {
	if s.reg == nil {
		return nil
	}
	return s.reg.Delete(coreKey(key))
}
func (s *settingsImpl) List(prefix string) ([]string, error) {
	if s.reg == nil {
		return nil, nil
	}
	return s.reg.PluginConfig(s.pluginName).List(prefix)
}
func coreKey(key string) string {
	if strings.HasPrefix(key, "core.") {
		return key
	}
	return "core." + key
}

func (s *settingsImpl) GetCore(key string) (interface{}, error) {
	if s.reg == nil {
		return nil, nil
	}
	return s.reg.Get(coreKey(key))
}
func (s *settingsImpl) SetCore(key string, value interface{}) error {
	if s.reg == nil {
		return nil
	}
	return s.reg.Set(coreKey(key), value)
}
func (s *settingsImpl) ListCore(prefix string) ([]string, error) {
	if s.reg == nil {
		return nil, nil
	}
	p := coreKey(prefix)
	if p == "core." {
		p = "core."
	}
	return s.reg.List(p), nil
}
func (s *settingsImpl) GetPlugin(plugin, key string) (interface{}, error) {
	if s.reg == nil {
		return nil, nil
	}
	return s.reg.PluginConfig(plugin).Get(key)
}
func (s *settingsImpl) SetPlugin(plugin, key string, value interface{}) error {
	if s.reg == nil {
		return nil
	}
	return s.reg.PluginConfig(plugin).Set(key, value)
}
func (s *settingsImpl) RemovePlugin(plugin, key string) error {
	if s.reg == nil {
		return nil
	}
	return s.reg.PluginConfig(plugin).Remove(key)
}
func (s *settingsImpl) ListPlugin(plugin, prefix string) ([]string, error) {
	if s.reg == nil {
		return nil, nil
	}
	return s.reg.PluginConfig(plugin).List(prefix)
}
func (s *settingsImpl) RegisterDef(def ConfigDef) {
	if s.reg == nil {
		return
	}
	s.reg.PluginConfig(s.pluginName).RegisterDef(internalConfig.ConfigDef{
		Key: def.Key, Type: def.Type, DisplayName: def.DisplayName, Description: def.Description,
		Category: def.Category, Options: def.Options,
		Default: stringifyDefault(def.Default),
		// ★ Secret 必须透传：丢了它，插件声明的敏感项就退化成普通项
		//（明文落盘 + 在设置页回显）。初版这三处都漏了这个字段，
		// 导致 SDK 里早就存在的 Secret 字段从来没生效过。
		Secret: def.Secret,
	})
}
func (s *settingsImpl) Defs(prefix string) []*ConfigDef {
	if s.reg == nil {
		return nil
	}
	defs := s.reg.PluginConfig(s.pluginName).ListDefs(prefix)
	out := make([]*ConfigDef, len(defs))
	for i, d := range defs {
		// ConfigDef = pubsdk.ConfigDef (type alias), so direct conversion works
		cpy := ConfigDef{
			Key: d.Key, Type: d.Type, DisplayName: d.DisplayName, Description: d.Description,
			Category: d.Category, Options: d.Options, Default: d.Default,
			Secret: d.Secret,
		}
		out[i] = &cpy
	}
	return out
}

// IsSecret 报告某个键是否敏感（E-3：WebUI 屏蔽明文用）。
//
// 判定完全委托给 config registry —— 展示与落盘加密必须**同一套判定**，
// 否则会出现“加密了但界面照旧显示明文”的漏洞。
func (s *settingsImpl) IsSecret(key string) bool {
	if s.reg == nil {
		return false
	}
	return s.reg.IsSecret(key)
}

func (s *settingsImpl) Dump() map[string]interface{} {
	if s.reg == nil {
		return nil
	}
	return s.reg.Dump()
}
func (s *settingsImpl) Plugins() []string {
	if s.reg == nil {
		return nil
	}
	names := s.reg.ListPlugins()
	result := make([]string, 0, len(names)+1)
	result = append(result, "core")
	result = append(result, names...)
	return result
}
func (s *settingsImpl) DefsCore(prefix string) []*ConfigDef {
	if s.reg == nil {
		return nil
	}
	// 只取核心命名空间：插件 def 已限定在 "plugin.<name>."，不能当核心 def 返回。
	return mapDefs(s.reg.ListCoreDefs(prefix))
}
func (s *settingsImpl) DefsPlugin(plugin, prefix string) []*ConfigDef {
	if s.reg == nil {
		return nil
	}
	return mapDefs(s.reg.PluginConfig(plugin).ListDefs(prefix))
}

func mapDefs(defs []*internalConfig.ConfigDef) []*ConfigDef {
	out := make([]*ConfigDef, len(defs))
	for i, d := range defs {
		out[i] = &ConfigDef{
			Key: d.Key, Type: d.Type, DisplayName: d.DisplayName, Description: d.Description,
			Category: d.Category, Options: d.Options, Default: d.Default,
			Secret: d.Secret,
		}
	}
	return out
}

func stringifyDefault(v interface{}) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(v)
	}
}

// Ensure settingsImpl satisfies SettingsAPI (pubsdk.SettingsAPI via type alias).
var _ SettingsAPI = (*settingsImpl)(nil)
