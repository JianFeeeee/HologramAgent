package security

// 判据：**加密能力只有一处实现**（2026-11-04 T3.2）。
//
// ## 用户裁定
//
// > store也应该调用security的加密能力！
//
// ## 改动前的事实（实测，非推测）
//
// 两套**完全重复**的 AES-256-GCM：
//
//	Store.seal/open     store.go:372,388    裸 base64
//	Codec.Seal/Open     codec.go:113,139    enc:v1: 前缀 + 域隔离 + 幂等
//
// 算法、nonce 布局（前置 + io.ReadFull(rand)）、密钥环轮换逻辑全都一样，
// 只有 Codec 多了前缀与域隔离。
//
// 危害不是"代码重复"这么轻：
//
//   - 两套实现各自演化，任何一侧的改动不会被另一侧的判据发现
//   - Store 缺域隔离 ⇒ 变量库与配置用同一把密钥（安全倒退）
//   - Store 缺前缀 ⇒ 无法区分"旧密文"与"明文"（惰性迁移的障碍）
//
// ## 本组判据守什么
//
//	① Store 写入的密文必须带 enc:v1: 前缀（旧实现是裸 base64）
//	② 变量库与配置**不得同域**（同域 = 一处泄漏可推导另一处）
//	③ 旧格式（裸 base64）密文必须仍可读，且读到后就地升级
//	④ 密钥轮换后旧密文仍可解（不能因换实现而丢数据）

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStoreWritesPrefixedCiphertext 钉住 ①：新写入必须带前缀。
//
// 变红条件：把 Store.Put 改回自建 seal（裸 base64）⇒ 红。
func TestStoreWritesPrefixedCiphertext(t *testing.T) {
	dir := t.TempDir()
	s, err := MustStore(Options{
		Path:    filepath.Join(dir, "v.json"),
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("MustStore: %v", err)
	}
	if _, err := s.Submit("ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", "test"); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// 从内存里读原始密文（绕过解密）。
	s.mu.RLock()
	var sealed string
	for _, e := range s.data {
		sealed = e.Sealed
	}
	s.mu.RUnlock()

	if sealed == "" {
		t.Fatal("没读出密文")
	}
	if !IsEncrypted(sealed) {
		t.Fatalf("★ Store 写入的密文没有 %q 前缀 —— 说明它还在用自建 seal。\n"+
			"加密必须委托 Codec（用户裁定：「store 也应该调用 security 的加密能力」）。\n"+
			"实际前缀（前 24 字符）: %q", EncPrefixV1, sealed[:min(24, len(sealed))])
	}
}

// TestStoreAndConfigUseDifferentDomains 钉住 ②：域隔离（**安全判据**）。
//
// 变红条件：把 storeCodecDomain 改成与 configCodecDomain 相同 ⇒ 红。
//
// 为什么这条必须有：同域意味着"用同一把密钥派生同一把子密钥加密两处不同
// 用途的数据"。一旦变量库的明文泄漏，它就成为配置密文的字典攻击素材。
// 这是安全倒退，不是风格问题。
func TestStoreAndConfigUseDifferentDomains(t *testing.T) {
	if storeCodecDomain == configCodecDomain {
		t.Fatalf("★ 变量库与配置用了同一个加密域 %q\n\n"+
			"两者必须不同域：同域时一处明文泄漏可推导另一处密文。\n"+
			"storeCodecDomain=%q configCodecDomain=%q",
			storeCodecDomain, storeCodecDomain, configCodecDomain)
	}
	// 更强的判据：实际用同一把主密钥构造两个 codec，密文必须互不可解。
	const key = "0123456789abcdef0123456789abcdef"
	storeCodec := NewLegacyRingCodec(storeCodecDomain, [][]byte{[]byte(key)})
	cfgCodec, err := NewConfigCodec([]byte(key), nil)
	if err != nil {
		t.Fatalf("NewConfigCodec: %v", err)
	}

	sealedByStore, err := storeCodec.Seal("same-plaintext")
	if err != nil {
		t.Fatalf("store Seal: %v", err)
	}
	if plain, oerr := cfgCodec.Open(sealedByStore); oerr == nil {
		t.Fatalf("★ 配置 codec 解开了变量库的密文（明文 %q）—— 域隔离失效", plain)
	}
}

// TestLegacyCiphertextStillReadableAndMigrates 钉住 ③：旧格式可读 + 惰性迁移。
//
// 变红条件：
//   - openWithStoreRing 不兼容无前缀格式 ⇒ 旧数据全丢（红）
//   - 读到旧格式后不回写 ⇒ 文件里仍是旧格式（红）
func TestLegacyCiphertextStillReadableAndMigrates(t *testing.T) {
	dir := t.TempDir()
	const key = "0123456789abcdef0123456789abcdef"
	path := filepath.Join(dir, "v.json")

	// ① 用**旧格式**（裸 base64，无前缀）造一条密文 —— 模拟 T3.2 之前的数据。
	legacyCodec := NewLegacyRingCodec(storeCodecDomain, [][]byte{[]byte(key)})
	legacySealed, err := legacyCodec.Seal("ghp_LEGACY_TOKEN_0123456789")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Seal 带前缀；去掉它才是真正的旧格式（裸 base64）。
	oldFormat := strings.TrimPrefix(legacySealed, EncPrefixV1)
	if IsEncrypted(oldFormat) {
		t.Fatal("构造旧格式失败（仍有前缀）")
	}

	// ② 写一份带旧格式条目的库文件。
	s, err := MustStore(Options{
		Path:    path,
		KeyFn:   func() ([]byte, error) { return []byte(key), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("MustStore: %v", err)
	}
	s.mu.Lock()
	s.data["sec_legacy01"] = &Entry{Alias: "sec_legacy01", Sealed: oldFormat, Source: "intercepted"}
	if err := s.saveLocked(); err != nil {
		t.Fatalf("saveLocked: %v", err)
	}
	s.mu.Unlock()

	// ③ 重新加载后必须能**解开**旧格式（否则旧数据全丢）。
	s2, err := MustStore(Options{
		Path:    path,
		KeyFn:   func() ([]byte, error) { return []byte(key), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("重新加载: %v", err)
	}
	plain, n, _ := s2.Fill("token=@{sec_legacy01}")
	if n != 1 {
		t.Fatalf("★ 旧格式密文解不开（回填 0 处）—— 存量数据全丢。实际: %q", plain)
	}
	if !strings.Contains(plain, "ghp_LEGACY_TOKEN") {
		t.Fatalf("回填结果不对: %q", plain)
	}

	// ④ 惰性迁移：读到旧格式后，落盘应升级为新格式。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读落盘文件: %v", err)
	}
	if strings.Contains(string(raw), oldFormat) {
		t.Fatalf("★ 旧格式密文仍留在磁盘上 —— 惰性迁移没有回写。\n" +
			"两种格式永久共存会让'落盘都是新格式'这类判据永远无法成立。")
	}
	if !strings.Contains(string(raw), EncPrefixV1) {
		t.Fatalf("迁移后应出现 %q 前缀，实际文件内容（前 200 字符）: %s",
			EncPrefixV1, string(raw[:min(200, len(raw))]))
	}
}

// TestRotationKeepsOldCiphertextReadable 钉住 ④：轮换后旧密文仍可解。
//
// 变红条件：RotateKey 只保留新钥、丢掉旧钥 ⇒ 旧条目全变"解不开"。
func TestRotationKeepsOldCiphertextReadable(t *testing.T) {
	dir := t.TempDir()
	oldKey := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	newKey := []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	path := filepath.Join(dir, "v.json")
	s, err := MustStore(Options{
		Path:    path,
		KeyFn:   func() ([]byte, error) { return oldKey, nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("MustStore: %v", err)
	}
	alias, err := s.Submit("ghp_ROTATE_ME_0123456789", "test")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// 轮换到新钥（旧钥进历史环）。
	if err := s.RotateKey(newKey); err != nil {
		t.Fatalf("RotateKey: %v", err)
	}

	// 轮换后必须仍能回填（否则所有历史 @{sec_xxx} 失效）。
	out, n, _ := s.Fill("token=@{" + alias + "}")
	if n != 1 || !strings.Contains(out, "ghp_ROTATE_ME") {
		t.Fatalf("★ 轮换后旧条目解不开（回填 %d 处）: %q\n"+
			"这会静默作废所有历史引用 —— 用户看到的是一次凭据替换失败。", n, out)
	}
}

// min 是 Go 版本早于 1.21 时的兜底（本仓支持较老工具链）。
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
