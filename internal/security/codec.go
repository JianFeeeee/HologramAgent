package security

// 域隔离的加密编解码器（供内核其它模块复用主密钥）。
//
// ## 为什么要域隔离
//
// 同一把主密钥会服务多类用途：变量别名的 HMAC、配置落盘的 AES、
// （未来可能的）审计日志签名……若它们**共用同一把密钥**，就会产生
// 跨域攻击面：一处的输出可能在另一处被当作合法输入。
//
// 所以每类用途用 **HKDF 风格的派生子密钥**：
//
//	subKey = HMAC-SHA256(masterKey, "homeagent/<domain>/v1")
//
// 这样一把主密钥可以安全地派生出无限多个互不相关的子密钥，
// 而部署方只需要管一把密钥。
//
// ## 与 Alias() 的关系
//
// `Alias()` 是**单向**的（明文 → 别名，不可逆），用 HMAC 做哈希。
// `Codec` 是**双向**的（明文 ⇄ 密文），用 AES-GCM 做加密。
// 二者用途不同、密钥不同 —— 这就是域隔离的意义。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
)

// EncPrefixV1 是密文的自描述前缀。
//
// 为何用前缀而不是加列/加表：**无需 schema 迁移**，且明文旧数据
// 天然被识别（没有前缀就是明文），逐条惰性加密即可。
// 版本号 `v1` 让将来换算法时可以两代并存（读到 v1 用旧算法解）。
const EncPrefixV1 = "enc:v1:"

// ErrBadCiphertext 表示密文格式非法或密钥不对。
var ErrBadCiphertext = errors.New("security: 密文无法解密（密钥不对或数据损坏）")

// Codec 是用派生密钥做对称加解密的编解码器。
//
// key 为 nil 时 **Enabled()==false**，所有 Seal 退化为「原样返回」。
// 这是刻意设计：编解码器未装配时不能崩，但调用方可以据此判断
// 「密钥不可用」并选择响亮失败。
type Codec struct {
	key    []byte
	domain string
	// oldKeys 是轮换前的派生子密钥，仅用于 Open。
	oldKeys [][]byte
}

// NewCodec 由主密钥派生出某个用途的子密钥。
func NewCodec(masterKey []byte, domain string) (*Codec, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("security: 主密钥必须是 32 字节，实际 %d", len(masterKey))
	}
	if domain == "" {
		return nil, errors.New("security: codec 域不能为空（域隔离是安全要求）")
	}
	m := hmac.New(sha256.New, masterKey)
	m.Write([]byte("homeagent/" + domain + "/v1"))
	return &Codec{key: m.Sum(nil), domain: domain}, nil
}

// Enabled 报告编解码器是否可用。
func (c *Codec) Enabled() bool { return c != nil && len(c.key) == 32 }

// NewCodecWithOldKeys 构造支持平滑轮换的 codec。
//
// current 是新主密钥；oldMasters 是旧主密钥，**只用于解开旧密文**。
// 轮换时若不带旧钥，已落盘的密文会全部解不开（响亮失败但服务不可用）
// —— 那就是 E-1 要解决的问题。
func NewCodecWithOldKeys(current []byte, domain string, oldMasters [][]byte) (*Codec, error) {
	if len(current) != 32 {
		return nil, fmt.Errorf("security: 主密钥必须是 32 字节，实际 %d", len(current))
	}
	if domain == "" {
		return nil, errors.New("security: codec 域不能为空（域隔离是安全要求）")
	}
	derive := func(master []byte) []byte {
		m := hmac.New(sha256.New, master)
		m.Write([]byte("homeagent/" + domain + "/v1"))
		return m.Sum(nil)
	}
	c := &Codec{key: derive(current), domain: domain}
	for _, old := range oldMasters {
		if len(old) == 32 {
			c.oldKeys = append(c.oldKeys, derive(old))
		}
	}
	return c, nil
}

// Domain 返回用途域（便于诊断，比如日志里说清楚是哪一域的密文解不开）。
func (c *Codec) Domain() string {
	if c == nil {
		return ""
	}
	return c.domain
}

// Seal 加密并带上版本前缀。
//
// 未启用时**原样返回**（明文），这样调用方可以在未装配密钥的
// 环境（测试、本地开发）继续工作 —— 但生产必须装配（见 IsEncrypted）。
func (c *Codec) Seal(plain string) (string, error) {
	if !c.Enabled() {
		return plain, nil
	}
	// 已加密的不要再加一层（幂等）
	if IsEncrypted(plain) {
		return plain, nil
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return EncPrefixV1 + encodeB64(sealed), nil
}

// Open 解密。非密文（无前缀）原样返回 —— 这是**存量明文兼容**的关键：
// 旧数据不需要一次性迁移就能继续可读。
func (c *Codec) Open(stored string) (string, error) {
	if !IsEncrypted(stored) {
		return stored, nil // 明文（旧数据或未启用）
	}
	if !c.Enabled() {
		return "", fmt.Errorf("%w: 有密文但编解码器未启用（域 %s）",
			ErrNoMasterKey, c.Domain())
	}
	raw, err := decodeB64(strings.TrimPrefix(stored, EncPrefixV1))
	if err != nil {
		return "", fmt.Errorf("%w: base64 解码失败", ErrBadCiphertext)
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("%w: 密文太短", ErrBadCiphertext)
	}
	// ★ 先试当前钥，再依次试历史钥（E-1 轮换过渡期）。
	// 不能只试当前钥：轮换后旧条目仍是旧钥密文，只试新钥会让它们
	// 全部变成“解不开”，而那正是要避免的服务不可用。
	keys := append([][]byte{c.key}, c.oldKeys...)
	for _, k := range keys {
		block, err := aes.NewCipher(k)
		if err != nil {
			continue
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			continue
		}
		plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
		if err == nil {
			return string(plain), nil
		}
	}
	return "", fmt.Errorf("%w (域 %s)", ErrBadCiphertext, c.Domain())
}

// NewLegacyRingCodec 构造一个用**现成密钥环**（不做域派生）的编解码器。
//
// ★ 专供 Store 使用（2026-11-04 T3.2）。与 NewCodec 的区别只有一点：
//
//	NewCodec        ：用一个主密钥 HMAC 派生出**该域**的子密钥
//	NewLegacyRingCodec：直接吃一组密钥（Store 的 keys 环，含历史代）
//
// 为什么 Store 不能走 NewCodec 的域派生：它的密文是**用主密钥本身**加密的
// （历史数据），换成派生密钥会让所有存量条目解不开 —— 那是数据丢失，
// 不是迁移。
//
// keys[0] 是当前加密钥，其余只用于 Open（轮换过渡期）。
func NewLegacyRingCodec(domain string, keys [][]byte) *Codec {
	c := &Codec{domain: domain}
	if len(keys) > 0 && len(keys[0]) == 32 {
		c.key = keys[0]
		for _, k := range keys[1:] {
			if len(k) == 32 {
				c.oldKeys = append(c.oldKeys, k)
			}
		}
	}
	return c
}

// OpenLegacy 解开**无前缀**的旧格式密文（裸 base64 + nonce 前置）。
//
// ★ 为什么必须单独一个方法而不是让 Open 兼容：
//
// `Open` 对无前缀输入**原样返回**（那是"存量明文兼容"的正确行为——
// 老配置里存的就是明文）。但 Store 的旧密文恰好也是无前缀的裸 base64，
// 两者在字面上无法区分。
//
// 所以：
//
//	配置（config）→ 用 Open：无前缀 = 明文，原样返回
//	变量库（Store）→ 读到无前缀时先试 OpenLegacy：能解开 = 旧密文
//	                  ⇒ 立刻用 Seal 回写为新格式（惰性迁移）
//
// 把歧义留在**调用方**（它知道自己存的是什么），而不是让 Open 猜。
func (c *Codec) OpenLegacy(stored string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("%w: 有密文但编解码器未启用（域 %s）", ErrNoMasterKey, c.Domain())
	}
	raw, err := decodeB64(stored)
	if err != nil {
		return "", fmt.Errorf("%w: 旧格式 base64 解码失败", ErrBadCiphertext)
	}
	keys := append([][]byte{c.key}, c.oldKeys...)
	for _, k := range keys {
		block, err := aes.NewCipher(k)
		if err != nil {
			continue
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			continue
		}
		if len(raw) < gcm.NonceSize() {
			continue
		}
		plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
		if err == nil {
			return string(plain), nil
		}
	}
	return "", fmt.Errorf("%w: 旧格式密文解不开（域 %s）", ErrBadCiphertext, c.Domain())
}

// IsEncrypted 报告一个已存储的值是否是本模块的密文。
//
// ★ 单调为真：密文解密后可能恰好以 `enc:v1:` 开头（极小概率），
// 但那会导致重复解密失败而非静默错误，可接受。
func IsEncrypted(stored string) bool {
	return strings.HasPrefix(stored, EncPrefixV1)
}

// ---------------- 配置落盘编解码器 ----------------

// ★ 2026-11-04（T3.2）：包级单例 `globalCodec` 与访问器 `ConfigCodec()`
// 已删除（用户裁定「所有共享数据统一挂载一个 kernel 提供的挂载点上暴露」）。
//
// 配置加密器现在归 `security.Runtime`：
//
//	security 模块装配 → rt.SetCodec(c) → ks.Security
//	config 模块装配    → cfgReg.SetCodec(ks.Security.Codec())
//
// 与变量库（ks.Security.Store()）同源同生命周期，没有任何第二条通道。

// NewConfigCodec 构造配置落盘用的编解码器（**不**注册任何全局状态）。
//
// 当前钥负责 Seal；旧钥仅参与 Open（E-1 平滑轮换）。
// 调用方拿到实例后负责注入（见 Runtime.SetCodec）。
func NewConfigCodec(masterKey []byte, oldKeys [][]byte) (*Codec, error) {
	return NewCodecWithOldKeys(masterKey, configCodecDomain, oldKeys)
}

// configCodecDomain 是配置落盘加密的域。
//
// ★ 必须与变量库的域（storeCodecDomain）不同 —— 见那边的说明。
// 域在这里集中定义，避免"两处各写一个字符串"（拼错了不会报错，
// 只会让两个域意外重合，而那是安全倒退）。
const configCodecDomain = "config-at-rest"

// NeedsRekey 报告该密文是否**不是**当前主密钥加的（即轮换后尚未重加密）。
//
// 用途：轮换后需要知道哪些 config 项要重写；只需先试当前钥能不能开。
// 非密文（明文/旧数据）返回 false（那是 EncryptExistingSecrets 的事）。
func (c *Codec) NeedsRekey(stored string) bool {
	if !c.Enabled() || !IsEncrypted(stored) {
		return false
	}
	_, err := c.openWithSingleKey(c.key, stored)
	return err != nil
}

// Rekey 把一条密文（可能是旧钥加的）用当前钥重新加密。
// 非密文原样返回；旧钥也解不开则**响亮报错**（不静默丢数据）。
func (c *Codec) Rekey(stored string) (string, error) {
	if !c.Enabled() || !IsEncrypted(stored) {
		return stored, nil
	}
	if _, err := c.openWithSingleKey(c.key, stored); err == nil {
		return stored, nil // 已是当前钥，幂等
	}
	plain, err := c.Open(stored)
	if err != nil {
		return "", err
	}
	return c.Seal(plain)
}

// openWithSingleKey 只用指定的一把钥试解（供 NeedsRekey 判定）。
func (c *Codec) openWithSingleKey(key []byte, stored string) (string, error) {
	raw, err := decodeB64(strings.TrimPrefix(stored, EncPrefixV1))
	if err != nil {
		return "", ErrBadCiphertext
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", ErrBadCiphertext
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", ErrBadCiphertext
	}
	return string(plain), nil
}

// ★ 2026-11-04：`ClearConfigCodec` 已删除。
//
// 它清的是包级单例。现在"关闭加密"通过不注入（或注入 nil）表达：
//
//	rt.SetCodec(nil)              → config.codecOf() == nil
//	cfgReg.SetCodec(nil)
//
// 比"清空一个全局变量"更明确：状态属于对象，不属于进程。
