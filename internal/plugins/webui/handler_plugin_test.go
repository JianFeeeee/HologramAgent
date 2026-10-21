package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// mockPluginMgr 是 sdk.PluginManager 的最小实现，用于 handler 路由层测试。
type mockPluginMgr struct {
	builtins   map[string]bool
	disabled   []sdk.DisabledPluginInfo
	isDisabled map[string]bool
	runtimes   map[string]sdk.PluginRuntimeInfo

	removed   []string
	reloadN   int
	removeErr error
}

func (m *mockPluginMgr) ListLoadedPlugins() []string { return nil }
func (m *mockPluginMgr) ListDisabledPlugins() []sdk.DisabledPluginInfo {
	return m.disabled
}
func (m *mockPluginMgr) IsPluginDisabled(name string) bool {
	return m.isDisabled[name]
}
func (m *mockPluginMgr) IsBuiltinPlugin(name string) bool {
	return m.builtins[name]
}
func (m *mockPluginMgr) DisablePlugin(name, by string) error {
	if m.isDisabled == nil {
		m.isDisabled = map[string]bool{}
	}
	m.isDisabled[name] = true
	return nil
}
func (m *mockPluginMgr) EnablePlugin(name string) error {
	delete(m.isDisabled, name)
	return nil
}
func (m *mockPluginMgr) RemovePlugin(name string) error {
	if m.removeErr != nil {
		return m.removeErr
	}
	m.removed = append(m.removed, name)
	return nil
}
func (m *mockPluginMgr) ReloadPlugins() (string, error) {
	m.reloadN++
	return "reloaded", nil
}
func (m *mockPluginMgr) ReloadOne(name string) error     { return nil }
func (m *mockPluginMgr) StopAndUnload(name string) error { return nil }

// ★ SDK 1.4.0 起 PluginManager 新增 3 个"运行期停/启"方法（与持久禁用以区分）。
// mock 必须跟上接口 —— 否则本包**编译失败**（不是判据红，是整个包挂），
// 表现为 `webui [build failed]`，容易被误读成"webui 坏了"。
func (m *mockPluginMgr) StopPlugin(name string) error            { return nil }
func (m *mockPluginMgr) StartPlugin(name string) error           { return nil }
func (m *mockPluginMgr) IsPluginRuntimeStopped(name string) bool { return false }
func (m *mockPluginMgr) PluginMetas() map[string]sdk.PluginMeta {
	return nil
}
func (m *mockPluginMgr) PluginDir() string { return "" }
func (m *mockPluginMgr) PluginRuntime(name string) (sdk.PluginRuntimeInfo, bool) {
	info, ok := m.runtimes[name]
	return info, ok
}
func (m *mockPluginMgr) ListPluginRuntimes() []sdk.PluginRuntimeInfo {
	out := make([]sdk.PluginRuntimeInfo, 0, len(m.runtimes))
	for _, v := range m.runtimes {
		out = append(out, v)
	}
	return out
}

// newHandlerWithMock 构造带 mock PluginManager 的 Handler（绕过 SDK 组装）。
func newHandlerWithMock(m *mockPluginMgr) *Handler {
	h := NewHandler(nil)
	h.pluginMgr = m
	return h
}

func TestHandlePluginByID_DisabledList(t *testing.T) {
	m := &mockPluginMgr{
		disabled: []sdk.DisabledPluginInfo{{Name: "foo"}},
	}
	h := newHandlerWithMock(m)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/disabled", nil)
	w := httptest.NewRecorder()
	h.handlePluginByID(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if body := w.Body.String(); len(body) == 0 || !contains(body, "foo") {
		t.Fatalf("expected disabled list with foo, got %s", body)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestHandlePluginByID_ReloadPost(t *testing.T) {
	m := &mockPluginMgr{}
	h := newHandlerWithMock(m)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/plugins/reload", nil)
	w := httptest.NewRecorder()
	h.handlePluginByID(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if m.reloadN != 1 {
		t.Fatalf("expected ReloadPlugins called once, got %d", m.reloadN)
	}
}

func TestHandlePluginByID_ReloadDeleteRejected(t *testing.T) {
	// DELETE /plugins/reload 不允许把保留字当插件名反代成卸载
	m := &mockPluginMgr{}
	h := newHandlerWithMock(m)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/reload", nil)
	w := httptest.NewRecorder()
	h.handlePluginByID(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	if len(m.removed) != 0 {
		t.Fatalf("expected no plugin removed, got %v", m.removed)
	}
}

func TestHandlePluginByID_PathTraversalRejected(t *testing.T) {
	cases := []string{"../evil", "a/b", "a..b-ok-but-dots-only-check", "..%2Fetc"}
	for _, name := range cases {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/"+name, nil)
		w := httptest.NewRecorder()
		h := newHandlerWithMock(&mockPluginMgr{})
		h.handlePluginByID(w, req)
		// 含路径分隔符或以 .. 开头的名称必须被拒绝（400/405），绝不能反代到 pluginmgr
		if w.Code != http.StatusBadRequest && w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
			t.Errorf("name %q: expected 4xx rejection, got %d", name, w.Code)
		}
	}
}

func TestHandlePluginByID_InvalidNamesRejected(t *testing.T) {
	cases := []string{"has%20space", "has%3Acolon", "back%5Cslash"}
	for _, name := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/"+name, nil)
		w := httptest.NewRecorder()
		h := newHandlerWithMock(&mockPluginMgr{})
		h.handlePluginByID(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("name %q: expected 400, got %d", name, w.Code)
		}
	}
}

func TestHandlePluginByID_ValidNamePassesValidation(t *testing.T) {
	// 合法插件名（含点/横线/下划线）不应被名称校验拦截；
	// 这里 pluginmgr 未运行会得到 502 Bad Gateway，但绝不应该是 400。
	m := &mockPluginMgr{}
	h := newHandlerWithMock(m)

	for _, name := range []string{"my-plugin", "plugin_v2", "weather.so"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/"+name, nil)
		w := httptest.NewRecorder()
		h.handlePluginByID(w, req)
		if w.Code == http.StatusBadRequest {
			t.Errorf("valid name %q should pass validation, got 400", name)
		}
	}
}

func TestHandlePluginByID_DisableEnable(t *testing.T) {
	m := &mockPluginMgr{builtins: map[string]bool{"webui": true}}
	h := newHandlerWithMock(m)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/plugins/webui/disable", nil)
	w := httptest.NewRecorder()
	h.handlePluginByID(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("disable: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !m.isDisabled["webui"] {
		t.Fatal("webui should be disabled in mock")
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/plugins/webui/enable", nil)
	w = httptest.NewRecorder()
	h.handlePluginByID(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("enable: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if m.isDisabled["webui"] {
		t.Fatal("webui should be re-enabled")
	}
}
