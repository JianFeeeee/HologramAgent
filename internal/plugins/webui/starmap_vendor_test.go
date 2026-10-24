package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ===== 星图依赖 CDN 不可达 =====
//
// 真实现象：用户点「星图」看到「3D 星图不可用（CDN 加载失败）」。
// 服务端 curl 同一批 URL 是 200 —— 说明**不是**服务端的问题，是浏览器
// 访问不到那两个公共 CDN（内网 / 出口受限 / 断网 / 被墙）。
//
// 为什么必须本地化而不是「换个 CDN」：
// 换 CDN 只是把同一个赌注重下一遍。HologramAgent 明确支持离线/内网部署，
// 前端却有 4 个硬依赖在公网上 ⇒ 断网时星图必坏、且用户无从修复。
// marked / DOMPurify 同理（Markdown 渲染与 XSS 净化）。
//
// 判据钉住：页面**不得**再引用任何外部 CDN。

// html 不得引用公网 CDN 资源（这是根因的直接判据）。
func TestDashboardHasNoExternalCDN(t *testing.T) {
	html := readDashboardHTML(t)

	// 只查 src/href 的绝对 URL；页面里出现的普通文本不算依赖。
	re := regexp.MustCompile(`(?:src|href)\s*=\s*["'](https?://[^"']+)["']`)
	matches := re.FindAllStringSubmatch(html, -1)
	for _, m := range matches {
		url := m[1]
		t.Errorf("dashboard.html 仍引用外部 CDN：%s\n"+
			"     断网/内网/出口受限环境下该功能必坏（星图即因此失效）。\n"+
			"     修法：把库 vendor 进二进制，由本服务同源提供。", url)
	}
}

// 具体到星图：three.js 与 OrbitControls 必须来自本地路由。
func TestStarmapDependenciesAreLocal(t *testing.T) {
	html := readDashboardHTML(t)
	if strings.Contains(html, "cdnjs.cloudflare.com/ajax/libs/three.js") {
		t.Error("three.js 仍走 cdnjs CDN —— 星图在无公网环境下必坏")
	}
	if strings.Contains(html, "three@0.128.0/examples/js/controls/OrbitControls.js") {
		t.Error("OrbitControls 仍走 jsdelivr CDN —— 星图无法旋转视角")
	}
	// 必须有本地引用
	if !strings.Contains(html, "/static/three.min.js") {
		t.Error("未引用本地 /static/three.min.js")
	}
	if !strings.Contains(html, "/static/OrbitControls.js") {
		t.Error("未引用本地 /static/OrbitControls.js")
	}
}

// 本地路由必须真的能返回这些库（且是 JS 内容类型，不是 HTML 错误页）。
//
// ★ 这一条是「容易假绿」的地方：路由写了但没 embed 成功时，
//
//	返回 404 或空体，页面依然报「CDN 加载失败」，症状与修复前完全一样。
//	所以判据必须实际取内容并验证它**像 three.js**（含特征串）。
func TestStaticVendorRoutesServeRealLibraries(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cases := []struct {
		path     string
		mustHave string
	}{
		// three.js UMD 头里有 REVISION；OrbitControls 是非压缩源码
		{"/static/three.min.js", "REVISION"},
		{"/static/OrbitControls.js", "OrbitControls"},
		{"/static/marked.min.js", "marked"},
		{"/static/purify.min.js", "DOMPurify"},
	}
	for _, c := range cases {
		resp, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Errorf("%s: %v", c.path, err)
			continue
		}
		body := readAllLimited(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s 应 200（否则浏览器拿到的是错误页，症状与修复前一模一样），实际 %d",
				c.path, resp.StatusCode)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
			t.Errorf("%s 的 Content-Type = %q，应为 javascript（浏览器会拒绝执行）", c.path, ct)
		}
		// ★ 必须同时判「状态码」与「内容」。变异验证发现：把 w.Write(data)
		//   去掉（只写 200 头 + 空体）时，**只判状态码的判据会通过** ——
		//   而浏览器拿到空体同样报「加载失败」，症状与修复前一模一样。
		//   这类「假绿」是本轮踩到的真陷阱，所以体积与特征串都要判。
		if len(body) < 1000 {
			t.Errorf("%s 体积仅 %d 字节，疑似空体/占位（这几个库都是几万~几十万字节）。"+
				"空体在浏览器里的症状与 404 完全一样，但更难看出是文件没写出来",
				c.path, len(body))
			continue
		}
		if !strings.Contains(body, c.mustHave) {
			t.Errorf("%s 内容不含特征串 %q —— 不是真正的库（可能被 embed 成了错误文本）",
				c.path, c.mustHave)
		}
	}
}

// vendor 文件必须真的存在于源码树（embed 的前提）。
//
// 单独判这一条的原因：//go:embed 找不到文件会**编译失败**，
// 但如果有人改成运行时读文件，缺文件就变成「运行时才发现」——
// 从「编译期报错」退化成「用户看到星图坏了」。用判据钉住前提。
func TestVendorFilesExistInSourceTree(t *testing.T) {
	dir := filepath.Join(".", "static")
	for _, name := range []string{
		"three.min.js", "OrbitControls.js", "marked.min.js", "purify.min.js",
	} {
		p := filepath.Join(dir, name)
		st, err := os.Stat(p)
		if err != nil {
			t.Errorf("缺少 vendor 文件 %s —— 星图/Markdown 依赖它，缺了就静默失效", p)
			continue
		}
		if st.Size() < 1000 {
			t.Errorf("%s 仅 %d 字节，疑似下载失败的占位文件", p, st.Size())
		}
	}
}

func readDashboardHTML(t *testing.T) string {
	t.Helper()
	raw, err := dashboardFS.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("读 dashboard.html: %v", err)
	}
	// 模板占位符不影响 CDN 检查，保留原样即可
	return string(raw)
}

func readAllLimited(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil || len(buf) > 2*1024*1024 {
			break
		}
	}
	return string(buf)
}
