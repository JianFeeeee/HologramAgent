package security

import (
	"strings"
	"testing"
)

// ★★ 判据：私钥块规则**不能误伤**。
//
// 放宽了尾部词匹配后必须确认：正常文本里的 "PRIVATE KEY" 字样不该被拦。
func TestSSHKeys_NoFalsePositives(t *testing.T) {
	s := newTestStore(t, true)

	benign := []struct{ desc, text string }{
		{"文档里提私钥概念", "私钥文件以 -----BEGIN OPENSSH PRIVATE KEY----- 开头，以对应的 END 行结尾。"},
		{"只引用头尾两行（无内容）",
			"-----BEGIN RSA PRIVATE KEY-----\n-----END RSA PRIVATE KEY-----"},
		{"短内容（< 64 字节）",
			"-----BEGIN RSA PRIVATE KEY-----\nshortcontent\n-----END RSA PRIVATE KEY-----"},
		{"讨论 SSH 配置", "把 PRIVATE KEY 权限设成 0600，authorized_keys 用 0644"},
		{"代码里的常量名", `const PRIVATE_KEY_HEADER = "-----BEGIN"`},
		{"正常的技术文档", "本章讨论密钥管理与轮换策略，涉及 PRIVATE KEY 的使用规范。"},
	}
	for _, c := range benign {
		out, evts := s.Replace(c.text, "sshfp", Patterns)
		if len(evts) > 0 {
			t.Errorf("★ 误伤「%s」: %q → %v", c.desc, c.text, evts)
		}
		if out != c.text {
			t.Errorf("未命中却改动了文本「%s」", c.desc)
		}
	}
	t.Log("✅ 私钥块规则无误伤（含短内容/只引头尾/文档讨论）")
}

// ★ 判据：真正的长私钥体必须拦（与上面的"短内容"形成对照）。
func TestSSHKeys_LongBodyIsCaught(t *testing.T) {
	s := newTestStore(t, true)
	body := strings.Repeat("MIIEowIBAAKCAQEA7xYz9aBc", 6) // > 64 字节
	text := "-----BEGIN RSA PRIVATE KEY-----\n" + body + "\n-----END RSA PRIVATE KEY-----"

	out, evts := s.Replace(text, "sshl", Patterns)
	if len(evts) == 0 {
		t.Fatal("★ 真实长度的私钥体未被拦截")
	}
	if strings.Contains(out, "MIIEowIBAAKCAQEA") {
		t.Fatal("密钥体残留")
	}
	t.Logf("✅ %d 字节密钥体被拦（别名 %s）", len(text), evts[0].Alias)
}
