package test

import (
	"errors"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/plugins/cfgmgr"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// The module boundary is Start's public tool registrar. A caller invokes the
// published tools; no private handler or internal state is accessed.
type configTools map[string]sdk.ToolHandler

func startConfigManager(t *testing.T, settings sdk.SettingsAPI) configTools {
	t.Helper()
	tools := configTools{}
	p := cfgmgr.New("cfgmgr")
	host := sdk.New("cfgmgr", sdk.SDKConfig{Settings: settings, RegTool: func(name string, _ sdk.ToolDef, handler sdk.ToolHandler) error {
		tools[name] = handler
		return nil
	}})
	if err := p.Start(host); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Stop(); err != nil {
			t.Error(err)
		}
	})
	return tools
}

func (tools configTools) call(t *testing.T, name string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	handler := tools[name]
	if handler == nil {
		t.Fatalf("module did not publish %s", name)
	}
	value, err := handler(args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	result, ok := value.(map[string]interface{})
	if !ok {
		t.Fatalf("%s returned %T, want result object", name, value)
	}
	return result
}

func settingsForModule(t *testing.T) sdk.SettingsAPI {
	t.Helper()
	reg := config.NewConfigRegistry("")
	t.Cleanup(func() { _ = reg.Close() })
	for _, name := range []string{"alpha", "beta"} {
		reg.PluginConfig(name).RegisterDef(config.ConfigDef{Key: "display.theme", Type: "string"})
	}
	return sdk.NewSettings("cfgmgr", reg)
}

func TestConfigurationWorkflowKeepsScopesSeparate(t *testing.T) {
	tools := startConfigManager(t, settingsForModule(t))
	for scope, value := range map[string]string{"core": "system", "alpha": "light", "beta": "dark"} {
		set := tools.call(t, "config_set", map[string]interface{}{"scope": scope, "key": "display.theme", "value": value})
		if set["status"] != "ok" {
			t.Fatalf("write %s failed: %v", scope, set)
		}
	}
	// Read after all writes: a routing mistake must not be hidden by immediate
	// set/get round trips through the same incorrectly routed namespace.
	for scope, value := range map[string]string{"core": "system", "alpha": "light", "beta": "dark"} {
		got := tools.call(t, "config_get", map[string]interface{}{"scope": scope, "key": "display.theme"})
		if got["value"] != value || got["scope"] != scope {
			t.Fatalf("scope %s: %v", scope, got)
		}
		listed := tools.call(t, "config_list_keys", map[string]interface{}{"scope": scope, "prefix": "display."})
		items, ok := listed["items"].([]map[string]interface{})
		listedKey := "display.theme"
		if scope == "core" {
			listedKey = "core.display.theme"
		}
		if !ok || len(items) != 1 || items[0]["key"] != listedKey || items[0]["value"] != value {
			t.Fatalf("scope %s listing: %v", scope, listed)
		}
		empty := tools.call(t, "config_list_keys", map[string]interface{}{"scope": scope, "prefix": "absent."})
		if empty["count"] != 0 {
			t.Fatalf("prefix ignored: %v", empty)
		}
	}
}

// Embed the public dependency interface and override one operation to model a
// storage failure. Assertions concern tool results, not calls or call order.
type unavailableWrites struct{ sdk.SettingsAPI }

func (unavailableWrites) SetCore(string, interface{}) error { return errors.New("storage unavailable") }

func TestBatchReportsPartialFailureAndPersistsSuccessfulItems(t *testing.T) {
	tools := startConfigManager(t, unavailableWrites{settingsForModule(t)})
	got := tools.call(t, "config_batch_set", map[string]interface{}{"items": []interface{}{
		map[string]interface{}{"scope": "core", "key": "display.theme", "value": "blocked"},
		"invalid item",
		map[string]interface{}{"scope": "alpha", "key": "display.theme", "value": "updated"},
		map[string]interface{}{"scope": "beta", "value": "missing key"},
	}})
	if got["applied"] != 1 || got["total"] != 4 {
		t.Fatalf("batch summary: %v", got)
	}
	results, ok := got["results"].([]map[string]interface{})
	if !ok || len(results) != 4 {
		t.Fatalf("batch results: %v", got)
	}
	for i, result := range results {
		if result["index"] != i {
			t.Fatalf("lost item identity: %v", results)
		}
		if i == 2 {
			if result["status"] != "ok" {
				t.Fatalf("successful item: %v", result)
			}
		} else if result["error"] == nil {
			t.Fatalf("failed item reported success: %v", result)
		}
	}
	if results[0]["error"] != "storage unavailable" {
		t.Fatalf("lost storage diagnostic: %v", results[0])
	}
	read := tools.call(t, "config_get", map[string]interface{}{"scope": "alpha", "key": "display.theme"})
	if read["value"] != "updated" {
		t.Fatalf("successful item not persisted: %v", read)
	}
	failed := tools.call(t, "config_set", map[string]interface{}{"scope": "core", "key": "display.theme", "value": "x"})
	if failed["error"] != "storage unavailable" || failed["status"] == "ok" {
		t.Fatalf("false write success: %v", failed)
	}
}

func TestInvalidRequestsAndMissingSettingsAreExplicitFailures(t *testing.T) {
	tools := startConfigManager(t, settingsForModule(t))
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"empty", map[string]interface{}{}, "scope and key are required"},
		{"missing key", map[string]interface{}{"scope": "core"}, "scope and key are required"},
		{"missing scope", map[string]interface{}{"key": "display.theme"}, "scope and key are required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tools.call(t, "config_get", tc.args)
			if got["error"] != tc.want {
				t.Fatalf("unexpected validation result: %v", got)
			}
		})
	}
	for _, name := range []string{"config_set", "config_list_keys", "config_get_defs", "config_batch_set"} {
		got := tools.call(t, name, map[string]interface{}{})
		if got["error"] == nil {
			t.Fatalf("%s accepted missing required input: %v", name, got)
		}
	}
	missing := startConfigManager(t, nil)
	for _, name := range []string{"config_get", "config_set", "config_list_keys", "config_get_defs", "config_batch_set", "config_dump", "config_list_plugins"} {
		got := missing.call(t, name, map[string]interface{}{"scope": "core", "key": "display.theme", "value": "x"})
		if got["error"] != "settings API not available" {
			t.Fatalf("%s hid unavailable dependency: %v", name, got)
		}
	}
}
