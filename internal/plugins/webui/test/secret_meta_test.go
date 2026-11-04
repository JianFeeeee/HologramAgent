package test

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
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/plugins/webui"
	internalSDK "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func TestSecretFlagReachesSettingsMeta(t *testing.T) {
	dir := t.TempDir()
	reg := internalConfig.NewConfigRegistry(dir + "/config.db")
	reg.SetCodec(internalConfig.NewTestCodec(t))
	defer reg.Close()
	seedWebUIConfig(reg)

	// 插件用**两种写法**各声明一个敏感项
	sett := internalSDK.NewSettings("secplug", reg)
	sett.RegisterDef(internalSDK.ConfigDef{
		Key: "by_flag", Type: "string", DisplayName: "标志式", Secret: true,
	})
	sett.RegisterDef(internalSDK.ConfigDef{
		Key: "by_type", Type: "password", DisplayName: "类型式",
	})

	// 走**真实 HTTP 处理器**：NewHandler + Handler() 都是导出接口，
	// 请求经完整路由分发，不直接调未导出的 handleSettings。
	h := webui.NewHandler(internalSDK.New("webui", internalSDK.SDKConfig{
		Settings: internalSDK.NewSettings("webui", reg),
		EventBus: events.NewBus(),
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings?prefix=plugin.secplug", nil)
	w := httptest.NewRecorder()
	serveRoutes(t, h, w, req)
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
	reg := internalConfig.NewConfigRegistry(dir + "/config.db")
	reg.SetCodec(internalConfig.NewTestCodec(t))
	defer reg.Close()
	seedWebUIConfig(reg)

	sett := internalSDK.NewSettings("webui", reg)
	sett.RegisterDef(internalSDK.ConfigDef{Key: "apikey", Type: "password"})
	if err := sett.Set("apikey", "ui-visible-secret-123456"); err != nil {
		t.Fatal(err)
	}

	h := webui.NewHandler(internalSDK.New("webui", internalSDK.SDKConfig{
		Settings: internalSDK.NewSettings("webui", reg),
		EventBus: events.NewBus(),
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings?prefix=plugin.webui", nil)
	w := httptest.NewRecorder()
	serveRoutes(t, h, w, req)

	if strings.Contains(w.Body.String(), "ui-visible-secret-123456") {
		t.Fatal("E-3：设置列表不应回显敏感明文")
	}
	if !strings.Contains(w.Body.String(), webui.SecretPlaceholder) {
		t.Fatal("设置列表应显示敏感值占位符")
	}
	// 显式揭示仍可核对（单键、当前 WebUI 插件范围）。
	reveal := httptest.NewRecorder()
	serveRoutes(t, h, reveal, httptest.NewRequest(http.MethodGet,
		"/api/v1/settings/reveal?key=plugin.webui.apikey", nil))
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

// serveRoutes 通过**导出的 RegisterRoutes** 建一个只含业务路由的 mux，
// 跳过 logged / gzipMW / proxyDispatch 三层中间件。
//
// 为什么不用 Handler()：那三层中间件依赖完整装配（history store、settings
// 里的 webui username/password、data dir 等）。本判据测的是 settings 的
// meta 与 reveal，不是请求日志与反代 —— 用全链路会把无关依赖拖进来，
// 一个缺配置就 nil 解引用，红的不是被测对象。
//
// RegisterRoutes 是导出的，所以这仍是**黑箱**：拿到的是真实路由表，
// 只是不带外层中间件。
func serveRoutes(t *testing.T, h *webui.Handler, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	// 带 API key 通过 requireAPI（这是生产上非浏览器客户端的正常路径，
	// 见 requireWeb/requireAPI 里 validAPIKey 优先于 cookie session）。
	r.Header.Set("X-API-Key", testAPIKey)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	mux.ServeHTTP(w, r)
}
