package security

// 主密钥管理（设计文档 T3）。
//
// ## 密钥从哪来
//
// 优先顺序（第一个可用者胜出）：
//
//	1. systemd 凭据文件 $CREDENTIALS_DIRECTORY/homed_security_key
//	   —— 生产首选：systemd 解密到 tmpfs（只读、不在磁盘、不进 environ）
//	2. 环境变量 HOMED_SECURITY_KEY（hex 64 字符 / base64）
//	   —— 兼容手写 systemd / 容器注入
//	3. <dataDir>/security.key 文件（0600，首次启动自动生成）
//	   —— 开发机 / 无 systemd 场景
//
// ## 为什么把 systemd 凭据放在 env 之前
//
// 环境变量对同用户进程**可见**（/proc/<pid>/environ），任何以该用户身份运行的
// 进程都能读到主密钥 —— 那等于把整层加密打开。systemd 的 LoadCredential 把
// 内容落到一个 tmpfs 文件（$CREDENTIALS_DIRECTORY，仅本进程可读 0400），
// 既不在磁盘上留副本，也不出现在 environ 里。
//
// 详见 docs/zh/kernel-secret-firewall-design.md E-4。
//
// ## 为何不放进 config.db
//
// config.db 是 0644 **全局可读**的。密钥进去等于没有加密
//（设计文档硬约束：凭据绝不进 config.db）。
//
// ## 密钥丢失的后果（必须响亮）
//
// 密钥变了 ⇒ 已落盘的变量全部解不开。这时**不能静默重建库**
//（那会看起来"一切正常"而实际所有凭据都丢了）。
// 判据 TestWrongKeyFailsLoudly 固化这个行为。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// EnvKeyName 是主密钥的环境变量名。
const EnvKeyName = "HOMED_SECURITY_KEY"

// CredentialDirEnv 是 systemd 提供的凭据目录（`LoadCredential`/`SetCredential`）。
const CredentialDirEnv = "CREDENTIALS_DIRECTORY"

// CredentialName 是 systemd 凭据名。
//
// ★ 这个名字必须与 unit 里的 `LoadCredential=<ID>:<path>` **逐字符一致**：
// 实测（systemd 262）下划线**不会**被改写 —— `LoadCredential=homed_security_key:...`
// 产生的文件名就是 `homed_security_key`。这个名字是我们自己选的，
// 只要两边一致即可（文档只要求"可作为文件名的短 ASCII 串"）。
const CredentialName = "homed_security_key"

// KeyFileName 是密钥文件名（位于数据目录）。
const KeyFileName = "security.key"

// LoadOrCreateKey 按优先级取主密钥；没有则生成一个并落盘。
//
// dataDir 是内核数据目录（config.db 所在目录）。
func LoadOrCreateKey(dataDir string) ([]byte, string, error) {
	// ⓪ 内嵌主密钥（构建期 -ldflags 注入）——**最高优先级**。
	//
	// 为何最高：内嵌的目的就是「二进制在、密钥就在」，若被 systemd/env/磁盘
	// 盖过，那它在那些来源缺失时仍需回退，"不丢"就不成立。
	//
	// ★ 但它**不删除**其它来源：旧密文仍需旧钥可读，见 AdditionalDecryptKeys。
	if k, ok := loadEmbeddedKey(dataDir); ok {
		return k, "embedded (构建期注入，指纹 " + EmbeddedKeyFingerprint() + ")", nil
	}

	// ① systemd 凭据（生产首选：tmpfs、不在磁盘、不进 environ）
	if dir := strings.TrimSpace(os.Getenv(CredentialDirEnv)); dir != "" {
		path := filepath.Join(dir, CredentialName)
		b, err := os.ReadFile(path)
		if err != nil {
			// CredentialsDirectory 存在但凭据缺失 ⇒ **响亮失败**。
			// 静默回退到磁盘文件会让「忘记配凭据」表现成「一切正常」，
			// 而实际用的是旧密钥 —— 那正是本模块最怕的静默降级。
			return nil, "", fmt.Errorf(
				"security: systemd 凭据目录已设置（%s）但读不到 %s —— "+
					"unit 里应配 LoadCredential=%s:<密钥文件路径>；"+
					"拒绝静默回退到磁盘密钥: %w",
				dir, CredentialName, CredentialName, err)
		}
		key, err := parseKey(strings.TrimSpace(string(b)))
		if err != nil {
			return nil, "", fmt.Errorf("security: systemd 凭据 %s 格式非法: %w", path, err)
		}
		return key, "systemd credential " + CredentialName, nil
	}

	// ② 环境变量（兼容手写 systemd / 容器）
	if raw := strings.TrimSpace(os.Getenv(EnvKeyName)); raw != "" {
		key, err := parseKey(raw)
		if err != nil {
			return nil, "", fmt.Errorf("security: %s 格式非法: %w", EnvKeyName, err)
		}
		return key, EnvKeyName, nil
	}

	// ③ 磁盘文件
	path := filepath.Join(dataDir, KeyFileName)
	if b, err := os.ReadFile(path); err == nil {
		if fi, serr := os.Stat(path); serr == nil {
			if perm := fi.Mode().Perm(); perm != 0o600 {
				return nil, "", fmt.Errorf(
					"security: 主密钥文件权限不安全：%s 是 %04o，必须是 0600",
					path, perm)
			}
		}
		key, err := parseKey(strings.TrimSpace(string(b)))
		if err != nil {
			return nil, "", fmt.Errorf("security: 主密钥文件损坏: %w", err)
		}
		return key, path, nil
	} else if !os.IsNotExist(err) {
		return nil, "", err
	}

	// ③ 首次启动：生成
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, "", fmt.Errorf("security: 生成主密钥失败: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, "", err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return nil, "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, "", err
	}
	return key, path, nil
}

// AdditionalDecryptKeys 返回**当前钥之外**、为了能读出已落盘密文而必须装入
// 解密集合的钥。
//
// ## 为何必需（否则内嵌会一次性丢光数据）
//
// 一台机器先用磁盘钥 `security.key` 加密过 config.db / 变量库，然后升级到
// **内嵌了另一把钥**的二进制。此时若只信内嵌钥：
//
//	① 旧密文用内嵌钥解 → 全部失败
//	② 内核遇到解不开的密文会**响亮失败**（这是对的设计，不静默重建）
//	⇒ 服务起不来；若用户此时选择"重建库"⇒ **凭据全丢**
//
// 所以内嵌钥上台时，磁盘钥（以及 keyring history）必须继续参与**解密**，
// 只是不再负责加密新数据。这正是 E-1 密钥环的同一套思路：
// 「当前钥负责 Seal；旧钥只参与 Open」。
//
// 内嵌钥与磁盘钥相同、或磁盘钥不存在时，返回空（无额外钥）。
func AdditionalDecryptKeys(dataDir string, current []byte) [][]byte {
	var out [][]byte
	if !EmbeddedKeyPresent() {
		return nil
	}
	path := filepath.Join(dataDir, KeyFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	diskKey, err := parseKey(string(trimSpaceBytes(b)))
	if err != nil || len(diskKey) != 32 {
		return nil
	}
	if !sameKey(diskKey, current) {
		out = append(out, diskKey)
		log.Printf("[security] loaded %s as an **additional decrypt key** (embedded key is current) -- "+
			"old ciphertext still readable; do not delete this file until all ciphertext is confirmed re-encrypted", path)
	}
	return out
}

// parseKey 接受 hex(64) 或原样 32 字节。
func parseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	if len(s) == 32 {
		return []byte(s), nil
	}
	return nil, fmt.Errorf("长度 %d 非法（需 64 位 hex 或 32 字节）", len(s))
}
