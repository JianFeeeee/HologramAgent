package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ★ 用**本机真实密钥**验证（比构造样本可靠）。
func TestSSHKeys_RealFiles(t *testing.T) {
	s := newTestStore(t, true)

	home, _ := os.UserHomeDir()
	cands := []string{
		filepath.Join(home, ".ssh", "id_rsa"),
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(home, ".ssh", "id_ecdsa"),
	}
	found := 0
	for _, p := range cands {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		found++
		text := string(b)
		out, evts := s.Replace(text, "ssh", Patterns)
		hdr := strings.SplitN(text, "\n", 2)[0]
		if len(evts) == 0 {
			t.Errorf("★ %s（%s）**未被拦截** —— 私钥明文会进上下文", filepath.Base(p), hdr)
			continue
		}
		if strings.Contains(out, "PRIVATE KEY") {
			t.Errorf("★ %s 拦了但密钥内容仍残留", filepath.Base(p))
		}
		t.Logf("  ✓ %-14s %-38s → %s", filepath.Base(p), hdr, evts[0].Alias)
	}
	if found == 0 {
		t.Skip("本机没有密钥文件")
	}
}

// 覆盖各类真实密钥头的**合成样本**（PEM 与 OpenSSH 都有）。
func TestSSHKeys_AllHeaderForms(t *testing.T) {
	s := newTestStore(t, true)
	body := strings.Repeat("MIIEowIBAAKCAQEA7xYz", 8) +
		"\n" + strings.Repeat("b3BlbnNzaC1rZXkAAAAB", 8)

	headers := []string{
		"-----BEGIN PRIVATE KEY-----",     // PKCS#8
		"-----BEGIN RSA PRIVATE KEY-----", // PKCS#1
		"-----BEGIN DSA PRIVATE KEY-----",
		"-----BEGIN EC PRIVATE KEY-----",
		"-----BEGIN OPENSSH PRIVATE KEY-----",   // 现代 ssh-keygen 默认
		"-----BEGIN ENCRYPTED PRIVATE KEY-----", // 带口令
		"-----BEGIN PGP PRIVATE KEY BLOCK-----", // PGP
		"-----BEGIN SSH2 ENCRYPTED PRIVATE KEY-----",
	}
	for _, h := range headers {
		footer := strings.Replace(h, "BEGIN", "END", 1)
		text := h + "\n" + body + "\n" + footer
		_, evts := s.Replace(text, "ssh", Patterns)
		if len(evts) == 0 {
			t.Errorf("  ✗ %-42s 未拦截", h)
		} else {
			t.Logf("  ✓ %-42s", h)
		}
	}
}
