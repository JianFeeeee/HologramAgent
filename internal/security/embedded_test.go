package security

// 内嵌主密钥的判据。
//
// 这组判据要证明的核心**不是**"内嵌能生效"，而是
// **"内嵌不会把已有数据变成读不出来的垃圾"** —— 那才是这条改动真正的风险。
// 一个只测"有内嵌钥就用它"的判据会全绿，而线上老机器升级后会解不开库。

import (
	"os"
	"path/filepath"
	"testing"
)

// withEmbeddedKey 临时把内嵌钥设成 k（测试结束还原）。
//
// 用直接赋值而不是 ldflags：判据要能覆盖"内嵌/未内嵌"两种形态。
func withEmbeddedKey(t *testing.T, k string) {
	t.Helper()
	old := embeddedKeyHex
	embeddedKeyHex = k
	t.Cleanup(func() { embeddedKeyHex = old })
}

// ★★★ 判据：未内嵌时行为**逐字不变**（否则是个破坏性变更）。
//
// 默认 Makefile 构建出的二进制没有内嵌钥，必须在所有老部署上照常工作：
// 仍走 systemd/env/磁盘链，且没有磁盘钥时仍会生成。
func TestNoEmbeddedKeyKeepsLegacyBehavior(t *testing.T) {
	withEmbeddedKey(t, "")
	if EmbeddedKeyPresent() {
		t.Fatal("空串不应算作已内嵌")
	}
	dir := t.TempDir()

	// ① 无磁盘钥 → 生成并落盘（老行为）
	k1, src1, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if src1 == "" || src1[0:9] == "embedded" {
		t.Errorf("未内嵌时来源不该是 embedded: %q", src1)
	}
	p := filepath.Join(dir, KeyFileName)
	if fi, err := os.Stat(p); err != nil {
		t.Fatalf("应生成 %s: %v", KeyFileName, err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("密钥文件权限应为 0600，实际 %v", fi.Mode().Perm())
	}
	// ② 再读应拿到同一把（稳定）
	k2, _, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !sameKey(k1, k2) {
		t.Fatal("同一目录两次读取应得到同一把钥")
	}
	// ③ 无额外解密钥
	if extra := AdditionalDecryptKeys(dir, k1); len(extra) != 0 {
		t.Errorf("未内嵌时不该有额外解密钥: %d", len(extra))
	}
}

// ★★★ 判据：内嵌钥被采用，且**来源可辨识**、指纹不泄漏密钥本身。
func TestEmbeddedKeyIsUsed(t *testing.T) {
	hexKey := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	withEmbeddedKey(t, hexKey)
	if !EmbeddedKeyPresent() {
		t.Fatal("应报告已内嵌")
	}
	want, _ := parseKey(hexKey)

	dir := t.TempDir()
	got, src, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !sameKey(got, want) {
		t.Fatal("★ 未采用内嵌钥")
	}
	// 来源必须能看出是内嵌（运维要能一眼判断"这把钥从哪来"）
	if len(src) < 8 || src[:8] != "embedded" {
		t.Errorf("来源应标明 embedded: %q", src)
	}
	// 指纹不得包含完整密钥
	fp := EmbeddedKeyFingerprint()
	if fp == "" || len(fp) != 8 {
		t.Errorf("指纹形状不对: %q", fp)
	}
	if len(fp) >= len(hexKey) {
		t.Error("★ 指纹不得等于/长于密钥本身（会泄漏）")
	}
	// 内嵌优先于磁盘：即使磁盘有另一把钥，也以内嵌为准
	other, _ := parseKey("1111111111111111111111111111111111111111111111111111111111111111")
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte(hexKeyOf(other)), 0o600); err != nil {
		t.Fatal(err)
	}
	got2, _, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !sameKey(got2, want) {
		t.Error("★ 内嵌钥应优先于磁盘钥")
	}
}

// ★★★ **最关键的一条**：内嵌钥与磁盘钥不同时，磁盘钥必须进入**解密集合**。
//
// 这是"内嵌"与"防丢失"的交界处。反例（本判据要挡住的）：
//
//	老机器用磁盘钥加密过 config.db → 升级到内嵌了另一把钥的二进制
//	→ 只信内嵌钥 → 旧密文全解不开 → 内核响亮失败 → 用户"重建库" ⇒ **凭据全丢**
//
// 那恰好与内嵌的初衷相反。所以磁盘钥必须继续参与 Open。
func TestEmbeddedKeyKeepsDiskKeyForDecrypt(t *testing.T) {
	hexKey := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	withEmbeddedKey(t, hexKey)
	embedded, _ := parseKey(hexKey)

	dir := t.TempDir()
	// 磁盘上是**另一把**钥（模拟已存在的旧部署）
	disk, _ := parseKey("2222222222222222222222222222222222222222222222222222222222222222")
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte(hexKeyOf(disk)), 0o600); err != nil {
		t.Fatal(err)
	}

	extra := AdditionalDecryptKeys(dir, embedded)
	if len(extra) != 1 {
		t.Fatalf("★ 磁盘钥必须作为额外解密钥装入，否则旧密文全丢；实际 %d 把", len(extra))
	}
	if !sameKey(extra[0], disk) {
		t.Fatal("额外解密钥不是磁盘那把")
	}

	// ★ 端到端：用磁盘钥加密的密文，在"当前钥=内嵌钥 + 额外钥=磁盘钥"下**必须能解开**
	dc, err := NewCodec(disk, "config-at-rest")
	if err != nil {
		t.Fatal(err)
	}
	cipherText, err := dc.Seal("旧的敏感值")
	if err != nil {
		t.Fatal(err)
	}
	nc, err := NewCodecWithOldKeys(embedded, "config-at-rest", extra)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := nc.Open(cipherText)
	if err != nil {
		t.Fatalf("★ 旧密文解不开（这正是内嵌会导致的数据丢失）: %v", err)
	}
	if plain != "旧的敏感值" {
		t.Fatalf("解出的值不对: %q", plain)
	}
}

// ★★★ 判据：磁盘钥与内嵌钥**相同**时不重复添加（幂等、不污染解密集合）。
func TestEmbeddedKeySameAsDiskNoDuplicate(t *testing.T) {
	hexKey := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	withEmbeddedKey(t, hexKey)
	k, _ := parseKey(hexKey)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte(hexKeyOf(k)), 0o600); err != nil {
		t.Fatal(err)
	}
	if extra := AdditionalDecryptKeys(dir, k); len(extra) != 0 {
		t.Errorf("同钥不该产生额外解密钥: %d", len(extra))
	}
}

// ★★★ 判据：内嵌钥格式非法时**不崩**、且退回其它来源。
//
// 内嵌是构建脚本注入的，格式错是构建问题。运行期直接崩会让
// "二进制起来了但服务全挂"难以归因；而回退路径本身是可信的，安全性不损失。
func TestEmbeddedKeyMalformedFallsBack(t *testing.T) {
	for _, bad := range []string{"not-hex", "abcd", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"} {
		withEmbeddedKey(t, bad)
		dir := t.TempDir()
		k, src, err := LoadOrCreateKey(dir)
		if err != nil {
			t.Fatalf("格式 %q 不该导致失败: %v", bad, err)
		}
		if len(k) != 32 {
			t.Errorf("格式 %q 应回退到 32 字节钥，实际 %d", bad, len(k))
		}
		if len(src) >= 8 && src[:8] == "embedded" {
			t.Errorf("★ 非法内嵌钥不该被采用（否则会用一把错钥加密新数据）: %q", src)
		}
	}
}

// ★★★ 判据：内嵌升级场景下，**keyring 的 current 也要进解密集合**（不能 Fatal）。
//
// 一台已经轮换过钥的机器（有 security.keyring）升级到内嵌二进制时，
// keyring 的 current 是**旧磁盘钥**，与当前（内嵌）钥不同。
// 按原有逻辑这会 Fatal「拒绝静默选错钥」—— 但内嵌场景下它不是错钥，
// 是"密钥来源升级了"，必须按额外解密钥保留。
func TestKeyringCurrentBecomesDecryptKeyUnderEmbedded(t *testing.T) {
	hexKey := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	withEmbeddedKey(t, hexKey)
	embedded, _ := parseKey(hexKey)

	dir := t.TempDir()
	// 老机器状态：磁盘钥 + keyring（current = 磁盘钥，含一把更老的历史钥）
	disk, _ := parseKey("3333333333333333333333333333333333333333333333333333333333333333")
	older, _ := parseKey("4444444444444444444444444444444444444444444444444444444444444444")
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte(hexKeyOf(disk)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RotateKeyFile(dir, older, disk); err != nil {
		t.Fatal(err)
	}

	// ★ 必须成功（不 Fatal），且能把 disk 与 older 都作为解密钥拿到
	old, err := LoadKeyring(dir, embedded)
	if err != nil {
		t.Fatalf("★ 内嵌升级场景下 LoadKeyring 不该失败（会让老机器起不来）: %v", err)
	}
	has := func(k []byte) bool {
		for _, o := range old {
			if sameKey(o, k) {
				return true
			}
		}
		return false
	}
	if !has(disk) {
		t.Error("★ keyring 的 current（旧磁盘钥）必须进解密集合")
	}
	if !has(older) {
		t.Error("keyring 的 history 必须进解密集合")
	}
	// 当前（内嵌）钥不该出现在解密集合里（它本来就在）
	if has(embedded) {
		t.Error("当前钥不该重复出现在解密集合里")
	}
}

// ★★★ 判据：**没有**内嵌时，keyring 与磁盘钥不一致仍然必须响亮失败。
//
// 上面那条放宽只对内嵌场景成立；没有内嵌时"选错钥"依然是严重错误，
// 必须保持原有的拒绝行为（否则这个放宽会变成普遍性的安全退化）。
func TestKeyringMismatchStillFatalWithoutEmbedded(t *testing.T) {
	withEmbeddedKey(t, "")
	dir := t.TempDir()
	disk, _ := parseKey("5555555555555555555555555555555555555555555555555555555555555555")
	older, _ := parseKey("6666666666666666666666666666666666666666666666666666666666666666")
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte(hexKeyOf(disk)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RotateKeyFile(dir, older, disk); err != nil {
		t.Fatal(err)
	}
	// 传一把**不相干**的当前钥 → 必须报错
	stranger, _ := parseKey("7777777777777777777777777777777777777777777777777777777777777777")
	if _, err := LoadKeyring(dir, stranger); err == nil {
		t.Fatal("★ 无内嵌时 keyring current 与当前钥不一致必须响亮失败（不得静默选错钥）")
	}
}

func hexKeyOf(k []byte) string {
	const hx = "0123456789abcdef"
	out := make([]byte, 0, 64)
	for _, b := range k {
		out = append(out, hx[b>>4], hx[b&0xf])
	}
	return string(out)
}
