package seq

import (
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 插件注册：与其它内置插件同一范式（见 skillmgr/plugin.go 的 init）。
//
// 之所以用 init + factory 而非在 all.go 里直接 new：内置插件统一由
// registry 按名字工厂化加载，all.go 只负责 import 触发注册。
func init() {
	plugin.RegisterPluginMeta("seq", "工具序列", "Tool Sequence")
	plugin.RegisterFactory("seq", func(name string, _ map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}
