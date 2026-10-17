package security

import (
	"strings"
	"testing"
)

// 盘点：什么会被拦、什么不会。这份清单直接对应生产行为。
func TestSurvey_WhatIsCaught(t *testing.T) {
	s := newTestStore(t, true)

	caught := []struct{ name, text string }{
		{"GitHub token", "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"},
		{"GitHub fine-grained", "github_pat_" + strings.Repeat("A", 60)},
		{"GitLab", "glpat-FAKE-TEST-TOKEN-NOT-REAL"},
		{"OpenAI", "sk-FAKE-TEST-KEY-NOT-REAL-0000000"},
		{"Anthropic", "sk-ant-FAKE-TEST-KEY-NOT-REAL-00"},
		{"AWS", "AKIA0000FAKE0000TEST"},
		{"AWS 临时", "ASIA0000FAKE0000TEST"},
		{"Slack", "xoxb-FAKE-TEST-TOKEN-NOT-REAL"},
		{"Bearer 头", "Authorization: Bearer " + strings.Repeat("a", 40)},
		{"私钥块", "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("MIIE", 20) + "\n-----END RSA PRIVATE KEY-----"},
		{"赋值式 api_key", `api_key = "FAKE-TEST-ASSIGNED-VALUE-000"`},
		{"赋值式 password", `password: "FAKE-TEST-PASSWORD-NOT-REAL"`},
		{"赋值式 access_token", `access_token="FAKE-TEST-TOKEN-NOT-REAL-00"`},
		{"赋值式 client_secret", `client_secret = "FAKE-TEST-TOKEN-NOT-REAL-00"`},
		{"单引号", `api_key = 'FAKE-TEST-ASSIGNED-VALUE-000'`},
		{"无引号", `api_key=FAKE-TEST-ASSIGNED-VALUE-000`},
	}
	t.Log("=== 会被拦截（→ 换成 @{sec_xxx}）===")
	for _, c := range caught {
		out, evts := s.Replace(c.text, "survey", Patterns)
		if len(evts) == 0 {
			t.Errorf("  ✗ %-22s 未拦截: %q", c.name, c.text)
			continue
		}
		if strings.Contains(out, c.text) {
			t.Errorf("  ✗ %-22s 拦了但原文还在", c.name)
			continue
		}
		t.Logf("  ✓ %-22s → %s", c.name, evts[0].Alias)
	}
}

func TestSurvey_WhatIsNotCaught(t *testing.T) {
	s := newTestStore(t, true)

	// ★ 这些**刻意不拦**，每条都有理由。列出来是为了让人知道边界在哪。
	notCaught := []struct{ name, text, why string }{
		{"40位 hex（Gitea token 形态）", "0123456789abcdef0123456789abcdef01234567",
			"与 git commit SHA 形态完全相同 —— 拦它会误伤每个 commit id"},
		{"裸 32 位十六进制", "a1b2c3d4e5f60718293a4b5c6d7e8f90",
			"与 UUID/会话 id/哈希撞车，太泛"},
		{"JWT", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghijklmnop",
			"形态宽泛，误伤风险高于收益（且通常带 Bearer 前缀，那条会拦）"},
		{"密码短语", "correct horse battery staple correct",
			"无固定形态，无法与正常文本区分"},
		{"PIN/短口令", "884213", "太短，任何数字都会撞"},
		{"手机号/身份证", "13800138000", "PII 不是本模块目标（且误伤面极大）"},
		{"普通 URL 里的密码", "https://user:pass@host/path",
			"需要专门规则；当前未覆盖（已知缺口）"},
		{"SSH 私钥**文件路径**", "/home/u/.ssh/id_rsa",
			"拦路径不拦内容 —— 文件内容被读出时会命中私钥块规则"},
		{"环境变量**名字**", "${GITEA_TOKEN}", "名字不是秘密，值是"},
		{"中文口令", "我的密码是甲乙丙丁戊己庚辛",
			"无固定字符集，正则无法可靠识别"},
	}
	t.Log("=== 刻意不拦（每条都有理由）===")
	for _, c := range notCaught {
		out, evts := s.Replace(c.text, "survey", Patterns)
		if len(evts) > 0 {
			t.Logf("  ! %-26s 实际被拦了（{P}）: %s", c.name, c.why)
			continue
		}
		if out != c.text {
			t.Errorf("  ✗ %-26s 未拦却改动了文本", c.name)
			continue
		}
		t.Logf("  - %-26s 原因: %s", c.name, c.why)
	}
}
