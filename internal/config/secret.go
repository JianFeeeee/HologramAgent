package config

// 配置落盘加密（敏感项 at-rest encryption）。
//
// ## 要解决的问题
//
// `config.db` 是 **0644 全局可读**的（它同目录还有 WAL/SHM），
// 而里面装着 LLM API 密钥、WebUI 口令、插件凭据等 —— 任何本机用户
// `sqlite3 config.db 'select * from config'` 就能拿到全部明文。
//
// ## 做法：在**存储边界**加密，读写接口不变
//
//	Set/Register/seed  →  Seal  →  DB 里是 `enc:v1:<base64>`
//	Get/Dump/ToConfig  →  Open  →  调用方永远只看到明文
//
// ★ 关键取舍：**读接口保持"返回明文"**。
//
// 初稿曾考虑让 `Dump()` 返回掩码、让 WebUI 看不到明文。但那样会同时
// 打断 `ToConfig()`（它靠 Dump 重建 LLMSource，含 api_key）与任何
// 需要真实值的内部读取方 —— 而它们**本来就在可信边界内**。
// 本需求要防的是「静态文件泄漏」，不是「内部读取」。
//
// ## 标记来源（两种都支持）
//
//	1. `ConfigDef.Secret == true`   ← SDK 里早就有这个字段，但从未被实现
//	2. `ConfigDef.Type == "password"` / `"secret"`
//
// ## 无 def 的键怎么办
//
// 插件的 key 常常没有 def（`Set()` 可以直接写任意键）。
// 所以还要靠**键名模式**兜底（`secretKeyPattern`），否则
// `plugin.qq.token` 这类会被漏掉。

import (
	"database/sql"
	"log"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// secretSegmentPattern 是无 def 时的兜底判定。
//
// ## 为何不能直接 `strings.Contains(key, "token")`
//
// 初版就是这么写的，结果把这两个**非敏感**项也加密了：
//
//	core.llm.max_tokens                  ← 输出 token 上限
//	core.agent.context.max_target_tokens ← 上下文预算
//
// 功能上无害（透明解密，仍可用），但语义错了，且库变得难以人工检查。
// 根因：`max_tokens` 包含子串 "token"，而它是**复数、指数量**。
//
// ## 现在的判据：按**词段**匹配
//
// 先把键按 `.` `_` `-` 切成词段，再看：
//
//	词段完全等于敏感词          → 命中（token / password / apikey …）
//	词段以 `_<敏感词>` 结尾       → 命中（api_key / access_token …）
//
// 关键差别：`max_tokens` 的末词段是 `tokens`（复数），不等于 `token`，
// 也不以 `_token` 结尾 ⇒ **不命中**。而 `api_key` 末词段是 `key`
// 且整串以 `_key` 结尾 ⇒ 命中。
var secretWords = []string{
	"token", "password", "passwd", "secret", "credential", "credentials",
	"apikey", "key",
}

// secretExactSegments 是允许“整词即敏感”的情形。
//
// `key` 单独成词段（如某插件的配置就叫 `key`）算敏感；但像 `keyboard`、
// `keys`（复数）这样的就不算。
func looksLikeSecretKey(key string) bool {
	lower := strings.ToLower(key)
	// 切成词段：先按命名空间分隔，再拆下划线/连字符
	for _, part := range strings.FieldsFunc(lower, func(r rune) bool {
		return r == '.' || r == '_' || r == '-'
	}) {
		for _, w := range secretWords {
			if part == w {
				return true
			}
		}
	}
	// 复合形态：整串以 _<敏感词> 结尾（api_key / access_token / client_secret）
	for _, w := range secretWords {
		if strings.HasSuffix(lower, "_"+w) || strings.HasSuffix(lower, "-"+w) {
			return true
		}
	}
	return false
}

// isSecretKey 判定一个（全局或插件内局部的）键是否敏感。
//
// 判定顺序：显式 def（最权威） → 键名模式（兜底）。
func (r *ConfigRegistry) isSecretKey(key string) bool {
	// 调用方须持锁（读或写皆可）
	if d, ok := r.defs[key]; ok {
		if d.Secret || d.Type == "password" || d.Type == "secret" {
			return true
		}
	}
	return looksLikeSecretKey(key)
}

// isSecretKeyLocal 供 PluginSettings 用（键是插件内局部键，无命名空间前缀）。
func (p *PluginSettings) isSecretKeyLocal(key string) bool {
	return p.registry.isSecretKey("plugin." + p.name + "." + key)
}

// sealForStorage 在写库前加密（仅对敏感键 + 编解码器已装配时）。
//
// 返回值语义：加密发生则 mem=false（调用方据此决定是否需回读校验）；
// 非敏感键或未装配 codec 时原样返回。
func sealForStorage(key, value string) (string, bool) {
	if !security.IsEncrypted(value) {
		c := security.ConfigCodec()
		if c == nil || !c.Enabled() {
			return value, false
		}
		if sealed, err := c.Seal(value); err == nil {
			return sealed, true
		} else {
			log.Printf("[config] encrypting %s failed (will be written as plaintext, this entry is exposed): %v", key, err)
		}
	}
	return value, false
}

// openFromStorage 在读库后解密。
//
// 非密文原样返回（**存量明文兼容**）：老数据不需要一次性迁移。
// 密文但解不开 ⇒ **响亮失败**（返回错误而非密文），因为把密文
// 当明文交出去会让下游拿到垃圾（比如把 `enc:v1:...` 当 API key 发出去）。
func openFromStorage(key, stored string) (string, error) {
	if !security.IsEncrypted(stored) {
		return stored, nil
	}
	c := security.ConfigCodec()
	if c == nil || !c.Enabled() {
		return "", security.ErrNoMasterKey
	}
	plain, err := c.Open(stored)
	if err != nil {
		log.Printf("[config] decrypting %s failed: %v", key, err)
		return "", err
	}
	return plain, nil
}

// EncryptExistingSecrets 把库里**尚为明文**的敏感项就地加密（启动时跑一次）。
//
// ## 为何需要
//
// 加密只对新写入生效。升级前库里已有的 api_key / 口令仍是明文 ——
// 不迁移的话"升级了但密钥还在裸奔"，而且用户无法察觉。
//
// ## 可回滚
//
// 只改值形态、不改键名与表结构，旧版本二进制读明文项仍然工作
// （它不认 `enc:v1:` 前缀，会把密文当明文用 —— 那是**功能坏但可见**的
// 失败，不是静默错值）。所以升级要么配套换二进制，要么备份。
//
// 返回 (加密条数, 错误)。幂等：已加密的项不会被二次加密。
func (r *ConfigRegistry) EncryptExistingSecrets() (int, error) {
	c := security.ConfigCodec()
	if c == nil || !c.Enabled() {
		return 0, security.ErrNoMasterKey
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	type row struct{ table, key, val string }
	var todo []row

	// ① 全局 config 表
	rows, err := r.db.Query(`SELECT key, value FROM config`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			continue
		}
		if !r.isSecretKey(k) || security.IsEncrypted(v) || v == "" {
			continue
		}
		todo = append(todo, row{"config", k, v})
	}
	rows.Close()

	// ② 各插件表
	for _, name := range r.ListPluginsLocked() {
		table := r.pluginTableName(name)
		pr, err := r.db.Query("SELECT key, value FROM " + table)
		if err != nil {
			continue
		}
		for pr.Next() {
			var k, v string
			if err := pr.Scan(&k, &v); err != nil {
				continue
			}
			if !r.isSecretKey("plugin."+name+"."+k) || security.IsEncrypted(v) || v == "" {
				continue
			}
			todo = append(todo, row{table, k, v})
		}
		pr.Close()
	}

	n := 0
	for _, t := range todo {
		sealed, err := c.Seal(t.val)
		if err != nil {
			log.Printf("[config] migration-encrypt %s.%s failed: %v", t.table, t.key, err)
			continue
		}
		if _, err := r.db.Exec(
			"UPDATE "+t.table+" SET value = ? WHERE key = ?", sealed, t.key); err != nil {
			log.Printf("[config] migration-encrypt %s.%s write-back failed: %v", t.table, t.key, err)
			continue
		}
		n++
	}
	if n > 0 {
		log.Printf("[config] encrypted %d existing sensitive config entries (no plaintext at rest)", n)
	}
	return n, nil
}

// RekeySecrets 把库里的密文重加密到**当前**主密钥（E-1 密钥轮换后的收口）。
//
// 与 EncryptExistingSecrets 的区别：
//   - 后者处理「明文 → 密文」（升级路径）；
//   - 本函数处理「旧钥密文 → 新钥密文」（轮换路径）。
//
// 为何必须有：轮换只换密钥文件 / keyring 是不够的 —— 旧密文仍由旧钥保护，
// 而旧钥必须留在环里才能读它们。不重加密的话，旧钥永远不会退出。
//
// 幂等：已是当前钥的项不动。解不开的项**响亮计数**并跳过（不静默删）。
func (r *ConfigRegistry) RekeySecrets() (int, int, error) {
	c := security.ConfigCodec()
	if c == nil || !c.Enabled() {
		return 0, 0, security.ErrNoMasterKey
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	tables := []string{"config"}
	for _, name := range r.ListPluginsLocked() {
		tables = append(tables, r.pluginTableName(name))
	}

	rekeyed, failed := 0, 0
	for _, table := range tables {
		rows, err := r.db.Query("SELECT key, value FROM " + table)
		if err != nil {
			continue
		}
		type pair struct{ k, v string }
		var need []pair
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				continue
			}
			if c.NeedsRekey(v) {
				need = append(need, pair{k, v})
			}
		}
		rows.Close()

		for _, p := range need {
			fresh, err := c.Rekey(p.v)
			if err != nil {
				// ★ 解不开不能静默跳：那是“密钥环缺一代”的信号，必须可见。
				failed++
				log.Printf("[config] re-encrypt %s.%s failed (old key not in keyring?): %v", table, p.k, err)
				continue
			}
			if _, err := r.db.Exec("UPDATE "+table+" SET value = ? WHERE key = ?", fresh, p.k); err != nil {
				failed++
				log.Printf("[config] re-encrypt %s.%s write-back failed: %v", table, p.k, err)
				continue
			}
			rekeyed++
		}
	}
	if rekeyed > 0 || failed > 0 {
		log.Printf("[config] key-rotation re-encrypt: %d succeeded, %d failed", rekeyed, failed)
	}
	return rekeyed, failed, nil
}

// AuditSecrets 统计库里密文的可读性（E-2：备份跨机器恢复的诊断）。
//
// 为何需要：备份恢复到另一台机器时，DB 里的密文是**旧机器的密钥**加的。
// 此前只有读某一条具体配置时才会报错，用户看到的是「某个功能坏了」，
// 而不是「这份备份与当前密钥不匹配」。这个审计把事实一次性说清楚。
//
// 返回 (可解, 不可解)。不可解 > 0 就是密钥不匹配/缺代的硬信号。
func (r *ConfigRegistry) AuditSecrets() (int, int, error) {
	c := security.ConfigCodec()
	if c == nil || !c.Enabled() {
		return 0, 0, security.ErrNoMasterKey
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	tables := []string{"config"}
	for _, name := range r.ListPluginsLocked() {
		tables = append(tables, r.pluginTableName(name))
	}
	ok, bad := 0, 0
	for _, table := range tables {
		rows, err := r.db.Query("SELECT value FROM " + table)
		if err != nil {
			continue
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				continue
			}
			if !security.IsEncrypted(v) {
				continue // 明文（未加密项）不计入
			}
			if _, err := c.Open(v); err == nil {
				ok++
			} else {
				bad++
			}
		}
		rows.Close()
	}
	return ok, bad, nil
}

// ListPluginsLocked 是 ListPlugins 的不加锁版本（调用方已持锁）。
func (r *ConfigRegistry) ListPluginsLocked() []string {
	rows, err := r.db.Query(
		`SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'config_%' ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			continue
		}
		out = append(out, strings.TrimPrefix(t, "config_"))
	}
	return out
}

// IsSecret 报告某个（全局或 plugin. 前缀的）键是否敏感。
//
// 用途（E-3）：WebUI 在返回设置时必须用它**屏蔽明文**，否则任何能打开
// 设置页的人都看得到全部密钥（含其他插件的）。
//
// 它刻意复用与落盘加密**同一套判定**（isSecretKey）：两处判定若不一致，
// 就会出现「已加密但界面照旧显示明文」或反向的漏洞。
func (r *ConfigRegistry) IsSecret(key string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.isSecretKey(key)
}

// SecretKeys 返回当前被判定为敏感的键（供诊断 / 测试 / WebUI 标记）。
func (r *ConfigRegistry) SecretKeys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	seen := map[string]bool{}
	for k := range r.defs {
		if r.isSecretKey(k) && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// DB 暴露底层连接（仅供**同仓测试**读取原始密文用）。
//
// 语义警告：它绕过全部加解密与权限逻辑，仅用于判据里断言
// 「磁盘上确实是密文」。生产代码不得使用。
func (r *ConfigRegistry) DB() *sql.DB { return r.db }

// ResetTestCodec 为判据装配一把固定主密钥。
//
// 单独放在非 _test 文件里，是因为 proc 包（另一个包）的端到端判据
// 也需要它 —— 跨包无法直接调 *_test.go 里的辅助函数。
func ResetTestCodec(t interface {
	Helper()
	Fatalf(string, ...interface{})
}) {
	if _, err := security.InitConfigCodec(
		[]byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatalf("装配测试 codec 失败: %v", err)
	}
}
