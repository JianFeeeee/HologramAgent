package test

// 判据：**手动反代路由的对外契约**。
//
// ## 为什么需要
//
// webui 的反代（`/api/v1/proxy` + `/api/v1/proxy/services`）是前端拼子域
// 链接的唯一依据，也是 `SetManualProxyRoutes` 这个导出入口的唯一消费者。
// 现有 10 条 webui 判据里**反代零覆盖**。
//
// 而它有几条硬契约，写错的方向都是"用户点链接打不开"：
//
//	· 非法行（字段不够）必须占位并标错，不能静默丢掉
//     —— 静默丢的后果是用户配了却看不到，以为配置没生效
//	· 同一 host 重复登记时**先到者赢**，后者带 Err
//     —— 否则两个插件抢同一子域会互相覆盖，前端链接随机失效
//	· `auth=` 选项必须规范化（未知值不能被当成"无鉴权"）
//     —— 未写 auth 若原样透出空串，就变成无鉴权（鉴权降级）
//	· 上游不可达必须在清单里标出来（用户能自助诊断）
//
// ## 判据怎么起
//
// `SetManualProxyRoutes` + 两个 HTTP 端点都是导出面。
// `/api/v1/proxy/services` 会**真拨 TCP** 探上游可达性，所以判据里起
// 真实 httptest 上游（而不是假装可达）—— 反过来也用它钉"不可达时标错"。

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

// newProxyHandler 起一个只带 settings 的 Handler（反代清单不依赖更多装配）。
func newProxyHandler(t *testing.T) *webui.Handler {
	t.Helper()
	dir := t.TempDir()
	reg := internalConfig.NewConfigRegistry(dir + "/config.db")
	reg.SetCodec(internalConfig.NewTestCodec(t))
	t.Cleanup(func() { reg.Close() })
	seedWebUIConfig(reg)

	h := webui.NewHandler(internalSDK.New("webui", internalSDK.SDKConfig{
		Settings: internalSDK.NewSettings("webui", reg),
		EventBus: events.NewBus(),
	}))
	t.Cleanup(func() { h.Close() })
	// 每个判据都从空表开始：manual 路由是**进程级全局状态**，
	// 不清会互相污染（判据顺序一变就红，那种红没有诊断价值）。
	webui.SetManualProxyRoutes("")
	t.Cleanup(func() { webui.SetManualProxyRoutes("") })
	return h
}

// proxyInfo 请求 /api/v1/proxy 并解出 body。
func proxyInfo(t *testing.T, h *webui.Handler) map[string]interface{} {
	t.Helper()
	w := httptest.NewRecorder()
	serveRoutes(t, h, w, httptest.NewRequest(http.MethodGet, "/api/v1/proxy", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", w.Code, w.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v (%s)", err, w.Body.String())
	}
	return out
}

// ---- 判据 1：手动路由真的进表，非法行必须占位标错 ----

// TestManualRoutesInvalidLineIsKeptAndMarked：字段不足的行必须**留在表里**
// 并带错误，而不是被静默丢弃。
//
// ★ 为什么要"留着"：静默丢的后果是用户配了一条路由、清单里什么都没有，
// 于是他分不清"配置没生效"和"我写错了"。
//
// 断言 total 与 count 的关系：非法行计入 total，但不计入可用路由 ——
// 两者之差就是"配了但不可用"的数量。
func TestManualRoutesInvalidLineIsKeptAndMarked(t *testing.T) {
	h := newProxyHandler(t)
	// 两行：一条合法（字段够）、一条非法（只有一个字段）
	webui.SetManualProxyRoutes("good http://127.0.0.1:9\nalone")
	info := proxyInfo(t, h)

	if got := toInt(info["total"]); got != 2 {
		t.Fatalf("★ total=%d，期望 2（非法行也必须留在表里，否则用户以为配置没生效）: %v",
			got, info)
	}
	if got := toInt(info["count"]); got != 1 {
		t.Fatalf("★ count=%d，期望 1（非法行不该被算作可用路由）: %v", got, info)
	}
}

// ---- 判据 2：auth= 选项规范化 ----

// TestManualRouteAuthOptionIsNormalized：`auth=` 的取值必须经过
// `EffectiveProxyAuth` 归一化，未写/存量别名都归到默认保护。
//
// 语义（与 sdk.EffectiveProxyAuth 一致）：
//   - 空 / 未写       ⇒ 默认保护（ProxyAuthAgent）
//   - 存量别名         ⇒ 归一成新值
//   - auth=none        ⇒ 显式放行
//
// ★ 判据落在"清单里报告的 Auth 字段"—— 前端据此决定是否走带凭据的链接。
// 若未写 auth 原样透出空串，等于**鉴权降级**（空 ≠ 保护）。
func TestManualRouteAuthOptionIsNormalized(t *testing.T) {
	h := newProxyHandler(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	webui.SetManualProxyRoutes(
		"default " + up.URL + "\n" + // 不写 auth ⇒ 默认保护
			"none " + up.URL + " auth=none\n" + // 显式无鉴权
			"legacy " + up.URL + " auth=homeagent\n") // 存量别名

	byHost := map[string]map[string]interface{}{}
	for _, s := range proxyServices(t, h) {
		byHost[toString(s["host"])] = s
	}
	for _, host := range []string{"default", "none", "legacy"} {
		s, ok := byHost[host]
		if !ok {
			t.Fatalf("★ 路由 %q 没出现在服务清单里: %v", host, byHost)
		}
		auth := toString(s["auth"])
		switch host {
		case "default":
			if auth != internalSDK.ProxyAuthAgent {
				t.Errorf("★ 未写 auth 的路由鉴权=%q，应为默认保护 %q —— "+
					"空串会变成无鉴权（鉴权降级）", auth, internalSDK.ProxyAuthAgent)
			}
		case "none":
			if auth != internalSDK.ProxyAuthNone {
				t.Errorf("★ 显式 auth=none 的路由鉴权=%q", auth)
			}
		case "legacy":
			if auth != internalSDK.ProxyAuthAgent {
				t.Errorf("★ 存量别名 auth=homeagent 应归一为 %q，实际 %q",
					internalSDK.ProxyAuthAgent, auth)
			}
		}
		if s["ok"] != true {
			t.Errorf("★ 上游可达却报告 ok=%v（error=%v）: 探测链路有问题",
				s["ok"], s["error"])
		}
	}
}

// ---- 判据 3：不可达上游必须被标出来 ----

// TestUnreachableUpstreamIsMarked：上游拨不通时清单必须报告 ok=false 且
// 带错误文案，而不是显示成正常。
//
// ★ 这条的价值是"用户能自助诊断"：子域链接打不开时，前端拿到的 services
// 里写着"上游不可达"比什么都不说好得多。
func TestUnreachableUpstreamIsMarked(t *testing.T) {
	h := newProxyHandler(t)
	// 127.0.0.1:1 在本机必然拒绝连接（无服务监听）
	webui.SetManualProxyRoutes("dead http://127.0.0.1:1")

	svcs := proxyServices(t, h)
	if len(svcs) != 1 {
		t.Fatalf("服务清单应有 1 条，实际 %d: %v", len(svcs), svcs)
	}
	s := svcs[0]
	if s["ok"] == true {
		t.Fatalf("★ 不可达的上游被报告成 ok=true —— 用户会以为能用")
	}
	msg := toString(s["error"])
	if !strings.Contains(msg, "不可达") && !strings.Contains(msg, "unreachable") {
		t.Fatalf("★ 错误文案没说清是上游不通（用户无法自助诊断）: %q", msg)
	}
}

// ---- 判据 4：重复 host 先到者赢 ----

// TestDuplicateHostKeepsFirstAndMarksLater：同一 host 登记两次时，
// **第一次**的保留、后来的带错误 —— 而不是静默互相覆盖。
//
// 后果对比：覆盖会让"哪个插件占了这子域"取决于加载顺序，前端链接时好时坏。
func TestDuplicateHostKeepsFirstAndMarksLater(t *testing.T) {
	h := newProxyHandler(t)
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up2.Close()

	webui.SetManualProxyRoutes("dup " + up1.URL + "\ndup " + up2.URL)
	svcs := proxyServices(t, h)

	var kept, rejected int
	for _, s := range svcs {
		if toString(s["host"]) != "dup" {
			continue
		}
		if s["ok"] == true {
			kept++
			if !strings.Contains(toString(s["target"]), strings.TrimPrefix(up1.URL, "http://")) {
				t.Fatalf("★ 重复 host 保留的不是**先到的**那条（target=%q）",
					toString(s["target"]))
			}
		} else {
			rejected++
			msg := toString(s["error"])
			if !strings.Contains(msg, "占用") && !strings.Contains(msg, "冲突") {
				t.Fatalf("★ 后到的重复路由没说明原因: %q", msg)
			}
		}
	}
	if kept != 1 || rejected != 1 {
		t.Fatalf("★ 重复 host 的结果是 kept=%d rejected=%d，期望各 1: %v", kept, rejected, svcs)
	}
}

// ---- 判据 5：两个端点对同一张表 ----

// TestProxyServicesCountMatchesInfo：两个端点读的是**同一张表**。
//
// ★ 为什么单列一条：它们是两个独立 handler，各自调 currentProxyTable()。
// 若其中一个缓存了旧表，用户会看到"总览说有 3 条、列表只有 2 条"这种
// 无法解释的不一致。
func TestProxyServicesCountMatchesInfo(t *testing.T) {
	h := newProxyHandler(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	webui.SetManualProxyRoutes("one " + up.URL + "\ntwo " + up.URL)

	info := proxyInfo(t, h)
	var okCount int
	for _, s := range proxyServices(t, h) {
		if s["ok"] == true {
			okCount++
		}
	}
	if got, want := toInt(info["count"]), okCount; got != want {
		t.Fatalf("★ /api/v1/proxy 的 count=%d 与 /proxy/services 的可达条数=%d 不一致",
			got, want)
	}
}

// 一条**不写**的判据：Path 前缀冲突
//
// `buildProxyTable` 还有一条规则：两条路由共用同一非空 `Path` 时，后到者
// 标错**并从 routes 表里移除**（`delete(t.routes, key)`）—— 与 host 冲突
// "保留先到者"不同，因为同一前缀指向两个上游必然反代错。
//
// 实测这条在当前**导出面**上不可达：`parseManualRoutes` 的语法是
// `host target [ws|auth=x]`，**不支持声明 path**；path 只能来自插件 manifest
// 声明，而那个入口 `proxyDecl` 未导出（`SetProxyDeclProvider` 虽导出，
// 但参数类型未导出，外部包构造不出来）。
//
// 所以不写判据 —— 为不可达的场景建断言就是过度防御。
// 要覆盖它得先把 `Path` 加进手动语法（那本身是新功能）。

// ---- 支撑 ----

// proxyServices 请求 /api/v1/proxy/services 并解出 services 数组。
func proxyServices(t *testing.T, h *webui.Handler) []map[string]interface{} {
	t.Helper()
	w := httptest.NewRecorder()
	serveRoutes(t, h, w, httptest.NewRequest(http.MethodGet, "/api/v1/proxy/services", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Services []map[string]interface{} `json:"services"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 services 失败: %v (%s)", err, w.Body.String())
	}
	return body.Services
}

func toInt(v interface{}) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return -1
}

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
