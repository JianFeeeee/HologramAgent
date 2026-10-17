package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	internalConfig "github.com/JianFeeeee/HomeAgent/internal/config"
	"github.com/JianFeeeee/HomeAgent/internal/sdk"
	"github.com/JianFeeeee/HomeAgent/internal/supervisor"
	"github.com/JianFeeeee/HomeAgent/pkg/types"
)

// ★ E-3：设置列表默认不得返回任何敏感明文（包括其他插件的）。
func TestSettingsMasksSecretValues(t *testing.T) {
	reg := internalConfig.NewConfigRegistry("")
	reg.RegisterDef(internalConfig.ConfigDef{Key: "core.llm.api_key", Type: "string", Secret: true})
	if err := reg.Set("core.llm.api_key", "CORE_SECRET"); err != nil {
		t.Fatal(err)
	}
	qq := reg.PluginConfig("qq")
	qq.RegisterDef(internalConfig.ConfigDef{Key: "access_token", Type: "string", Secret: true})
	if err := qq.Set("access_token", "QQ_SECRET"); err != nil {
		t.Fatal(err)
	}
	sup := supervisor.New(&types.Config{Daemon: types.DaemonConfig{CheckInterval: 1, HeartbeatInterval: 1}})
	sup.Start()
	defer sup.Shutdown()
	h := NewHandler(testSDK(sdk.SDKConfig{Supervisor: supervisor.NewSDKAdapter(sup), Settings: sdk.NewSettings("webui", reg), Config: sdk.NewConfig(&types.Config{})}))
	w := httptest.NewRecorder()
	h.handleSettings(w, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "CORE_SECRET") || strings.Contains(body, "QQ_SECRET") {
		t.Fatalf("★ settings 泄露敏感明文: %s", body)
	}
	if !strings.Contains(body, SecretPlaceholder) {
		t.Fatal("敏感值应显示占位符")
	}
}

// ★ E-3：单键显式揭示允许核心/本插件，拒绝其他插件。
func TestSettingsRevealScope(t *testing.T) {
	reg := internalConfig.NewConfigRegistry("")
	reg.RegisterDef(internalConfig.ConfigDef{Key: "core.llm.api_key", Type: "string", Secret: true})
	reg.Set("core.llm.api_key", "CORE_SECRET")
	web := reg.PluginConfig("webui")
	web.RegisterDef(internalConfig.ConfigDef{Key: "api_key", Type: "string", Secret: true})
	web.Set("api_key", "WEB_SECRET")
	qq := reg.PluginConfig("qq")
	qq.RegisterDef(internalConfig.ConfigDef{Key: "access_token", Type: "string", Secret: true})
	qq.Set("access_token", "QQ_SECRET")
	sup := supervisor.New(&types.Config{Daemon: types.DaemonConfig{CheckInterval: 1, HeartbeatInterval: 1}})
	sup.Start()
	defer sup.Shutdown()
	h := NewHandler(testSDK(sdk.SDKConfig{Supervisor: supervisor.NewSDKAdapter(sup), Settings: sdk.NewSettings("webui", reg), Config: sdk.NewConfig(&types.Config{})}))
	for _, tc := range []struct {
		key, want string
		status    int
	}{
		{"core.llm.api_key", "CORE_SECRET", http.StatusOK},
		{"plugin.webui.api_key", "WEB_SECRET", http.StatusOK},
		{"plugin.qq.access_token", "不允许读取其他插件", http.StatusForbidden},
	} {
		w := httptest.NewRecorder()
		path := "/api/v1/settings/reveal?key=" + tc.key
		h.handleSettingsReveal(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != tc.status {
			t.Errorf("%s status=%d want=%d body=%s", tc.key, w.Code, tc.status, w.Body.String())
			continue
		}
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s body lacks %q: %s", tc.key, tc.want, w.Body.String())
		}
		if tc.status == http.StatusOK {
			var got map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got["secret"] != true {
				t.Errorf("%s should mark secret=true", tc.key)
			}
		}
	}
}

// ★★★ 判据：**掩码占位符绝不能经 PUT 存回内核**。
//
// 真实故障（2026-10-16，生产实测）：
//
//	① 设置页 GET 把 api_key 掩码成 `••••••••`（E-3 防泄漏，本身是对的）
//	② 前端「保存」按钮会**一并回传**该组全部字段（含刚被掩码的 api_key）——
//	   用户往往只想改同组的 base_url/model
//	③ 写侧此前**完全不检查占位符** ⇒ `••••••••` 被当真实值存进
//	   `core.llm.sources.<n>.api_key`
//	④ commondev 的 pi 源导入器优先读 `sources.*` ⇒ 把掩码写进 pi 的 auth.json
//	⑤ pi 拿掩码调模型 ⇒ 401 invalid api key（表现为"插件装了但模型不回复"）
//
// 铁证：内核 `core.llm.api_key`（主键）解密后是**真 key**，
// 而 `core.llm.sources.llmsproxy.api_key`（子键）解密后是 `••••••••`。
//
// ⇒ 占位符是"这里有个值但我不告诉你"的**标记**，不是值。
//
//	把它存回去等于用标记覆盖真值 —— 这不是输入校验问题，是语义错误。
func TestSettingsPutRejectsSecretPlaceholder(t *testing.T) {
	reg := internalConfig.NewConfigRegistry("")
	reg.RegisterDef(internalConfig.ConfigDef{Key: "core.llm.sources.x.api_key", Type: "string", Secret: true})
	if err := reg.Set("core.llm.sources.x.api_key", "REAL_KEY_VALUE"); err != nil {
		t.Fatal(err)
	}
	sup := supervisor.New(&types.Config{Daemon: types.DaemonConfig{CheckInterval: 1, HeartbeatInterval: 1}})
	sup.Start()
	defer sup.Shutdown()
	h := NewHandler(testSDK(sdk.SDKConfig{Supervisor: supervisor.NewSDKAdapter(sup), Settings: sdk.NewSettings("webui", reg), Config: sdk.NewConfig(&types.Config{})}))

	put := func(v interface{}) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]interface{}{
			"key": "core.llm.sources.x.api_key", "value": v,
		})
		req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(string(body)))
		w := httptest.NewRecorder()
		h.handleSettings(w, req)
		return w
	}

	// ① 掩码必须被拒绝（这是缺陷本体）
	w := put(SecretPlaceholder)
	if w.Code == http.StatusOK {
		t.Fatalf("★ 掩码占位符被接受了（接口返回 200）—— 真值已被覆盖: %s", w.Body.String())
	}
	// ② 拒绝之后真值**必须还在**（拒绝而不是写坏）
	got, _ := reg.Get("core.llm.sources.x.api_key")
	if s, _ := got.(string); s != "REAL_KEY_VALUE" {
		t.Fatalf("★ 拒绝后真值被改写: %q（拒绝必须是原子的：不写）", s)
	}
	// ③ 错误信息必须告诉用户**怎么办**（否则他不知道如何保存同组其他字段）
	msg := w.Body.String()
	for _, want := range []string{"掩码", "不要保存"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误应含 %q 以指导操作: %s", want, msg)
		}
	}

	// ④ 真值仍然可以正常保存（不能误伤）
	if w := put("NEW_REAL_KEY"); w.Code != http.StatusOK {
		t.Fatalf("真值应可保存: %d %s", w.Code, w.Body.String())
	}
	got, _ = reg.Get("core.llm.sources.x.api_key")
	if s, _ := got.(string); s != "NEW_REAL_KEY" {
		t.Fatalf("真值保存失败: %q", s)
	}

	// ⑤ 相似但不全等的值不得被误杀（真 key 可能含任意字符）
	for _, v := range []string{"•••", "••••••••X", "x••••••••", "••••••••••••••••"} {
		if w := put(v); w.Code != http.StatusOK {
			t.Errorf("值 %q 不该被当作占位符拒绝: %d %s", v, w.Code, w.Body.String())
		}
	}
}

// ★★★ **行为判据**：前端保存时**真的不会**把掩码回传。
//
// 为什么不能只看源码字符串（本会话反复踩的坑）：初版断言
// "saveSetting 里出现 isSecretPlaceholderValue(inp.value)"，
// 把条件改成 `if (false && isSecretPlaceholderValue(...))` 就绕过了 ——
// 判据全绿而缺陷照样存在。这里把 dashboard.js 里的两个函数**抠出来真跑**
// （用 node + 假 DOM/假 api），断言"发出去的请求体"里没有掩码。
func TestDashboardSkipsPlaceholderOnSave(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("本机无 node，跳过行为判据")
	}
	b, err := os.ReadFile("dashboard.js")
	if err != nil {
		t.Skipf("读不到 dashboard.js: %v", err)
	}
	src := string(b)

	// 抠出需要的两个函数（按名字定位到下一个顶层 function/async function）
	extract := func(name string) string {
		// ★ 先试 async 版本：否则会把 `async function saveSetting(` 从
		// "function saveSetting(" 处切开，丢掉 async 前缀 ⇒ await 语法错误。
		i := strings.Index(src, "async function "+name+"(")
		if i < 0 {
			i = strings.Index(src, "function "+name+"(")
		}
		if i < 0 {
			return ""
		}
		rest := src[i:]
		// 找下一个顶层声明作为边界
		best := -1
		for _, marker := range []string{"\nasync function ", "\nfunction "} {
			if j := strings.Index(rest[1:], marker); j >= 0 {
				e := j + 1
				if best < 0 || e < best {
					best = e
				}
			}
		}
		if best > 0 {
			return rest[:best]
		}
		return rest
	}

	phFn := extract("isSecretPlaceholderValue")
	if phFn == "" {
		t.Fatal("★ 缺少 isSecretPlaceholderValue（前端会把掩码当真值回传）")
	}
	saveFn := extract("saveSetting")
	if saveFn == "" {
		t.Fatal("找不到 saveSetting")
	}

	harness := `
// ---- 假环境：只提供被抠出的两个函数需要的东西 ----
var state = { meta: {}, settings: {} };
var sent = [];
function api(path, opts) { sent.push(JSON.parse(opts.body)); return Promise.resolve({status:"ok"}); }
function toast() {}
function __(a) { return a; }
var INPUTS = {};
var document = { getElementById: function(id) { return INPUTS[id] || null; } };

` + phFn + "\n" + saveFn + `

// ---- 用例 ----
(async function () {
  var out = [];
  function mkInput(v) { return { value: v, style: {} }; }

  // ① 掩码：必须**不发请求**
  INPUTS["inp-core_llm_sources_x_api_key"] = mkInput("\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022");
  sent.length = 0;
  await saveSetting("core.llm.sources.x.api_key");
  out.push(["mask", sent.length]);

  // ② 真值：必须**发请求**且发的是真值
  INPUTS["inp-core_llm_sources_x_api_key"] = mkInput("sk-real-value");
  sent.length = 0;
  await saveSetting("core.llm.sources.x.api_key");
  out.push(["real", sent.length, sent[0] && sent[0].value]);

  // ③ 含圆点的真值不得被误杀
  INPUTS["inp-some_key"] = mkInput("a\u2022b");
  sent.length = 0;
  await saveSetting("some.key");
  out.push(["dot", sent.length]);

  // ④ 空值/普通值正常保存
  INPUTS["inp-plain"] = mkInput("hello");
  sent.length = 0;
  await saveSetting("plain");
  out.push(["plain", sent.length]);

  console.log(JSON.stringify(out));
})();
`
	tmp := filepath.Join(t.TempDir(), "h.js")
	if err := os.WriteFile(tmp, []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, tmp).CombinedOutput()
	if err != nil {
		t.Fatalf("harness 执行失败: %v\n%s", err, out)
	}
	var got []interface{}
	if err := json.Unmarshal(bytesTrim(out), &got); err != nil {
		t.Fatalf("harness 输出无法解析: %v\n%s", err, out)
	}
	if len(got) != 4 {
		t.Fatalf("harness 未跑完: %s", out)
	}
	get := func(i int) []interface{} { return got[i].([]interface{}) }

	// ① 掩码：0 次请求
	if n := get(0)[1].(float64); n != 0 {
		t.Errorf("★ 掩码被回传了（发了 %v 次请求）—— 真值会被掩码覆盖", n)
	}
	// ② 真值：发 1 次且值正确
	r := get(1)
	if len(r) < 3 || r[1].(float64) != 1 {
		t.Fatalf("★ 真值应正常保存，实际: %v", r)
	}
	if r[2].(string) != "sk-real-value" {
		t.Errorf("★ 保存的值不对: %v", r[2])
	}
	// ③ 含圆点的真值不得误杀
	if n := get(2)[1].(float64); n != 1 {
		t.Errorf("★ 含圆点的真值被误杀（%v 次请求）", n)
	}
	// ④ 普通值正常
	if n := get(3)[1].(float64); n != 1 {
		t.Errorf("普通值未保存: %v", n)
	}
}

func bytesTrim(b []byte) []byte {
	i := strings.IndexByte(string(b), '{')
	if i < 0 {
		return b
	}
	j := strings.LastIndexByte(string(b), '}')
	if j < i {
		return b
	}
	return b[i : j+1]
}
