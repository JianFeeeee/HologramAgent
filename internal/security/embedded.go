package security

// 内嵌主密钥（**字面内嵌**，按用户 2026-10-16 的明确选择）。
//
// ## 这是什么
//
// 把主密钥在**编译期**烧进二进制，运行期作为密钥来源链的**最高优先级**
// （在 systemd 凭据之前）。目的是「防丢失」：二进制在，密钥就在，
// 不存在"security.key 被误删/没跟着备份"导致全部密文永久解不开。
//
// ## ★ 安全代价（必须写清，不能含糊）
//
// 这条改动的代价是**真实的、且方向与加密的初衷相反**：
//
//	加密要防的是「文件被读走」。而内嵌后：
//	  · homed 是 0755、config.db 是 0644 —— 二者同级可达
//	  · 于是"能读二进制"⇒"能拿密钥"⇒"能解密 config.db"
//	  ⇒ **对本机读取者，加密不再提供任何保护**
//	  · 且二进制会进备份/镜像/分发包
//	    （实测：/home/program/_guard_backup/*/ 里 homed 与 config.db 成对出现）
//
// 换言之：内嵌换来"不会因丢文件而丢数据"，代价是"文件被读走就等于全泄"。
// 这是用户知情后的选择，不是我们的疏忽 —— 但**任何降级/绕过都必须留痕**，
// 所以下面每次用到内嵌钥都会 log.Printf 说明当前密钥来自内嵌。
//
// ## 为什么用 ldflags 而不是 go:embed 一个文件
//
// 本仓有两个**公开**远端（gitcode / github）。`go:embed` 要求密钥是一个
// 仓库内的文件 —— 那意味着它会被提交、被推送、永久留在 git 历史里，
// 且任何 clone 的人都能拿到。`-ldflags -X` 让密钥只存在于**构建命令行**
// 与最终二进制中，仓库里一行都不留。
//
//	go build -ldflags "-X .../internal/security.embeddedKeyHex=<64位hex>" ./cmd/homed
//
// ## 为什么不是"只信内嵌"（丢数据的陷阱）
//
// 最危险的做法是：有内嵌钥就**一切以它为准**、忽略别处。
// 那样一台已经用磁盘钥加密过的老机器，升级到这个二进制后会：
//
//	① 用内嵌钥去解磁盘钥加的密文 → 全部解不开
//	② 若此时"解不开就重建库"→ **静默丢掉全部凭据**
//
// 所以这里**只把内嵌钥当一个来源**，并且在磁盘/keyring 存在时把它作为
// **额外的解密钥**（可读旧密文），而不是替换现有钥。优先顺序见
// LoadOrCreateKey：内嵌 > systemd > env > 磁盘。
//
// ★ 并且内嵌钥与磁盘钥**不一致**时必须响亮告警（不能静默选一把继续）：
// 那意味着这台机器上存在两套互不可解的密文，用户必须知道。

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// embeddedKeyHex 由构建时的 -ldflags 注入（64 位 hex）。
//
// ★ 默认空值：没有注入时，行为与改动前**逐字相同**（走 systemd/env/磁盘链）。
// 这是必须的 —— 否则用默认 Makefile 构建出的二进制会在所有老部署上
// "找不到密钥"，那是一个破坏性变更。
var embeddedKeyHex string

// EmbeddedKeyPresent 报告本二进制是否内嵌了主密钥。
func EmbeddedKeyPresent() bool { return embeddedKeyHex != "" }

// embeddedKey 解出内嵌钥（未内嵌或格式非法时返回 nil）。
//
// 格式非法**不** Fatal：内嵌是由构建脚本注入的，格式错是构建问题；
// 运行期直接崩会让"二进制起来了但服务全挂"难以归因。这里降级为
// 响亮告警 + 回退到其它来源（安全性上不损失：回退路径本身是可信的）。
func embeddedKey() []byte {
	if embeddedKeyHex == "" {
		return nil
	}
	k, err := parseKey(embeddedKeyHex)
	if err != nil {
		log.Printf("[security] ★ 内嵌主密钥格式非法（构建时 -ldflags 注入有误），已忽略: %v", err)
		return nil
	}
	if len(k) != 32 {
		log.Printf("[security] ★ 内嵌主密钥长度 %d 非法（需 32 字节），已忽略", len(k))
		return nil
	}
	return k
}

// loadEmbeddedKey 取内嵌钥并说明来源（供 LoadOrCreateKey 使用）。
//
// 它额外做一件 LoadOrCreateKey 不该独自承担的事：**报告与磁盘钥是否一致**。
func loadEmbeddedKey(dataDir string) ([]byte, bool) {
	k := embeddedKey()
	if k == nil {
		return nil, false
	}
	// 与磁盘钥比对（仅提示，不阻断）
	if diskPath := filepath.Join(dataDir, KeyFileName); fileExistsSimple(diskPath) {
		if b, err := os.ReadFile(diskPath); err == nil {
			if dk, err := parseKey(string(trimSpaceBytes(b))); err == nil {
				if !sameKey(dk, k) {
					log.Printf("[security] ★★ 内嵌主密钥与 %s **不一致**："+
						"该机器上存在两套互不可解的密文（内嵌钥能解一部分、磁盘钥能解另一部分）。"+
						"本次以**内嵌钥为当前钥**，磁盘钥请通过 security.keyring 的 history "+
						"或 HOMED_SECURITY_KEY 保留，否则旧密文会读不出来", diskPath)
				}
			}
		}
	}
	return k, true
}

// EmbeddedKeyFingerprint 返回内嵌钥的短指纹（用于日志/自检，**不泄漏密钥**）。
//
// 用 hex 前 8 位：够区分两把不同的钥，又不足以从指纹还原。
// 绝不打印密钥本身 —— 日志会进 journal、被导出、被贴进 issue。
func EmbeddedKeyFingerprint() string {
	k := embeddedKey()
	if k == nil {
		return ""
	}
	s := fmt.Sprintf("%x", k)
	return s[:8]
}

func trimSpaceBytes(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isSpaceByte(b[i]) {
		i++
	}
	for j > i && isSpaceByte(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// fileExistsSimple 是本地最小实现（避免与包内其它 fileExists 重名冲突）。
func fileExistsSimple(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
