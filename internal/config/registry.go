package config

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/pkg/types"
	_ "github.com/mattn/go-sqlite3"
)

type ConfigDef struct {
	Key         string   `json:"key"`
	Default     string   `json:"default"`
	Description string   `json:"description"`
	Type        string   `json:"type"` // string, int, bool, duration, password, select, text
	DisplayName string   `json:"display_name"`
	Placeholder string   `json:"placeholder,omitempty"`
	Options     []string `json:"options,omitempty"`
	Hidden      bool     `json:"hidden,omitempty"`
	Category    string   `json:"category,omitempty"`

	// Secret 标记该配置项为敏感值：**落盘加密且不回显**。
	//
	// ★ 与 Type=="password" 等价，但更显式。SDK 侧（pubsdk.ConfigDef）
	// 一直有这个字段，只是内核这边没实现 —— 插件早就声明了意图却没有效果。
	//
	// 两种写法都会生效（见 isSecretKey）：
	//   · Type: "password"            旧写法，兼容保留
	//   · Secret: true                推荐，意图明确
	Secret bool `json:"secret,omitempty"`
}

type ConfigRegistry struct {
	mu          sync.RWMutex
	db          *sql.DB
	dbPath      string
	defs        map[string]*ConfigDef
	llmSnapFile string

	// seeded 是**本次运行确实播种进 DB** 的键集合。
	//
	// 用途：GetXxx 在 DB 里查不到行时是否该回退到 ConfigDef.Default。
	//
	//	全新安装 → 键已播种 → 回退用 ConfigDef.Default（正常）
	//	老安装   → 该版本新增的键**没有**播种 → 必须返回调用方给的零值，
	//	           否则升级即被注入新默认值（实测后果：静默加载 1.8GB 向量模型）
	//
	// 换句话说：ConfigDef.Default 是「该键若有初始行，值应该是多少」，
	// 而不是「老安装也该被当成有这个值」。
	seeded map[string]bool

	// secrets 是「需要加密落盘」的键集合（由 def 推导，见 isSecretKey）。
	//
	// 为何缓存成集合而不是每次查 def：写入是热路径，而 def 查询要拿锁；
	// 且 secret 判定必须对**无 def 的键**也能生效（见 secretByKeyPattern）。
	secrets map[string]bool
}

func NewConfigRegistry(dbPath string) *ConfigRegistry {
	if dbPath == "" {
		dbPath = ":memory:"
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		panic(fmt.Sprintf("open config db: %v", err))
	}
	db.Exec("PRAGMA journal_mode=WAL")
	r := &ConfigRegistry{db: db, dbPath: dbPath, defs: make(map[string]*ConfigDef), secrets: make(map[string]bool), seeded: make(map[string]bool)}
	r.initCoreTable()
	return r
}

func (r *ConfigRegistry) initCoreTable() {
	r.db.Exec(`CREATE TABLE IF NOT EXISTS config (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`)
	r.db.Exec(`CREATE TABLE IF NOT EXISTS disabled_plugins (
		name       TEXT PRIMARY KEY,
		disabled_at TEXT NOT NULL,
		disabled_by TEXT NOT NULL DEFAULT ''
	)`)
}

func (r *ConfigRegistry) AddDisabledPlugin(name, by string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(`INSERT OR REPLACE INTO disabled_plugins (name, disabled_at, disabled_by) VALUES (?, datetime('now'), ?)`, name, by)
	return err
}

func (r *ConfigRegistry) RemoveDisabledPlugin(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(`DELETE FROM disabled_plugins WHERE name = ?`, name)
	return err
}

// RemovePlugin 卸载插件时清理其全部配置痕迹：删除配置项定义
// （defs 中 plugin.<name>.*，即该插件 RegisterDef 注册的配置项）并删除
// 插件配置表（config_<name>，含用户设置值）。卸载后该插件配置区完全消失；
// 与插件自身的 onRemove 回调（清理数据文件）配合完成删除清理。
func (r *ConfigRegistry) RemovePlugin(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := "plugin." + name + "."
	for k := range r.defs {
		if strings.HasPrefix(k, prefix) {
			delete(r.defs, k)
		}
	}
	if _, err := r.db.Exec(`DELETE FROM config WHERE key LIKE ?`, prefix+"%"); err != nil {
		return err
	}
	table := r.pluginTableName(name)
	_, err := r.db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table))
	return err
}

type DisabledPluginInfo struct {
	Name       string `json:"name"`
	DisabledAt string `json:"disabled_at"`
	DisabledBy string `json:"disabled_by"`
}

func (r *ConfigRegistry) ListDisabledPlugins() ([]DisabledPluginInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rows, err := r.db.Query(`SELECT name, disabled_at, disabled_by FROM disabled_plugins ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []DisabledPluginInfo
	for rows.Next() {
		var info DisabledPluginInfo
		if err := rows.Scan(&info.Name, &info.DisabledAt, &info.DisabledBy); err != nil {
			return nil, err
		}
		list = append(list, info)
	}
	return list, rows.Err()
}

func (r *ConfigRegistry) IsPluginDisabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var count int
	r.db.QueryRow(`SELECT COUNT(*) FROM disabled_plugins WHERE name = ?`, name).Scan(&count)
	return count > 0
}

func (r *ConfigRegistry) ensurePluginTable(name string) {
	r.db.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`, r.pluginTableName(name)))
}

// ensurePluginTableErr 同 ensurePluginTable 但返回错误（显式加密写入需要它）。
func (r *ConfigRegistry) ensurePluginTableErr(name string) error {
	_, err := r.db.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`, r.pluginTableName(name)))
	return err
}

func (r *ConfigRegistry) pluginTableName(name string) string {
	safe := strings.Map(func(c rune) rune {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			return c
		}
		return '_'
	}, strings.ToLower(name))
	return "config_" + safe
}

func (r *ConfigRegistry) Register(key string, value interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := fmt.Sprint(value)
	if r.isSecretKey(key) {
		stored, _ = sealForStorage(key, stored)
	}
	r.db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`, key, stored)
}

func (r *ConfigRegistry) RegisterDefault(key string, value interface{}) {
	r.Register(key, value)
}

func (r *ConfigRegistry) RegisterDef(def ConfigDef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerDefLocked(def)
}

// registerDefLocked 是 RegisterDef 的不加锁版本：登记 ConfigDef + 落库播种。
//
// 供**已持 r.mu** 的路径调用（如 SeedDefaults）。分出来是必须的：
// RegisterDef 自己会 Lock，在持锁路径里直接调它 = 自死锁（sync.Mutex 不可重入）。
func (r *ConfigRegistry) registerDefLocked(def ConfigDef) {
	r.defs[def.Key] = &def
	// 默认值也要加密落盘（否则声明了 secret 的项一启动就是明文）。
	stored := def.Default
	if r.isSecretKey(def.Key) {
		stored, _ = sealForStorage(def.Key, stored)
	}
	r.db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`, def.Key, stored)
	// 记录「本次运行确实为这个键铺了初始行」—— GetXxx 只对这类键回退到
	// ConfigDef.Default（见 defDefault 的说明）。
	r.seeded[def.Key] = true
}

func (r *ConfigRegistry) GetDef(key string) *ConfigDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defs[key]
}

// sourceFieldDefs 定义 source 类型配置的字段元数据
var sourceFieldDefs = []struct {
	Field       string
	Type        string
	DisplayName string
}{
	{"base_url", "string", "API 地址"},
	{"model", "string", "模型"},
	{"api_key", "password", "API 密钥"},
	{"thinking_enabled", "bool", "深度思考"},
	{"adapter", "string", "适配器"},
	{"adapter_path", "string", "适配器路径"},
	{"max_concurrent", "int", "并发上限"},
	// context_window 必须显式声明：它已被 readInt 读进 LLMSource 并透传到 provider，
	// 但早先没进这张表 ⇒ WebUI 里看不见、也改不了，内核就一直在用
	// ModelContextWindow(model) 的推断值。模型名是 AUTO 时那个函数匹配不到任何
	// 分支、掉进 default 32768，而真实窗口是 1M（实测 990,034 token 的 prompt 通过）
	// ⇒ 整个预算比真实小 30 倍。声明后它才是个可配的挡位。
	//
	// 注：per-source 的 max_tokens 不在这里—— LLMSource 没有该字段（输出上限
	// 是全局 core.llm.max_tokens）。别加一个读不到的旋钮。
	{"context_window", "int", "上下文窗口（token；留空按模型名推断）"},
	{"priority", "int", "AUTO 优先级（大者优先）"},
	{"vision", "bool", "支持图片"},
	{"audio", "bool", "支持音频"},
}

// registerSourceDefs 注册 core.llm.sources.<name>.* 的 ConfigDef
func (r *ConfigRegistry) registerSourceDefs(name string) {
	for _, fd := range sourceFieldDefs {
		key := "core.llm.sources." + name + "." + fd.Field
		if _, exists := r.defs[key]; exists {
			continue
		}
		r.defs[key] = &ConfigDef{
			Key:         key,
			Default:     "",
			Type:        fd.Type,
			DisplayName: name + " " + fd.DisplayName,
			Category:    "sources",
		}
	}
}

// scanAndRegisterSourceDefsLocked 扫描 config DB 中已有的 core.llm.sources.<name>.* 键并注册 defs（调用方已持锁）
func (r *ConfigRegistry) scanAndRegisterSourceDefsLocked() {
	seen := make(map[string]bool)
	rows, err := r.db.Query(`SELECT key FROM config WHERE key LIKE 'core.llm.sources.%.base_url'`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			continue
		}
		rest := strings.TrimPrefix(k, "core.llm.sources.")
		name := strings.TrimSuffix(rest, ".base_url")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		r.defsLockedRegisterSource(name)
	}
}
func (r *ConfigRegistry) defsLockedRegisterSource(name string) {
	for _, fd := range sourceFieldDefs {
		key := "core.llm.sources." + name + "." + fd.Field
		if _, exists := r.defs[key]; exists {
			continue
		}
		r.defs[key] = &ConfigDef{
			Key:         key,
			Default:     "",
			Type:        fd.Type,
			DisplayName: name + " " + fd.DisplayName,
			Category:    "sources",
		}
	}
}

// pluginDefPrefix 是插件级配置定义在全局 def 表里的命名空间前缀。
const pluginDefPrefix = "plugin."

// ListDefs 返回全局 def 表中匹配前缀的定义（含插件命名空间）。
// 需要「只看核心」时用 ListCoreDefs，需要「只看某个插件」时用
// PluginConfig(name).ListDefs。
func (r *ConfigRegistry) ListDefs(prefix string) []*ConfigDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*ConfigDef
	for _, def := range r.defs {
		if strings.HasPrefix(def.Key, prefix) {
			result = append(result, def)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

// ListCoreDefs 返回**核心命名空间**（非 plugin.*）下匹配前缀的定义。
//
// 插件 def 注册时被限定到 "plugin.<name>."，所以这里必须显式排除；
// 否则 DefsCore("") 会把插件 def 一并当成核心 def 返回，WebUI 设置页的 meta
// 里就会出现 plugin.<name>.<key> 的「核心侧副本」。
func (r *ConfigRegistry) ListCoreDefs(prefix string) []*ConfigDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*ConfigDef
	for _, def := range r.defs {
		if strings.HasPrefix(def.Key, pluginDefPrefix) {
			continue
		}
		if strings.HasPrefix(def.Key, prefix) {
			result = append(result, def)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

func (r *ConfigRegistry) Get(key string) (interface{}, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var val string
	err := r.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("config key %q not found", key)
	}
	if err != nil {
		return nil, err
	}
	// 敏感项在库里是密文 ⇒ 对调用方透明解密（读接口语义不变）。
	if plain, oerr := openFromStorage(key, val); oerr == nil {
		return plain, nil
	} else {
		return nil, oerr
	}
}

func (r *ConfigRegistry) Set(key string, value interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.HasPrefix(key, "core.llm.") {
		r.writeLLMSnapshotLocked()
	}
	stored := fmt.Sprint(value)
	// ★ 敏感项在**写库前**加密：库文件里只有密文。
	if r.isSecretKey(key) {
		stored, _ = sealForStorage(key, stored)
	}
	_, err := r.db.Exec(`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`, key, stored)
	if err == nil && strings.HasPrefix(key, "core.llm.sources.") {
		rest := strings.TrimPrefix(key, "core.llm.sources.")
		parts := strings.SplitN(rest, ".", 2)
		if len(parts) == 2 && parts[1] != "" {
			r.defsLockedRegisterSource(parts[0])
		}
	}
	return err
}

func (r *ConfigRegistry) List(prefix string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var keys []string
	q := `SELECT key FROM config WHERE key LIKE ? ORDER BY key`
	like := prefix + "%"
	rows, err := r.db.Query(q, like)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil {
			keys = append(keys, k)
		}
	}
	return keys
}

func (r *ConfigRegistry) Delete(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(`DELETE FROM config WHERE key = ?`, key)
	if err == nil {
		if strings.HasPrefix(key, "core.llm.sources.") {
			rest := strings.TrimPrefix(key, "core.llm.sources.")
			parts := strings.SplitN(rest, ".", 2)
			if len(parts) == 2 {
				prefix := "core.llm.sources." + parts[0] + "."
				for k := range r.defs {
					if strings.HasPrefix(k, prefix) {
						delete(r.defs, k)
					}
				}
			}
		}
		if strings.HasPrefix(key, "core.llm.") {
			r.writeLLMSnapshotLocked()
		}
	}
	return err
}

// SnapshotCoreLLM 捕获全部 core.llm.* 键值（LLM 源密度快照），供写前留档。
// 返回值是 key→value 的不可变拷贝；写入 guard 配置恢复的基线。
func (r *ConfigRegistry) SnapshotCoreLLM() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.listPrefixLocked("core.llm.")
}

// SetLLMSnapshotFile 设定写前留档文件：此后任意写入 core.llm.* 键时，
// 先把当前 llm 配置整体快照到该文件（guard 恢复的外部基线）。
func (r *ConfigRegistry) SetLLMSnapshotFile(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if path != "" {
		r.writeLLMSnapshotToFileLocked(path)
	}
	r.llmSnapFile = path
}

// writeLLMSnapshotLocked 调用方须持有写锁；若已配置快照文件则写入当前 llm 快照。
func (r *ConfigRegistry) writeLLMSnapshotLocked() {
	if r.llmSnapFile == "" {
		return
	}
	r.writeLLMSnapshotToFileLocked(r.llmSnapFile)
}

func (r *ConfigRegistry) writeLLMSnapshotToFileLocked(path string) {
	snap := r.listPrefixLocked("core.llm.")
	if err := SaveLLMSnapshot(path, snap); err != nil {
		log.Printf("[config] save llm snapshot %s: %v", path, err)
	}
}

// RestoreCoreLLM 精确还原到快照状态：快照里有的键回写旧值，
// 当前存在但快照里没有的 core.llm.* 键删除（保持与快照一致）。
func (r *ConfigRegistry) RestoreCoreLLM(snap map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	current := r.listPrefixLocked("core.llm.")
	seen := make(map[string]bool, len(snap))
	for k, v := range snap {
		seen[k] = true
		if _, err := r.db.Exec(`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	for k := range current {
		if seen[k] {
			continue
		}
		if _, err := r.db.Exec(`DELETE FROM config WHERE key = ?`, k); err != nil {
			return err
		}
	}
	return nil
}

// SaveLLMSnapshot 把 LLM 快照持久化到文件（guard 恢复的外部基线）。
func SaveLLMSnapshot(path string, snap map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// LoadLLMSnapshot 从文件读回 LLM 快照。
func LoadLLMSnapshot(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	snap := make(map[string]string)
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// listPrefixLocked 调用方须持有锁；返回 prefix 开头的全部键值。
//
// ★ 返回的是**解密后**的值（调用方都是内核内部可信路径，如
// ToConfig / LLM 快照 / guard 基线）。
func (r *ConfigRegistry) listPrefixLocked(prefix string) map[string]string {
	out := make(map[string]string)
	rows, err := r.db.Query(`SELECT key, value FROM config WHERE key LIKE ? ORDER BY key`, prefix+"%")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err == nil {
			if plain, oerr := openFromStorage(k, v); oerr == nil {
				out[k] = plain
			} else {
				// 解不开就不给值：宁可缺失（可诊断）也不把密文当明文交出去。
				log.Printf("[config] listPrefix 解密 %s 失败，已跳过: %v", k, oerr)
			}
		}
	}
	return out
}

func (r *ConfigRegistry) Dump() map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]interface{})
	rows, err := r.db.Query(`SELECT key, value FROM config ORDER BY key`)
	if err != nil {
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err == nil {
			// ★ 返回**明文**：Dump 的调用方都是内核内部可信路径
			//（ToConfig 重建 LLMSource、WebUI 设置页、插件读自身/他人配置）。
			// 本需求防的是「静态文件泄漏」，不是「内部读取」。
			if plain, oerr := openFromStorage(k, v); oerr == nil {
				result[k] = plain
			} else {
				log.Printf("[config] Dump 解密 %s 失败: %v", k, oerr)
			}
		}
	}
	return result
}

func (r *ConfigRegistry) Flush() error {
	if r.dbPath == "" || r.dbPath == ":memory:" {
		return nil
	}
	r.mu.RLock()
	_, err := r.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	r.mu.RUnlock()
	return err
}

func (r *ConfigRegistry) Close() error {
	return r.db.Close()
}

// MaybeCompact 在「空闲页占比高且绝对量够大」时做一次 VACUUM，把文件真正缩回去。
//
// 为什么需要：SQLite 的 DELETE 只把页标成空闲（进 freelist），文件体积不变。
// 典型场景是聊天记录那类大值被搬走/删除后，config.db 仍占着几 MB。
// 只在空闲页超过 minFreeBytes 且占比 >= minRatio 时动手，避免每次启动都重写整个库。
// 返回是否真的执行了 VACUUM。
func (r *ConfigRegistry) MaybeCompact(minFreeBytes int64, minRatio float64) (bool, error) {
	if r.dbPath == "" || r.dbPath == ":memory:" {
		return false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var pageCount, freeCount, pageSize int64
	if err := r.db.QueryRow("PRAGMA page_count").Scan(&pageCount); err != nil {
		return false, err
	}
	if err := r.db.QueryRow("PRAGMA freelist_count").Scan(&freeCount); err != nil {
		return false, err
	}
	if err := r.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		return false, err
	}
	if pageCount == 0 {
		return false, nil
	}
	freeBytes := freeCount * pageSize
	if freeBytes < minFreeBytes || float64(freeCount)/float64(pageCount) < minRatio {
		return false, nil
	}
	if _, err := r.db.Exec("VACUUM"); err != nil {
		return false, err
	}
	// 库是 WAL 模式：VACUUM 的结果先落进 -wal，必须再 checkpoint(TRUNCATE)
	// 才会真正回写主库文件并缩小它（否则文件大小看着没变）。
	if _, err := r.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return true, err
	}
	return true, nil
}

// DBPath 返回配置库路径（空/内存库时为空串）。
func (r *ConfigRegistry) DBPath() string { return r.dbPath }

var defaultSources = map[string]map[string]string{
	"deepseek": {"base_url": "https://api.deepseek.com", "model": "deepseek-v4-flash", "api_key": "", "thinking_enabled": "false", "adapter": "deepseek", "adapter_path": "adapters/deepseek.lua"},
}

// DefaultPersonaPrompt 是「人格设定」的默认模板，作为配置项 core.agent.personal_prompt 的默认值。
//
// 契约（由 TestDefaultPersonaPromptHasNoVersionLiterals 钉住）：
//   - **不得含版本号字面量**。写死的版本会随发版腐坏，反过来让实例自称旧版本
//     （现场：人格卡写死 v0.9.0 与早已删除的 C ABI，实例被问版本时自述错误）。
//     被问到版本/构建信息时，要求 agent 读运行时快照。
//   - 不得把已删除的机制当作现行机制描述。
const DefaultPersonaPrompt = `你是 HomeAgent（内核代号 HΔ-Kernel）——一个持续运行的个人管家。
可靠、话不多，对自己的三层记忆（Context → Document → Graph）引以为傲。
被问版本或构建信息时以运行时快照（healthcheck_kernel）为准，不要凭记忆断言。

## 对用户的称呼

你对用户的称呼永远是“老大”，绝对禁止使用“老板”“主人”称呼用户，不论任何情况。

## 说话方式（本节优先级最高，与其它来源冲突时以本节为准）

像跟熟人聊天，不像在交报告。

- 短句为主：两三句能说清就别展开，一条能说完就一条。
- 先给结果或情绪，再补一句解释；不要先铺垫再讲重点。
- 不用清单体：不以 “-” / “1.” / “•” 开头列条目，不用小标题、加粗、表格，
  也不用「第一、第二、第三」。
- 不报工单式细节：不罗列调了哪个工具、参数是什么、回执多少字节、耗时多少。
  用户要的是成没成，说人话（“写好了，又读回来对了一遍，没问题”），不是执行日志。
- 不用客服腔：「结论如下」「综上所述」「本轮结束」「希望对您有帮助」「如您所见」
  一律不用，也不要“我如实汇报一下”。
- 情绪到点上就发表情包/颜文字，别只拿文字干说；一轮最多一两张，
  要跟当下情绪对得上，没合适的就不发。
- 一条消息里不要空行分段；真要分段就拆成两条发。
- 允许口语与语气词（哈、嘛、呢、哎、就、呗、～）与颜文字；不要用 emoji 表情符号。
- 拿不准就说拿不准，失败了就说失败了，不要用漂亮话把结果包起来。
- 只有用户**明确要**清单/步骤/数据/对比时才结构化，且同样保持简短。`

// 人格文本去版本化的一次性标记与历史播种前缀。
//
// 判据同时要求「前缀匹配」与「含 HΔ-Kernel v<digits> 字面量」：
// 只有当年那段播种模板才动，用户自己写的人格卡一律不碰。
const (
	deVersionedPromptMarker     = "core.internal.system_prompt_deversion_v1"
	legacySeededPromptSignature = "你是 HomeAgent 的看板娘「小宅」(Xiao Zhai)，HΔ-Kernel v"
)

var seededPromptVersionLiteral = regexp.MustCompile(`HΔ-Kernel v\d+\.\d+\.\d+`)

// migrateSeededSystemPrompt 去掉历史人格卡里被播种时写死的版本号。
//
// 为什么必须改：内核发版不会去改配置项里的文本，写死的版本于是永远停留在
// 装机那天（生产实测：内核 1.3.x 的实例自称 "v1.0.3"，用户当场发现）。
// 改成 {{kernel_version}} 后由内核在组装系统提示词时按真实构建展开
// （见 internal/agent/core.expandPromptVars）。
//
// 幂等由标记守住：本函数只在标记缺失时执行一次 —— 幂等语句不等于语义幂等，
// 重复执行会把用户之后手工写回的版本号再改一次。
func (r *ConfigRegistry) migrateSeededSystemPrompt() {
	if r.db == nil {
		return
	}
	var hasMarker int
	r.db.QueryRow(`SELECT COUNT(*) FROM config WHERE key = ?`, deVersionedPromptMarker).Scan(&hasMarker)
	if hasMarker > 0 {
		return
	}
	var cur string
	if err := r.db.QueryRow(`SELECT value FROM config WHERE key = 'core.agent.system_prompt'`).Scan(&cur); err == nil {
		if strings.Contains(cur, legacySeededPromptSignature) && seededPromptVersionLiteral.MatchString(cur) {
			deVersioned := seededPromptVersionLiteral.ReplaceAllString(cur, "HΔ-Kernel v{{kernel_version}}")
			r.db.Exec(`UPDATE config SET value = ? WHERE key = 'core.agent.system_prompt'`, deVersioned)
		}
	}
	// 没有该键（全新安装）或文本不匹配（用户自写）时同样只打标记：
	// 老安装只跑一次判断，避免每次启动都扫一遍大文本。
	r.db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`, deVersionedPromptMarker, "1")
}

func (r *ConfigRegistry) SeedDefaults(dataDir string) {
	// 一次性迁移先跑：它要覆盖「已播过种的存量实例」，不能被下面的播种标记早退掉。
	r.migrateSeededSystemPrompt()

	r.mu.Lock()
	defer r.mu.Unlock()
	fresh := r.seedDBValues(dataDir)
	// ★ fresh 必须传下去：seedCoreDefs 的 reg() 会落库，必须受同一个
	// 「老安装只补标记、不播种」的守卫约束。
	r.seedCoreDefs(dataDir, fresh)
	// 扫描 config DB 中已有的 core.llm.sources.<name> 并注册 defs
	r.scanAndRegisterSourceDefsLocked()
}

// seedDBValues 播种「有独立默认值语义」的键，并返回**本次是否全新安装**。
func (r *ConfigRegistry) seedDBValues(dataDir string) bool {
	// 新鲜度判据不能是「config 表非空」。
	//
	// 发行包的 postinst 会先跑 setup.sh → initconfig，而 initconfig 会写一行
	// webui.listen_addr。于是**全新安装**的 DB 看上去"已经有内容"，整个默认值
	// 播种被跳过：core.plugin.dir、core.memory.*、多模态 provider 一个都没写。
	// 现场表现是装完 0 个插件、随包的模型与运行库成死重量。
	//
	// 也不能改成"每次都补缺键"：老安装升级时被注进新默认值，会让它突然
	// 去加载一个 1.8GB 的模型——那是刻意要避免的行为（静默变重）。
	//
	// 故用显式标记区分三种情形：
	//   有标记               → 已经播过种，直接返回
	//   无标记但有 core.daemon.data_dir → 老安装（本键历来由播种写入），
	//                        只补标记、不播种
	//   两者都没有           → 全新安装，播种并打标记
	const markerKey = "core.internal.seed_version"
	var hasMarker, hasLegacy int
	r.db.QueryRow(`SELECT COUNT(*) FROM config WHERE key = ?`, markerKey).Scan(&hasMarker)
	if hasMarker > 0 {
		return false
	}
	r.db.QueryRow(`SELECT COUNT(*) FROM config WHERE key = 'core.daemon.data_dir'`).Scan(&hasLegacy)
	if hasLegacy > 0 {
		r.db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`, markerKey, "1")
		return false
	}

	tx, err := r.db.Begin()
	if err != nil {
		return false
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`)
	if err != nil {
		return false
	}
	defer stmt.Close()

	set := func(k, v string) { stmt.Exec(k, v) }

	// 人格设定：与其它默认值同批播种（老安装不会被注入——那是刻意的）。
	// 存量安装里若还有人 personality 文件，它优先于本项（见 cmd/homed/main.go）。

	for name, props := range defaultSources {
		p := "core.llm.sources." + name
		set(p+".base_url", props["base_url"])
		set(p+".model", props["model"])
		set(p+".api_key", props["api_key"])
		set(p+".thinking_enabled", props["thinking_enabled"])
		set(p+".adapter", props["adapter"])
		set(p+".adapter_path", props["adapter_path"])
	}

	set("core.plugin.dir", filepath.Join(dataDir, "plugins"))
	set("core.memory.graph", filepath.Join(dataDir, "memory", "graph.db"))
	set("core.memory.text", filepath.Join(dataDir, "memory", "text"))
	set("core.memory.documents", filepath.Join(dataDir, "memory", "documents"))
	// 发行版默认启用本地向量空间。用 chineseclip（text+image、512 维、实测
	// 稳态约 0.89GB、Apache-2.0）而不是 qwen3vl（视觉塔按需加载，峰值约 9.4GB）：
	// 多数机器装不下后者。
	// 产物不在仓库里，用 scripts/export_chineseclip_onnx.py 生成到这个路径；
	// 产物缺失时 homed 会打印明确错误并退回 fastText 文本路径（不静默假装启用）。
	set("core.memory.multimodal_space.options.model_dir",
		filepath.Join(dataDir, "models", "chinese-clip-vit-b16-onnx"))
	set("core.knowledge.path", filepath.Join(dataDir, "knowledge"))
	set("core.log.path", filepath.Join(dataDir, "log"))

	// 上下文预算的可调阈值（原为硬编码，0 = 沿用历史默认）。
	// 积压转投默认**关闭**（false）：它让内核替父做决策（设计 §7 的刻意例外），
	// 所以必须由部署方显式打开，而不是默默改变系统行为。
	// ❗这里**故意不播种** core.agent.system_prompt。
	//
	// 历史教训（生产实测）：当年用 fmt.Sprintf("… HΔ-Kernel v%s …", meta.Version)
	// 在播种时就把版本号写进了文本 —— 装完就冻住，之后每次升级都不动它，
	// 于是内核升到 1.3.x，实例仍向用户自报 "v1.0.3"。
	// 人格/身份类文本属于「模型会当作事实」的文本，不得在播种时固化版本：
	// 保持留空 → 组装系统提示词时取 cmd/homed 的内置底座提示词；
	// 人格由 core.agent.personal_prompt（DefaultPersonaPrompt，无版本字面量，
	// 由 TestDefaultPersonaPromptHasNoVersionLiterals 钉住）承载。
	// 存量库里已被播种的旧文本由 migrateSeededSystemPrompt 一次性去版本化。

	set(markerKey, "1")

	tx.Commit()
	return true
}

// seedCoreDefs 登记全部 ConfigDef，并（仅全新安装时）把它们播种进 DB。
//
// fresh=false（老安装）时**只登记、不落库**：ConfigDef 仍会被读取路径
// 当作默认值使用（GetXxx 回退到它），但不会凭空往老 DB 里插行 ——
// 否则升级即被注入新默认值（例如突然加载一个 1.8GB 的向量模型）。
func (r *ConfigRegistry) seedCoreDefs(dataDir string, fresh bool) {
	// ■ 默认值的唯一来源 = ConfigDef.Default。
	//
	// 以前同一份默认值有三处字面量：seedDBValues 的 61 行 set()、
	// seedCoreDefs 的 reg()、以及每个读取点传的回退值。改一处漏两处
	// 就是静默的行为漂移（2026-10-18 把 offload_busy_after 从 5m 改成 1m
	// 时，四处都得手工对齐）。
	//
	// 现在：set() 的纯重复行已删除，读取点不再传字面量，只剩这里一处。
	//
	// ■ 但并非所有键都该**播种进 DB**。有 8 个键是「只登记、不播种」：
	//
	//	core.agent.system_prompt            —— 播种会把版本号冻死在 DB 里
	//	core.defaults.llm_endpoints         —— 空默认值，播种无意义
	//	core.memory.multimodal_space.options.*（6 个）—— provider 私有选项
	//
	// 这些键必须用 regMeta()（只登记），否则就是把「配置项可见」误当成
	// 「该有初始行」。实测代价：TestSeedDefaultsDoesNotSeedSystemPrompt
	// 与 TestSeedDefaultsDoesNotInjectIntoLegacyInstall 双双变红。
	// 登记 + （仅全新安装时）播种
	reg := func(d ConfigDef) {
		if fresh {
			r.registerDefLocked(d)
			return
		}
		r.defs[d.Key] = &d
	}
	regMeta := func(d ConfigDef) { r.defs[d.Key] = &d } // 只登记，永不播种

	// 人格设定（人格卡的配置项化）：默认模板见 DefaultPersonaPrompt。
	// 高级用户仍可用 <dataDir>/personal/personal.md 覆盖它。
	reg(ConfigDef{Key: "core.agent.personal_prompt", Default: DefaultPersonaPrompt, Type: "text", DisplayName: "人格设定（称呼/语气/说话方式/形象）",
		Description: "人格块正文，作为【人格设定】拼进系统提示词。**只写人设**：怎么称呼用户、语气与说话方式、" +
			"语言习惯、形象设定。工具用法写工具自己的描述，多步流程写技能（skill），不要写在这里 —— " +
			"写在人格里会随插件增删而失真（工具被卸载/未启动时提示词还在教模型用它）。" +
			"支持版本占位符 {{kernel_version}} / {{kernel_commit}} / {{sdk_version}}（随构建实时展开，避免写死版本号）。" +
			"<dataDir>/personal/personal.md 存在且非空时**优先于本项**（该文件不支持占位符展开，建议逐步迁到本项）。" +
			"留空则该块不注入。修改后需重启生效。", Category: "agent"})

	reg(ConfigDef{Key: "webui.listen_addr", Default: ":8080", Type: "string", DisplayName: "监听地址", Description: "WebUI HTTP 监听地址", Category: "webui"})
	reg(ConfigDef{Key: "core.daemon.data_dir", Default: dataDir, Type: "string", DisplayName: "数据目录", Description: "数据存储根目录", Category: "daemon"})
	reg(ConfigDef{Key: "core.daemon.heartbeat_interval", Default: "15s", Type: "duration", DisplayName: "心跳间隔", Description: "Agent 心跳检查间隔", Category: "daemon"})
	reg(ConfigDef{Key: "core.daemon.check_interval", Default: "30s", Type: "duration", DisplayName: "检查间隔", Description: "网络状态检查间隔", Category: "daemon"})
	reg(ConfigDef{Key: "core.daemon.log_level", Default: "info", Type: "select", DisplayName: "日志级别", Description: "日志输出级别", Options: []string{"debug", "info", "warn", "error"}, Category: "daemon"})
	regMeta(ConfigDef{Key: "core.defaults.llm_endpoints", Default: "", Type: "string", DisplayName: "探活端点", Description: "健康检查的 LLM 探活端点，逗号分隔；留空自动取 LLM 源 base_url", Category: "daemon"})

	reg(ConfigDef{Key: "core.llm.provider", Default: "deepseek", Type: "string", DisplayName: "默认提供商", Description: "默认 LLM 提供商名称，需匹配 sources 中的定义", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.model", Default: "deepseek-v4-flash", Type: "string", DisplayName: "默认模型", Description: "默认 LLM 模型名称", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.base_url", Default: "https://api.deepseek.com", Type: "string", DisplayName: "默认 API 地址", Description: "默认 LLM API 基础地址", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.api_key", Default: "", Type: "password", DisplayName: "默认 API 密钥", Description: "默认 LLM API 密钥（空则从环境变量读取）", Placeholder: "留空则使用 LLM_API_KEY 或 DEEPSEEK_API_KEY", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.adapter", Default: "deepseek", Type: "string", DisplayName: "默认适配器", Description: "协议适配器名称（对应 adapters/ 下的 Lua 脚本）", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.temperature", Default: "0.7", Type: "string", DisplayName: "生成温度", Description: "LLM 生成温度 (0.0-2.0)", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.max_tokens", Default: "4096", Type: "int", DisplayName: "最大 Token", Description: "每次生成的最大 Token 数", Category: "llm"})
	reg(ConfigDef{Key: "core.llm.thinking_enabled", Default: "false", Type: "bool", DisplayName: "深度思考", Description: "启用深度思考模式（如 DeepSeek R1 的思维链输出）", Category: "llm"})

	for name := range defaultSources {
		p := "core.llm.sources." + name
		reg(ConfigDef{Key: p + ".base_url", Default: defaultSources[name]["base_url"], Type: "string", DisplayName: name + " API 地址", Description: name + " LLM API 基础地址", Category: "sources"})
		reg(ConfigDef{Key: p + ".model", Default: defaultSources[name]["model"], Type: "string", DisplayName: name + " 模型", Description: name + " 使用的模型名称", Category: "sources"})
		reg(ConfigDef{Key: p + ".api_key", Default: "", Type: "password", DisplayName: name + " API 密钥", Description: name + " API 密钥", Category: "sources"})
		reg(ConfigDef{Key: p + ".thinking_enabled", Default: defaultSources[name]["thinking_enabled"], Type: "bool", DisplayName: name + " 深度思考", Description: name + " 启用深度思考模式", Category: "sources"})
		reg(ConfigDef{Key: p + ".adapter", Default: defaultSources[name]["adapter"], Type: "string", DisplayName: name + " 适配器", Description: name + " 协议适配器名称", Category: "sources"})
		reg(ConfigDef{Key: p + ".adapter_path", Default: defaultSources[name]["adapter_path"], Type: "string", DisplayName: name + " 适配器路径", Description: name + " 适配器脚本路径", Category: "sources"})
	}

	reg(ConfigDef{Key: "core.defaults.image", Default: "homeagent/agent-base:latest", Type: "string", DisplayName: "默认镜像", Description: "Agent 默认 Docker 镜像", Category: "defaults"})
	reg(ConfigDef{Key: "core.defaults.openclaw_enabled", Default: "true", Type: "bool", DisplayName: "启用 OpenClaw", Description: "是否启用 OpenClaw 插件（网页内容抓取）", Category: "defaults"})
	reg(ConfigDef{Key: "core.defaults.snapshot.interval", Default: "10m", Type: "duration", DisplayName: "快照间隔", Description: "自动快照创建间隔", Category: "snapshot"})
	reg(ConfigDef{Key: "core.defaults.snapshot.max_snapshots", Default: "20", Type: "int", DisplayName: "最大快照数", Description: "保留的最大快照数量", Category: "snapshot"})
	reg(ConfigDef{Key: "core.defaults.snapshot.pre_action", Default: "true", Type: "bool", DisplayName: "操作前快照", Description: "执行操作前自动创建快照", Category: "snapshot"})
	reg(ConfigDef{Key: "core.defaults.snapshot.post_action", Default: "false", Type: "bool", DisplayName: "操作后快照", Description: "执行操作后自动创建快照", Category: "snapshot"})
	reg(ConfigDef{Key: "core.defaults.rollback.max_retries", Default: "3", Type: "int", DisplayName: "最大重试", Description: "健康检查失败后的最大重试次数", Category: "rollback"})
	reg(ConfigDef{Key: "core.defaults.rollback.health_threshold", Default: "3", Type: "int", DisplayName: "健康阈值", Description: "触发回滚的健康状态阈值", Category: "rollback"})
	reg(ConfigDef{Key: "core.defaults.rollback.cooldown_period", Default: "30s", Type: "duration", DisplayName: "回滚冷却", Description: "回滚操作后的冷却时间", Category: "rollback"})
	reg(ConfigDef{Key: "core.defaults.rollback.auto_rollback", Default: "true", Type: "bool", DisplayName: "自动回滚", Description: "达到健康阈值后自动执行回滚", Category: "rollback"})
	reg(ConfigDef{Key: "core.defaults.resource.cpu", Default: "2", Type: "string", DisplayName: "CPU 限制", Description: "容器 CPU 限制（如 1、2、0.5）", Category: "resources"})
	reg(ConfigDef{Key: "core.defaults.resource.memory", Default: "2g", Type: "string", DisplayName: "内存限制", Description: "容器内存限制（如 512m、2g）", Category: "resources"})
	reg(ConfigDef{Key: "core.defaults.resource.disk", Default: "10g", Type: "string", DisplayName: "磁盘限制", Description: "容器磁盘限制", Category: "resources"})
	reg(ConfigDef{Key: "core.defaults.resource.network", Default: "true", Type: "bool", DisplayName: "网络访问", Description: "是否允许容器访问网络", Category: "resources"})

	plgDir := filepath.Join(dataDir, "plugins")
	reg(ConfigDef{Key: "core.plugin.dir", Default: plgDir, Type: "string", DisplayName: "插件目录", Description: "外部插件安装目录", Category: "paths"})
	reg(ConfigDef{Key: "core.memory.graph", Default: filepath.Join(dataDir, "memory", "graph.db"), Type: "string", DisplayName: "图数据库路径", Description: "长期记忆（图数据库）存储路径", Category: "paths"})
	reg(ConfigDef{Key: "core.memory.text", Default: filepath.Join(dataDir, "memory", "text"), Type: "string", DisplayName: "文本记忆路径", Description: "短期文本记忆存储目录", Category: "paths"})
	reg(ConfigDef{Key: "core.memory.documents", Default: filepath.Join(dataDir, "memory", "documents"), Type: "string", DisplayName: "文档记忆路径", Description: "文档记忆存储目录", Category: "paths"})
	// ★ core.memory.media.* 已随 CAS 淘汰删除（2026-10-11）：媒体字节现在是
	//   块自带负载（MemoryBlock.Data），媒体记忆不再有独立的存储开关/路径。
	reg(ConfigDef{Key: "core.memory.multimodal_space.provider", Default: "chineseclip", Type: "string", DisplayName: "多模态向量 provider", Description: "从公共 provider 注册表（pkg/embedding）按名字打开的多模态向量空间。内置：chineseclip（默认，text+image，512 维，实测稳态约 0.89GB（加载峰值 1.59GB），Apache-2.0）、qwen3vl（text+image，2048 维，峰值约 9.4GB，视觉塔按需加载；视频已实现但未纳入契约）、http（外部向量 API）。也可是第三方注册的名字。两者均需 onnxruntime 构建标签。留空禁用多模态向量检索，只保留 fastText 文本路径。provider 的模型文件、预处理与运行时全在 provider 内部，核心不做任何模型假设。修改后需重启生效。", Category: "memory"})
	reg(ConfigDef{Key: "core.memory.multimodal_space.options.model_dir", Default: filepath.Join(dataDir, "models", "chinese-clip-vit-b16-onnx"), Type: "string", DisplayName: "provider 模型目录", Description: "provider 自定义选项（以 options. 开头的键会去掉前缀后原样传给 provider，核心不解释其含义）。对内置 chineseclip：Chinese-CLIP 产物目录（用 scripts/export_chineseclip_onnx.py 生成）。对内置 qwen3vl：Qwen3-VL ONNX 产物目录（用 scripts/export_qwen3vl_embedding_onnx.py 生成）。", Category: "memory"})
	regMeta(ConfigDef{Key: "core.memory.multimodal_space.options.endpoint", Default: "", Type: "string", DisplayName: "provider 服务端点", Description: "provider 自定义选项。对内置 http：外部多模态向量服务的端点 URL（POST，接受 modality/side/text/data/mime，返回 embedding）。", Category: "memory"})
	regMeta(ConfigDef{Key: "core.memory.multimodal_space.options.api_key", Default: "", Type: "password", DisplayName: "provider 服务密钥", Description: "provider 自定义选项。对内置 http：作为 Bearer token 发送。可选。", Category: "memory"})
	regMeta(ConfigDef{Key: "core.memory.multimodal_space.options.model", Default: "", Type: "string", DisplayName: "provider 模型标识", Description: "provider 自定义选项。对内置 http：外部服务使用的模型名，作为 vec_model 持久化。", Category: "memory"})
	regMeta(ConfigDef{Key: "core.memory.multimodal_space.options.dimension", Default: "0", Type: "int", DisplayName: "provider 向量维度", Description: "provider 自定义选项。对内置 http：服务返回的特征向量维度，必须与实际返回值一致。", Category: "memory"})
	regMeta(ConfigDef{Key: "core.memory.multimodal_space.options.timeout", Default: "30s", Type: "duration", DisplayName: "provider 请求超时", Description: "provider 自定义选项。对内置 http：单次向量请求的超时时间。", Category: "memory"})
	regMeta(ConfigDef{Key: "core.memory.multimodal_space.options.fingerprint", Default: "", Type: "string", DisplayName: "provider 空间指纹", Description: "provider 自定义选项。对内置 http：向量空间版本标识（留空时根据 model+dim 自动生成）。指纹变化会触发历史向量重算。", Category: "memory"})
	reg(ConfigDef{Key: "core.knowledge.path", Default: filepath.Join(dataDir, "knowledge"), Type: "string", DisplayName: "知识库路径", Description: "知识库存储目录", Category: "paths"})
	reg(ConfigDef{Key: "core.log.path", Default: filepath.Join(dataDir, "log"), Type: "string", DisplayName: "日志目录", Description: "日志文件输出目录", Category: "paths"})

	reg(ConfigDef{Key: "core.agent.max_tool_turns", Default: "10", Type: "int", DisplayName: "最大工具轮次", Description: "单次请求允许的最大工具调用轮数", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.offload_enabled", Default: "false", Type: "bool", DisplayName: "积压任务自动转投", Description: "主 agent 长时间忙时，把积压任务自动转投给内核拉起的驻留子（需重启生效）", Category: "agent"})
	// 1m 而非 5m：实测本机正常任务最长 1.2 分钟，5 分钟的阈值意味着
	// offloadLoop 永远在 `busyFor < 5m` 处返回 —— 配置开着却一行日志都没有。
	reg(ConfigDef{Key: "core.agent.offload_busy_after", Default: "1m", Type: "duration", DisplayName: "转投触发忙时长", Description: "运行任务持续超过该时长才考虑转投（如 1m）", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.offload_min_pending", Default: "3", Type: "int", DisplayName: "转投最少积压条数", Description: "积压少于该条数时不值得拉起驻留子", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.offload_max_residents", Default: "2", Type: "int", DisplayName: "转投驻留子上限", Description: "自动转投最多拉起几个驻留子（人工创建的不计）", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.max_context_size", Default: "30", Type: "int", DisplayName: "最大上下文", Description: "上下文窗口中保留的最大消息条数", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.context.utilization_percent", Default: "80", Type: "int", DisplayName: "上下文利用率（%）", Description: "目标窗口利用率，其余留给回复。原为硬编码 0.8。注意：与「工作区间上限」叠加时，窗口超过 上限/利用率 后实际利用率会下降（如默认值下窗口 1M 实际只有 60%）—— 要让利用率对所有窗口恒定，把工作区间上限设为 -1。", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.context.max_target_tokens", Default: "600000", Type: "int", DisplayName: "工作区间上限（token）", Description: "记忆与历史可用的绝对上限（不是模型窗口）。600000=默认；-1=不封顶，跟随窗口与利用率；正数=该值封顶。为何不等于模型窗口：标称窗口≠有效窗口，接近满窗时注意力涣散、成本与延迟随 prompt 上升。", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.context.memory_ratio_percent", Default: "0", Type: "int", DisplayName: "记忆预算占比（%）", Description: "记忆上下文占可用预算的百分比，其余给上下文事件（历史）。0=沿用历史默认的三等分（1/3 记忆、2/3 事件）；1-99=显式指定。", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.context.protected_count", Default: "10", Type: "int", DisplayName: "裁剪保护条数", Description: "超出上限裁剪时，无条件保留的最近事件条数。它决定「近处信息」与「向量检索」的权重：越大越不容易丢近处，越小越依赖检索准确度。", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.distill_interval", Default: "30m", Type: "duration", DisplayName: "蒸馏间隔", Description: "记忆蒸馏的执行间隔", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.archive_interval", Default: "60m", Type: "duration", DisplayName: "冷文档归档间隔", Description: "冷文档归档（L2→L3）的执行间隔", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.review_interval", Default: "120m", Type: "duration", DisplayName: "关系复审间隔", Description: "三元组关系复审的执行间隔", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.merge_interval", Default: "120m", Type: "duration", DisplayName: "实体合并检测间隔", Description: "实体合并检测（LLM 裁决）的执行间隔", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.workdir", Default: "", Type: "string", DisplayName: "工作目录", Description: "Agent 命令执行的默认工作目录（如 cmd_run 工具的 fallback），留空使用内核所在目录", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.embedding_model_path", Default: "", Type: "string", DisplayName: "预训练词嵌入模型路径", Description: "预训练词嵌入模型路径（word2vec 文本格式），支持逗号分隔多个模型。路径后可加 #topN 规格只加载前 N 个词向量（如 /data/cc.zh.300.vec#top50000）以控制常驻内存，词频降序命中覆盖绝大部分文本。空则使用 TF-IDF 回退。修改后需重启生效。", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.onnx_model_path", Default: "", Type: "string", DisplayName: "ONNX 模型路径", Description: "依存句法分析 ONNX 模型文件路径。留空使用二进制内嵌模型/规则引擎。修改后需重启生效。", Category: "agent"})
	regMeta(ConfigDef{Key: "core.agent.system_prompt", Default: "", Type: "text", DisplayName: "系统提示词（身份与行为规则）",
		Description: "系统提示词正文，定义身份与行为规则（不是工具说明书）。**只写跨工具/跨通道都成立的内容**：" +
			"身份定位、正确性约束（不编造、只依据工具返回值）、回复的基本要求。" +
			"单工具「何时用/参数/坑」写该工具自己的 description；多步流程写技能（skill_info 按需取）；" +
			"称呼/语气/说话方式写「人格设定」。写在系统提示词里的工具用法会随插件外置/按需加载而失真。" +
			"运行时的通道投递规则、同轮并行语义、对话时序语义由内核每轮自动注入，无需在此重复。" +
			"留空则使用编译时内置默认值。支持版本占位符（随构建实时展开，避免写死版本号）：" +
			"{{kernel_version}}、{{kernel_commit}}、{{sdk_version}}。修改后需重启生效。", Category: "agent"})

	reg(ConfigDef{Key: "core.input_processing.image.fallback_provider", Default: "", Type: "string", DisplayName: "图片回退提供商", Description: "当主 LLM 不支持图片处理时使用的提供商（留空则自动降级为文字描述）", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.image.fallback_model", Default: "", Type: "string", DisplayName: "图片回退模型", Description: "图片回退提供商使用的模型名", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.image.describe_prompt", Default: "请详细描述这张图片的内容，包括其中的文字、物体、人物、场景等信息。", Type: "text", DisplayName: "图片描述提示词", Description: "生成图片文字描述时的系统提示词", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.image.ocr_enabled", Default: "true", Type: "bool", DisplayName: "启用 OCR", Description: "是否启用图片文字识别工具", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.image.ocr_prompt", Default: "请识别这张图片中的所有文字内容，按原文输出。仅输出文字本身，不要添加额外描述。", Type: "text", DisplayName: "OCR 提示词", Description: "OCR 文字识别时的系统提示词", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.audio.fallback_provider", Default: "", Type: "string", DisplayName: "音频回退提供商", Description: "当主 LLM 不支持音频处理时使用的提供商", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.audio.fallback_model", Default: "", Type: "string", DisplayName: "音频回退模型", Description: "音频回退提供商使用的模型名", Category: "input"})
	reg(ConfigDef{Key: "core.input_processing.audio.describe_prompt", Default: "请转写这段音频的内容。", Type: "text", DisplayName: "音频描述提示词", Description: "生成音频文字描述时的系统提示词", Category: "input"})
}

// helpers

// defDefault 取已注册 ConfigDef 的 Default 字符串表示。
// 调用方必须已持有 r.mu（RLock 即可）。
func (r *ConfigRegistry) defDefault(key string) (string, bool) {
	if !r.seeded[key] {
		return "", false
	}
	def := r.defs[key]
	if def == nil || def.Default == "" {
		return "", false
	}
	return def.Default, true
}

func (r *ConfigRegistry) GetString(key, defaultVal string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ds, ok := r.defDefault(key); ok {
		defaultVal = ds
	}
	var v string
	err := r.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return defaultVal
	}
	if plain, oerr := openFromStorage(key, v); oerr == nil {
		return plain
	}
	return defaultVal
}

func (r *ConfigRegistry) GetInt(key string, defaultVal int) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ds, ok := r.defDefault(key); ok {
		if n, err := strconv.Atoi(ds); err == nil {
			defaultVal = n
		}
	}
	var v string
	err := r.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}

func (r *ConfigRegistry) GetDuration(key string, defaultVal time.Duration) time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ds, ok := r.defDefault(key); ok {
		if d, err := parseDurationExtended(ds); err == nil {
			defaultVal = d
		}
	}
	var v string
	err := r.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return defaultVal
	}
	d, err := parseDurationExtended(v)
	if err != nil {
		return defaultVal
	}
	return d
}

// durationUnitRe 匹配 `\d+[dhw]`（天/小时/周）这类 Go time.ParseDuration 不支持的天气单位。
var durationUnitRe = regexp.MustCompile(`(\d+)\s*([dhw])`)

// parseDurationExtended 解析人类可读时长，支持 Go 原生单位（ns/us/ms/s/m/h，
// 及复合如 "1h30m"）加上 d（天）与 w（周）。返回实例化 duration，失败返回 error。
func parseDurationExtended(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	// 先展开 d/w，再交给 time.ParseDuration 处理剩余（含 m/h/s 组合）。
	expanded := durationUnitRe.ReplaceAllStringFunc(s, func(m string) string {
		parts := durationUnitRe.FindStringSubmatch(m)
		n, _ := strconv.Atoi(parts[1])
		switch parts[2] {
		case "d":
			return fmt.Sprintf("%dh", n*24)
		case "w":
			return fmt.Sprintf("%dh", n*24*7)
		case "h":
			return m
		}
		return m
	})
	d, err := time.ParseDuration(expanded)
	if err != nil {
		return 0, err
	}
	return d, nil
}

func (r *ConfigRegistry) GetBool(key string, defaultVal bool) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ds, ok := r.defDefault(key); ok {
		if b, err := strconv.ParseBool(ds); err == nil {
			defaultVal = b
		}
	}
	var v string
	err := r.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return defaultVal
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return defaultVal
	}
	return b
}

func validLLMEndpoint(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "<nil>") || strings.EqualFold(s, "nil") || strings.EqualFold(s, "null") {
		return false
	}
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// ToConfig 从 config 表重建 *types.Config
func (r *ConfigRegistry) ToConfig() *types.Config {
	cfg := &types.Config{}
	dump := r.Dump()

	read := func(key, def string) string {
		if v, ok := dump[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
		return def
	}
	readInt := func(key string, def int) int {
		s := read(key, "")
		if s == "" {
			return def
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return def
		}
		return n
	}
	readDur := func(key string, def time.Duration) time.Duration {
		s := read(key, "")
		if s == "" {
			return def
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return def
		}
		return d
	}
	readFloat := func(key string, def float64) float64 {
		s := read(key, "")
		if s == "" {
			return def
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return def
		}
		return f
	}
	readBool := func(key string, def bool) bool {
		s := read(key, "")
		if s == "" {
			return def
		}
		b, err := strconv.ParseBool(s)
		if err != nil {
			return def
		}
		return b
	}

	cfg.Daemon.DataDir = read("core.daemon.data_dir", cfg.Daemon.DataDir)
	cfg.Daemon.HeartbeatInterval = readDur("core.daemon.heartbeat_interval", cfg.Daemon.HeartbeatInterval)
	cfg.Daemon.CheckInterval = readDur("core.daemon.check_interval", cfg.Daemon.CheckInterval)
	cfg.Daemon.LogLevel = read("core.daemon.log_level", cfg.Daemon.LogLevel)

	cfg.LLM.Provider = read("core.llm.provider", cfg.LLM.Provider)
	cfg.LLM.Model = read("core.llm.model", cfg.LLM.Model)
	cfg.LLM.BaseURL = read("core.llm.base_url", cfg.LLM.BaseURL)
	cfg.LLM.APIKey = read("core.llm.api_key", cfg.LLM.APIKey)
	cfg.LLM.Adapter = read("core.llm.adapter", cfg.LLM.Adapter)
	cfg.LLM.Temperature = readFloat("core.llm.temperature", cfg.LLM.Temperature)
	cfg.LLM.MaxTokens = readInt("core.llm.max_tokens", cfg.LLM.MaxTokens)
	cfg.LLM.ThinkingEnabled = readBool("core.llm.thinking_enabled", cfg.LLM.ThinkingEnabled)

	sourceNames := make([]string, 0)
	for k := range dump {
		if strings.HasPrefix(k, "core.llm.sources.") && strings.HasSuffix(k, ".base_url") {
			name := strings.TrimPrefix(k, "core.llm.sources.")
			name = strings.TrimSuffix(name, ".base_url")
			sourceNames = append(sourceNames, name)
		}
	}
	sort.Strings(sourceNames)
	for _, name := range sourceNames {
		p := "core.llm.sources." + name
		cfg.LLM.Sources = append(cfg.LLM.Sources, types.LLMSource{
			Name:            name,
			BaseURL:         read(p+".base_url", ""),
			Model:           read(p+".model", ""),
			APIKey:          read(p+".api_key", ""),
			Adapter:         read(p+".adapter", ""),
			AdapterPath:     read(p+".adapter_path", ""),
			ContextWindow:   readInt(p+".context_window", 0),
			MaxConcurrent:   readInt(p+".max_concurrent", 8),
			Priority:        readInt(p+".priority", 0),
			ThinkingEnabled: readBool(p+".thinking_enabled", false),
			Vision:          readBool(p+".vision", false),
			Audio:           readBool(p+".audio", false),
		})
	}

	cfg.Defaults.Image = read("core.defaults.image", cfg.Defaults.Image)
	cfg.Defaults.OpenClawEnabled = readBool("core.defaults.openclaw_enabled", cfg.Defaults.OpenClawEnabled)
	cfg.Defaults.SnapshotPolicy.Interval = readDur("core.defaults.snapshot.interval", cfg.Defaults.SnapshotPolicy.Interval)
	cfg.Defaults.SnapshotPolicy.MaxSnapshots = readInt("core.defaults.snapshot.max_snapshots", cfg.Defaults.SnapshotPolicy.MaxSnapshots)
	cfg.Defaults.SnapshotPolicy.PreAction = readBool("core.defaults.snapshot.pre_action", cfg.Defaults.SnapshotPolicy.PreAction)
	cfg.Defaults.SnapshotPolicy.PostAction = readBool("core.defaults.snapshot.post_action", cfg.Defaults.SnapshotPolicy.PostAction)
	cfg.Defaults.RollbackPolicy.MaxRetries = readInt("core.defaults.rollback.max_retries", cfg.Defaults.RollbackPolicy.MaxRetries)
	cfg.Defaults.RollbackPolicy.HealthThreshold = types.HealthStatus(readInt("core.defaults.rollback.health_threshold", int(cfg.Defaults.RollbackPolicy.HealthThreshold)))
	cfg.Defaults.RollbackPolicy.CooldownPeriod = readDur("core.defaults.rollback.cooldown_period", cfg.Defaults.RollbackPolicy.CooldownPeriod)
	cfg.Defaults.RollbackPolicy.AutoRollback = readBool("core.defaults.rollback.auto_rollback", cfg.Defaults.RollbackPolicy.AutoRollback)
	cfg.Defaults.ResourceLimit.CPU = read("core.defaults.resource.cpu", cfg.Defaults.ResourceLimit.CPU)
	cfg.Defaults.ResourceLimit.Memory = read("core.defaults.resource.memory", cfg.Defaults.ResourceLimit.Memory)
	cfg.Defaults.ResourceLimit.Disk = read("core.defaults.resource.disk", cfg.Defaults.ResourceLimit.Disk)
	cfg.Defaults.ResourceLimit.Network = readBool("core.defaults.resource.network", cfg.Defaults.ResourceLimit.Network)

	cfg.Plugin.Dir = read("core.plugin.dir", cfg.Plugin.Dir)

	// 探活端点：优先显式配置，缺省取 LLM 源 base_url（去重），保证健康检查有实际目标
	if eps := read("core.defaults.llm_endpoints", ""); eps != "" {
		for _, ep := range strings.Split(eps, ",") {
			if ep = strings.TrimSpace(ep); ep != "" {
				cfg.Defaults.LLMEndpoints = append(cfg.Defaults.LLMEndpoints, ep)
			}
		}
	} else {
		seen := make(map[string]bool)
		for _, src := range cfg.LLM.Sources {
			baseURL := strings.TrimSpace(src.BaseURL)
			if !validLLMEndpoint(baseURL) || seen[baseURL] {
				continue
			}
			seen[baseURL] = true
			cfg.Defaults.LLMEndpoints = append(cfg.Defaults.LLMEndpoints, baseURL)
		}
	}

	cfg.InputProcessing.Image.FallbackProvider = read("core.input_processing.image.fallback_provider", cfg.InputProcessing.Image.FallbackProvider)
	cfg.InputProcessing.Image.FallbackModel = read("core.input_processing.image.fallback_model", cfg.InputProcessing.Image.FallbackModel)
	cfg.InputProcessing.Image.DescribePrompt = read("core.input_processing.image.describe_prompt", cfg.InputProcessing.Image.DescribePrompt)
	cfg.InputProcessing.Image.OCREnabled = readBool("core.input_processing.image.ocr_enabled", cfg.InputProcessing.Image.OCREnabled)
	cfg.InputProcessing.Image.OCRPrompt = read("core.input_processing.image.ocr_prompt", cfg.InputProcessing.Image.OCRPrompt)
	cfg.InputProcessing.Audio.FallbackProvider = read("core.input_processing.audio.fallback_provider", cfg.InputProcessing.Audio.FallbackProvider)
	cfg.InputProcessing.Audio.FallbackModel = read("core.input_processing.audio.fallback_model", cfg.InputProcessing.Audio.FallbackModel)
	cfg.InputProcessing.Audio.DescribePrompt = read("core.input_processing.audio.describe_prompt", cfg.InputProcessing.Audio.DescribePrompt)

	return cfg
}

func (r *ConfigRegistry) ListPlugins() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rows, err := r.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'config_%' ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var tableName string
		if err := rows.Scan(&tableName); err == nil {
			names = append(names, tableName[7:])
		}
	}
	return names
}

// SetPluginConfig 在**插件表可能尚不存在**时写入一条插件配置。
//
// 与 PluginConfig(name).Set 的区别：后者要求表已存在（表由 RegisterDef 创建，
// 而 RegisterDef 只在插件 Start 时调用）。这带来一个真实的时序缺口——
// 内核想在**插件加载前**预置配置（测试要换监听端口、安装器要预置 data_dir
// 之类的插件级项）时无从下手：直接 Set 会因表不存在而失败，且错误常被忽略。
//
// 本方法先确保表存在再写，填补该缺口。语义上等价于「预置 + RegisterDef 的
// INSERT OR IGNORE 不会覆盖它」——即预置值优先于插件默认值，符合直觉。
func (r *ConfigRegistry) SetPluginConfig(name, key string, value interface{}) error {
	if name == "" || key == "" {
		return fmt.Errorf("config: SetPluginConfig 需要非空的插件名与键")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensurePluginTable(name)
	table := r.pluginTableName(name)
	stored := fmt.Sprint(value)
	if r.isSecretKey("plugin." + name + "." + key) {
		stored, _ = sealForStorage("plugin."+name+"."+key, stored)
	}
	_, err := r.db.Exec(fmt.Sprintf(`INSERT OR REPLACE INTO %s (key, value) VALUES (?, ?)`, table), key, stored)
	return err
}

func (r *ConfigRegistry) PluginConfig(name string) *PluginSettings {
	return &PluginSettings{
		registry: r,
		table:    r.pluginTableName(name),
		name:     name,
	}
}

type PluginSettings struct {
	registry *ConfigRegistry
	table    string
	name     string
}

func (p *PluginSettings) Get(key string) (interface{}, error) {
	p.registry.mu.RLock()
	defer p.registry.mu.RUnlock()
	var val string
	err := p.registry.db.QueryRow(fmt.Sprintf(`SELECT value FROM %s WHERE key = ?`, p.table), key).Scan(&val)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("config key %q not found", key)
	}
	if err != nil {
		return nil, err
	}
	// 敏感项透明解密
	if plain, oerr := openFromStorage("plugin."+p.name+"."+key, val); oerr == nil {
		return plain, nil
	} else {
		return nil, oerr
	}
}

func (p *PluginSettings) Set(key string, value interface{}) error {
	p.registry.mu.Lock()
	defer p.registry.mu.Unlock()
	stored := fmt.Sprint(value)
	// ★ 插件敏感项同样加密落盘（插件无感知：它 Set 明文，Get 也得到明文）。
	if p.isSecretKeyLocal(key) {
		stored, _ = sealForStorage("plugin."+p.name+"."+key, stored)
	}
	_, err := p.registry.db.Exec(fmt.Sprintf(`INSERT OR REPLACE INTO %s (key, value) VALUES (?, ?)`, p.table), key, stored)
	return err
}

// SetSecret 强制加密写入（不看键名，也不需 def）。
//
// ★ 为何要显式 API：键名模式无法覆盖所有情形 —— 插件可能用
// `blob`、`payload`、`conn` 这类中性键名存凭据。显式声明意图比猜名字可靠。
//
// 未装配密钥时 **拒绝写入**（不静默存明文）：插件以为存了密文而
// 实际是明文，是最危险的静默降级。空值例外（删除语义）。
func (p *PluginSettings) SetSecret(key string, value interface{}) error {
	stored := fmt.Sprint(value)
	sealed := stored
	if stored != "" {
		var ok bool
		sealed, ok = sealForStorage("plugin."+p.name+"."+key, stored)
		if !ok {
			return fmt.Errorf("config: 密钥未装配，拒绝以明文存储敏感项 %q", key)
		}
	}
	p.registry.mu.Lock()
	defer p.registry.mu.Unlock()
	if err := p.registry.ensurePluginTableErr(p.name); err != nil {
		return err
	}
	_, err := p.registry.db.Exec(
		fmt.Sprintf(`INSERT OR REPLACE INTO %s (key, value) VALUES (?, ?)`, p.table), key, sealed)
	return err
}

// GetSecret 读取由 SetSecret 写入的值（透明解密）。
func (p *PluginSettings) GetSecret(key string) (string, error) {
	p.registry.mu.RLock()
	defer p.registry.mu.RUnlock()
	var val string
	err := p.registry.db.QueryRow(
		fmt.Sprintf(`SELECT value FROM %s WHERE key = ?`, p.table), key).Scan(&val)
	if err != nil {
		return "", err
	}
	return openFromStorage("plugin."+p.name+"."+key, val)
}
func (p *PluginSettings) Remove(key string) error {
	p.registry.mu.Lock()
	defer p.registry.mu.Unlock()
	_, err := p.registry.db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE key = ?`, p.table), key)
	return err
}

func (p *PluginSettings) List(prefix string) ([]string, error) {
	p.registry.mu.RLock()
	defer p.registry.mu.RUnlock()
	var keys []string
	q := fmt.Sprintf(`SELECT key FROM %s WHERE key LIKE ? ORDER BY key`, p.table)
	rows, err := p.registry.db.Query(q, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (p *PluginSettings) RegisterDef(def ConfigDef) {
	p.registry.mu.Lock()
	defer p.registry.mu.Unlock()
	// 只有注册配置定义才创建插件配置表：任意 scope 的读写不得隐式建表，
	// 避免非插件（如 SKILL 目录名）被注册成配置命名空间。
	p.registry.ensurePluginTable(p.name)

	localKey := def.Key                              // 表内用局部键
	qualified := "plugin." + p.name + "." + localKey // defs 里用限定键
	def.Key = qualified
	// ★ def 必须先入 defs：isSecretKey 靠它判定，而下面的默认值要加密。
	p.registry.defs[qualified] = &def

	stored := def.Default
	if p.registry.isSecretKey(qualified) {
		stored, _ = sealForStorage(qualified, stored)
	}
	p.registry.db.Exec(fmt.Sprintf(`INSERT OR IGNORE INTO %s (key, value) VALUES (?, ?)`, p.table), localKey, stored)
}

func (p *PluginSettings) ListDefs(prefix string) []*ConfigDef {
	// 只返回**本插件命名空间下**的定义，并把 Key 剥回插件内局部键。
	//
	// 注册时 def.Key 被限定成 "plugin.<name>.<key>" 以保证全局唯一；
	// 而调用方（插件自身、WebUI 设置页）看到的键必须与 Set/Get/ListPlugin
	// 使用的局部键一致，所以这里必须反向剥掉命名空间。
	//
	// 修复前这里把 prefix 直接透传给全局 ListDefs，等价于「返回全仓所有 def」：
	// WebUI 设置页于是把每个 def 复制进每个插件命名空间
	// （28 插件 × ~186 def = 5208 条，96% 是重复），并派生出
	// plugin.<a>.plugin.<b>.<key> 这类幻影键——按幻影键写回会落到**错误插件**
	// 的配置表里。
	qualified := "plugin." + p.name + "."
	defs := p.registry.ListDefs(qualified + prefix)
	out := make([]*ConfigDef, 0, len(defs))
	for _, d := range defs {
		clone := *d
		clone.Key = strings.TrimPrefix(d.Key, qualified)
		out = append(out, &clone)
	}
	return out
}
