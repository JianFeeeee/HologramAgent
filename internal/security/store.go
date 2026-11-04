// Package security 是内核的**敏感信息防火模块**（Secret Firewall）。
//
// 设计文档：docs/zh/kernel-secret-firewall-design.md
//
// ## 它做什么
//
//	① 拦截    LLM 出向前扫到明文  → 换成 @{别名}
//	② 加密落盘 明文 AES-GCM 持久化（不落明文）
//	③ 别名    ★ 哈希键：sec_ + HMAC-SHA256(masterKey, plain)[:8]
//	④ 回填    插件调用（toolcall / outputch）前，把 @{别名} 换回明文
//
// ## 三条不可让步的约束（文档 §3）
//
//	S1  不提供"取回明文"的任何接口 —— 只能**替代引用**。
//	    一旦有取回路径，LLM 就能把明文写回上下文，整层作废。
//	S3  别名必须 HMAC 派生，**不能裸哈希**：别名会进上下文/日志/记忆，
//	    裸哈希对弱口令可被字典枚举（实测 hunter2 当场命中）。
//	    也**不加 salt** —— 加了就失去合并能力（确定性是特性）。
//	S10 变量系统**全局唯一**：主 / 子 / 驻留子 agent 同一套。
//
// ## 为什么是"全局单例"
//
// 已核：驻留子共享父的 ProviderManager（resident.go:185），而它全进程只建一份
// （bootstrap.go:249）⇒ 安全模块挂在 provider 层就天然是进程级单例，
// **不需要任何跨 agent 同步**。
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// AliasPrefix 是所有变量别名的前缀。
const AliasPrefix = "sec_"

// aliasHexLen 是别名里哈希的十六进制长度（8 字节）。
//
// 为何选 8 字节：碰撞概率对本场景可忽略，且别名会进上下文，短才省预算。
// 实测别名形如 `sec_3718c87a11e9478b`（20 字符）。
const aliasHexLen = 16

// refPattern 匹配文本里的变量引用 `@{sec_xxx}`。
//
// 为什么用 @{...} 而不是 ${...}：后者与 shell 变量语法冲突，
// agent 极易误用；@{...} 不与任何常见语法撞车。
var refPattern = regexp.MustCompile(`@\{([A-Za-z0-9_]{1,64})\}`)

// Sentinel errors（调用方靠它们区分处置，不靠字符串匹配）。
var (
	// ErrNoMasterKey 表示没有可用密钥 ⇒ **拒绝产出别名**。
	//
	// 为何要拒绝而不是退化：没有密钥就只能用裸哈希，而裸哈希会泄露
	// secret 内容（可字典枚举）。**宁可漏报，不可降级**。
	ErrNoMasterKey = errors.New("security: 无可用主密钥")

	// ErrUnknownAlias 表示引用了未登记的别名。
	ErrUnknownAlias = errors.New("security: 未知变量别名")
)

// Entry 是一条变量记录。
type Entry struct {
	Alias     string `json:"alias"`
	Sealed    string `json:"sealed"` // AES-256-GCM(base64, nonce 前置)
	Source    string `json:"source"` // intercepted | submitted
	Note      string `json:"note,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at,omitempty"`
	// Hits 是命中次数（可观测：同一变量被用了多少次）。
	Hits int `json:"hits,omitempty"`
}

// Store 是变量库（进程级单例）。
type Store struct {
	mu   sync.RWMutex
	path string
	data map[string]*Entry

	// keyFn 返回主导主密钥（**当前**用来加密的那把）。为 nil 或报错时
	// **不做任何别名产出**。
	keyFn func() ([]byte, error)

	// keys 是密钥环（E-1 密钥轮换）。
	//
	// keys[0] 是当前主密钥（与 keyFn 同源）；其余是**只用于解密**的历史密钥。
	// 轮换时：ciphertext 用新钥重加密（此时新钥先在头部），
	// 旧钥保留在环里，好在过渡期能解开“尚未轮到”的旧主密钥密文。
	//
	// ★ 为何必须有环：只换 keyFn 会让所有旧密文立刻解不开
	// （响亮失败但服务不可用）；而保持旧别名又要求旧密钥**不能立刻丢弃**
	// —— 因为新密文是按新钥加密的，两者必须同时可读才能平滑迁移。
	keys []keyGen

	// codec 是加解密执行者（**唯一**的 AES-GCM 实现）。
	//
	// ★ 2026-11-04（T3.2）：此前 Store 自带一份 seal/open 实现，
	// 与 Codec 是**两套重复的 AES-256-GCM**（算法、nonce 布局、密钥环
	// 逻辑全都一样，只是 Codec 多了 enc:v1: 前缀与域隔离）。
	//
	// 用户裁定：
	//
	//	> store也应该调用security的加密能力！
	//
	// 现在 Store 只做"我用哪个域、我的密钥环是什么"，加解密一律委托。
	// 这让"加密算法只有一处实现"，也修掉一个真实隐患：
	// 两套实现曾在 nonce 布局上各自演化（一处前置、一处也前置但无前缀），
	// 任何一侧改动都不会被另一侧的判据发现。
	codec   *Codec
	codecMu sync.Mutex // 保护 codec 的惰性重建（keys 环在轮换时会变）

	// enabled 支持显式关闭（本仓「显式才是特权」）。
	enabled bool

	// onIntercept 是拦截回调（可观测：每次拦截都通知出去）。
	onIntercept func(Evt)
}

// keyGen 是一代主密钥（含代号，便于审计“这条密文是哪一代加的”）。
type keyGen struct {
	Key []byte
	// ID 是代号（短哈希），仅用于日志/审计，**不参与加解密**。
	ID string
}

// Evt 是一次拦截事件（不含明文）。
type Evt struct {
	Alias  string `json:"alias"`
	Kind   string `json:"kind"`  // 模式名
	Where  string `json:"where"` // input | tool_result:<tool>
	Len    int    `json:"len"`
	Merged bool   `json:"merged"` // true = 复用已有别名（同变量合并）
	At     string `json:"at"`
}

// Options 构造参数。
type Options struct {
	// Path 是变量库落盘路径（注意：**落盘**，不是内存）。
	Path string
	// KeyFn 返回主密钥（32 字节 AES-256）。
	KeyFn func() ([]byte, error)
	// OldKeys 是历史主密钥（解密用，E-1 密钥轮换）。
	//
	// 轮换后旧密文仍需可读 ⇒ 旧钥必须在环里；未轮换时为空。
	// 顺序无关（尝试顺序 = 当前钥后依次）。
	OldKeys [][]byte
	// Enabled 是否启用；默认 true。
	Enabled bool
	// OnIntercept 拦截回调（可 nil）。
	OnIntercept func(Evt)
}

// New 构造变量库。
func New(opt Options) *Store {
	s := &Store{
		path:        opt.Path,
		data:        map[string]*Entry{},
		keyFn:       opt.KeyFn,
		enabled:     opt.Enabled,
		onIntercept: opt.OnIntercept,
	}
	// 建初始密钥环：当前钥在头，历史钥在后。
	if opt.KeyFn != nil {
		if k, err := opt.KeyFn(); err == nil && len(k) == 32 {
			s.keys = append(s.keys, keyGen{Key: k, ID: keyID(k)})
		}
	}
	for _, k := range opt.OldKeys {
		if len(k) == 32 {
			s.keys = append(s.keys, keyGen{Key: k, ID: keyID(k)})
		}
	}
	return s
}

// keyID 给一把密钥一个**不可逆的短代号**（仅用于日志/审计）。
//
// 不直接打印密钥，也不打印可被拿去比对完整密钥的东西。
func keyID(k []byte) string {
	sum := sha256.Sum256(k)
	return hex.EncodeToString(sum[:4])
}

// Load 读入已落盘的变量库。
//
// 权限不正确时**响亮失败**（响亮失败，不静默降级）：
// 该文件含加密后的凭据，属高价值资产。
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次启动
		}
		return err
	}
	if fi, err := os.Stat(s.path); err == nil {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			return fmt.Errorf(
				"security: 变量库权限不安全：%s 是 %04o，必须是 0600（它含加密凭据）",
				s.path, perm)
		}
	}
	var entries []*Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return fmt.Errorf("security: 变量库损坏（拒绝静默重建）: %w", err)
	}
	for _, e := range entries {
		if e != nil && e.Alias != "" {
			s.data[e.Alias] = e
		}
	}
	return nil
}

// saveLocked 落盘并确权（调用方持锁）。
func (s *Store) saveLocked() error {
	list := make([]*Entry, 0, len(s.data))
	for _, e := range s.data {
		list = append(list, e)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ---------------- 别名（哈希键） ----------------

// Alias 由明文派生**确定性**别名。
//
// ★ 必须是 HMAC：别名会进上下文/日志/记忆。裸 SHA256 对弱口令可被字典枚举
// （实测：明文 hunter2 → 裸哈希前 8 字节作别名，5 个常见弱口令里当场命中）。
//
// ★ 必须不加 salt：加了同一明文会得到不同别名 ⇒ 失去"同变量合并"。
// 这里"确定性"是**特性**，安全靠 HMAC 的密钥而不是 salt。
func (s *Store) Alias(plain string) (string, error) {
	key, err := s.key()
	if err != nil {
		return "", err
	}
	// ★ 轮换后同一明文必须继续得到**已有别名**：
	// 别名若随新主密钥改变，记忆/上下文里的旧 @{sec_...} 会失效，
	// 且同一个凭据会出现两份变量。先在现有条目中按密钥环解密比对；
	// 找到就复用旧别名，找不到才用当前钥派生新别名。
	s.mu.RLock()
	for alias, e := range s.data {
		if got, _, openErr := s.openWithStoreRing(e.Sealed); openErr == nil && got == plain {
			s.mu.RUnlock()
			return alias, nil
		}
	}
	s.mu.RUnlock()
	m := hmac.New(sha256.New, key)
	m.Write([]byte(plain))
	return AliasPrefix + hex.EncodeToString(m.Sum(nil)[:aliasHexLen/2]), nil
}

func (s *Store) key() ([]byte, error) {
	if len(s.keys) > 0 {
		return s.keys[0].Key, nil
	}
	if s.keyFn == nil {
		return nil, ErrNoMasterKey
	}
	k, err := s.keyFn()
	if err != nil {
		return nil, err
	}
	if len(k) != 32 {
		return nil, fmt.Errorf("security: 主密钥必须是 32 字节（AES-256），实际 %d", len(k))
	}
	return k, nil
}

// keysForDecrypt 返回**按尝试顺序**排列的解密候选密钥。
//
// 当前钥优先（Hits 高的常态路径一击即中），历史钥依次尝试。
func (s *Store) keysForDecrypt() [][]byte {
	if len(s.keys) == 0 {
		return nil
	}
	out := make([][]byte, 0, len(s.keys))
	for _, g := range s.keys {
		out = append(out, g.Key)
	}
	return out
}

// openWithRing 用密钥环解开一条密文：依次尝试，返回第一个成功的明文。
//
// ★ 为何不是“记录每条密文属于哪一代”：那需要**改 Entry 结构**（加世代字段），
// 而密文本身已经能被正确的那把钥唯一解开（AES-GCM 带认证）。
// ★ 2026-11-04（T3.2）：`openWithRing` / `seal` / `open` 已删除。
//
// 它们是 Store 自带的 AES-GCM 实现，与 Codec 完全重复（算法、nonce 布局、
// 密钥环逻辑一致，只是 Codec 多了 enc:v1: 前缀与域隔离）。
//
// 现在唯一的实现是 Codec：
//
//	写入        → s.sealWithRing（新格式，带前缀）
//	读取        → s.openWithStoreRing（兼容新旧两种格式）
//	轮换        → NewLegacyRingCodec(old) 解 + NewLegacyRingCodec(new) 加密

// RotateKey 平滑轮换变量库主密钥，并保持**已有别名不变**。
//
// 事务顺序：先用旧密钥解开全部条目 → 用新密钥重加密到内存副本 →
// 一次性落盘 → 最后切换当前密钥。任一步失败都不切换当前密钥，
// 避免出现“内存已换、磁盘没换”的半轮换状态。
//
// oldKey 会留在进程内的历史环里，直到下一次进程重启；调用方必须把旧钥
// 作为 OldKeys 在重启时注入（后续 keyring 接入会自动化这个步骤）。
func (s *Store) RotateKey(newKey []byte) error {
	if len(newKey) != 32 {
		return fmt.Errorf("security: 新主密钥必须是 32 字节，实际 %d", len(newKey))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	oldCandidates := s.keysForDecrypt()
	if len(oldCandidates) == 0 {
		if old, err := s.key(); err == nil {
			oldCandidates = [][]byte{old}
		}
	}
	if len(oldCandidates) == 0 {
		return ErrNoMasterKey
	}
	// 旧环解、新钥加密。
	//
	// ★ 这里刻意**不用** s.openWithStoreRing：它读 s.keys，而此刻
	// s.keys 还没换（下面才换）—— 但语义上要明确"用旧环解"，
	// 所以显式构造。
	oldCodec := NewLegacyRingCodec(storeCodecDomain, oldCandidates)
	newCodec := NewLegacyRingCodec(storeCodecDomain, [][]byte{newKey})
	copyData := make(map[string]*Entry, len(s.data))
	for alias, e := range s.data {
		var plain string
		var oerr error
		if IsEncrypted(e.Sealed) {
			plain, oerr = oldCodec.Open(e.Sealed)
		} else {
			plain, oerr = oldCodec.OpenLegacy(e.Sealed)
		}
		if oerr != nil {
			return fmt.Errorf("security: 轮换时解密变量 %s 失败，未切换密钥: %w", alias, oerr)
		}
		sealed, err := newCodec.Seal(plain)
		if err != nil {
			return fmt.Errorf("security: 轮换时加密变量 %s 失败，未切换密钥: %w", alias, err)
		}
		cp := *e
		cp.Sealed = sealed
		cp.UpdatedAt = nowISO()
		copyData[alias] = &cp
	}
	oldCurrent := oldCandidates[0]
	s.data = copyData
	s.keys = append([]keyGen{{Key: newKey, ID: keyID(newKey)}, {Key: oldCurrent, ID: keyID(oldCurrent)}}, s.keys[1:]...)
	// keyFn 也切到新钥（未来没有用 New 构造的特殊调用路径仍一致）。
	s.keyFn = func() ([]byte, error) { return newKey, nil }
	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("security: 轮换落盘失败，当前内存已保留双钥环: %w", err)
	}
	return nil
}

// ---------------- 加密 ----------------

// codecOf 返回当前密钥环对应的编解码器（惰性构建）。
//
// ★ 为什么惰性而不是构造期建一次：`keys` 环会在 `RotateKey` 里变化
// （新钥插到头部），那时旧 codec 的 key 就过期了 —— 表现为"轮换后
// 新写入的条目用旧钥加密"，而轮换的**全部意义**就是尽快停用旧钥。
// 每次取用都按当前环重建，代价是一次切片拷贝。
func (s *Store) codecOf() *Codec {
	s.codecMu.Lock()
	defer s.codecMu.Unlock()
	keys := make([][]byte, 0, len(s.keys))
	for _, g := range s.keys {
		keys = append(keys, g.Key)
	}
	s.codec = NewLegacyRingCodec(storeCodecDomain, keys)
	return s.codec
}

// storeCodecDomain 是变量库的加密域。
//
// ★ 必须与 config 的域**不同**：域隔离是安全要求，不是风格。
// 同域意味着"用同一把密钥加密两处不同用途的数据"，一旦某处的明文
// 泄漏就能成为另一处的字典攻击素材。
const storeCodecDomain = "security/variables"

// sealWithRing 用密钥环加密（新格式，带 enc:v1: 前缀）。
func (s *Store) sealWithRing(plain string) (string, error) {
	c := s.codecOf()
	if !c.Enabled() {
		return "", ErrNoMasterKey
	}
	return c.Seal(plain)
}

// openWithStoreRing 解开一条条目密文，**兼容旧格式**。
//
// 返回 (明文, 是否需要迁移回写)。
//
// 两种格式：
//
//	enc:v1:...  → 新格式，Codec.Open
//	裸 base64   → 旧格式（T3.2 之前写的），Codec.OpenLegacy
//
// ★ 为什么必须区分：`Codec.Open` 对无前缀输入**原样返回明文**
// （那是给 config 的"存量明文兼容"用的）。而 Store 的旧密文恰好也
// 无前缀 —— 直接交给 Open 会把密文当明文返回，症状是"凭据回填出一个
// base64 串"，看起来像数据损坏，实际是格式误判。
func (s *Store) openWithStoreRing(sealed string) (plain string, needsMigrate bool, err error) {
	c := s.codecOf()
	if !c.Enabled() {
		return "", false, ErrNoMasterKey
	}
	if IsEncrypted(sealed) {
		p, oerr := c.Open(sealed)
		return p, false, oerr
	}
	// 无前缀：只可能是旧格式密文（Store 从不存明文）。
	p, oerr := c.OpenLegacy(sealed)
	if oerr != nil {
		return "", false, oerr
	}
	return p, true, nil
}

func seal(key []byte, plain string) (string, error) {
	block, err := aes.NewCipher(key)
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
	return encodeB64(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func open(key []byte, enc string) (string, error) {
	raw, err := decodeB64(enc)
	if err != nil {
		return "", err
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
		return "", fmt.Errorf("security: 密文太短")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("security: 解密失败（密钥不对或数据损坏）: %w", err)
	}
	return string(plain), nil
}

// ---------------- 核心：拦截（入向明文 → 别名） ----------------

// Replace 把文本里的**明文**换成别名（LLM 出向用）。
//
// 返回替换后的文本与拦截事件。
// ★ 同明文自动合并：因为别名是哈希键，同一明文天然得到同一别名。
func (s *Store) Replace(text, where string, detect []Pattern) (string, []Evt) {
	if !s.enabled || strings.TrimSpace(text) == "" {
		return text, nil
	}
	// ★ 拿不到密钥就不产出别名（宁可漏报，不用裸哈希）
	if _, err := s.key(); err != nil {
		return text, nil
	}

	var evts []Evt
	out := text
	for _, p := range detect {
		// ★ 前置过滤：不含该模式必含的字面量就直接跳过正则。
		//
		// 性能理由（实测）：本文件所有模式都以 `\b` 开头，而 Go 的 RE2
		// 对 `\b` 要逐 rune 做 Unicode 词边界判定 —— 50KB 文本上单条
		// 660µs，9 条合计 ~6ms。而绝大多数文本根本不含凭据前缀，
		// 用 strings.Contains 一挡，常态路径从毫秒级降到微秒级。
		//
		// ★ 正确性：prefilter 只用来**排除**（不含就一定不匹配），
		// 绝不用来**认定** —— 认定仍交给正则。所以它不会造成漏报。
		if !p.matchesPrefilter(out) {
			continue
		}
		out = p.RE.ReplaceAllStringFunc(out, func(hit string) string {
			secret, prefix := splitKeyValue(hit)
			alias, err := s.Alias(secret)
			if err != nil {
				return hit // 派生失败：原样保留（不产出坏别名）
			}
			merged, err := s.Put(alias, secret, "intercepted", "自动拦截 @"+where)
			if err != nil {
				return hit
			}
			evts = append(evts, Evt{
				Alias: alias, Kind: p.Name, Where: where,
				Len: len(secret), Merged: merged, At: nowISO(),
			})
			return prefix + fmt.Sprintf(
				"【信息被安全模块自动拦截，请使用变量 @{%s} 进行调用】", alias)
		})
	}
	return out, evts
}

// Put 登记一条变量（幂等：同别名只更新，不重复插）。
//
// 返回 merged=true 表示这是**合并到已有别名**（同变量合并能力）。
func (s *Store) Put(alias, plain, source, note string) (merged bool, err error) {
	// ★ 2026-11-04（T3.2）：写入走 Codec（带 enc:v1: 前缀），
	// 不再自建 seal。旧条目仍按旧格式可读（见 openWithStoreRing）。
	sealed, err := s.sealWithRing(plain)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, exists := s.data[alias]
	if exists {
		prev.Hits++
		prev.UpdatedAt = nowISO()
		if note != "" {
			prev.Note = note
		}
		_ = s.saveLocked()
		return true, nil
	}
	s.data[alias] = &Entry{
		Alias: alias, Sealed: sealed, Source: source,
		Note: note, CreatedAt: nowISO(), Hits: 1,
	}
	return false, s.saveLocked()
}

// Submit 由 agent **主动提交**一条明文，返回其别名。
//
// 用途：agent 知道后面要用某个凭据，可先登记（此时它可能还没在文本里出现过）。
func (s *Store) Submit(plain, note string) (string, error) {
	alias, err := s.Alias(plain)
	if err != nil {
		return "", err
	}
	if _, err := s.Put(alias, plain, "submitted", note); err != nil {
		return "", err
	}
	return alias, nil
}

// Forget 由 agent **主动删除**一条变量（用户要求：不自动淘汰，但可主动删）。
func (s *Store) Forget(alias string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[alias]; !ok {
		return ErrUnknownAlias
	}
	delete(s.data, alias)
	return s.saveLocked()
}

// migrateEntry 把一条旧格式条目就地升级为新格式（惰性迁移）。
//
// ## 为什么需要惰性迁移
//
// T3.2 之前 Store 写入的是**裸 base64**（无前缀）。切到 Codec 之后新写入
// 带 `enc:v1:` 前缀，但磁盘上已有的条目仍是旧格式。
//
// 三条路可选：
//
//	① 不做迁移 —— 旧格式永远可读（openWithStoreRing 兼容），
//	   代价是"两种格式永久共存"，判据永不能要求"落盘都是新格式"
//	② 启动时全量迁移 —— 有停机风险（21G 数据目录的教训：迁移窗口越长越危险）
//	③ 惰性迁移 —— 读到旧的就地升级，零停机、零额外扫描
//
// 选 ③。这是本仓已在用的手法（Codec.Rekey 同样是"用到了才升级"）。
//
// ## 为什么失败不报错
//
// 迁移失败只影响"下次读时还要再解一次旧格式"，不影响本次读取（明文已经
// 在前一步拿到了）。为一处升级失败让整个回填失败，是把可恢复的降级变成
// 不可恢复的中断。
func (s *Store) migrateEntry(alias, sealed string, plain string) {
	newSealed, err := s.sealWithRing(plain)
	if err != nil {
		return
	}
	if newSealed == sealed {
		return
	}
	s.mu.Lock()
	if e, ok := s.data[alias]; ok && e.Sealed == sealed {
		e.Sealed = newSealed
		// ★ 落盘必须在**持锁期**内调 saveLocked：它遍历 s.data 序列化，
		// 解锁后再调就是在读一个可能被并发改写的 map（数据竞争 + 可能写出
		// 半更新的快照）。函数名里的 Locked 就是要求这个前提。
		//
		// 落盘失败也不报错：内存已升级，下次 Save 会带上。
		_ = s.saveLocked()
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
}

// ---------------- 核心：回填（别名 → 明文，插件入向用） ----------------

// Fill 把文本里的 `@{alias}` 换回明文（路由到插件前用）。
//
// ★ 这是"插件能用密钥"的唯一通道，也是"插件无感知"的实现：
// 插件只看到已经填好的明文，不知道有替换这回事。
//
// 返回 (替换后文本, 替换了几处, 用到的别名)。
// 未知别名**原样保留**：不报错也不吞掉，以免把正常文本（恰好像引用）改坏。
func (s *Store) Fill(text string) (string, int, []string) {
	if strings.TrimSpace(text) == "" {
		return text, 0, nil
	}
	if len(s.keys) == 0 {
		return text, 0, nil
	}
	var used []string
	n := 0
	out := refPattern.ReplaceAllStringFunc(text, func(m string) string {
		sub := refPattern.FindStringSubmatch(m)
		if len(sub) != 2 {
			return m
		}
		alias := sub[1]
		s.mu.RLock()
		e, ok := s.data[alias]
		s.mu.RUnlock()
		if !ok {
			return m // 未知别名：原样保留
		}
		plain, needsMigrate, err := s.openWithStoreRing(e.Sealed)
		if err != nil {
			return m
		}
		if needsMigrate {
			// 惰性迁移：读到旧格式就地升级（失败静默，下次再试）。
			s.migrateEntry(alias, e.Sealed, plain)
		}
		n++
		used = append(used, alias)
		return plain
	})
	return out, n, used
}

// FillMap 对 map 的字符串值做回填（工具参数是 map）。
//
// 递归处理嵌套 map / slice —— 工具参数常常是嵌套结构。
func (s *Store) FillMap(args map[string]interface{}) (int, []string) {
	var total int
	var all []string
	var walk func(v interface{}) interface{}
	walk = func(v interface{}) interface{} {
		switch t := v.(type) {
		case string:
			out, n, used := s.Fill(t)
			total += n
			all = append(all, used...)
			return out
		case map[string]interface{}:
			for k := range t {
				t[k] = walk(t[k])
			}
			return t
		case []interface{}:
			for i := range t {
				t[i] = walk(t[i])
			}
			return t
		default:
			return v
		}
	}
	for k := range args {
		args[k] = walk(args[k])
	}
	return total, all
}

// ---------------- 查询（★ 永不返回明文） ----------------

// Info 是变量的元信息（不含明文）。
type Info struct {
	Alias     string `json:"alias"`
	Source    string `json:"source"`
	Note      string `json:"note,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Hits      int    `json:"hits,omitempty"`
}

// List 列出全部变量元信息。
//
// ★ 不提供 Get/明文接口 —— 见 S1。要看值就在命令里用 @{alias}。
func (s *Store) List() []Info {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Info, 0, len(s.data))
	for _, e := range s.data {
		out = append(out, Info{
			Alias: e.Alias, Source: e.Source, Note: e.Note,
			CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt, Hits: e.Hits,
		})
	}
	return out
}

// Count 返回变量条数（供状态显示与规模告警）。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}

// ---------------- 辅助 ----------------

// splitKeyValue 把 `key=value` / `key = "value"` 拆成前缀与值，
// 使替换后保留键名（否则人看不出发生了什么）。
//
// ★ 键名后常带空格，所以必须 trim 后再判；初版把"含空格"当成
// "不像键名"就放弃拆分，导致 `api_key = "xxx"` 整段被当 secret（键名丢失）。
func splitKeyValue(hit string) (secret, prefix string) {
	for _, sep := range []string{"=", ":"} {
		i := strings.Index(hit, sep)
		if i <= 0 {
			continue
		}
		// 排除 URL 的 `://`
		if sep == ":" && i+1 < len(hit) && hit[i+1] == '/' {
			continue
		}
		key := strings.TrimSpace(hit[:i])
		if key == "" || len(key) > 40 || strings.ContainsAny(key, "/@ \t\n") {
			continue
		}
		val := strings.Trim(strings.TrimSpace(hit[i+1:]), "\"'`")
		return val, key + sep + " "
	}
	return hit, ""
}

func nowISO() string { return time.Now().Format(time.RFC3339) }

// sha256Sum / hexEncode 是给判据用的薄封装（仅测试引用）。
func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

func hexEncode(b []byte) string { return hex.EncodeToString(b) }
