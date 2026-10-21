package cfgmgr

import (
	"encoding/json"
	"log"
	"sort"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func init() {
	plugin.RegisterPluginMeta("cfgmgr", "配置管理", "Config Manager")
	plugin.RegisterFactory("cfgmgr", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name string
}

func New(name string) *Plugin {
	return &Plugin{name: name}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)

	s.RegisterTool("config_list_plugins", sdk.ToolDef{
		Name:        "config_list_plugins",
		Description: "列出所有插件的配置命名空间，返回每个插件的名称和配置项数量",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		// 已核实只读：只列举插件名
		ParallelSafe: true,
	}, p.handleListPlugins(s))

	s.RegisterTool("config_get", sdk.ToolDef{
		Name:        "config_get",
		Description: "获取指定插件或核心的配置项值。scope=core 读取核心配置，scope=插件名 读取该插件的配置",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"scope": map[string]interface{}{"type": "string", "description": "配置作用域：core 表示核心配置，插件名表示该插件的配置"},
				"key":   map[string]interface{}{"type": "string", "description": "配置项键名"},
			},
			"required": []string{"scope", "key"},
		},
		// 已核实只读：底层 ConfigRegistry 有 RWMutex，且本工具不改任何状态
		ParallelSafe: true,
	}, p.handleGet(s))

	s.RegisterTool("config_set", sdk.ToolDef{
		Name:        "config_set",
		Description: "设置指定插件或核心的配置项值。scope=core 写入核心配置，scope=插件名 写入该插件的配置。修改后立即生效，无需重启",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"scope": map[string]interface{}{"type": "string", "description": "配置作用域：core 表示核心配置，插件名表示该插件的配置"},
				"key":   map[string]interface{}{"type": "string", "description": "配置项键名"},
				"value": map[string]interface{}{"type": "string", "description": "配置项值（字符串形式，布尔用 true/false，数字用字面量）"},
			},
			"required": []string{"scope", "key", "value"},
		},
		// 写配置：即使底层有锁也按声明序执行
		Serial: true,
	}, p.handleSet(s))

	s.RegisterTool("config_list_keys", sdk.ToolDef{
		Name:        "config_list_keys",
		Description: "列出指定插件或核心的所有配置项键名和当前值。scope=core 列出核心配置，scope=插件名 列出该插件配置",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"scope":  map[string]interface{}{"type": "string", "description": "配置作用域：core 或插件名"},
				"prefix": map[string]interface{}{"type": "string", "description": "键名前缀过滤（可选，默认空表示全部）"},
			},
			"required": []string{"scope"},
		},
		// 已核实只读:只列举键值
		ParallelSafe: true,
	}, p.handleListKeys(s))

	s.RegisterTool("config_get_defs", sdk.ToolDef{
		Name:        "config_get_defs",
		Description: "获取指定插件或核心的配置项定义（类型、描述、默认值、选项等），用于了解每个配置项的含义和合法值",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"scope":  map[string]interface{}{"type": "string", "description": "配置作用域：core 或插件名"},
				"prefix": map[string]interface{}{"type": "string", "description": "键名前缀过滤（可选）"},
			},
			"required": []string{"scope"},
		},
		// 已核实只读：返回配置项定义，不改状态
		ParallelSafe: true,
	}, p.handleGetDefs(s))

	s.RegisterTool("config_dump", sdk.ToolDef{
		Name:        "config_dump",
		Description: "导出所有配置（核心+所有插件）的完整快照，用于全局审查或备份",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		// 已核实只读：导出当前配置
		ParallelSafe: true,
	}, p.handleDump(s))

	s.RegisterTool("config_batch_set", sdk.ToolDef{
		Name:        "config_batch_set",
		Description: "批量设置配置项，一次修改多个键值对。items 为 JSON 数组，每项包含 scope/key/value",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"items": map[string]interface{}{
					"type":        "array",
					"description": "配置项数组，每项: {\"scope\":\"core|插件名\", \"key\":\"键名\", \"value\":\"值\"}",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"scope": map[string]interface{}{"type": "string", "description": "配置作用域"},
							"key":   map[string]interface{}{"type": "string", "description": "配置项键名"},
							"value": map[string]interface{}{"type": "string", "description": "配置项值"},
						},
						"required": []string{"scope", "key", "value"},
					},
				},
			},
			"required": []string{"items"},
		},
		// 批量写配置：同上
		Serial: true,
	}, p.handleBatchSet(s))

	log.Printf("[cfgmgr] started")
	return nil
}

func (p *Plugin) Stop() error {
	return nil
}

func (p *Plugin) handleListPlugins(s *sdk.PluginSDK) sdk.ToolHandler {
	return func(args map[string]interface{}) (interface{}, error) {
		sett := s.Settings()
		if sett == nil {
			return map[string]interface{}{"error": "settings API not available"}, nil
		}
		plugins := sett.Plugins()
		result := make([]map[string]interface{}, 0, len(plugins))
		for _, name := range plugins {
			entry := map[string]interface{}{"name": name}
			if name != "core" {
				keys, err := sett.ListPlugin(name, "")
				if err == nil {
					entry["key_count"] = len(keys)
				}
			} else {
				keys, err := sett.ListCore("")
				if err == nil {
					entry["key_count"] = len(keys)
				}
			}
			result = append(result, entry)
		}
		return map[string]interface{}{"plugins": result}, nil
	}
}

func (p *Plugin) handleGet(s *sdk.PluginSDK) sdk.ToolHandler {
	return func(args map[string]interface{}) (interface{}, error) {
		sett := s.Settings()
		if sett == nil {
			return map[string]interface{}{"error": "settings API not available"}, nil
		}
		scope, _ := args["scope"].(string)
		key, _ := args["key"].(string)
		if scope == "" || key == "" {
			return map[string]interface{}{"error": "scope and key are required"}, nil
		}

		var val interface{}
		var err error
		if scope == "core" {
			val, err = sett.GetCore(key)
		} else {
			val, err = sett.GetPlugin(scope, key)
		}
		if err != nil {
			return map[string]interface{}{"error": err.Error(), "scope": scope, "key": key}, nil
		}
		return map[string]interface{}{"scope": scope, "key": key, "value": val}, nil
	}
}

func (p *Plugin) handleSet(s *sdk.PluginSDK) sdk.ToolHandler {
	return func(args map[string]interface{}) (interface{}, error) {
		sett := s.Settings()
		if sett == nil {
			return map[string]interface{}{"error": "settings API not available"}, nil
		}
		scope, _ := args["scope"].(string)
		key, _ := args["key"].(string)
		value, _ := args["value"].(string)
		if scope == "" || key == "" {
			return map[string]interface{}{"error": "scope and key are required"}, nil
		}

		var err error
		if scope == "core" {
			err = sett.SetCore(key, value)
		} else {
			err = sett.SetPlugin(scope, key, value)
		}
		if err != nil {
			return map[string]interface{}{"error": err.Error()}, nil
		}
		log.Printf("[cfgmgr] set %s.%s = %s", scope, key, value)
		return map[string]interface{}{"status": "ok", "scope": scope, "key": key, "value": value}, nil
	}
}

func (p *Plugin) handleListKeys(s *sdk.PluginSDK) sdk.ToolHandler {
	return func(args map[string]interface{}) (interface{}, error) {
		sett := s.Settings()
		if sett == nil {
			return map[string]interface{}{"error": "settings API not available"}, nil
		}
		scope, _ := args["scope"].(string)
		prefix, _ := args["prefix"].(string)
		if scope == "" {
			return map[string]interface{}{"error": "scope is required"}, nil
		}

		var keys []string
		var err error
		if scope == "core" {
			keys, err = sett.ListCore(prefix)
		} else {
			keys, err = sett.ListPlugin(scope, prefix)
		}
		if err != nil {
			return map[string]interface{}{"error": err.Error()}, nil
		}
		sort.Strings(keys)

		items := make([]map[string]interface{}, 0, len(keys))
		for _, k := range keys {
			var val interface{}
			if scope == "core" {
				val, _ = sett.GetCore(k)
			} else {
				val, _ = sett.GetPlugin(scope, k)
			}
			items = append(items, map[string]interface{}{"key": k, "value": val})
		}
		return map[string]interface{}{"scope": scope, "count": len(items), "items": items}, nil
	}
}

func (p *Plugin) handleGetDefs(s *sdk.PluginSDK) sdk.ToolHandler {
	return func(args map[string]interface{}) (interface{}, error) {
		sett := s.Settings()
		if sett == nil {
			return map[string]interface{}{"error": "settings API not available"}, nil
		}
		scope, _ := args["scope"].(string)
		prefix, _ := args["prefix"].(string)
		if scope == "" {
			return map[string]interface{}{"error": "scope is required"}, nil
		}

		defPrefix := buildDefPrefix(scope, prefix)
		defs := sett.Defs(defPrefix)
		result := make([]map[string]interface{}, 0, len(defs))
		for _, d := range defs {
			if d == nil {
				continue
			}
			entry := map[string]interface{}{
				"key":          d.Key,
				"type":         d.Type,
				"display_name": d.DisplayName,
				"description":  d.Description,
			}
			if d.Default != nil {
				entry["default"] = d.Default
			}
			if len(d.Options) > 0 {
				entry["options"] = d.Options
			}
			if d.Category != "" {
				entry["category"] = d.Category
			}
			if d.Secret {
				entry["secret"] = true
			}
			result = append(result, entry)
		}
		return map[string]interface{}{"scope": scope, "count": len(result), "defs": result}, nil
	}
}

func (p *Plugin) handleDump(s *sdk.PluginSDK) sdk.ToolHandler {
	return func(args map[string]interface{}) (interface{}, error) {
		sett := s.Settings()
		if sett == nil {
			return map[string]interface{}{"error": "settings API not available"}, nil
		}
		dump := sett.Dump()
		return map[string]interface{}{"config": dump}, nil
	}
}

func (p *Plugin) handleBatchSet(s *sdk.PluginSDK) sdk.ToolHandler {
	return func(args map[string]interface{}) (interface{}, error) {
		sett := s.Settings()
		if sett == nil {
			return map[string]interface{}{"error": "settings API not available"}, nil
		}
		rawItems, ok := args["items"].([]interface{})
		if !ok || len(rawItems) == 0 {
			return map[string]interface{}{"error": "items must be a non-empty array"}, nil
		}

		var results []map[string]interface{}
		applied := 0
		for i, raw := range rawItems {
			item, ok := raw.(map[string]interface{})
			if !ok {
				results = append(results, map[string]interface{}{"index": i, "error": "invalid item format"})
				continue
			}
			scope, _ := item["scope"].(string)
			key, _ := item["key"].(string)
			value, _ := item["value"].(string)
			if scope == "" || key == "" {
				results = append(results, map[string]interface{}{"index": i, "error": "scope and key required"})
				continue
			}

			var err error
			if scope == "core" {
				err = sett.SetCore(key, value)
			} else {
				err = sett.SetPlugin(scope, key, value)
			}
			if err != nil {
				results = append(results, map[string]interface{}{"index": i, "scope": scope, "key": key, "error": err.Error()})
				continue
			}
			applied++
			results = append(results, map[string]interface{}{"index": i, "scope": scope, "key": key, "value": value, "status": "ok"})
		}
		log.Printf("[cfgmgr] batch set: %d/%d applied", applied, len(rawItems))
		return map[string]interface{}{"applied": applied, "total": len(rawItems), "results": results}, nil
	}
}

func buildDefPrefix(scope, prefix string) string {
	if scope == "core" {
		if prefix == "" {
			return "core."
		}
		return "core." + prefix
	}
	p := "plugin." + scope + "."
	if prefix != "" {
		p += prefix
	}
	return p
}

var _ = json.Marshal
