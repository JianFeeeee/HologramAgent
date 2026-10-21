package pluginmgr

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func TestCmpVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.1", -1},
		{"1.0", "1.0.0", 0},
		{"v2.0.0", "1.9.9", 1},
		{"2.0.0", "10.0.0", -1},     // 数字比较而非字典序
		{"1.0.0-alpha", "1.0.0", 0}, // 非数字段按 0
	}
	for _, c := range cases {
		if got := cmpVersion(c.a, c.b); got != c.want {
			t.Errorf("cmpVersion(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

// ---- 最小 mock SDK ----

type pmSettings struct{}

func (m *pmSettings) DataDir() string { return "/tmp/mock_data" }

func (m *pmSettings) Get(string) (interface{}, error)               { return nil, nil }
func (m *pmSettings) Set(string, interface{}) error                 { return nil }
func (m *pmSettings) SetSecret(string, interface{}) error           { return nil }
func (m *pmSettings) GetSecret(string) (string, error)              { return "", nil }
func (m *pmSettings) List(string) ([]string, error)                 { return nil, nil }
func (m *pmSettings) GetCore(string) (interface{}, error)           { return nil, nil }
func (m *pmSettings) SetCore(string, interface{}) error             { return nil }
func (m *pmSettings) ListCore(string) ([]string, error)             { return nil, nil }
func (m *pmSettings) GetPlugin(string, string) (interface{}, error) { return nil, nil }
func (m *pmSettings) SetPlugin(string, string, interface{}) error   { return nil }
func (m *pmSettings) ListPlugin(string, string) ([]string, error)   { return nil, nil }
func (m *pmSettings) RegisterDef(sdk.ConfigDef)                     {}
func (m *pmSettings) Defs(string) []*sdk.ConfigDef                  { return nil }
func (m *pmSettings) Dump() map[string]interface{}                  { return nil }
func (m *pmSettings) Plugins() []string                             { return nil }
func (m *pmSettings) DefsCore(string) []*sdk.ConfigDef              { return nil }
func (m *pmSettings) DefsPlugin(string, string) []*sdk.ConfigDef    { return nil }
func (m *pmSettings) Remove(string) error                           { return nil }
func (m *pmSettings) RemoveCore(string) error                       { return nil }
func (m *pmSettings) RemovePlugin(string, string) error             { return nil }

// IsSecret：E-3 起 SettingsAPI 要求实现（判断某键是否按密文存储）。
func (m *pmSettings) IsSecret(string) bool { return false }

// fakePluginMgr 记录调用；StopAndUnload 只记标志，不真正操作。
type fakePluginMgr struct {
	mu             sync.Mutex
	stopAndUnloads []string
	loaded         map[string]bool // 升级前"在跑"的插件，供 ListPluginRuntimes 返回
	starts         []string
}

func (f *fakePluginMgr) ListLoadedPlugins() []string                   { return nil }
func (f *fakePluginMgr) ListDisabledPlugins() []sdk.DisabledPluginInfo { return nil }
func (f *fakePluginMgr) IsPluginDisabled(string) bool                  { return false }
func (f *fakePluginMgr) IsBuiltinPlugin(string) bool                   { return false }
func (f *fakePluginMgr) DisablePlugin(string, string) error            { return nil }
func (f *fakePluginMgr) EnablePlugin(string) error                     { return nil }
func (f *fakePluginMgr) StopPlugin(string) error                       { return nil }
func (f *fakePluginMgr) StartPlugin(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, name)
	return nil
}
func (f *fakePluginMgr) IsPluginRuntimeStopped(string) bool            { return false }
func (f *fakePluginMgr) RemovePlugin(string) error                     { return nil }
func (f *fakePluginMgr) StopAndUnload(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopAndUnloads = append(f.stopAndUnloads, name)
	return nil
}
func (f *fakePluginMgr) ReloadPlugins() (string, error) { return "", nil }
func (f *fakePluginMgr) ReloadOne(string) error         { return nil }
func (f *fakePluginMgr) PluginMetas() map[string]sdk.PluginMeta {
	return map[string]sdk.PluginMeta{}
}
func (f *fakePluginMgr) PluginDir() string { return "" }
func (f *fakePluginMgr) PluginRuntime(string) (sdk.PluginRuntimeInfo, bool) {
	return sdk.PluginRuntimeInfo{}, false
}
func (f *fakePluginMgr) ListPluginRuntimes() []sdk.PluginRuntimeInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sdk.PluginRuntimeInfo, 0, len(f.loaded))
	for name, loaded := range f.loaded {
		out = append(out, sdk.PluginRuntimeInfo{Name: name, Loaded: loaded})
	}
	return out
}

// buildHmap 构造一个最小 .hmap 包。
func buildHmap(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	manifest := map[string]interface{}{
		"name": name, "name_zh": name, "name_en": name,
		"version": version, "entry": "plugin.bin",
	}
	mData, _ := json.Marshal(manifest)
	f, _ := zw.Create("plugin.json")
	f.Write(mData)
	bin, _ := zw.Create("plugin.bin")
	bin.Write([]byte("binary-" + name + "-" + version))
	zw.Close()
	return buf.Bytes()
}

func TestInstallThenUpgradeKeepsConfig(t *testing.T) {
	dir := t.TempDir()
	fm := &fakePluginMgr{}
	bus := events.NewBus()
	sdkInst := sdk.New("pluginmgr", sdk.SDKConfig{
		Settings:  &pmSettings{},
		EventBus:  bus,
		PluginMgr: fm,
	})

	p := &Plugin{pluginDir: dir, sdk: sdkInst}

	// 1. 首次安装 v1.0.0
	r1, _ := p.installFromData(buildHmap(t, "demo", "1.0.0"), false)
	m1 := r1.(map[string]interface{})
	if m1["status"] != "installed" {
		t.Fatalf("install failed: %v", m1)
	}
	if _, err := os.Stat(filepath.Join(dir, "demo", "plugin.json")); err != nil {
		t.Fatalf("installed dir missing: %v", err)
	}

	// 2. 不带 overwrite 重装 → 报 already exists + remove_first hint
	r2, _ := p.installFromData(buildHmap(t, "demo", "1.0.0"), false)
	m2 := r2.(map[string]interface{})
	if m2["error"] != "plugin already exists" || m2["hint"] == "" {
		t.Fatalf("expected already-exists with hint, got %v", m2)
	}
	if m2["current"] != "1.0.0" {
		t.Fatalf("current version not reported: %v", m2)
	}

	// 3. overwrite 升级 v1.0.0 → v2.0.0
	r3, _ := p.installFromData(buildHmap(t, "demo", "2.0.0"), true)
	m3 := r3.(map[string]interface{})
	if m3["status"] != "installed" || m3["action"] != "upgraded" {
		t.Fatalf("upgrade failed: %v", m3)
	}
	if m3["previous_version"] != "1.0.0" {
		t.Fatalf("previous_version = %v", m3["previous_version"])
	}
	if m3["config_kept"] != true {
		t.Fatalf("config_kept should be true: %v", m3)
	}
	// StopAndUnload 应被调用且不触发 RemovePlugin（不删配置）
	fm.mu.Lock()
	calls := append([]string{}, fm.stopAndUnloads...)
	fm.mu.Unlock()
	if len(calls) != 1 || calls[0] != "demo" {
		t.Fatalf("StopAndUnload not called once with demo: %v", calls)
	}
	// 新二进制写入
	binData, err := os.ReadFile(filepath.Join(dir, "demo", "plugin.bin"))
	if err != nil {
		t.Fatalf("read new bin: %v", err)
	}
	if string(binData) != "binary-demo-2.0.0" {
		t.Fatalf("bin not overwritten: %q", string(binData))
	}

	// 4. 降级 v2.0.0 → v1.5.0
	r4, _ := p.installFromData(buildHmap(t, "demo", "1.5.0"), true)
	m4 := r4.(map[string]interface{})
	if m4["action"] != "downgraded" {
		t.Fatalf("downgrade action = %v", m4)
	}
}

func TestExtractFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	fm := &fakePluginMgr{}
	bus := events.NewBus()
	sdkInst := sdk.New("pluginmgr", sdk.SDKConfig{
		Settings:  &pmSettings{},
		EventBus:  bus,
		PluginMgr: fm,
	})
	p := &Plugin{pluginDir: dir, sdk: sdkInst}

	// 先装 v1.0.0
	if r, _ := p.installFromData(buildHmap(t, "rollback", "1.0.0"), false); r.(map[string]interface{})["status"] != "installed" {
		t.Fatal("install failed")
	}

	// 构造损坏包：zip 但缺 plugin.json（extractPackage 会失败）
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("plugin.bin")
	f.Write([]byte("corrupt"))
	zw.Close()

	// 畸形包在 validatePackage 层就拒绝，未达 extract——模拟 extract 失败：
	// 直接注入非法平台文件触发 extractPackage 错误
	bad := buildHmap(t, "rollback", "9.9.9")
	// 篡改使 extract 失败：附加一个越界路径
	var rb bytes.Buffer
	zw2 := zip.NewWriter(&rb)
	f2, _ := zw2.Create("../../evil")
	f2.Write([]byte("x"))
	mf, _ := zw2.Create("plugin.json")
	mData, _ := json.Marshal(map[string]interface{}{"name": "rollback", "version": "9.9.9", "entry": "plugin.bin"})
	mf.Write(mData)
	zw2.Close()
	bad = rb.Bytes()

	r, _ := p.installFromData(bad, true)
	m := r.(map[string]interface{})
	if m["error"] == nil {
		t.Fatalf("expected error for corrupt package, got %v", m)
	}
	if m["rollback"] != nil {
		t.Fatalf("rollback itself failed: %v", m)
	}
	// 旧版应被恢复
	mfest, err := plugin.ReadManifest(filepath.Join(dir, "rollback"))
	if err != nil || mfest.Version != "1.0.0" {
		t.Fatalf("old version not restored: %v / %v", mfest, err)
	}
}

// ★ 升级不该顺手改变运行状态（2026-10-20 生产实证）。
//
// 现场：重装 qq 插件后它静默停着（StopAndUnload 只停不启），agent 发现工具
// 全没了，只能 get_plugin_tools ×N → plugin_start 摸回来。安装流程的语义应当是
// "换个包"，不是"把正在用的插件关掉"。
func TestUpgradeRestoresRunningState(t *testing.T) {
	dir := t.TempDir()
	fm := &fakePluginMgr{loaded: map[string]bool{}}
	bus := events.NewBus()
	sdkInst := sdk.New("pluginmgr", sdk.SDKConfig{
		Settings:  &pmSettings{},
		EventBus:  bus,
		PluginMgr: fm,
	})
	p := &Plugin{pluginDir: dir, sdk: sdkInst}

	if r, _ := p.installFromData(buildHmap(t, "demo", "1.0.0"), false); r.(map[string]interface{})["status"] != "installed" {
		t.Fatal("install failed")
	}

	// 升级前它是在跑的
	fm.mu.Lock()
	fm.loaded["demo"] = true
	fm.mu.Unlock()

	r, _ := p.installFromData(buildHmap(t, "demo", "2.0.0"), true)
	m := r.(map[string]interface{})
	if m["was_running"] != true {
		t.Fatalf("was_running 应为 true: %v", m)
	}
	if m["running_restored"] != true {
		t.Fatalf("升级后应自动拉回运行状态: %v", m)
	}
	fm.mu.Lock()
	starts := append([]string{}, fm.starts...)
	fm.mu.Unlock()
	if len(starts) != 1 || starts[0] != "demo" {
		t.Fatalf("StartPlugin 应被调用一次: %v", starts)
	}
}

// 升级前没在跑的插件，安装后也不该被强行拉起。
func TestUpgradeDoesNotStartStoppedPlugin(t *testing.T) {
	dir := t.TempDir()
	fm := &fakePluginMgr{loaded: map[string]bool{}}
	bus := events.NewBus()
	sdkInst := sdk.New("pluginmgr", sdk.SDKConfig{Settings: &pmSettings{}, EventBus: bus, PluginMgr: fm})
	p := &Plugin{pluginDir: dir, sdk: sdkInst}

	if r, _ := p.installFromData(buildHmap(t, "demo", "1.0.0"), false); r.(map[string]interface{})["status"] != "installed" {
		t.Fatal("install failed")
	}
	r, _ := p.installFromData(buildHmap(t, "demo", "2.0.0"), true)
	m := r.(map[string]interface{})
	if m["was_running"] != false || m["running_restored"] != false {
		t.Fatalf("未运行的插件不应被拉起: %v", m)
	}
	if len(fm.starts) != 0 {
		t.Fatalf("StartPlugin 不应被调用: %v", fm.starts)
	}
}
