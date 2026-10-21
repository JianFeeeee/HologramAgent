package plugin

import (
	"fmt"
	"os"
	"path/filepath"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// tryLoadLua 从插件目录加载 main.lua（Lua 插件）。
// 返回 nil,nil 表示目录中没有 main.lua。
func tryLoadLua(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	luaPath := filepath.Join(dir, luaEntry)
	if _, err := os.Stat(luaPath); os.IsNotExist(err) {
		return nil, nil
	}

	plg, err := newLuaPlugin(luaPath, name)
	if err != nil {
		return nil, fmt.Errorf("lua plugin %s: %w", name, err)
	}
	return plg, nil
}
