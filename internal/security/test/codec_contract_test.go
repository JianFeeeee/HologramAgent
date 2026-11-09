package test

// internal/security 的黑盒判据：只用导出构造 + 公开方法，输入 → 输出。
// 不读源码、不碰 Codec/Store 内部字段、不用任何 ForTest 钩子。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

func fixedKey(byteVal byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byteVal
	}
	return k
}

func newCodec(t *testing.T, key []byte, domain string) *security.Codec {
	t.Helper()
	c, err := security.NewCodec(key, domain)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	return c
}

// TestCodecSealOpenRoundTrip：Seal → Open 得回原文。
func TestCodecSealOpenRoundTrip(t *testing.T) {
	c := newCodec(t, fixedKey(7), "test-domain")
	const plain = "sk-super-secret-value-123"
	sealed, err := c.Seal(plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed == plain {
		t.Fatal("★ Seal 返回了原文 —— 没有加密")
	}
	got, err := c.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != plain {
		t.Fatalf("★ Open = %q，期望 %q", got, plain)
	}
}

// TestCodecDomainSeparation：不同 domain 的密文互不通用。
func TestCodecDomainSeparation(t *testing.T) {
	a := newCodec(t, fixedKey(7), "domain-a")
	b := newCodec(t, fixedKey(7), "domain-b")
	sealed, err := a.Seal("payload")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(sealed); err == nil {
		t.Fatal("★ 另一个 domain 能解开密文 —— 域隔离失效")
	}
}

// TestCodecWrongKeyCannotOpen：换钥后旧密文不可读。
func TestCodecWrongKeyCannotOpen(t *testing.T) {
	a := newCodec(t, fixedKey(1), "d")
	b := newCodec(t, fixedKey(2), "d")
	sealed, err := a.Seal("payload")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(sealed); err == nil {
		t.Fatal("★ 错误密钥能解开密文")
	}
}

// TestCodecEnabledAndDomain：元信息输出。
func TestCodecEnabledAndDomain(t *testing.T) {
	c := newCodec(t, fixedKey(3), "my-domain")
	if !c.Enabled() {
		t.Fatal("★ 有效密钥下 Enabled()=false")
	}
	if got := c.Domain(); got != "my-domain" {
		t.Fatalf("★ Domain() = %q，期望 my-domain", got)
	}
	var nilCodec *security.Codec
	if nilCodec.Enabled() {
		t.Fatal("★ nil Codec 报告 Enabled=true")
	}
}

// TestCodecOldKeysStillDecrypt：轮换后历史密文仍可读。
func TestCodecOldKeysStillDecrypt(t *testing.T) {
	old := fixedKey(4)
	cur := fixedKey(5)
	oldCodec := newCodec(t, old, "d")
	sealed, err := oldCodec.Seal("legacy-value")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := security.NewCodecWithOldKeys(cur, "d", [][]byte{old})
	if err != nil {
		t.Fatalf("NewCodecWithOldKeys: %v", err)
	}
	got, err := rotated.Open(sealed)
	if err != nil {
		t.Fatalf("★ 轮换后旧密文读不出：%v", err)
	}
	if got != "legacy-value" {
		t.Fatalf("★ 旧密文解出 %q", got)
	}
}

// TestCodecOpenLegacyPlaintextPassthrough：无密文前缀的旧数据原样透传。
//
// 这是明确的兼容契约：加密是后来加的，库里存在未加密的历史值。
// 把它们判成错误会让整库读不出来。
func TestCodecOpenLegacyPlaintextPassthrough(t *testing.T) {
	c := newCodec(t, fixedKey(9), "d")
	for _, in := range []string{"", "legacy-plain-value", "abc"} {
		got, err := c.Open(in)
		if err != nil {
			t.Fatalf("★ Open(%q) 报错：%v —— 旧明文应原样透传", in, err)
		}
		if got != in {
			t.Fatalf("★ Open(%q) = %q，期望原样返回", in, got)
		}
	}
}

// TestCodecOpenRejectsCorruptedCiphertext：带密文前缀但内容坏掉必须报错。
func TestCodecOpenRejectsCorruptedCiphertext(t *testing.T) {
	c := newCodec(t, fixedKey(9), "d")
	for _, in := range []string{
		security.EncPrefixV1 + "!!!not-base64!!!",
		security.EncPrefixV1,
	} {
		if _, err := c.Open(in); err == nil {
			t.Fatalf("★ Open(%q) 没报错 —— 损坏密文被静默吞下", in)
		}
	}
}
