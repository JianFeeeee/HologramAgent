package test

// 仅提供"把被测模块装配起来"的输入构造，不含任何源码/资源读取断言。

import (
	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
)

const testAPIKey = "test-api-key"

// seedWebUIConfig 写入 webui 插件运行所需的配置（输入）。
func seedWebUIConfig(reg *internalConfig.ConfigRegistry) {
	c := reg.PluginConfig("webui")
	c.RegisterDef(internalConfig.ConfigDef{Key: "api_key", Default: ""})
	c.RegisterDef(internalConfig.ConfigDef{Key: "username", Default: "admin"})
	c.RegisterDef(internalConfig.ConfigDef{Key: "password", Default: ""})
	c.RegisterDef(internalConfig.ConfigDef{Key: "session_ttl_hours", Default: "24"})
	c.Set("api_key", testAPIKey)
	c.Set("username", "admin")
	c.Set("password", "secret-pass")
	c.Set("session_ttl_hours", "24")
}
