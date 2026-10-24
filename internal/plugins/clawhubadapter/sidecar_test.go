package clawhubadapter

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// mockSettings implements pubsdk.SettingsAPI for tests
type mockSettings struct{}

func (m *mockSettings) DataDir() string { return "/tmp/mock_data" }

func (m *mockSettings) Get(key string) (interface{}, error)                   { return nil, nil }
func (m *mockSettings) Set(key string, value interface{}) error               { return nil }
func (m *mockSettings) SetSecret(key string, value interface{}) error         { return nil }
func (m *mockSettings) GetSecret(key string) (string, error)                  { return "", nil }
func (m *mockSettings) List(prefix string) ([]string, error)                  { return nil, nil }
func (m *mockSettings) GetCore(key string) (interface{}, error)               { return nil, nil }
func (m *mockSettings) SetCore(key string, value interface{}) error           { return nil }
func (m *mockSettings) ListCore(prefix string) ([]string, error)              { return nil, nil }
func (m *mockSettings) GetPlugin(plugin, key string) (interface{}, error)     { return nil, nil }
func (m *mockSettings) SetPlugin(plugin, key string, value interface{}) error { return nil }
func (m *mockSettings) ListPlugin(plugin, prefix string) ([]string, error)    { return nil, nil }

// IsSecret：E-3 起 SettingsAPI 要求实现（判断某键是否按密文存储）。
// 判据实现返回 false（mock 里没有密文语义），够用即可。
func (m *mockSettings) IsSecret(key string) bool                          { return false }
func (m *mockSettings) RegisterDef(def pubsdk.ConfigDef)                  {}
func (m *mockSettings) Defs(prefix string) []*pubsdk.ConfigDef            { return nil }
func (m *mockSettings) Dump() map[string]interface{}                      { return nil }
func (m *mockSettings) Plugins() []string                                 { return nil }
func (m *mockSettings) DefsCore(prefix string) []*sdk.ConfigDef           { return nil }
func (m *mockSettings) DefsPlugin(plugin, prefix string) []*sdk.ConfigDef { return nil }
func (m *mockSettings) Remove(key string) error                           { return nil }
func (m *mockSettings) RemoveCore(key string) error                       { return nil }
func (m *mockSettings) RemovePlugin(plugin, key string) error             { return nil }

func TestLaunchSidecarNoMainJS(t *testing.T) {
	tmpDir := t.TempDir()
	sp, err := launchSidecar(tmpDir, "nonexistent", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sp != nil {
		t.Fatal("expected nil for dir without main.js")
	}
}

func TestLaunchSidecarAndListTools(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join("testdata", "echoplugin", "main.js")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read test plugin: %v", err)
	}
	dst := filepath.Join(tmpDir, "main.js")
	if err := os.WriteFile(dst, data, 0755); err != nil {
		t.Fatalf("write test plugin: %v", err)
	}

	sp, err := launchSidecar(tmpDir, "echoplugin", "")
	if err != nil {
		t.Fatalf("launch sidecar: %v", err)
	}
	defer sp.Close()

	tools, err := sp.ListTools()
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	if len(tools) == 0 {
		t.Fatal("expected at least one tool")
	}

	found := false
	for _, tool := range tools {
		if tool.Name == "echo" {
			found = true
			if tool.Description == "" {
				t.Error("expected non-empty description for echo tool")
			}
		}
	}
	if !found {
		t.Fatal("expected 'echo' tool in list")
	}
	t.Logf("tools: %+v", tools)
}

func TestCallEchoTool(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join("testdata", "echoplugin", "main.js")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read test plugin: %v", err)
	}
	dst := filepath.Join(tmpDir, "main.js")
	if err := os.WriteFile(dst, data, 0755); err != nil {
		t.Fatalf("write test plugin: %v", err)
	}

	sp, err := launchSidecar(tmpDir, "echoplugin", "")
	if err != nil {
		t.Fatalf("launch sidecar: %v", err)
	}
	defer sp.Close()

	result, err := sp.CallTool("echo", map[string]interface{}{
		"text": "hello world",
	})
	if err != nil {
		t.Fatalf("call echo tool: %v", err)
	}

	expected := "Echo: hello world"
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
	t.Logf("echo result: %s", result)
}

func TestCallAddTool(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join("testdata", "echoplugin", "main.js")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read test plugin: %v", err)
	}
	dst := filepath.Join(tmpDir, "main.js")
	if err := os.WriteFile(dst, data, 0755); err != nil {
		t.Fatalf("write test plugin: %v", err)
	}

	sp, err := launchSidecar(tmpDir, "echoplugin", "")
	if err != nil {
		t.Fatalf("launch sidecar: %v", err)
	}
	defer sp.Close()

	result, err := sp.CallTool("add", map[string]interface{}{
		"a": 3.0,
		"b": 4.0,
	})
	if err != nil {
		t.Fatalf("call add tool: %v", err)
	}

	expected := "7"
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
	t.Logf("add result: %s", result)
}

func TestCallNonexistentTool(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join("testdata", "echoplugin", "main.js")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read test plugin: %v", err)
	}
	dst := filepath.Join(tmpDir, "main.js")
	if err := os.WriteFile(dst, data, 0755); err != nil {
		t.Fatalf("write test plugin: %v", err)
	}

	sp, err := launchSidecar(tmpDir, "echoplugin", "")
	if err != nil {
		t.Fatalf("launch sidecar: %v", err)
	}
	defer sp.Close()

	_, err = sp.CallTool("nonexistent", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent tool")
	}
	t.Logf("expected error: %v", err)
}

func TestConcurrentCalls(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join("testdata", "echoplugin", "main.js")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read test plugin: %v", err)
	}
	dst := filepath.Join(tmpDir, "main.js")
	if err := os.WriteFile(dst, data, 0755); err != nil {
		t.Fatalf("write test plugin: %v", err)
	}

	sp, err := launchSidecar(tmpDir, "echoplugin", "")
	if err != nil {
		t.Fatalf("launch sidecar: %v", err)
	}
	defer sp.Close()

	done := make(chan bool, 5)
	for i := 0; i < 5; i++ {
		go func(n int) {
			result, err := sp.CallTool("add", map[string]interface{}{
				"a": float64(n),
				"b": float64(n * 2),
			})
			if err != nil {
				t.Errorf("concurrent call %d: %v", n, err)
			}
			if result != fmt.Sprintf("%d", n+n*2) {
				t.Errorf("call %d: expected %d, got %s", n, n+n*2, result)
			}
			done <- true
		}(i)
	}
	for i := 0; i < 5; i++ {
		<-done
	}
}

// ---- Simulator + OpenClaw plugin tests ----

func launchSimulator(t *testing.T, pluginDir, name string) *sidecarProcess {
	t.Helper()
	simPath, err := filepath.Abs(filepath.Join("simulator", "main.js"))
	if err != nil {
		t.Fatalf("abs simulator path: %v", err)
	}
	sp, err := launchProcess("node", simPath, pluginDir, name, "")
	if err != nil {
		t.Fatalf("launch simulator for %s: %v", name, err)
	}
	return sp
}

func TestSimulatorWithOCSimple(t *testing.T) {
	pluginDir, err := filepath.Abs(filepath.Join("testdata", "oc-simple"))
	if err != nil {
		t.Fatalf("abs testdata: %v", err)
	}
	sp := launchSimulator(t, pluginDir, "oc-simple")
	defer sp.Close()

	tools, err := sp.ListTools()
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d: %+v", len(tools), tools)
	}

	found := map[string]bool{"greet": false, "ping": false}
	for _, tool := range tools {
		found[tool.Name] = true
	}
	if !found["greet"] || !found["ping"] {
		t.Fatalf("expected greet and ping tools, got %+v", tools)
	}

	result, err := sp.CallTool("greet", map[string]interface{}{"name": "Test"})
	if err != nil {
		t.Fatalf("call greet: %v", err)
	}
	if result != "Hello, Test!" {
		t.Fatalf("expected 'Hello, Test!', got %q", result)
	}
}

func TestSimulatorWithOCPackage(t *testing.T) {
	pluginDir, err := filepath.Abs(filepath.Join("testdata", "oc-pkg"))
	if err != nil {
		t.Fatalf("abs testdata: %v", err)
	}
	sp := launchSimulator(t, pluginDir, "oc-pkg")
	defer sp.Close()

	tools, err := sp.ListTools()
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d: %+v", len(tools), tools)
	}

	found := map[string]bool{"add": false, "info": false}
	for _, tool := range tools {
		found[tool.Name] = true
		if tool.Name == "add" {
			if tool.InputSchema == nil {
				t.Error("add tool should have inputSchema")
			}
		}
	}
	if !found["add"] || !found["info"] {
		t.Fatalf("expected add and info tools, got %+v", tools)
	}

	result, err := sp.CallTool("add", map[string]interface{}{"a": 10.0, "b": 20.0})
	if err != nil {
		t.Fatalf("call add: %v", err)
	}
	if result != "30" {
		t.Fatalf("expected '30', got %q", result)
	}

	infoResult, err := sp.CallTool("info", nil)
	if err != nil {
		t.Fatalf("call info: %v", err)
	}
	if infoResult == "" {
		t.Fatal("expected non-empty info result")
	}
	t.Logf("info result: %s", infoResult)
}

func TestLoadOCPluginViaPluginStart(t *testing.T) {
	skillsDir := t.TempDir()

	ocSimpleDir := filepath.Join(skillsDir, "oc-simple")
	if err := os.MkdirAll(ocSimpleDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"openclaw.plugin.json", "index.js"} {
		src := filepath.Join("testdata", "oc-simple", name)
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(ocSimpleDir, name), data, 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	ocPkgDir := filepath.Join(skillsDir, "oc-pkg")
	if err := os.MkdirAll(filepath.Join(ocPkgDir, "lib"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"package.json", "lib/entry.js"} {
		src := filepath.Join("testdata", "oc-pkg", name)
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		dst := filepath.Join(ocPkgDir, name)
		if err := os.WriteFile(dst, data, 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	p := New("openclaw", skillsDir)

	var registeredTools []string
	registeredHandlers := make(map[string]sdk.ToolHandler)
	sdk := sdk.New("openclaw", sdk.SDKConfig{
		Settings: &mockSettings{},
		RegTool: func(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
			registeredTools = append(registeredTools, name)
			registeredHandlers[name] = handler
			return nil
		},
	})

	if err := p.Start(sdk); err != nil {
		t.Fatalf("start plugin: %v", err)
	}
	defer p.Stop()

	expected := []string{"oc-simple_greet", "oc-simple_ping", "oc-pkg_add", "oc-pkg_info"}
	for _, exp := range expected {
		found := false
		for _, name := range registeredTools {
			if name == exp {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected tool %q not registered. got: %v", exp, registeredTools)
		}
	}

	handler, ok := registeredHandlers["oc-simple_greet"]
	if !ok {
		t.Fatal("greet handler not registered")
	}
	result, err := handler(map[string]interface{}{"name": "OpenClaw"})
	if err != nil {
		t.Fatalf("exec greet: %v", err)
	}
	if result != "Hello, OpenClaw!" {
		t.Fatalf("expected 'Hello, OpenClaw!', got %v", result)
	}

	handler, ok = registeredHandlers["oc-pkg_add"]
	if !ok {
		t.Fatal("add handler not registered")
	}
	result, err = handler(map[string]interface{}{"a": 7.0, "b": 8.0})
	if err != nil {
		t.Fatalf("exec add: %v", err)
	}
	if result != "15" {
		t.Fatalf("expected '15', got %v", result)
	}
}
