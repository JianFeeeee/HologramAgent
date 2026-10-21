package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// proxyTestSettings 造一份带 api_key 的 webui 插件设置，供受保护路由用
// X-API-Key 通过鉴权（真实部署里浏览器走门户会话，脚本走 key）。
func proxyTestSettings(t *testing.T) *sdk.PluginSDK {
	t.Helper()
	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)
	return testSDK(sdk.SDKConfig{Settings: sdk.NewSettings("webui", cfgReg)})
}

// authHeaders 给受保护路由的测试请求带上凭证。
func authHeaders(r *http.Request) {
	r.Header.Set("X-API-Key", "test-api-key")
}

// ---- 子域标签抽取 ----

func TestProxyHostLabel(t *testing.T) {
	cases := []struct {
		host, base, want string
	}{
		// 默认基座：*.localhost（零配置可用）
		{"huawei.localhost:8080", "localhost", "huawei"},
		{"huawei.localhost", "localhost", "huawei"},
		{"HUAWEI.LOCALHOST:8080", "localhost", "huawei"}, // 大小写不敏感
		// 基域名自身不是插件路由
		{"localhost:8080", "localhost", ""},
		{"localhost", "localhost", ""},
		// 自定义基域名
		{"devices.webui.example.com", "webui.example.com", "devices"},
		{"webui.example.com", "webui.example.com", ""},
		// 多级子域不接（避免"哪个是插件"含混）
		{"a.b.webui.example.com", "webui.example.com", ""},
		// 非本基域名的 Host 不接
		{"evil.com", "webui.example.com", ""},
		{"notlocalhost.com", "localhost", ""},
		// 边界/异常输入
		{"", "localhost", ""},
		{"   ", "localhost", ""},
		{".localhost:8080", "localhost", ""},
		{"..localhost", "localhost", ""},
		// 后缀但标签为空
		{".webui.example.com", "webui.example.com", ""},
	}
	for _, c := range cases {
		if got := proxyHostLabel(c.host, c.base); got != c.want {
			t.Errorf("proxyHostLabel(%q, %q) = %q，期望 %q", c.host, c.base, got, c.want)
		}
	}
}

// ---- 手填条目解析 ----

func TestParseManualRoutes(t *testing.T) {
	text := `
# 注释行
grafana 127.0.0.1:3000
devices 127.0.0.1:9890 ws auth=none

prom http://127.0.0.1:9090
`
	got := parseManualRoutes(text)
	if len(got) != 3 {
		t.Fatalf("解析出 %d 条，期望 3：%+v", len(got), got)
	}
	if got[0].Host != "grafana" || got[0].Target != "127.0.0.1:3000" || got[0].WebSocket {
		t.Errorf("第 1 条不对: %+v", got[0])
	}
	if got[1].Host != "devices" || !got[1].WebSocket || got[1].Auth != "none" {
		t.Errorf("第 2 条没解析出 ws/auth=none: %+v", got[1])
	}
	if got[2].Target != "http://127.0.0.1:9090" {
		t.Errorf("第 3 条带 scheme 的地址被破坏: %+v", got[2])
	}
	// 空白与注释行不得产出条目
	if len(parseManualRoutes("\n\n#x\n")) != 0 {
		t.Error("空行/注释行不应产出条目")
	}
	// 大写标签归一化为小写
	if r := parseManualRoutes("GRAFANA 127.0.0.1:3000"); len(r) != 1 || r[0].Host != "grafana" {
		t.Errorf("标签未归一化为小写: %+v", r)
	}
}

// ---- 路由表构建：冲突不得静默覆盖 ----

func TestBuildProxyTableConflictNotSilentlyOverridden(t *testing.T) {
	decls := []proxyDecl{
		{Plugin: "a", Name: "ui", Host: "dup", Target: "127.0.0.1:1001"},
		{Plugin: "b", Name: "ui", Host: "dup", Target: "127.0.0.1:1002"},
	}
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl { return decls })
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	tbl := currentProxyTable()
	// 先到者占住标签
	r, ok := tbl.routes["dup"]
	if !ok {
		t.Fatal("先声明的条目应占住标签")
	}
	if r.Plugin != "a" {
		t.Errorf("标签被后者覆盖了：当前属于 %s，期望 a（后者应被判冲突）", r.Plugin)
	}
	// 后者必须仍然可见（带错误），而不是静默消失
	var loser *ProxyRoute
	for _, x := range tbl.ordered {
		if x.Plugin == "b" {
			loser = x
		}
	}
	if loser == nil {
		t.Fatal("冲突的后者从表里消失了——用户将无法察觉两个插件撞了标签")
	}
	if loser.Err == "" {
		t.Error("冲突条目应带错误说明")
	}
}

// ---- 非法声明不得进路由，但必须可见 ----

func TestBuildProxyTableKeepsInvalidVisible(t *testing.T) {
	decls := []proxyDecl{
		{Plugin: "bad", Name: "x", Host: "bad_host", Target: "127.0.0.1:1"}, // host 非法
		{Plugin: "good", Name: "y", Host: "good", Target: "127.0.0.1:2"},
	}
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl { return decls })
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	tbl := currentProxyTable()
	if _, ok := tbl.routes["bad_host"]; ok {
		t.Error("非法声明不应参与路由")
	}
	if _, ok := tbl.routes["good"]; !ok {
		t.Error("合法声明应参与路由")
	}
	var bad *ProxyRoute
	for _, x := range tbl.ordered {
		if x.Plugin == "bad" {
			bad = x
		}
	}
	if bad == nil || bad.Err == "" {
		t.Error("非法声明必须留在表里并带原因（配置页要能看见）")
	}
}

// ---- Host 分发端到端：这是「通用反代」的核心行为 ----

func TestServeProxyHostRoutesByHost(t *testing.T) {
	var gotPath, gotXFF, gotProto, gotCookie string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotProto = r.Header.Get("X-Forwarded-Proto")
		gotCookie = r.Header.Get("Cookie")
		w.WriteHeader(200)
		w.Write([]byte("from-upstream"))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{Plugin: "demo", Name: "ui", Host: "demo", Target: up.Listener.Addr().String()}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.Host = "demo.localhost:8080"
	authHeaders(r)
	r.Header.Set("Cookie", "homeagent_session=SECRET")
	if !h.serveProxyHost(rec, r) {
		t.Fatal("插件子域的请求应被反代处理")
	}
	if rec.Code != 200 || rec.Body.String() != "from-upstream" {
		t.Fatalf("反代未透传：code=%d body=%q", rec.Code, rec.Body.String())
	}
	// ★ 插件前端的根绝对路径必须原样到达上游（这正是选 Host 路由的理由）
	if gotPath != "/api/status" {
		t.Errorf("上游收到的路径 = %q，期望 /api/status（根路径必须原样保留）", gotPath)
	}
	if gotXFF == "" {
		t.Error("未注入 X-Forwarded-For（旧实现缺失项）")
	}
	if gotProto == "" {
		t.Error("未注入 X-Forwarded-Proto（旧实现缺失项）")
	}
	// 门户 cookie 不得泄漏给上游
	if strings.Contains(gotCookie, "SECRET") {
		t.Errorf("门户会话 cookie 被泄漏给上游: %q", gotCookie)
	}
}

// 未声明的子域必须明确报错，而不是静默落到主站页面（那会让用户以为地址对了）
func TestServeProxyHostUnknownLabel(t *testing.T) {
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	h := NewHandler(nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "nosuchplugin.localhost:8080"
	if !h.serveProxyHost(rec, r) {
		t.Fatal("插件子域（即使未声明）应由反代层应答")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未声明子域应返回 404，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "nosuchplugin") {
		t.Error("错误信息应含具体标签，便于排错")
	}
}

// 主站 Host 不得被反代层截走
func TestServeProxyHostMainSiteUntouched(t *testing.T) {
	h := NewHandler(nil)
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "localhost:8080"
	if h.serveProxyHost(httptest.NewRecorder(), r) {
		t.Error("主站 Host 不应被反代层处理")
	}
}

// ---- WebSocket 必须显式声明 ----

func TestProxyWebSocketRequiresDeclaration(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{
			{Plugin: "nws", Name: "ui", Host: "nws", Target: "127.0.0.1:1", WebSocket: false},
			{Plugin: "wsx", Name: "gw", Host: "wsx", Target: "127.0.0.1:1", WebSocket: true},
		}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(nil)
	// 未声明 ws：升级请求必须被明确拒绝（400），而不是当普通请求透传
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ws", nil)
	r.Host = "nws.localhost:8080"
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	h.serveProxyHost(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未声明 websocket 的升级请求应 400，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "websocket") {
		t.Error("拒绝原因应说明缺 websocket 声明")
	}
}

// ---- 认证可声明：none 直通，homeagent 拦截 ----

func TestProxyAuthDeclaration(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{
			{Plugin: "open", Name: "ui", Host: "open", Target: up.Listener.Addr().String(), Auth: sdk.ProxyAuthNone},
			{Plugin: "prot", Name: "ui", Host: "prot", Target: up.Listener.Addr().String(), Auth: sdk.ProxyAuthHomeAgent},
			{Plugin: "dflt", Name: "ui", Host: "dflt", Target: up.Listener.Addr().String()}, // 空 = 受保护
		}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(nil)

	// auth=none：匿名也必须通（设备链路的前提）
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "open.localhost:8080"
	h.serveProxyHost(rec, r)
	if rec.Code != 200 {
		t.Errorf("auth=none 应放行匿名请求，实际 %d", rec.Code)
	}

	// 显式 homeagent 与默认（空）：无凭证必须 401
	for _, host := range []string{"prot.localhost:8080", "dflt.localhost:8080"} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		h.serveProxyHost(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 无凭证应 401，实际 %d", host, rec.Code)
		}
		var body map[string]string
		json.Unmarshal(rec.Body.Bytes(), &body)
		if body["hint"] == "" {
			t.Errorf("%s 的 401 应给出可操作提示", host)
		}
	}
}

// ---- 上游 3xx 必须原样透传（不得由反代跟随）----

func TestProxyDoesNotFollowUpstreamRedirect(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/go" {
			// 指向内网敏感地址：旧实现（http.DefaultClient）会跟过去并把
			// 内网 URL 泄给客户端 / 或回 502
			http.Redirect(w, r, "http://127.0.0.1:1/internal-secret", http.StatusFound)
			return
		}
		w.Write([]byte("leaked"))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{Plugin: "demo", Name: "ui", Host: "demo", Target: up.Listener.Addr().String()}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/go", nil)
	r.Host = "demo.localhost:8080"
	authHeaders(r)
	h.serveProxyHost(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("上游 302 应原样透传，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if hits != 1 {
		t.Errorf("反代跟随了重定向（上游被打了 %d 次，应为 1）", hits)
	}
	if strings.Contains(rec.Body.String(), "leaked") {
		t.Error("反代跟随重定向后把内网响应体返回给了客户端")
	}
}

// ---- 流式响应必须逐帧下发（不得缓冲到上游关闭）----
//
// 判据设计说明（这里踩过一次坑，记下来）：
// httputil.ReverseProxy 对 **text/event-stream** 与 **ContentLength = -1**
// （chunked）的响应会自动立即 flush，与 FlushInterval 无关。所以只测这两种，
// 判据是**假的**——把 FlushInterval 改成 0（关掉）也照样绿，变异验证抓不到。
// 真正依赖 FlushInterval 的是「**已知 Content-Length** 但分片慢速下发」的响应
// （长轮询、带进度的大文件）。本判据专门构造这种响应。
func TestProxyStreamsIncrementallyWithKnownLength(t *testing.T) {
	const total = 32
	release := make(chan struct{})
	started := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 显式 Content-Length：让 ReverseProxy 不走"自动立即 flush"分支，
		// 从而真正检验 FlushInterval 设置。
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(200)
		w.Write([]byte("first-chunk-here"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		<-release // 首片发出后阻塞：若反代缓冲，客户端读不到第一片
		w.Write([]byte(strings.Repeat("x", total-len("first-chunk-here"))))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{Plugin: "demo", Name: "ui", Host: "demo", Target: up.Listener.Addr().String()}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(&proxyTestHandler{h: NewHandler(proxyTestSettings(t))}).ServeHTTP(w, r)
	}))
	defer front.Close()

	req, _ := http.NewRequest("GET", front.URL+"/stream", nil)
	req.Host = "demo.localhost:8080"
	req.Header.Set("X-API-Key", "test-api-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	<-started
	buf := make([]byte, 64)
	got := make(chan int, 1)
	go func() {
		n, _ := resp.Body.Read(buf)
		got <- n
	}()
	select {
	case n := <-got:
		if n <= 0 {
			t.Fatalf("读第一片失败: n=%d", n)
		}
		if !strings.Contains(string(buf[:n]), "first-chunk-here") {
			t.Fatalf("第一片内容异常: %q", string(buf[:n]))
		}
	case <-timeAfter(2):
		close(release)
		t.Fatal("反代缓冲了响应：上游首片已 flush 且 Content-Length 已知，客户端却读不到（FlushInterval 未设为立即）")
	}
	close(release)
}

// proxyTestHandler 只暴露反代 Host 分发，避免测试依赖 requireWeb 的登录态。
type proxyTestHandler struct{ h *Handler }

func (p *proxyTestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.h.serveProxyHost(w, r) {
		return
	}
	http.NotFound(w, r)
}

// ---- 服务入口清单（给前端渲染选项卡）----

func TestListProxyServicesIncludesURLAndErrors(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{
			{Plugin: "huawei_smarthome", Name: "ui", Host: "huawei-smarthome", Target: "127.0.0.1:12100"},
			{Plugin: "broken", Name: "x", Host: "bad host", Target: "127.0.0.1:1"},
		}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	svcs := h.listProxyServices("http", ":8080", "localhost:8080", "localhost")
	if len(svcs) != 2 {
		t.Fatalf("入口数 = %d，期望 2（含坏条目）", len(svcs))
	}
	var good, bad *proxyServiceEntry
	for i := range svcs {
		if svcs[i].Plugin == "huawei_smarthome" {
			good = &svcs[i]
		}
		if svcs[i].Plugin == "broken" {
			bad = &svcs[i]
		}
	}
	if good == nil || !good.OK {
		t.Fatalf("合法声明应 OK: %+v", good)
	}
	// 入口链接必须可直接点击：带 scheme、子域标签、同一端口
	want := "http://huawei-smarthome.localhost:8080"
	if good.URL != want {
		t.Errorf("服务入口 URL = %q，期望 %q", good.URL, want)
	}
	if bad == nil || bad.OK || bad.Error == "" {
		t.Errorf("坏条目必须带错误且 OK=false: %+v", bad)
	}
	if bad.URL != "" {
		t.Error("坏条目不应给出可点链接")
	}
}

func TestHandleProxyServicesJSON(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{Plugin: "demo", Name: "ui", Host: "demo", Target: "127.0.0.1:1"}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/proxy/services", nil)
	r.Host = "localhost:8080"
	authHeaders(r)
	h.handleProxyServices(rec, r)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var out struct {
		Services   []proxyServiceEntry `json:"services"`
		BaseDomain string              `json:"base_domain"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.BaseDomain != "localhost" {
		t.Errorf("base_domain = %q，期望 localhost（零配置默认）", out.BaseDomain)
	}
	// 上游 127.0.0.1:1 不可达 → 必须如实标出不 OK，而不是假装可用
	if len(out.Services) != 1 || out.Services[0].OK {
		t.Errorf("不可达上游应标 OK=false: %+v", out.Services)
	}
}

// 服务入口链接在 https 反代场景下必须换成 https（否则远程点不开）
func TestProxySchemeAndPortRemote(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "portal.example.com"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "portal.example.com")
	scheme, port := h.proxySchemeAndPort(r)
	if scheme != "https" {
		t.Errorf("scheme = %q，期望 https", scheme)
	}
	if port != "" {
		t.Errorf("https 默认端口应为空（省略 443），实际 %q", port)
	}
	// 本机 http:8080
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Host = "localhost:8080"
	s2, p2 := h.proxySchemeAndPort(r2)
	if s2 != "http" || p2 != ":8080" {
		t.Errorf("本机应为 http/:8080，实际 %q/%q", s2, p2)
	}
}

// timeAfter 是 time.After 的薄封装（测试里多处用，集中一处便于调整）。
func timeAfter(seconds int) <-chan time.Time {
	return time.After(time.Duration(seconds) * time.Second)
}

// ---- 端到端：模拟真实插件 UI 的「根绝对路径」行为 ----
//
// 这是选 Host 路由而非路径前缀的**核心理由**，必须有判据钉住：
// 插件前端写 `fetch('/api/status')`，经 Host 反代后必须打到**上游**的
// /api/status，而不是 webui 自己的 /api/v1/*。若哪天改成路径前缀方案，
// 这条会红。
func TestProxyPreservesRootAbsolutePathsLikeRealPluginUI(t *testing.T) {
	var got []string
	// 伪造一个「插件 UI + API」上游：页面里含根绝对路径的 fetch，
	// /api/status 返回插件自己的数据（与门户 /api/v1/status 完全不同）。
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<script>fetch('/api/status').then(r=>r.json())</script>`))
		case "/api/status":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"plugin":"huawei","devices":3}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{Plugin: "huawei_smarthome", Name: "ui", Host: "huawei-smarthome", Target: up.Listener.Addr().String()}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))

	// 1) 打开插件首页
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "huawei-smarthome.localhost:8080"
	authHeaders(r)
	if !h.serveProxyHost(rec, r) {
		t.Fatal("应被反代处理")
	}
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "fetch('/api/status')") {
		t.Fatalf("插件首页未透传: code=%d", rec.Code)
	}

	// 2) 页面里的根绝对路径请求 → 必须打到上游 plugins 的 /api/status
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/status", nil)
	r2.Host = "huawei-smarthome.localhost:8080"
	authHeaders(r2)
	if !h.serveProxyHost(rec2, r2) {
		t.Fatal("插件子域下的 /api/status 应被反代处理")
	}
	var data map[string]interface{}
	if err := json.Unmarshal(rec2.Body.Bytes(), &data); err != nil {
		t.Fatalf("上游 JSON 未透传: %s", rec2.Body.String())
	}
	if data["plugin"] != "huawei" {
		t.Fatalf("根绝对路径被错路由了：拿到的不是插件数据而是 %v", data)
	}
	// 3) 路径必须原样到上游（前缀剥除在这里是错的）
	if len(got) == 0 || got[len(got)-1] != "/api/status" {
		t.Errorf("上游收到的路径 = %v，末项应为 /api/status", got)
	}
}

// ---- 自动发现：读插件目录里的 plugin.json ----
//
// readPluginProxyDecls 是「自动发现」的核心，必须单独钉住：它决定了用户
// 是否需要手填端口。
func TestReadPluginProxyDecls(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		d := filepath.Join(dir, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "plugin.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// ① 显式 host + ws + auth
	write("a", `{"name":"a","entry":"plugin.bin","proxies":[
	  {"name":"ui","host":"aaa","target":"127.0.0.1:12100"},
	  {"name":"gw","host":"aaa-gw","target":"127.0.0.1:9890","websocket":true,"auth":"none"}]}`)
	// ② 省略 host → 由插件名派生（下划线转连字符）
	write("b_plugin", `{"name":"b_plugin","entry":"plugin.bin","proxies":[{"target":"127.0.0.1:9999"}]}`)
	// ③ 没有 proxies 字段 → 不产出
	write("c", `{"name":"c","entry":"plugin.bin"}`)
	// ④ 非插件目录（无 plugin.json）→ 跳过，不得 panic
	if err := os.MkdirAll(filepath.Join(dir, "notaplugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// ⑤ 坏 JSON → 跳过该插件，不影响其它
	write("d", `{ this is not json`)

	decls := readPluginProxyDecls(dir)
	if len(decls) != 3 {
		t.Fatalf("发现 %d 条声明，期望 3：%+v", len(decls), decls)
	}
	byHost := map[string]proxyDecl{}
	for _, d := range decls {
		byHost[d.Host] = d
	}
	if d, ok := byHost["aaa"]; !ok || d.Target != "127.0.0.1:12100" || d.WebSocket {
		t.Errorf("条目 aaa 不对: %+v", d)
	}
	if d, ok := byHost["aaa-gw"]; !ok || !d.WebSocket || d.Auth != "none" {
		t.Errorf("条目 aaa-gw 未带上 ws/auth: %+v", d)
	}
	// 省略 host 的必须由插件名派生为合法 DNS label
	if d, ok := byHost["b-plugin"]; !ok {
		t.Errorf("省略 host 的声明未按插件名派生（期望 b-plugin）: %+v", byHost)
	} else if !sdk.ValidProxyHostLabel(d.Host) {
		t.Errorf("派生的 host 不合法: %q", d.Host)
	}
	// 空目录不得 panic、返回空
	if got := readPluginProxyDecls(""); len(got) != 0 {
		t.Error("空目录应返回空")
	}
	if got := readPluginProxyDecls(filepath.Join(dir, "does-not-exist")); len(got) != 0 {
		t.Error("不存在的目录应返回空而不是 panic")
	}
}

// 自动发现的声明必须真的能路由（打通「扫目录 → 建表 → 转发」整条链）
func TestAutoDiscoveryRoutesEndToEnd(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("discovered-upstream"))
	}))
	defer up.Close()

	dir := t.TempDir()
	d := filepath.Join(dir, "huawei_smarthome")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "plugin.json"), []byte(`{
	  "name":"huawei_smarthome","entry":"plugin.bin",
	  "proxies":[{"name":"ui","target":"`+up.Listener.Addr().String()+`","auth":"none"}]}`), 0o644)

	// 用真实的自动发现回调（而不是注入假声明）——这才测到发现链路
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl { return readPluginProxyDecls(dir) })
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	// 标签由插件名派生：huawei_smarthome → huawei-smarthome
	r.Host = "huawei-smarthome.localhost:8080"
	if !h.serveProxyHost(rec, r) {
		t.Fatal("自动发现的声明应被反代处理")
	}
	if rec.Code != 200 || rec.Body.String() != "discovered-upstream" {
		t.Fatalf("自动发现的声明未生效: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

// ---- 凭证头按 auth 区分：真实端到端发现过这个 bug ----
//
// auth=none 的路由，凭证是给**上游**的（设备网关的接入令牌走 X-API-Key），
// 必须原样转发；无条件剥掉会让设备链路全部 401。
// auth=homeagent 的路由，凭证是给门户的，绝不能泄漏给上游。
//
// 说明：这条判据是**事后补的**。此前的单测用「不校验凭证的假上游」，
// 抓不到这个 bug；是隔离实例上跑真实 remotedevice（真令牌、真校验）
// 才发现「直连 200、经反代 401」。
func TestProxyCredentialHeadersDependOnAuth(t *testing.T) {
	type probe struct {
		cookie string
		apiKey string
		bearer string
	}
	got := make(chan probe, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- probe{
			cookie: r.Header.Get("Cookie"),
			apiKey: r.Header.Get("X-API-Key"),
			bearer: r.Header.Get("Authorization"),
		}
		w.Write([]byte("ok"))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{
			{Plugin: "dev", Name: "gw", Host: "dev", Target: up.Listener.Addr().String(), Auth: sdk.ProxyAuthNone},
			{Plugin: "ui", Name: "ui", Host: "ui", Target: up.Listener.Addr().String(), Auth: sdk.ProxyAuthHomeAgent},
		}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	call := func(host, apiKey string) probe {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		// 同时带上三样：受保护路由要靠门户 key 过鉴权，而我们要断言的是
		// **这三样都不该到上游**；不受保护路由则用设备令牌，且必须到上游。
		r.Header.Set("X-API-Key", apiKey)
		r.Header.Set("Authorization", "Bearer UPSTREAM-BEARER")
		r.Header.Set("Cookie", "homeagent_session=PORTAL")
		h.serveProxyHost(rec, r)
		select {
		case p := <-got:
			return p
		case <-time.After(3 * time.Second):
			t.Fatalf("%s 未到达上游", host)
			return probe{}
		}
	}

	// auth=none：上游自己的令牌必须保留
	// auth=none：用的就是设备令牌（它同时也是发给上游的凭证）
	p := call("dev.localhost:8080", "UPSTREAM-DEVICE-TOKEN")
	if p.apiKey != "UPSTREAM-DEVICE-TOKEN" {
		t.Errorf("auth=none 时 X-API-Key 必须转发给上游（设备令牌），实际 %q", p.apiKey)
	}
	if p.bearer != "Bearer UPSTREAM-BEARER" {
		t.Errorf("auth=none 时 Authorization 应转发，实际 %q", p.bearer)
	}

	// auth=homeagent：门户凭证绝不能泄漏
	// auth=homeagent：用门户 key 通过鉴权，再看它有没有被转发出去
	q := call("ui.localhost:8080", "test-api-key")
	if q.apiKey != "" {
		t.Errorf("auth=homeagent 时 X-API-Key（门户密钥）泄漏给上游: %q", q.apiKey)
	}
	if strings.Contains(q.cookie, "PORTAL") {
		t.Errorf("auth=homeagent 时门户 cookie 泄漏给上游: %q", q.cookie)
	}
	if q.bearer != "" {
		t.Errorf("auth=homeagent 时 Authorization 泄漏给上游: %q", q.bearer)
	}
}

// ---- Host 分发必须先于门户路由判定 ----
//
// 真实端到端踩到的坑：插件子域上的路径若与门户某条更具体的路由同名
// （例如 /api/v1/device/online），会被那条**面向门户**的路由截走——
// 表现为 401，且响应体是门户的 JSON 而非上游的响应。
//
// 本判据经由完整 RegisterRoutes（而不是直接调 serveProxyHost）验证：
// 走一遍真实 mux，确认插件子域被 Host 分发接住。
func TestProxyHostTakesPrecedenceOverPortalRoutes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("UPSTREAM-ANSWER"))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{
			Plugin: "dev", Name: "gw", Host: "dev",
			Target: up.Listener.Addr().String(), Auth: sdk.ProxyAuthNone,
		}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	h.RegisterRoutes(http.NewServeMux())

	// 路径刻意选一个门户也注册了更具体模式的路径。
	// 若 Host 分发没生效，会被 /api/v1/device/ 的 requireAPI 接走 → 401 JSON。
	for _, p := range []string{"/api/v1/device/online", "/api/v1/status", "/api/v1/plugins/", "/"} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", p, nil)
		r.Host = "dev.localhost:8080"
		h.Handler().ServeHTTP(rec, r)
		if rec.Code != 200 || rec.Body.String() != "UPSTREAM-ANSWER" {
			t.Errorf("插件子域 %s 被门户路由截走了：code=%d body=%q（应为上游响应）",
				p, rec.Code, rec.Body.String())
		}
	}

	// 主门户 Host 上，同样的路径必须仍走门户自己的路由（不能被反代吞掉）
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/status", nil)
	r.Host = "localhost:8080"
	r.Header.Set("X-API-Key", "test-api-key")
	h.Handler().ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("门户自身的 /api/v1/status 不可用：%d", rec.Code)
	}
	var st map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("门户 /api/v1/status 返回的不是门户 JSON（被反代吞了？）: %s", rec.Body.String())
	}
	if st["status"] != "running" {
		t.Errorf("门户 /api/v1/status 返回异常: %v", st)
	}
}

// ---- 设备网关发现：客户端自动链接的权威来源 ----
//
// 改造后网关在 devices.<基域名>，而客户端无从知道基域名与子域标签。
// 让服务端回答「网关在哪」是唯一不漂移的做法。
func TestDeviceGatewayDiscovery(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{
			Plugin: "remotedevice", Name: "gateway", Host: "devices",
			Target: "127.0.0.1:9890", WebSocket: true, Auth: sdk.ProxyAuthNone,
		}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))

	// 本机 http:8080
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/device/gateway", nil)
	r.Host = "localhost:8080"
	h.handleDeviceGatewayDiscovery(rec, r)
	var got struct {
		Available bool   `json:"available"`
		URL       string `json:"url"`
		Host      string `json:"host"`
		Base      string `json:"base_domain"`
		Auth      string `json:"auth"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Available {
		t.Fatalf("应报告网关可用: %s", rec.Body.String())
	}
	if got.URL != "ws://devices.localhost:8080/api/v1/device/ws" {
		t.Errorf("url = %q，期望 ws://devices.localhost:8080/api/v1/device/ws", got.URL)
	}
	if got.Host != "devices.localhost" {
		t.Errorf("host = %q", got.Host)
	}
	if got.Auth != sdk.ProxyAuthNone {
		t.Errorf("auth = %q", got.Auth)
	}
	// ★ 门户同源形态必须一并给出：*.localhost 只有浏览器能解析，
	// 设备客户端走系统解析器会失败（实测：getent/Go 均解析不到）。
	var full struct {
		URLPortal string `json:"url_portal"`
		Preferred string `json:"preferred"`
	}
	json.Unmarshal(rec.Body.Bytes(), &full)
	if full.URLPortal != "ws://localhost:8080/api/v1/device/ws" {
		t.Errorf("url_portal = %q，期望门户同源形态 ws://localhost:8080/api/v1/device/ws", full.URLPortal)
	}
	if full.Preferred != "url_portal" {
		t.Errorf("preferred = %q，非浏览器客户端应优先门户同源形态", full.Preferred)
	}

	// 远程 https 反代：必须给出 wss 且省略 443
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/v1/device/gateway", nil)
	r2.Host = "portal.example.com"
	r2.Header.Set("X-Forwarded-Proto", "https")
	r2.Header.Set("X-Forwarded-Host", "portal.example.com")
	h.handleDeviceGatewayDiscovery(rec2, r2)
	var got2 struct {
		URL string `json:"url"`
	}
	json.Unmarshal(rec2.Body.Bytes(), &got2)
	// 外部入口是 portal.example.com 时，插件服务自然挂在
	// devices.portal.example.com —— 子域基名取自**实际入口**，
	// 而不是本机配置的 localhost（后者对远程用户毫无意义）。
	if got2.URL != "wss://devices.portal.example.com/api/v1/device/ws" {
		t.Errorf("https 场景 url = %q，期望基于 X-Forwarded-Host 的 wss 地址且无端口", got2.URL)
	}

	// ★ 安全：不得把设备令牌带回来（门户凭证不该换来设备执行权）
	body := rec.Body.String()
	for _, leak := range []string{"ws_token", "device_gateway_token", "test-api-key", "token\":\""} {
		if strings.Contains(body, leak) {
			t.Errorf("发现端点泄漏了凭证相关字段 %q: %s", leak, body)
		}
	}
}

// 没有声明设备网关时必须明确报告不可用（客户端据此回退自配地址），
// 而不是给一个连不上的 URL。
func TestDeviceGatewayDiscoveryUnavailable(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl { return nil })
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	rec := httptest.NewRecorder()
	h.handleDeviceGatewayDiscovery(rec, httptest.NewRequest("GET", "/api/v1/device/gateway", nil))
	var got struct {
		Available bool   `json:"available"`
		URL       string `json:"url"`
		Hint      string `json:"hint"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Available {
		t.Error("无声明时应报告不可用")
	}
	if got.URL != "" {
		t.Errorf("不可用时不应给出 URL，实际 %q", got.URL)
	}
	if got.Hint == "" {
		t.Error("不可用时应给出可操作提示")
	}
}

// 发现端点必须排在门户的旧路径反代（/api/v1/device/）之前——
// 否则会被 requireAPI + 旧反代接走。
func TestDeviceGatewayDiscoveryBeatsLegacyDeviceRoute(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{
			Plugin: "remotedevice", Name: "gateway", Host: "devices",
			Target: "127.0.0.1:9890", WebSocket: true, Auth: sdk.ProxyAuthNone,
		}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	h.RegisterRoutes(http.NewServeMux())
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/device/gateway", nil)
	r.Host = "localhost:8080"
	r.Header.Set("X-API-Key", "test-api-key")
	h.Handler().ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Available bool `json:"available"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.Available {
		t.Errorf("发现端点被旧 /api/v1/device/ 路由截走了: %s", rec.Body.String())
	}
}

// ---- 路径挂载：非浏览器客户端（无 DNS 依赖）----
//
// *.localhost 只有浏览器内置解析特例（RFC 6761），普通进程走系统解析器
// 解析不到（实测：getent/Go 均失败）。路径挂载挂在门户自身 host 下，
// 设备客户端因此可用它已硬编码的 /api/v1/device/ws。
func TestProxyPathMount(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte("PATH-MOUNT-OK"))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{
			Plugin: "remotedevice", Name: "gateway", Host: "devices",
			Path:   "/api/v1/device",
			Target: up.Listener.Addr().String(),
			Auth:   sdk.ProxyAuthNone,
		}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))

	// 门户 host + 声明路径 → 必须被反代（无 DNS 依赖的那条路）
	for _, p := range []string{"/api/v1/device/online", "/api/v1/device/ws", "/api/v1/device"} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", p, nil)
		r.Host = "127.0.0.1:8080"
		if !h.serveProxyHost(rec, r) {
			t.Errorf("%s 应被路径挂载接住", p)
			continue
		}
		if rec.Code != 200 || rec.Body.String() != "PATH-MOUNT-OK" {
			t.Errorf("%s → code=%d body=%q", p, rec.Code, rec.Body.String())
		}
	}
	// ★ 路径必须**原样保留**：设备客户端沿用它已硬编码的路径，
	// 剥前缀会让上游 404。
	if gotPath != "/api/v1/device" {
		t.Errorf("上游收到的路径 = %q，期望原样 /api/v1/device（不剥前缀）", gotPath)
	}

	// 边界：同前缀但不同路径段**不得**被劫持
	for _, p := range []string{"/api/v1/devicefoo", "/api/v1/devices/x"} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", p, nil)
		r.Host = "127.0.0.1:8080"
		if h.serveProxyHost(rec, r) {
			t.Errorf("%s 不该被 /api/v1/device 前缀劫持（边界必须卡在路径分隔符）", p)
		}
	}

	// 子域形态同时仍然可用
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/any", nil)
	r.Host = "devices.localhost:8080"
	if !h.serveProxyHost(rec, r) || rec.Body.String() != "PATH-MOUNT-OK" {
		t.Errorf("子域形态失效: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

// 路径前缀冲突同样不得静默覆盖
func TestProxyPathConflictNotOverridden(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{
			{Plugin: "a", Name: "x", Host: "a", Path: "/api/v1/dup", Target: "127.0.0.1:1001"},
			{Plugin: "b", Name: "y", Host: "b", Path: "/api/v1/dup", Target: "127.0.0.1:1002"},
		}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	tbl := currentProxyTable()
	first := tbl.paths["/api/v1/dup"]
	if first == nil || first.Plugin != "a" {
		t.Fatalf("先声明者应占住路径前缀: %+v", first)
	}
	var loser *ProxyRoute
	for _, x := range tbl.ordered {
		if x.Plugin == "b" {
			loser = x
		}
	}
	if loser == nil || loser.Err == "" {
		t.Error("路径冲突的后者必须可见并带原因，不能静默消失")
	}
}

// 发现端点必须同时给出两种形态，并标出优先项
func TestDeviceGatewayDiscoveryOffersPathForm(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{
			Plugin: "remotedevice", Name: "gateway", Host: "devices",
			Path:   "/api/v1/device",
			Target: "127.0.0.1:9890", WebSocket: true, Auth: sdk.ProxyAuthNone,
		}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/device/gateway", nil)
	r.Host = "portal.example.com"
	r.Header.Set("X-Forwarded-Proto", "https")
	h.handleDeviceGatewayDiscovery(rec, r)
	var got struct {
		URL       string `json:"url"`
		URLPortal string `json:"url_portal"`
		Preferred string `json:"preferred"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.URL == "" {
		t.Error("必须给出子域形态（浏览器用）")
	}
	if got.URLPortal == "" {
		t.Error("必须给出门户同源形态（非浏览器用，无 DNS 依赖）")
	}
	if got.Preferred != "url_portal" {
		t.Errorf("preferred = %q，应对非浏览器更稳的形态", got.Preferred)
	}
}

// ★ 发现端点不得被路径挂载劫持。
//
// 真实实测踩到：remotedevice 声明了 Path="/api/v1/device"（auth=none，
// 凭设备令牌），于是 /api/v1/device/gateway 被它接走转给上游，上游回 401
// —— 客户端因此永远发现不到网关。
//
// 这条判据走**完整生产链**：既确认发现端点没被劫持，也确认同前缀下的
// 真实设备路径仍归反代。
func TestDiscoveryEndpointNotHijackedByPathMount(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("UPSTREAM-DEVICE"))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{
			Plugin: "remotedevice", Name: "gateway", Host: "devices",
			Path:      "/api/v1/device",
			Target:    up.Listener.Addr().String(),
			WebSocket: true, Auth: sdk.ProxyAuthNone,
		}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	h.RegisterRoutes(http.NewServeMux())

	// 发现端点：必须由门户处理（返回 available 字段），不得转给上游
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/device/gateway", nil)
	r.Host = "127.0.0.1:18080"
	r.Header.Set("X-API-Key", "test-api-key")
	h.Handler().ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("发现端点 code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Available bool   `json:"available"`
		URLPortal string `json:"url_portal"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("发现端点返回的不是门户 JSON（被路径挂载劫持了？）: %s", rec.Body.String())
	}
	if !got.Available {
		t.Error("应报告网关可用")
	}
	if !strings.Contains(got.URLPortal, "/api/v1/device/ws") {
		t.Errorf("url_portal = %q，应指向声明路径", got.URLPortal)
	}

	// 同前缀下的真实设备路径仍必须归反代（无 auth 需求：auth=none）
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/v1/device/online", nil)
	r2.Host = "127.0.0.1:18080"
	h.Handler().ServeHTTP(rec2, r2)
	if rec2.Body.String() != "UPSTREAM-DEVICE" {
		t.Errorf("设备路径未走反代: code=%d body=%q", rec2.Code, rec2.Body.String())
	}
}

// ★ 服务入口链接的端口必须恰好出现一次。
//
// 真实 bug（生产部署后立即暴露）：请求 Host 自带端口（生产实测 Host 是
// 127.0.0.1:8080），而无条件再追加监听端口，拼出
//
//	"http://127.0.0.1:8080:8080/api/v1/device/"   ← 链接点不开
//
// 单测抓不到的原因：此前测试用的 Host 不含端口。补上这条覆盖两种输入。
func TestPortalHostPortExactlyOnce(t *testing.T) {
	cases := []struct{ host, port, want string }{
		// Host 已带端口 → 不得重复追加
		{"127.0.0.1:8080", ":8080", "127.0.0.1:8080"},
		{"portal.example.com:443", ":8080", "portal.example.com:443"},
		// Host 不含端口 → 补上监听端口
		{"127.0.0.1", ":8080", "127.0.0.1:8080"},
		{"portal.example.com", ":18080", "portal.example.com:18080"},
		// 空输入 → 兜底 localhost
		{"", ":8080", "localhost:8080"},
		// 端口为空 → 原样（由调用方/scheme 决定默认端口）
		{"portal.example.com", "", "portal.example.com"},
		// 端口号不带冒号也要能处理
		{"127.0.0.1", "8080", "127.0.0.1:8080"},
	}
	for _, c := range cases {
		if got := portalHostWithPort(c.host, c.port); got != c.want {
			t.Errorf("portalHostWithPort(%q, %q) = %q，期望 %q", c.host, c.port, got, c.want)
		}
	}
}

// 端到端：Host 自带端口时，服务入口的两种形态都必须可点（无重复端口）。
func TestProxyServiceURLsWithPortInHost(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{
			Plugin: "remotedevice", Name: "gateway", Host: "devices",
			Path: "/api/v1/device", Target: "127.0.0.1:9890",
			WebSocket: true, Auth: sdk.ProxyAuthNone,
		}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))

	// 模拟生产：请求 Host 自带端口
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/proxy/services", nil)
	r.Host = "127.0.0.1:8080"
	r.Header.Set("X-API-Key", "test-api-key")
	h.handleProxyServices(rec, r)

	var out struct {
		Services []struct {
			URL       string `json:"url"`
			URLPortal string `json:"url_portal"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Services) != 1 {
		t.Fatalf("服务数 = %d", len(out.Services))
	}
	if s := out.Services[0].URLPortal; strings.Contains(s, "8080:8080") {
		t.Errorf("url_portal 端口重复: %q", s)
	}
	// 这条路由是**别名模式**（设备网关，客户端已硬编码 /api/v1/device/ws），
	// 所以不能补尾斜杠 —— path 在这里是上游真实路径语义，补了会让人
	// 以为存在一个 /api/v1/device/ 的根。
	if s := out.Services[0].URLPortal; s != "http://127.0.0.1:8080/api/v1/device" {
		t.Errorf("url_portal = %q（别名模式不该补尾斜杠）", s)
	}
	// 子域形态也必须只有一次端口
	if s := out.Services[0].URL; strings.Contains(s, "8080:8080") {
		t.Errorf("url 端口重复: %q", s)
	}
}

// 发现端点在 Host 自带端口时同样不得拼重复端口。
func TestDeviceGatewayDiscoveryNoDuplicatePort(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{
			Plugin: "remotedevice", Name: "gateway", Host: "devices",
			Path: "/api/v1/device", Target: "127.0.0.1:9890",
			WebSocket: true, Auth: sdk.ProxyAuthNone,
		}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(proxyTestSettings(t))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/device/gateway", nil)
	r.Host = "127.0.0.1:8080"
	// 不设 X-Forwarded-*：用最朴素的场景（真实生产直连就是这样）
	h.handleDeviceGatewayDiscovery(rec, r)

	var got struct {
		URL       string `json:"url"`
		URLPortal string `json:"url_portal"`
		HTTPURL   string `json:"http_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"url": got.URL, "url_portal": got.URLPortal, "http_url": got.HTTPURL} {
		if strings.Contains(v, "8080:8080") {
			t.Errorf("%s 端口重复: %q", name, v)
		}
	}
	if got.URLPortal != "ws://127.0.0.1:8080/api/v1/device/ws" {
		t.Errorf("url_portal = %q", got.URLPortal)
	}
}

// ---- 外部入口 base_url ----
//
// 真实场景：webui 经 frp/nginx 穿透到 https://homeagent.example.com。
// 此时请求可能带内网 Host、或缺失协议，按请求推导会拼出用户点不开的链接
// （本项目实测：外层未放行子域，只有 homeagent.jianfgit.xyz 这一个 Host
// 带通配证书，三级子域外部握手失败）。
//
// base_url 让「外部入口」成为**可配置的部署事实**，而不是靠猜。
func TestBaseURLOverridesRequestDerived(t *testing.T) {
	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)
	webuiCfg := cfgReg.PluginConfig("webui")
	webuiCfg.Set("base_url", "https://homeagent.example.com")
	settings := sdk.NewSettings("webui", cfgReg)

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{
			Plugin: "remotedevice", Name: "gateway", Host: "devices",
			Path: "/api/v1/device", Target: "127.0.0.1:9890",
			WebSocket: true, Auth: sdk.ProxyAuthNone,
		}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(testSDK(sdk.SDKConfig{Settings: settings}))

	// 请求来自内网（Host 是 127.0.0.1:8080）—— 这正是穿透场景的真实样子
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/device/gateway", nil)
	r.Host = "127.0.0.1:8080"
	h.handleDeviceGatewayDiscovery(rec, r)

	var got struct {
		URL       string `json:"url"`
		URLPortal string `json:"url_portal"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// 门户同源形态必须是外部可点的
	if got.URLPortal != "wss://homeagent.example.com/api/v1/device/ws" {
		t.Errorf("url_portal = %q，期望用 base_url 推导的 wss 外部地址", got.URLPortal)
	}
	// 子域形态也必须用 base_url 的域（example.com → devices.example.com）
	if got.URL != "wss://devices.homeagent.example.com/api/v1/device/ws" {
		t.Errorf("url = %q，子域应基于 base_url 的主机名", got.URL)
	}

	// 服务清单同样以 base_url 为准
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/v1/proxy/services", nil)
	r2.Host = "127.0.0.1:8080"
	r2.Header.Set("X-API-Key", "test-api-key")
	h.handleProxyServices(rec2, r2)
	var out struct {
		BaseDomain string `json:"base_domain"`
		EntryURL   string `json:"entry_url"`
		Services   []struct {
			URLPortal string `json:"url_portal"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.EntryURL != "https://homeagent.example.com" {
		t.Errorf("entry_url = %q", out.EntryURL)
	}
	if out.BaseDomain != "homeagent.example.com" {
		t.Errorf("base_domain = %q，应取自 base_url", out.BaseDomain)
	}
	if len(out.Services) != 1 || !strings.HasPrefix(out.Services[0].URLPortal, "https://homeagent.example.com/") {
		t.Errorf("服务入口未用 base_url: %+v", out.Services)
	}
}

// 未配 base_url 时：X-Forwarded-* 优先于请求自身（反代层已给权威信息）。
func TestEntryPrefersForwardedHeaders(t *testing.T) {
	h := NewHandler(proxyTestSettings(t))
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "10.0.0.5:8080" // 内网地址（穿透场景常见）
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "portal.example.com")
	scheme, host, domain := h.resolveEntry(r)
	if scheme != "https" {
		t.Errorf("scheme = %q，应取 X-Forwarded-Proto", scheme)
	}
	if host != "portal.example.com" {
		t.Errorf("host = %q，应取 X-Forwarded-Host", host)
	}
	if domain != "portal.example.com" {
		t.Errorf("domain = %q，应取 X-Forwarded-Host 的主机名", domain)
	}

	// 都没有时回落到请求自身
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Host = "192.168.2.60:8080"
	s2, h2, d2 := h.resolveEntry(r2)
	if s2 != "http" || h2 != "192.168.2.60:8080" {
		t.Errorf("回落失败: scheme=%q host=%q", s2, h2)
	}
	if d2 != "localhost" {
		t.Errorf("未配 base_url 且无 XFF 时，基域名应回落配置默认值，实际 %q", d2)
	}
}

// base_url 末尾斜杠/多余空格必须被容忍（手填配置最常见的两种手误）。
func TestBaseURLTolerant(t *testing.T) {
	for _, raw := range []string{"https://h.example.com/", "  https://h.example.com  ", "https://h.example.com"} {
		cfgReg := internalConfig.NewConfigRegistry("")
		seedWebUIConfig(cfgReg)
		cfgReg.PluginConfig("webui").Set("base_url", raw)
		h := NewHandler(testSDK(sdk.SDKConfig{Settings: sdk.NewSettings("webui", cfgReg)}))
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = "127.0.0.1:8080"
		_, host, domain := h.resolveEntry(r)
		if host != "h.example.com" {
			t.Errorf("base_url=%q → host=%q，期望 h.example.com（应容忍尾斜杠/空格）", raw, host)
		}
		if domain != "h.example.com" {
			t.Errorf("base_url=%q → domain=%q", raw, domain)
		}
	}
}

// ---- 路径挂载的两种语义（必须由声明者选，不能猜）----
//
// 别名模式（strip_path=false，默认）：Path 是上游真实路径的一部分。
//
//	设备网关就是这种 —— 客户端硬编码 /api/v1/device/ws，不可能知道反代。
//
// 前缀模式（strip_path=true）：Path 只是门户上的挂载点，上游不知道它。
//
//	自带 UI 的服务是这种 —— 前端用相对路径，被挂到哪里都对。
//
// 猜错的结果是全部请求 404，且看起来像上游故障，所以必须显式声明。
func TestProxyPathAliasVsStrip(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte("ok"))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{
			// 别名：原样保留（机器接口，客户端已硬编码路径）
			{Plugin: "gw", Name: "gateway", Host: "gw", Path: "/api/v1/device",
				Target: up.Listener.Addr().String(), Auth: sdk.ProxyAuthNone},
			// 前缀：剥掉后转发（自带 UI 的服务）
			{Plugin: "ui", Name: "ui", Host: "ui", Path: "/p/myapp", StripPath: true,
				Target: up.Listener.Addr().String(), Auth: sdk.ProxyAuthNone},
		}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(nil)
	call := func(p string) string {
		gotPath = ""
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", p, nil)
		r.Host = "127.0.0.1:8080"
		if !h.serveProxyHost(rec, r) {
			t.Fatalf("%s 未被路径挂载接住", p)
		}
		return gotPath
	}

	// 别名模式：上游必须收到**一模一样**的路径
	if p := call("/api/v1/device/ws"); p != "/api/v1/device/ws" {
		t.Errorf("别名模式：上游收到 %q，期望原样 /api/v1/device/ws", p)
	}
	if p := call("/api/v1/device/online"); p != "/api/v1/device/online" {
		t.Errorf("别名模式：上游收到 %q", p)
	}

	// 前缀模式：上游必须收到**剥掉前缀之后**的路径
	if p := call("/p/myapp/api/status"); p != "/api/status" {
		t.Errorf("前缀模式：上游收到 %q，期望 /api/status（前缀应被剥掉）", p)
	}
	if p := call("/p/myapp/"); p != "/" {
		t.Errorf("前缀模式根：上游收到 %q，期望 /", p)
	}
	// 无尾斜杠时**不再转发**，而是 301 到带尾斜杠的形态 ——
	// 否则浏览器算出的相对路径基准会退回上一级（见
	// TestProxyStripPathRedirectsToTrailingSlash）。这里断言它确实
	// 没有把 /p/myapp 当路径转给上游。
	gotPath = ""
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/p/myapp", nil)
	r.Host = "127.0.0.1:8080"
	h.serveProxyHost(rec, r)
	if rec.Code != http.StatusMovedPermanently || gotPath != "" {
		t.Errorf("前缀模式无尾斜杠应 301 且不转发，实际 code=%d upstream_path=%q", rec.Code, gotPath)
	}
}

// 路径前缀匹配必须**最长优先**，且边界卡在分隔符上。
func TestProxyPathLongestPrefixWins(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte("ok"))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{
			{Plugin: "a", Name: "short", Host: "a", Path: "/p/app", StripPath: true,
				Target: up.Listener.Addr().String(), Auth: sdk.ProxyAuthNone},
			{Plugin: "b", Name: "long", Host: "b", Path: "/p/app/admin", StripPath: true,
				Target: up.Listener.Addr().String(), Auth: sdk.ProxyAuthNone},
		}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(nil)
	call := func(p string) string {
		gotPath = ""
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", p, nil)
		r.Host = "127.0.0.1:8080"
		h.serveProxyHost(rec, r)
		return gotPath
	}

	// 更长前缀必须胜出（否则 /p/app/admin/x 会被 /p/app 抢走）
	if p := call("/p/app/admin/x"); p != "/x" {
		t.Errorf("最长前缀未生效：上游收到 %q，期望 /x（由 /p/app/admin 处理）", p)
	}
	if p := call("/p/app/other"); p != "/other" {
		t.Errorf("短前缀处理: %q，期望 /other", p)
	}

	// 边界：/p/app 不得匹配 /p/apple（否则会劫持无关路径）
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/p/apple/pie", nil)
	r.Host = "127.0.0.1:8080"
	if h.serveProxyHost(rec, r) {
		t.Errorf("/p/apple 被 /p/app 前缀劫持了（边界必须卡在路径分隔符）: gotPath=%q", gotPath)
	}
}

// ---- 前缀模式的尾斜杠（相对路径的基准）----
//
// 真实踩到的 bug：/p/huawei 能打开但页面里所有 fetch 都 404。
// 原因是相对路径以「当前文档目录」为基准 —— 没有尾斜杠时浏览器把最后
// 一段当文件名，目录退回上一级，fetch('api/status') 打到 /p/api/status。
// 表现为「页面能开、数据全空」，极易误判成插件故障。
func TestProxyStripPathRedirectsToTrailingSlash(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("upstream:" + r.URL.Path))
	}))
	defer up.Close()

	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{
			// 前缀模式：需要尾斜杠重定向
			{Plugin: "ui", Name: "ui", Host: "ui", Path: "/p/app", StripPath: true,
				Target: up.Listener.Addr().String(), Auth: sdk.ProxyAuthNone},
			// 别名模式：绝不能重定向（路径是上游真实语义）
			{Plugin: "gw", Name: "gw", Host: "gw", Path: "/api/v1/device",
				Target: up.Listener.Addr().String(), Auth: sdk.ProxyAuthNone},
		}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	h := NewHandler(nil)
	do := func(p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", p, nil)
		r.Host = "127.0.0.1:8080"
		if !h.serveProxyHost(rec, r) {
			t.Fatalf("%s 未被接住", p)
		}
		return rec
	}

	// 前缀模式 + 无尾斜杠 → 必须 301 到带尾斜杠
	rec := do("/p/app")
	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("/p/app 应 301 到 /p/app/，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/p/app/" {
		t.Errorf("Location = %q，期望 /p/app/", loc)
	}

	// 查询串必须保留
	rec = do("/p/app?a=1&b=2")
	if loc := rec.Header().Get("Location"); loc != "/p/app/?a=1&b=2" {
		t.Errorf("带查询串的 Location = %q，期望 /p/app/?a=1&b=2", loc)
	}

	// 有尾斜杠 → 正常转发到上游根
	rec = do("/p/app/")
	if rec.Code != http.StatusOK || rec.Body.String() != "upstream:/" {
		t.Errorf("/p/app/ 应转发到上游 /，实际 %d %q", rec.Code, rec.Body.String())
	}

	// 子路径不受影响（不重定向）
	rec = do("/p/app/api/status")
	if rec.Code != http.StatusOK || rec.Body.String() != "upstream:/api/status" {
		t.Errorf("/p/app/api/status 应转发到 /api/status，实际 %d %q", rec.Code, rec.Body.String())
	}

	// ★ 别名模式绝不能重定向：/api/v1/device 是上游真实路径，加斜杠会毁掉语义
	rec = do("/api/v1/device")
	if rec.Code == http.StatusMovedPermanently {
		t.Errorf("别名模式的 /api/v1/device 被重定向了 —— 那类客户端的路径是上游真实语义，不能改")
	}
	if rec.Body.String() != "upstream:/api/v1/device" {
		t.Errorf("别名模式应原样转发 /api/v1/device，实际 %q", rec.Body.String())
	}
}

// ---- 服务入口必须给用户**能用**的那个链接 ----
//
// 真实验收里发现的用户可见缺口：API 同时返回 url（子域）与 url_portal
// （路径），但前端只用了 url —— 而子域形态在穿透部署下**恰恰是坏的**
// （外层只放行一个 Host、三级子域证书不匹配）。
// 用户点「打开」得到的是打不开的地址，还以为是插件的问题。
func TestProxyServicesOffersBothForms(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{
			Plugin: "huawei_smarthome", Name: "ui", Host: "huawei-smarthome",
			Path: "/p/huawei", StripPath: true,
			Target: "127.0.0.1:12100", Auth: sdk.ProxyAuthHomeAgent,
		}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)
	cfgReg.PluginConfig("webui").Set("base_url", "https://homeagent.example.com")
	h := NewHandler(testSDK(sdk.SDKConfig{Settings: sdk.NewSettings("webui", cfgReg)}))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/proxy/services", nil)
	r.Host = "127.0.0.1:8080"
	r.Header.Set("X-API-Key", "test-api-key")
	h.handleProxyServices(rec, r)

	var out struct {
		EntryURL string `json:"entry_url"`
		Services []struct {
			URL       string `json:"url"`
			URLPortal string `json:"url_portal"`
			StripPath bool   `json:"strip_path"`
			Path      string `json:"path"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Services) != 1 {
		t.Fatalf("期望 1 条服务，实际 %d", len(out.Services))
	}
	s := out.Services[0]

	// 两种形态都必须给出：路径形态给无 DNS 依赖的客户端/穿透场景，
	// 子域形态给局域网内浏览器。缺一个就会有用户点不开。
	if s.URLPortal != "https://homeagent.example.com/p/huawei/" {
		t.Errorf("url_portal = %q，路径形态缺失或不是入口下的地址", s.URLPortal)
	}
	if s.URL != "https://huawei-smarthome.homeagent.example.com" {
		t.Errorf("url = %q，子域形态应为 <host>.<入口主机名>", s.URL)
	}
	// 前缀模式的入口必须带尾斜杠（否则浏览器算错相对路径基准）
	if !strings.HasSuffix(s.URLPortal, "/") {
		t.Errorf("strip_path 路由的 url_portal 必须以 / 结尾（相对路径基准），实际 %q", s.URLPortal)
	}
	// 前端据此决定要不要显示「子域」次选按钮
	if !s.StripPath || s.Path != "/p/huawei" {
		t.Errorf("path/strip_path 未透出，前端无法区分两种开关: %+v", s)
	}
}

// 别名模式（设备网关）的 url_portal 不能加尾斜杠 —— 那会改坏上游路径语义。
func TestProxyServicesAliasKeepsExactPath(t *testing.T) {
	prev := declProvider
	SetProxyDeclProvider(func() []proxyDecl {
		return []proxyDecl{{
			Plugin: "remotedevice", Name: "gateway", Host: "devices",
			Path: "/api/v1/device", Target: "127.0.0.1:9890",
			WebSocket: true, Auth: sdk.ProxyAuthNone,
		}}
	})
	manualProxyRoutes = ""
	InvalidateProxyRoutes()
	t.Cleanup(func() { SetProxyDeclProvider(prev); InvalidateProxyRoutes() })

	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)
	h := NewHandler(testSDK(sdk.SDKConfig{Settings: sdk.NewSettings("webui", cfgReg)}))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/proxy/services", nil)
	r.Host = "127.0.0.1:8080"
	r.Header.Set("X-API-Key", "test-api-key")
	h.handleProxyServices(rec, r)

	var out struct {
		Services []struct {
			URLPortal string `json:"url_portal"`
		} `json:"services"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Services) != 1 {
		t.Fatal("期望 1 条服务")
	}
	if strings.HasSuffix(out.Services[0].URLPortal, "/") {
		t.Errorf("别名模式的 url_portal 不应以 / 结尾（那是上游真实路径语义）：%q",
			out.Services[0].URLPortal)
	}
}
