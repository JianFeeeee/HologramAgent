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
	"sync"
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

// IsEncrypted 报告一个已存储的值是否是本模块的密文。
//
// ★ 单调为真：密文解密后可能恰好以 `enc:v1:` 开头（极小概率），
// 但那会导致重复解密失败而非静默错误，可接受。
func IsEncrypted(stored string) bool {
	return strings.HasPrefix(stored, EncPrefixV1)
}

// ---------------- 全局配置编解码器 ----------------

var (
	globalCodecMu sync.RWMutex
	globalCodec   *Codec
)

// InitConfigCodec 装配配置落盘用的编解码器（内核启动时调用一次）。
func InitConfigCodec(masterKey []byte) (*Codec, error) {
	return InitConfigCodecWithOldKeys(masterKey, nil)
}

// InitConfigCodecWithOldKeys 装配支持 E-1 平滑轮换的配置 codec。
// 当前钥负责 Seal；旧钥仅参与 Open。

func InitConfigCodecWithOldKeys(masterKey []byte, oldKeys [][]byte) (*Codec, error) {
	c, err := NewCodecWithOldKeys(masterKey, "config-at-rest", oldKeys)
	if err != nil {
		return nil, err
	}
	globalCodecMu.Lock()
	globalCodec = c
	globalCodecMu.Unlock()
	return c, nil
}

// ConfigCodec 取配置编解码器；未装配时返回 nil（调用方原样读写）。
func ConfigCodec() *Codec {
	globalCodecMu.RLock()
	defer globalCodecMu.RUnlock()
	return globalCodec
}

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

// ClearConfigCodec 卸下配置编解码器（测试与"显式关闭加密"用）。
//
// 单独一个函数而不是让 InitConfigCodec(nil) 承担：
// 后者语义模糊（nil 是"没密钥"还是"要清空"？），且会掩盖调用方的错误
// —— 曾因此让一个判据假绿（以为清空了，其实上一把密钥还在）。
func ClearConfigCodec() {
	globalCodecMu.Lock()
	globalCodec = nil
	globalCodecMu.Unlock()
}
