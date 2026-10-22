package sdk

import (
	"os"
	"strings"
	"testing"
)

// 存量 plugin.json 里写的是旧值 "homeagent"（本机实测 huawei_smarthome）。
// 改名后必须**继续接受并归一化**它 —— 否则那些插件在加载时被判为非法声明，
// 反代直接不生效，症状是"插件好好的但页面打不开"，且没有任何报错指向改名。
func TestProxyAuthLegacyValueStillAccepted(t *testing.T) {
	if !ValidProxyAuth(ProxyAuthAgentLegacy) {
		t.Fatalf("旧值 %q 必须仍被接受", ProxyAuthAgentLegacy)
	}
	if ProxyAuthAgentLegacy == ProxyAuthAgent {
		t.Fatal("旧值应与新值不同，否则本判据测不到东西")
	}
	// ★ 归一化是最关键的一步：下游一律只可能拿到新值，
	// 否则每个判断点都要写两遍比较，漏一处就是鉴权绕过。
	if got := EffectiveProxyAuth(ProxyAuthAgentLegacy); got != ProxyAuthAgent {
		t.Fatalf("旧值应归一化为 %q，实际 %q", ProxyAuthAgent, got)
	}
}

func TestProxyAuthDefaultsToHomeAgent(t *testing.T) {
	// 空串必须归一化为「HomeAgent 统一保护」——这是安全默认。
	// 若哪天有人把默认改成 none，这条会立刻红。
	if got := EffectiveProxyAuth(""); got != ProxyAuthAgent {
		t.Fatalf("空 auth 应归一化为 %q，实际 %q", ProxyAuthAgent, got)
	}
	if got := EffectiveProxyAuth(ProxyAuthNone); got != ProxyAuthNone {
		t.Fatalf("显式 none 应保持 none，实际 %q", got)
	}
	for _, ok := range []string{"", ProxyAuthAgent, ProxyAuthNone} {
		if !ValidProxyAuth(ok) {
			t.Errorf("%q 应合法", ok)
		}
	}
	for _, bad := range []string{"nope", "HOMEAGENT", "None", "true"} {
		if ValidProxyAuth(bad) {
			t.Errorf("%q 应非法", bad)
		}
	}
}

func TestValidProxyHostLabel(t *testing.T) {
	legit := []string{"huawei", "a", "a-b", "abc123", "0", "x" + string(make([]byte, 0)) + "yz"}
	for _, s := range legit {
		if !ValidProxyHostLabel(s) {
			t.Errorf("%q 应为合法 label", s)
		}
	}
	bad := []string{
		"", "-a", "a-", "-", "a_b", "a.b", "A", "aB", "a b",
		"a/b", "a:b", string(make([]byte, 64)), // 超长 63
	}
	for _, s := range bad {
		if ValidProxyHostLabel(s) {
			t.Errorf("%q 应为非法 label", s)
		}
	}
	// 边界：恰好 63 合法，64 非法
	l63 := ""
	for i := 0; i < 63; i++ {
		l63 += "a"
	}
	if !ValidProxyHostLabel(l63) {
		t.Error("63 字符应为合法 label")
	}
	if ValidProxyHostLabel(l63 + "a") {
		t.Error("64 字符应为非法 label")
	}
}

func TestNormalizeProxyHost(t *testing.T) {
	cases := map[string]string{
		"huawei_smarthome": "huawei-smarthome", // 下划线不是合法 DNS label
		"webui":            "webui",
		"UPPER_Case":       "upper-case",
		"a__b":             "a--b",
		"__x__":            "x",
		"---":              "plugin", // 全非法 → 保守回退
		"":                 "plugin",
		"a.b.c":            "abc",
	}
	for in, want := range cases {
		if got := NormalizeProxyHost(in); got != want {
			t.Errorf("NormalizeProxyHost(%q) = %q，期望 %q", in, got, want)
		}
	}
	// 归一化结果必须自身合法（产物自洽）
	for _, in := range []string{"huawei_smarthome", "UPPER_Case", "__x__", "a.b.c", "非常长的名字非常长的名字非常长的名字非常长的名字非常长的名字非常长的名字非常长的名字"} {
		if got := NormalizeProxyHost(in); !ValidProxyHostLabel(got) {
			t.Errorf("NormalizeProxyHost(%q) = %q 不合法", in, got)
		}
	}
}

func TestValidateProxyDef(t *testing.T) {
	valid := []ProxyDef{
		{Target: "127.0.0.1:12100"},
		{Target: "http://127.0.0.1:12100"},
		{Target: "127.0.0.1:12100", Host: "huawei"},
		{Target: "127.0.0.1:12100", Auth: ProxyAuthNone},
		{Target: "127.0.0.1:12100", Auth: ProxyAuthAgent, WebSocket: true},
		{Target: "127.0.0.1:3000/base", Host: "x"},
		{Target: "https://example.com", Host: "ext"}, // 远程上游也允许（由 auth 决定安全性）
	}
	for _, d := range valid {
		if msg := ValidateProxyDef(d); msg != "" {
			t.Errorf("%+v 应合法，却报: %s", d, msg)
		}
	}

	bad := []ProxyDef{
		{},                                       // 无 target
		{Target: "   "},                          // 空白 target
		{Target: "127.0.0.1:12100", Auth: "yes"}, // auth 非法
		{Target: "127.0.0.1:12100", Host: "a_b"}, // host 非法
		{Target: "127.0.0.1:12100", Host: "-x"},
		{Target: "127.0.0.1:12100", Host: "X"},
		{Target: "://12100"},           // 无主机
		{Target: "http:///path"},       // 无主机
		{Target: "127.0.0.1:notaport"}, // 端口非数字
	}
	for _, d := range bad {
		if msg := ValidateProxyDef(d); msg == "" {
			t.Errorf("%+v 应被拒绝，却通过了", d)
		}
	}
}

// ---- 单一入口原则 ----

// 被反代的插件必须能同时适配 Host 形态与 Path 形态。这两条判据把
// 「插件内部不得用根绝对路径」这条契约钉在**可执行**的层面：
// 声明合法不代表它的资源能被两种形态访问到 —— 后者取决于插件前端的写法，
// 而 SDK 只能把要求写清楚并给出校验工具。
func TestSingleEntryPrincipleDocumented(t *testing.T) {
	// Path 形态下插件前端必须用相对路径，否则请求会打到门户自己。
	// 这是**文档级约定**，只能靠 review 与这份判据共同保证：
	// 判据确保 SDK 里确实写明了这条要求（防止后来者删掉注释）。
	src, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"单一入口原则",
		"相对路径",
		"根绝对路径",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("SDK 文档缺少「%s」—— 单一入口原则是反代的硬要求，不能只存在于口头约定里", want)
		}
	}
}

// strip_path 的两种语义必须由声明者显式选，且非法组合要被挡住。
func TestStripPathValidation(t *testing.T) {
	// 合法：两种模式
	for _, d := range []ProxyDef{
		{Target: "127.0.0.1:1", Path: "/p/app", StripPath: true},
		{Target: "127.0.0.1:1", Path: "/api/v1/device", StripPath: false},
	} {
		if msg := ValidateProxyDef(d); msg != "" {
			t.Errorf("应合法却被拒: %+v → %s", d, msg)
		}
	}
	// 非法：strip_path 但没有 path（没有可剥的前缀）
	if msg := ValidateProxyDef(ProxyDef{Target: "127.0.0.1:1", StripPath: true}); msg == "" {
		t.Error("strip_path=true 而无 path 应被拒（没有可剥的前缀）")
	}
	// 非法：前缀模式挂到根会吞掉整个门户。
	// 实际由「不应以 / 结尾」规则挡下（"/" 同时是前缀又以 / 结尾），
	// 这里断言的是**行为**：这种声明无论如何都不能通过。
	if msg := ValidateProxyDef(ProxyDef{Target: "127.0.0.1:1", Path: "/", StripPath: true}); msg == "" {
		t.Error("path=\"/\" + strip_path 应被拒（会覆盖整个门户）")
	}
}
