package webui

// 判据：**Secret 标志必须活着到达 WebUI 的 meta**。
//
// 由来：SDK 的 ConfigDef 一直有 Secret 字段，但内核侧三处转换
// （settingsImpl.RegisterDef / Defs / mapDefs）全部漏掉了它 ——
// 插件声明了 secret，前端拿到的 meta 里却没有这个标记，
// 于是敏感项被渲染成普通文本框明文回显。
//
// 这条判据钉住「声明 → meta」这一段，防止再次被漏掉。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	internalSDK "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func TestSecretFlagReachesSettingsMeta(t *testing.T) {
	dir := t.TempDir()
	internalConfig.ResetTestCodec(t)
	reg := internalConfig.NewConfigRegistry(dir + "/config.db")
	defer reg.Close()

	// 插件用**两种写法**各声明一个敏感项
	sett := internalSDK.NewSettings("secplug", reg)
	sett.RegisterDef(internalSDK.ConfigDef{
		Key: "by_flag", Type: "string", DisplayName: "标志式", Secret: true,
	})
	sett.RegisterDef(internalSDK.ConfigDef{
		Key: "by_type", Type: "password", DisplayName: "类型式",
	})

	// 走真实 HTTP 处理器
	h := &Handler{settings: internalSDK.NewSettings("webui", reg)}
	req := httptest.NewRequest(http.MethodGet, "/api/settings?prefix=plugin.secplug", nil)
	w := httptest.NewRecorder()
	h.handleSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		Meta map[string]map[string]interface{} `json:"meta"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	byFlag, ok := body.Meta["plugin.secplug.by_flag"]
	if !ok {
		t.Fatalf("meta 里没有 by_flag；实际键: %v", keysOfMeta(body.Meta))
	}
	if byFlag["secret"] != true {
		t.Errorf("Secret:true 未传递到 meta（前端会明文回显）: %v", byFlag)
	}
	byType, ok := body.Meta["plugin.secplug.by_type"]
	if !ok {
		t.Fatalf("meta 里没有 by_type；实际键: %v", keysOfMeta(body.Meta))
	}
	if byType["type"] != "password" {
		t.Errorf("password 类型应保持: %v", byType)
	}
}

func keysOfMeta(m map[string]map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 判据：敏感项在 WebUI 列表里的默认值**必须屏蔽**（E-3），
// 单键 reveal 才能看到明文；磁盘上仍是密文。
func TestSecretPlainInUIButCipherOnDisk(t *testing.T) {
	dir := t.TempDir()
	internalConfig.ResetTestCodec(t)
	reg := internalConfig.NewConfigRegistry(dir + "/config.db")
	defer reg.Close()

	sett := internalSDK.NewSettings("webui", reg)
	sett.RegisterDef(internalSDK.ConfigDef{Key: "apikey", Type: "password"})
	if err := sett.Set("apikey", "ui-visible-secret-123456"); err != nil {
		t.Fatal(err)
	}

	h := &Handler{settings: internalSDK.NewSettings("webui", reg)}
	req := httptest.NewRequest(http.MethodGet, "/api/settings?prefix=plugin.webui", nil)
	w := httptest.NewRecorder()
	h.handleSettings(w, req)

	if strings.Contains(w.Body.String(), "ui-visible-secret-123456") {
		t.Fatal("E-3：设置列表不应回显敏感明文")
	}
	if !strings.Contains(w.Body.String(), SecretPlaceholder) {
		t.Fatal("设置列表应显示敏感值占位符")
	}
	// 显式揭示仍可核对（单键、当前 WebUI 插件范围）。
	reveal := httptest.NewRecorder()
	h.handleSettingsReveal(reveal, httptest.NewRequest(http.MethodGet, "/api/settings/reveal?key=plugin.webui.apikey", nil))
	if reveal.Code != http.StatusOK || !strings.Contains(reveal.Body.String(), "ui-visible-secret-123456") {
		t.Fatalf("显式 reveal 应返回当前插件的明文: status=%d body=%s", reveal.Code, reveal.Body.String())
	}
	var raw string
	if err := reg.DB().QueryRow(
		`SELECT value FROM config_webui WHERE key='apikey'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "ui-visible-secret") {
		t.Errorf("磁盘上不应有明文: %q", raw)
	}
}
