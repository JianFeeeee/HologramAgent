package plugin

import (
	"fmt"
	"testing"
)

// ── 自动禁用的判因边界 ────────────────────────────────────────
//
// 这两条钉住**只禁版本不兼容**。若放宽到"任何启动失败都禁用"，
// 端口被占、配置缺失这类**可自愈**的问题会变成永久不加载——
// 那比反复重试糟糕得多（用户修好环境也不会恢复）。

// 判据 1：主版本不匹配 ⇒ 认得出（应当自动禁用）。
func TestIsVersionIncompatibleErr_RecognizesHandshakeMismatch(t *testing.T) {
	// 与 process.go 里握手失败的真实文案一致
	err := fmt.Errorf("proc: %s 主版本不匹配（插件 SDK %s，内核 %s）——请用配套 hmapdev 重编",
		"qq", "1.0.0", "1.4.0")
	if !isVersionIncompatibleErr(err) {
		t.Fatal("主版本不匹配必须被识别为版本不兼容")
	}
	// 详情必须进 reason（用户在界面上要看到"插件 SDK 1.0.0 / 内核 1.4.0"）
	reason := formatIncompatibleReason(err)
	if !containsAll(reason, "1.0.0", "1.4.0", "不兼容") {
		t.Errorf("reason 必须带上两端版本号，实际: %s", reason)
	}
}

// 判据 2：其它启动失败 ⇒ **不**认（不该自动禁用）。
func TestIsVersionIncompatibleErr_IgnoresTransientFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"端口被占", fmt.Errorf("listen tcp :8080: bind: address already in use")},
		{"配置缺失", fmt.Errorf("plugin qq: missing required config napcat_url")},
		{"进程崩溃", fmt.Errorf("proc: qq 退出码 1")},
		{"插件不存在", fmt.Errorf("plugin foo not installed")},
	}
	for _, c := range cases {
		if isVersionIncompatibleErr(c.err) {
			t.Errorf("%s 不该被判为版本不兼容（会把可自愈故障变成永久禁用）: %v", c.name, c.err)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
