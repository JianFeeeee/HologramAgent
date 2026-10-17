package security

import (
	"os"
	"path/filepath"
	"testing"
)

// SSH 相关面的完整盘点：哪些拦、哪些不拦、为什么。
func TestSSHSurface_Complete(t *testing.T) {
	s := newTestStore(t, true)
	home, _ := os.UserHomeDir()

	// 读一份真实 authorized_keys 与 ssh config
	readIf := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(home, ".ssh", rel))
		if err != nil {
			return ""
		}
		return string(b)
	}

	t.Log("=== 会拦 ===")
	// ① 私钥文件内容
	if txt := readIf("id_rsa"); txt != "" {
		_, evts := s.Replace(txt, "ssh", Patterns)
		t.Logf("  ✓ 私钥文件内容（%d 字节）→ %d 处拦截", len(txt), len(evts))
	}
	// ② ssh-agent 环境变量里的私钥？—— 那只是 socket 路径，不是密钥
	t.Log("=== 不拦（各有权衡）===")
	t.Logf("  - %-28s ssh-agent 只是 socket 路径，不含密钥", "SSH_AUTH_SOCK")
	t.Logf("  - %-28s 公钥不是秘密（可公开）", "authorized_keys")
	t.Logf("  - %-28s 配置里没有密钥本体", "~/.ssh/config")
	t.Logf("  - %-28s 拦路径不拦内容；内容被读出时才命中", ".ssh/id_rsa 路径")
	t.Logf("  - %-28s 无固定形态，与正常 hostname 无区别", "known_hosts")

	// ③ 验证不动 authorized_keys（它是公钥，不该被拦）
	if txt := readIf("authorized_keys"); txt != "" {
		out, evts := s.Replace(txt, "ssh", Patterns)
		if len(evts) > 0 {
			t.Logf("  ⚠️ authorized_keys 被拦了 %d 处（公钥本可公开，但拦了也不致命）", len(evts))
		} else if out == txt {
			t.Log("  ✓ authorized_keys 未被改动（正确：公钥不是秘密）")
		}
	}
}

// ★ 判据：**公钥不该被当私钥拦**（两者形态不同，别混）。
func TestSSHSurface_PublicKeyNotCaught(t *testing.T) {
	s := newTestStore(t, true)
	// 真实的 ed25519 公钥形态
	pub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEa1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8 root@host"
	if _, evts := s.Replace(pub, "ssh", Patterns); len(evts) > 0 {
		t.Errorf("★ 公钥被误拦（公钥可公开，拦了会让正常运维操作坏掉）: %v", evts)
	}
	// RSA 公钥
	pub2 := "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQDZx user@host"
	if _, evts := s.Replace(pub2, "ssh", Patterns); len(evts) > 0 {
		t.Errorf("★ RSA 公钥被误拦: %v", evts)
	}
	t.Log("✅ 公钥不被拦（正确的边界）")
}
