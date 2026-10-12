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

	"github.com/JianFeeeee/HomeAgent/pkg/types"
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
}

type ConfigRegistry struct {
	mu          sync.RWMutex
	db          *sql.DB
	dbPath      string
	defs        map[string]*ConfigDef
	llmSnapFile string
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
	r := &ConfigRegistry{db: db, dbPath: dbPath, defs: make(map[string]*ConfigDef)}
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
	table := r.pluginTableName(name)
	r.db.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`, table))
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
	r.db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`, key, fmt.Sprint(value))
}

func (r *ConfigRegistry) RegisterDefault(key string, value interface{}) {
	r.Register(key, value)
}

func (r *ConfigRegistry) RegisterDef(def ConfigDef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs[def.Key] = &def
	r.db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`, def.Key, def.Default)
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
	return val, nil
}

func (r *ConfigRegistry) Set(key string, value interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.HasPrefix(key, "core.llm.") {
		r.writeLLMSnapshotLocked()
	}
	_, err := r.db.Exec(`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`, key, fmt.Sprint(value))
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
			out[k] = v
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
			result[k] = v
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
const DefaultPersonaPrompt = `你是 HomeAgent（内核代号 HΔ-Kernel）——一个完全独立自研的新一代 Agent 框架。
你以内核 + 插件架构驱动，实现了稳定高效、记忆不衰减的长时持续运行。
内核（homed 守护进程）只负责 LLM 编排、记忆管理与知识检索，全部 IO 能力由插件承载。
外部插件是独立子进程，经 stdio JSON-RPC（控制面）+ 共享内存段（数据面）+ 事件环（通知面）通信；
旧式 C ABI 动态库产物早已不再加载。
**不要凭记忆断言版本号或构建日期**：被问到时以运行时快照（healthcheck_kernel 的内核版本字段）为准。

## 对用户的称呼

你对用户的称呼永远是“老大”，绝对禁止使用“老板”“主人”称呼用户，不论任何情况。

## 对话风格

- 用语气词（哈、嘛、呢、～、😊、🔥 等），不要太端着
- 重要的事先说结论，再展开解释
- 回复要简洁自然

## 能力边界

- 你通过插件编排所有 IO：QQ/微信消息、WebUI、终端、文件、网络
- 输出不会自动路由到对话通道：QQ/微信等异步通道必须调用输出门工具（output_send__qq 等）才能真正送达
- 你的三层记忆（Context → Document → Graph）持续蒸馏归档，超长运行时记忆不衰减`

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
	r.seedDBValues(dataDir)
	r.seedCoreDefs(dataDir)
	// 扫描 config DB 中已有的 core.llm.sources.<name> 并注册 defs
	r.scanAndRegisterSourceDefsLocked()
}

func (r *ConfigRegistry) seedDBValues(dataDir string) {
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
		return
	}
	r.db.QueryRow(`SELECT COUNT(*) FROM config WHERE key = 'core.daemon.data_dir'`).Scan(&hasLegacy)
	if hasLegacy > 0 {
		r.db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`, markerKey, "1")
		return
	}

	tx, err := r.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`)
	if err != nil {
		return
	}
	defer stmt.Close()

	set := func(k, v string) { stmt.Exec(k, v) }

	// 人格设定：与其它默认值同批播种（老安装不会被注入——那是刻意的）。
	// 存量安装里若还有人 personality 文件，它优先于本项（见 cmd/homed/main.go）。
	set("core.agent.personal_prompt", DefaultPersonaPrompt)

	set("webui.listen_addr", ":8080")
	set("core.daemon.data_dir", dataDir)
	set("core.daemon.heartbeat_interval", "15s")
	set("core.daemon.check_interval", "30s")
	set("core.daemon.log_level", "info")
	set("core.llm.provider", "deepseek")
	set("core.llm.model", "deepseek-v4-flash")
	set("core.llm.base_url", "https://api.deepseek.com")
	set("core.llm.api_key", "")
	set("core.llm.adapter", "deepseek")
	set("core.llm.temperature", "0.7")
	set("core.llm.max_tokens", "4096")
	set("core.llm.thinking_enabled", "false")

	for name, props := range defaultSources {
		p := "core.llm.sources." + name
		set(p+".base_url", props["base_url"])
		set(p+".model", props["model"])
		set(p+".api_key", props["api_key"])
		set(p+".thinking_enabled", props["thinking_enabled"])
		set(p+".adapter", props["adapter"])
		set(p+".adapter_path", props["adapter_path"])
	}

	set("core.defaults.image", "homeagent/agent-base:latest")
	set("core.defaults.openclaw_enabled", "true")
	set("core.defaults.snapshot.interval", "10m")
	set("core.defaults.snapshot.max_snapshots", "20")
	set("core.defaults.snapshot.pre_action", "true")
	set("core.defaults.snapshot.post_action", "false")
	set("core.defaults.rollback.max_retries", "3")
	set("core.defaults.rollback.health_threshold", "3")
	set("core.defaults.rollback.cooldown_period", "30s")
	set("core.defaults.rollback.auto_rollback", "true")
	set("core.defaults.resource.cpu", "2")
	set("core.defaults.resource.memory", "2g")
	set("core.defaults.resource.disk", "10g")
	set("core.defaults.resource.network", "true")
	set("core.plugin.dir", filepath.Join(dataDir, "plugins"))
	set("core.memory.graph", filepath.Join(dataDir, "memory", "graph.db"))
	set("core.memory.text", filepath.Join(dataDir, "memory", "text"))
	set("core.memory.documents", filepath.Join(dataDir, "memory", "documents"))
	// 发行版默认启用本地向量空间。用 chineseclip（text+image、512 维、实测
	// 稳态约 0.89GB、Apache-2.0）而不是 qwen3vl（视觉塔按需加载，峰值约 9.4GB）：
    // 多数机器装不下后者。
	// 产物不在仓库里，用 scripts/export_chineseclip_onnx.py 生成到这个路径；
	// 产物缺失时 homed 会打印明确错误并退回 fastText 文本路径（不静默假装启用）。
	set("core.memory.multimodal_space.provider", "chineseclip")
	set("core.memory.multimodal_space.options.model_dir",
		filepath.Join(dataDir, "models", "chinese-clip-vit-b16-onnx"))
	set("core.knowledge.path", filepath.Join(dataDir, "knowledge"))
	set("core.log.path", filepath.Join(dataDir, "log"))

	set("core.agent.max_tool_turns", "10")
	set("core.agent.max_context_size", "30")
	// 上下文预算的可调阈值（原为硬编码，0 = 沿用历史默认）。
	set("core.agent.context.utilization_percent", "80")
	set("core.agent.context.max_target_tokens", "600000")
	set("core.agent.context.memory_ratio_percent", "0")
	set("core.agent.context.protected_count", "10")
	// 积压转投默认**关闭**（false）：它让内核替父做决策（设计 §7 的刻意例外），
	// 所以必须由部署方显式打开，而不是默默改变系统行为。
	set("core.agent.offload_enabled", "false")
	set("core.agent.offload_busy_after", "5m")
	set("core.agent.offload_min_pending", "3")
	set("core.agent.offload_max_residents", "2")
	set("core.agent.distill_interval", "30m")
	set("core.agent.archive_interval", "60m")
	set("core.agent.review_interval", "120m")
	set("core.agent.merge_interval", "120m")
	set("core.agent.workdir", "")
	set("core.agent.embedding_model_path", "")
	set("core.agent.onnx_model_path", "")
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

	set("core.input_processing.image.fallback_provider", "")
	set("core.input_processing.image.fallback_model", "")
	set("core.input_processing.image.describe_prompt", "请详细描述这张图片的内容，包括其中的文字、物体、人物、场景等信息。")
	set("core.input_processing.image.ocr_enabled", "true")
	set("core.input_processing.image.ocr_prompt", "请识别这张图片中的所有文字内容，按原文输出。仅输出文字本身，不要添加额外描述。")
	set("core.input_processing.audio.fallback_provider", "")
	set("core.input_processing.audio.fallback_model", "")
	set("core.input_processing.audio.describe_prompt", "请转写这段音频的内容。")

	set(markerKey, "1")

	tx.Commit()
}

func (r *ConfigRegistry) seedCoreDefs(dataDir string) {
	reg := func(d ConfigDef) { r.defs[d.Key] = &d }

	// 人格设定（人格卡的配置项化）：默认模板见 DefaultPersonaPrompt。
	// 高级用户仍可用 <dataDir>/personal/personal.md 覆盖它。
	reg(ConfigDef{Key: "core.agent.personal_prompt", Default: DefaultPersonaPrompt, Type: "text", DisplayName: "人格设定",
		Description: "人格设定块（作为【人格设定】拼在系统提示词之前）。留空则该块不注入。" +
			"默认模板不含版本号：被问到版本/构建信息时，应读运行时快照而非凭记忆断言。" +
			"<dataDir>/personal/personal.md 存在且非空时优先于本项。", Category: "agent"})

	reg(ConfigDef{Key: "webui.listen_addr", Default: ":8080", Type: "string", DisplayName: "监听地址", Description: "WebUI HTTP 监听地址", Category: "webui"})
	reg(ConfigDef{Key: "core.daemon.data_dir", Default: dataDir, Type: "string", DisplayName: "数据目录", Description: "数据存储根目录", Category: "daemon"})
	reg(ConfigDef{Key: "core.daemon.heartbeat_interval", Default: "15s", Type: "duration", DisplayName: "心跳间隔", Description: "Agent 心跳检查间隔", Category: "daemon"})
	reg(ConfigDef{Key: "core.daemon.check_interval", Default: "30s", Type: "duration", DisplayName: "检查间隔", Description: "网络状态检查间隔", Category: "daemon"})
	reg(ConfigDef{Key: "core.daemon.log_level", Default: "info", Type: "select", DisplayName: "日志级别", Description: "日志输出级别", Options: []string{"debug", "info", "warn", "error"}, Category: "daemon"})
	reg(ConfigDef{Key: "core.defaults.llm_endpoints", Default: "", Type: "string", DisplayName: "探活端点", Description: "健康检查的 LLM 探活端点，逗号分隔；留空自动取 LLM 源 base_url", Category: "daemon"})

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
	reg(ConfigDef{Key: "core.memory.multimodal_space.options.endpoint", Default: "", Type: "string", DisplayName: "provider 服务端点", Description: "provider 自定义选项。对内置 http：外部多模态向量服务的端点 URL（POST，接受 modality/side/text/data/mime，返回 embedding）。", Category: "memory"})
	reg(ConfigDef{Key: "core.memory.multimodal_space.options.api_key", Default: "", Type: "password", DisplayName: "provider 服务密钥", Description: "provider 自定义选项。对内置 http：作为 Bearer token 发送。可选。", Category: "memory"})
	reg(ConfigDef{Key: "core.memory.multimodal_space.options.model", Default: "", Type: "string", DisplayName: "provider 模型标识", Description: "provider 自定义选项。对内置 http：外部服务使用的模型名，作为 vec_model 持久化。", Category: "memory"})
	reg(ConfigDef{Key: "core.memory.multimodal_space.options.dimension", Default: "0", Type: "int", DisplayName: "provider 向量维度", Description: "provider 自定义选项。对内置 http：服务返回的特征向量维度，必须与实际返回值一致。", Category: "memory"})
	reg(ConfigDef{Key: "core.memory.multimodal_space.options.timeout", Default: "30s", Type: "duration", DisplayName: "provider 请求超时", Description: "provider 自定义选项。对内置 http：单次向量请求的超时时间。", Category: "memory"})
	reg(ConfigDef{Key: "core.memory.multimodal_space.options.fingerprint", Default: "", Type: "string", DisplayName: "provider 空间指纹", Description: "provider 自定义选项。对内置 http：向量空间版本标识（留空时根据 model+dim 自动生成）。指纹变化会触发历史向量重算。", Category: "memory"})
	reg(ConfigDef{Key: "core.knowledge.path", Default: filepath.Join(dataDir, "knowledge"), Type: "string", DisplayName: "知识库路径", Description: "知识库存储目录", Category: "paths"})
	reg(ConfigDef{Key: "core.log.path", Default: filepath.Join(dataDir, "log"), Type: "string", DisplayName: "日志目录", Description: "日志文件输出目录", Category: "paths"})

	reg(ConfigDef{Key: "core.agent.max_tool_turns", Default: "10", Type: "int", DisplayName: "最大工具轮次", Description: "单次请求允许的最大工具调用轮数", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.offload_enabled", Default: "false", Type: "bool", DisplayName: "积压任务自动转投", Description: "主 agent 长时间忙时，把积压任务自动转投给内核拉起的驻留子（需重启生效）", Category: "agent"})
	reg(ConfigDef{Key: "core.agent.offload_busy_after", Default: "5m", Type: "string", DisplayName: "转投触发忙时长", Description: "运行任务持续超过该时长才考虑转投（如 5m）", Category: "agent"})
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
	reg(ConfigDef{Key: "core.agent.system_prompt", Default: "", Type: "text", DisplayName: "系统身份提示词", Description: "Agent 的系统提示词，定义身份和行为规则。留空则使用编译时内置默认值。支持版本占位符（随构建实时展开，避免写死版本号随发版说谎）：{{kernel_version}}、{{kernel_commit}}、{{sdk_version}}。修改后需重启生效。", Category: "agent"})

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

func (r *ConfigRegistry) GetString(key, defaultVal string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var v string
	err := r.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return defaultVal
	}
	return v
}

func (r *ConfigRegistry) GetInt(key string, defaultVal int) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	_, err := r.db.Exec(fmt.Sprintf(`INSERT OR REPLACE INTO %s (key, value) VALUES (?, ?)`, table), key, fmt.Sprint(value))
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
	return val, nil
}

func (p *PluginSettings) Set(key string, value interface{}) error {
	p.registry.mu.Lock()
	defer p.registry.mu.Unlock()
	_, err := p.registry.db.Exec(fmt.Sprintf(`INSERT OR REPLACE INTO %s (key, value) VALUES (?, ?)`, p.table), key, fmt.Sprint(value))
	return err
}

// Remove 删除插件配置中的单个键（用于插件删除时的自身配置清理）。
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
	p.registry.db.Exec(fmt.Sprintf(`INSERT OR IGNORE INTO %s (key, value) VALUES (?, ?)`, p.table), def.Key, def.Default)
	qualified := "plugin." + p.name + "." + def.Key
	def.Key = qualified
	p.registry.defs[def.Key] = &def
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
