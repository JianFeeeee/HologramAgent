package security

// 密钥环持久化（E-1）。
//
// 主密钥轮换不能只替换 security.key：变量库与 config.db 里已有密文仍由旧钥
// 加密。keyring 文件保存「当前钥 + 历史只读钥」，权限 0600；进程启动时把
// 历史钥装入 Store/Codec，读旧密文，写新密文。

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

const KeyringFileName = "security.keyring"

type keyringFile struct {
	Current string   `json:"current"`
	History []string `json:"history,omitempty"`
}

// LoadKeyring 读取持久化密钥环。没有 keyring 时返回 current 和空 history，
// 保持已有 security.key 部署完全兼容。
func LoadKeyring(dataDir string, current []byte) ([][]byte, error) {
	path := filepath.Join(dataDir, KeyringFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("security: 密钥环权限不安全：%s 是 %04o，必须是 0600", path, fi.Mode().Perm())
	}
	var f keyringFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("security: 密钥环损坏: %w", err)
	}
	if f.Current != "" {
		k, err := parseKey(f.Current)
		if err != nil {
			return nil, fmt.Errorf("security: 密钥环 current 无效: %w", err)
		}
		// ★ 内嵌钥场景：keyring 的 current 是**旧的磁盘钥**，而本进程当前钥是内嵌钥。
		// 这不是「选错钥」，而是「密钥来源升级了」——必须把 keyring current
		// 当作**额外解密钥**保留（否则它加的密文全读不出来），而不是拒绝启动。
		//
		// 为何不能按旧逻辑 Fatal：内嵌的目的就是防丢；一个"有 keyring 的老机器
		// 升级到内嵌二进制就再也起不来"的行为，恰好造成它要防的那个后果。
		if !sameKey(k, current) && EmbeddedKeyPresent() {
			log.Printf("[security] 密钥环 current 与当前（内嵌）钥不同——"+
				"按**额外解密钥**保留，旧密文仍可读: %s", filepath.Join(dataDir, KeyringFileName))
			return appendHistoryKeys(f.History, current, k)
		}
		if len(current) != 32 || !sameKey(k, current) {
			return nil, fmt.Errorf("security: security.key 与密钥环 current 不一致（拒绝静默选错钥）")
		}
	}
	var old [][]byte
	for i, raw := range f.History {
		k, err := parseKey(raw)
		if err != nil {
			return nil, fmt.Errorf("security: 密钥环 history[%d] 无效: %w", i, err)
		}
		if !sameKey(k, current) {
			old = append(old, k)
		}
	}
	return old, nil
}

// RotateKeyFile 原子地把 current 换成 newKey，并把旧 current 放入 history。
// 调用方随后应以 newKey + 返回的旧钥初始化 Store/Codec，并执行数据重加密。
func RotateKeyFile(dataDir string, current, newKey []byte) error {
	if len(current) != 32 || len(newKey) != 32 {
		return fmt.Errorf("security: 轮换钥匙必须都是 32 字节")
	}
	path := filepath.Join(dataDir, KeyringFileName)
	f := keyringFile{Current: hex.EncodeToString(newKey), History: []string{hex.EncodeToString(current)}}
	if b, err := os.ReadFile(path); err == nil {
		var old keyringFile
		if json.Unmarshal(b, &old) == nil {
			f.History = append(f.History, old.History...)
		}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// appendHistoryKeys 把 history 全部、加上 extra 里不等于 current 的钥，组成解密集合。
//
// 与正常路径的区别：正常情况下 `current` 必须是 keyring 的 current；
// 内嵌升级场景下 keyring current 反而成为"额外钥"，
// 于是解密集合 = history + keyring.current。
func appendHistoryKeys(history []string, current, extra []byte) ([][]byte, error) {
	var out [][]byte
	if len(extra) == 32 && !sameKey(extra, current) {
		out = append(out, extra)
	}
	for i, raw := range history {
		k, err := parseKey(raw)
		if err != nil {
			return nil, fmt.Errorf("security: 密钥环 history[%d] 无效: %w", i, err)
		}
		if !sameKey(k, current) {
			out = append(out, k)
		}
	}
	return out, nil
}

func sameKey(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var x byte
	for i := range a {
		x |= a[i] ^ b[i]
	}
	return x == 0
}
