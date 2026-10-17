package skillmgr

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JianFeeeee/HomeAgent/internal/events"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// ---- 测试辅助 ----

type nilSettings struct{}

func (m *nilSettings) DataDir() string { return "/tmp/mock_data" }

func (m *nilSettings) Get(string) (interface{}, error)     { return nil, nil }
func (m *nilSettings) Set(string, interface{}) error       { return nil }
func (m *nilSettings) SetSecret(string, interface{}) error { return nil }
func (m *nilSettings) GetSecret(string) (string, error)    { return "", nil }

// IsSecret：E-3 起 SettingsAPI 要求实现（判断某键是否按密文存储）。
func (m *nilSettings) IsSecret(string) bool                          { return false }
func (m *nilSettings) List(string) ([]string, error)                 { return nil, nil }
func (m *nilSettings) GetCore(string) (interface{}, error)           { return nil, nil }
func (m *nilSettings) SetCore(string, interface{}) error             { return nil }
func (m *nilSettings) ListCore(string) ([]string, error)             { return nil, nil }
func (m *nilSettings) GetPlugin(string, string) (interface{}, error) { return nil, nil }
func (m *nilSettings) SetPlugin(string, string, interface{}) error   { return nil }
func (m *nilSettings) ListPlugin(string, string) ([]string, error)   { return nil, nil }
func (m *nilSettings) RegisterDef(pubsdk.ConfigDef)                  {}
func (m *nilSettings) Defs(string) []*pubsdk.ConfigDef               { return nil }
func (m *nilSettings) Dump() map[string]interface{}                  { return nil }
func (m *nilSettings) Plugins() []string                             { return nil }
func (m *nilSettings) DefsCore(string) []*sdk.ConfigDef              { return nil }
func (m *nilSettings) DefsPlugin(string, string) []*sdk.ConfigDef    { return nil }
func (m *nilSettings) Remove(string) error                           { return nil }
func (m *nilSettings) RemoveCore(string) error                       { return nil }
func (m *nilSettings) RemovePlugin(string, string) error             { return nil }

// toolSpy 记录注册的工具，并可按名调用 handler。
type toolSpy struct {
	mu       sync.Mutex
	handlers map[string]func(args map[string]interface{}) (interface{}, error)
}

func newToolSpy() *toolSpy {
	return &toolSpy{handlers: map[string]func(map[string]interface{}) (interface{}, error){}}
}

func (s *toolSpy) Register(name string, def interface{}, h func(args map[string]interface{}) (interface{}, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[name] = h
	return nil
}

func (s *toolSpy) call(t *testing.T, name string, args map[string]interface{}) (interface{}, error) {
	t.Helper()
	s.mu.Lock()
	h, ok := s.handlers[name]
	s.mu.Unlock()
	if !ok {
		t.Fatalf("tool %q not registered", name)
	}
	return h(args)
}

// newTestPlugin 构造带真实事件总线的 skillmgr 实例。
func newTestPlugin(t *testing.T) (*Plugin, *toolSpy, *events.Bus) {
	t.Helper()
	dir := t.TempDir()
	bus := events.NewBus()
	p := New("skillmgr", dir)

	reg := func(name string, def interface{}, h func(args map[string]interface{}) (interface{}, error)) error {
		_ = name
		_ = def
		_ = h
		return nil
	}
	_ = reg // 占位避免误用

	spy := newToolSpy()
	cfgSDK := sdk.New("skillmgr", sdk.SDKConfig{
		RegTool: func(name string, def sdk.ToolDef, h sdk.ToolHandler) error {
			return spy.Register(name, def, func(args map[string]interface{}) (interface{}, error) {
				return h(args)
			})
		},
		Settings: &nilSettings{},
		EventBus: bus,
	})
	if err := p.Start(cfgSDK); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { p.Stop() })
	return p, spy, bus
}

func writeSkill(t *testing.T, dir, name, description string) string {
	t.Helper()
	sd := filepath.Join(dir, name)
	os.MkdirAll(sd, 0755)
	content := "---\nname: " + name + "\nversion: 1.0.0\n---\n\n# " + name + "\n\n" + description + "\n"
	if err := os.WriteFile(filepath.Join(sd, SkillFileName), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return sd
}

const sampleSkillContent = `---
name: demo-skill
version: 0.2.0
author: tester
---

# demo-skill

用于测试的演示技能。

## 使用时机

测试时使用。

## 操作步骤

### step-1

第一步说明。

- target: 目标参数

执行示例：

` + "```bash\necho demo\n```" + `
`

// ---- 用例 ----

func TestScanExistingLoadsPureSkillsOnly(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "alpha desc")
	writeSkill(t, dir, "beta", "beta desc")

	// sidecar 目录（main.js）应被跳过
	scDir := filepath.Join(dir, "sidecar-thing")
	os.MkdirAll(scDir, 0755)
	os.WriteFile(filepath.Join(scDir, "main.js"), []byte("//"), 0644)

	// OC plugin 目录应被跳过
	ocDir := filepath.Join(dir, "oc-thing")
	os.MkdirAll(ocDir, 0755)
	os.WriteFile(filepath.Join(ocDir, "openclaw.plugin.json"), []byte("{}"), 0644)

	// 隐藏目录跳过
	hd := filepath.Join(dir, ".hidden")
	os.MkdirAll(hd, 0755)
	os.WriteFile(filepath.Join(hd, SkillFileName), []byte("x"), 0644)

	p := New("skillmgr", dir)
	bus := events.NewBus()
	sdkInst := sdk.New("skillmgr", sdk.SDKConfig{
		Settings: &nilSettings{},
		EventBus: bus,
	})
	if err := p.Start(sdkInst); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	names := map[string]bool{}
	for _, e := range p.snapshot() {
		names[e.sk.Name()] = true
	}
	if !names["alpha"] || !names["beta"] {
		t.Fatalf("expected alpha+beta loaded, got %v", names)
	}
	if names["sidecar-thing"] || names["oc-thing"] || names[".hidden"] {
		t.Fatalf("sidecar/OC/hidden should not load, got %v", names)
	}
}

func TestSkillDetectedEventHandoff(t *testing.T) {
	p, _, bus := newTestPlugin(t)

	// 模拟 clawhubadapter 移交事件
	dir := p.skillsDir
	sd := filepath.Join(dir, "handed")
	os.MkdirAll(sd, 0755)
	os.WriteFile(filepath.Join(sd, SkillFileName), []byte(sampleSkillContent), 0644)

	bus.Publish(&events.Event{
		Type:      detectEvent,
		Source:    "clawhubadapter",
		Payload:   map[string]interface{}{"path": sd},
		Timestamp: time.Now().Unix(),
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := p.get("handed"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := p.get("handed"); !ok {
		t.Fatal("handed-off skill not registered after event")
	}
}

func TestCreateValidateLoadFlow(t *testing.T) {
	p, spy, _ := newTestPlugin(t)

	// 两步式：先骨架
	out, err := spy.call(t, "skill_create", map[string]interface{}{
		"name":        "weather-notify",
		"description": "天气通知",
	})
	if err != nil {
		t.Fatalf("create skeleton: %v", err)
	}
	_ = out
	if !fileExists(filepath.Join(p.skillsDir, "weather-notify", SkillFileName)) {
		t.Fatal("skeleton SKILL.md missing")
	}

	// 第二步：完整内容覆盖 → 自动加载
	if _, err := spy.call(t, "skill_create", map[string]interface{}{
		"name":    "weather-notify",
		"content": sampleSkillContent,
	}); err != nil {
		t.Fatalf("create full: %v", err)
	}
	if _, ok := p.get("weather-notify"); !ok {
		t.Fatal("full-content skill not loaded under its own name")
	}

	// 空白 content 走模板分支：生成骨架而非报错
	if _, err := spy.call(t, "skill_create", map[string]interface{}{
		"name":        "bad-skill",
		"description": "",
	}); err != nil {
		t.Fatalf("skeleton-only create should succeed: %v", err)
	}
	if !fileExists(filepath.Join(p.skillsDir, "bad-skill", SkillFileName)) {
		t.Fatal("bad-skill skeleton missing")
	}

	// 有内容但缺描述的 content 被校验拒绝
	if _, err := spy.call(t, "skill_create", map[string]interface{}{
		"name":    "no-desc",
		"content": "---\nname: no-desc\n---\n\n## 步骤\n",
	}); err == nil {
		t.Fatal("content without description should be rejected by ValidateSKILLContent")
	}

	// 非法名称被拒
	if _, err := spy.call(t, "skill_create", map[string]interface{}{"name": "../evil"}); err == nil {
		t.Fatal("path traversal name should be rejected")
	}
}

func TestExportInstallRoundTrip(t *testing.T) {
	p, spy, _ := newTestPlugin(t)

	// 先创建并加载一个 skill
	if _, err := spy.call(t, "skill_create", map[string]interface{}{
		"name":    "roundtrip",
		"content": strings.ReplaceAll(sampleSkillContent, "demo-skill", "roundtrip"),
	}); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(p.skillsDir, "..", "exports")
	packPath := filepath.Join(outDir, "roundtrip.skm")
	if _, err := spy.call(t, "skill_export", map[string]interface{}{"name": "roundtrip"}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if !fileExists(packPath) {
		t.Fatalf("pack missing at %s", packPath)
	}

	// 卸载 + 删文件后从包重装
	if _, err := spy.call(t, "skill_unload", map[string]interface{}{"name": "roundtrip", "delete_files": true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.get("roundtrip"); ok {
		t.Fatal("skill should be unloaded")
	}
	if fileExists(filepath.Join(p.skillsDir, "roundtrip")) {
		t.Fatal("files should be deleted")
	}

	if _, err := spy.call(t, "skill_install", map[string]interface{}{"source": packPath}); err != nil {
		t.Fatalf("install: %v", err)
	}
	e, ok := p.get("roundtrip")
	if !ok {
		t.Fatal("reinstalled skill not loaded")
	}
	if e.sk.Version() != "0.2.0" {
		t.Fatalf("unexpected version %q", e.sk.Version())
	}
}

func TestEnableDisable(t *testing.T) {
	p, spy, _ := newTestPlugin(t)
	writeSkill(t, p.skillsDir, "toggle", "toggle desc")
	p.scanExisting()

	if _, err := spy.call(t, "skill_disable", map[string]interface{}{"name": "toggle"}); err != nil {
		t.Fatal(err)
	}
	if e, _ := p.get("toggle"); e.enabled {
		t.Fatal("should be disabled")
	}
	if _, err := spy.call(t, "skill_enable", map[string]interface{}{"name": "toggle"}); err != nil {
		t.Fatal(err)
	}
	if e, _ := p.get("toggle"); !e.enabled {
		t.Fatal("should be enabled")
	}
}

func TestPackTarSlipRejected(t *testing.T) {
	tmp := t.TempDir()

	// 构造带 .. 逃逸条目的恶意包
	packPath := filepath.Join(tmp, "evil.skm")
	f, _ := os.Create(packPath)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "../../../evil.txt", Mode: 0644, Size: 3}
	tw.WriteHeader(hdr)
	tw.Write([]byte("bad"))
	tw.Close()
	gz.Close()
	f.Close()

	dst := filepath.Join(tmp, "out")
	if _, err := unpackSkill(packPath, dst); err == nil {
		t.Fatal("tar slip entry must be rejected")
	}
}

func TestSkillIndex(t *testing.T) {
	p, spy, _ := newTestPlugin(t)

	if idx := p.SkillIndex(); idx != "" {
		t.Fatalf("empty registry should give empty index, got %q", idx)
	}

	if _, err := spy.call(t, "skill_create", map[string]interface{}{
		"name":    "idx-test",
		"content": strings.ReplaceAll(sampleSkillContent, "demo-skill", "idx-test"),
	}); err != nil {
		t.Fatal(err)
	}
	idx := p.SkillIndex()
	if !strings.Contains(idx, "idx-test v0.2.0") || !strings.Contains(idx, "用于测试的演示技能。") {
		t.Fatalf("index missing name/desc: %q", idx)
	}

	// 禁用后不出现在索引里
	spy.call(t, "skill_disable", map[string]interface{}{"name": "idx-test"})
	if idx := p.SkillIndex(); strings.Contains(idx, "idx-test") {
		t.Fatalf("disabled skill should be excluded from index: %q", idx)
	}
}
