package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func newPersonaHandler(t *testing.T) (*Handler, *internalConfig.ConfigRegistry) {
	t.Helper()
	dir := t.TempDir()
	cfgReg := internalConfig.NewConfigRegistry(filepath.Join(dir, "config.db"))
	cfgReg.SeedDefaults(dir)
	t.Cleanup(func() { cfgReg.Close() })
	h := NewHandler(testSDK(sdk.SDKConfig{Settings: sdk.NewSettings("webui", cfgReg)}))
	return h, cfgReg
}

func doPersona(t *testing.T, h *Handler, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/api/v1/persona", rd)
	w := httptest.NewRecorder()
	h.handlePersona(w, req)
	return w
}

// 首启向导的后端契约：GET 报告状态、POST 三选一、并且**只问一次**。
func TestPersonaWizardFlow(t *testing.T) {
	h, cfgReg := newPersonaHandler(t)

	// 1. 全新安装：未初始化，current_prompt 回落到内置默认模板
	w := doPersona(t, h, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET 状态码 %d", w.Code)
	}
	var got struct {
		Initialized   bool   `json:"initialized"`
		CurrentPrompt string `json:"current_prompt"`
		FileOverride  bool   `json:"file_override"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Initialized {
		t.Fatal("全新安装不应已初始化")
	}
	if got.CurrentPrompt != internalConfig.DefaultPersonaPrompt {
		t.Fatal("未设置时应回落到内置默认模板")
	}
	if got.FileOverride {
		t.Fatal("没有人格文件时不应报告 file_override")
	}

	// 2. 「稍后再说」= 保留默认、打标记、不再问
	w = doPersona(t, h, http.MethodPost, `{"mode":"later"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("later 状态码 %d: %s", w.Code, w.Body.String())
	}
	if v := cfgReg.GetString(internalConfig.PersonaInitMarkerKey, ""); v == "" {
		t.Fatal("later 也必须打一次性标记（否则每次启动都问）")
	}
	if v := cfgReg.GetString(internalConfig.PersonaPromptKey, ""); v != internalConfig.DefaultPersonaPrompt {
		t.Fatalf("later 不应改动人格，实际 %q", v)
	}

	// 3. 已初始化后 GET 应报 true
	w = doPersona(t, h, http.MethodGet, "")
	got.Initialized = false
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Initialized {
		t.Fatal("打过标记后应报告已初始化")
	}

	// 4. 自定义：写入内容 + 需要重启（人格在启动时载入）
	h2, cfgReg2 := newPersonaHandler(t)
	w = doPersona(t, h2, http.MethodPost, `{"mode":"custom","content":"你是测试人格"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("custom 状态码 %d: %s", w.Code, w.Body.String())
	}
	var pr struct {
		RestartRequired bool `json:"restart_required"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &pr)
	if !pr.RestartRequired {
		t.Fatal("自定义人格应提示需要重启才生效")
	}
	if v := cfgReg2.GetString(internalConfig.PersonaPromptKey, ""); v != "你是测试人格" {
		t.Fatalf("自定义内容未写库: %q", v)
	}

	// 5. 空内容的 custom 必须被拒（否则等于静默清空人格）
	h3, _ := newPersonaHandler(t)
	if w = doPersona(t, h3, http.MethodPost, `{"mode":"custom","content":"   "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("空内容应 400，实际 %d", w.Code)
	}
	// 6. 未知 mode 必须被拒
	if w = doPersona(t, h3, http.MethodPost, `{"mode":"nope"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("未知 mode 应 400，实际 %d", w.Code)
	}
	// 7. 被拒的请求不得打标记（否则向导会被跳过）
	if v := cfgReg2.GetString(internalConfig.PersonaInitMarkerKey, ""); v == "" {
		t.Fatal("前置条件：第 4 步已打标记")
	}
	h4, cfgReg4 := newPersonaHandler(t)
	_ = doPersona(t, h4, http.MethodPost, `{"mode":"nope"}`)
	if v := cfgReg4.GetString(internalConfig.PersonaInitMarkerKey, ""); v != "" {
		t.Fatal("被拒的请求不应打标记")
	}
}

// 存在人格文件时 GET 要报告 file_override（它会覆盖配置项，向导应提示用户）。
func TestPersonaWizardReportsFileOverride(t *testing.T) {
	h, cfgReg := newPersonaHandler(t)
	dir := cfgReg.GetString("core.daemon.data_dir", "")
	if dir == "" {
		t.Fatal("播种应写入 core.daemon.data_dir")
	}
	if err := os.MkdirAll(filepath.Join(dir, "personal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "personal", "personal.md"), []byte("旧人格"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := doPersona(t, h, http.MethodGet, "")
	var got struct {
		FileOverride bool `json:"file_override"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.FileOverride {
		t.Fatal("存在 personal.md 时必须报告 file_override")
	}
}
