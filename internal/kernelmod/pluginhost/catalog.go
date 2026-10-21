// Package pluginhost 是 plugin-host 模块：**插件宿主**（设计 §2.6 + 用户要求）。
//
// ## 职责（用户的说法）
//
// > 插件管理器不应该在被拉起后就去读插件列表获取元数据并写入缓存准备拉起吗？
//
// 本模块做四件事：
//
//	① **扫一次**插件目录 → 元数据**缓存**（PluginCatalog）
//	② 按**三形态**分流（自动启动 / 启用但不自动启动 / 禁用）
//	③ 按依赖声明（requires_facilities）决定**逐个插件**的拉起时机
//	④ 执行拉起（用 stage-host 产出的 Registry）
//
// ## ★ 为什么它是**内核模块**而不是插件（架构决定）
//
// 用户选定的方案 A。理由（自举问题）：
//
//	· `pluginmgr` 是**插件** ⇒ 它自己要被拉起 ⇒ 若调度逻辑在它内部，
//	  就得先拉起自己才能拉起自己（循环）。
//	· 而调度器要订阅内核的 syncbus（内核设施），依附内核模块更自然。
//
// ⇒ 调度放内核模块（本包），`pluginmgr` 插件只保留**人机界面**职责
// （列列表/启停/装卸的工具与 HTTP）。
//
// ## ★ 依赖（关键：**不依赖 agent**）
//
//	Requires: config, lua, supervisor, tracker, memory, textmem, docmem,
//	          knowledge, kernel-io, stage-host
//
// 插件**加载**需要的是 stage-host（Registry + StageHost）与几个底层设施，
// **不需要** agent。原先把 wirePluginSDK 放在 agent 模块里，导致插件加载
// 被迫排在 agent 之后（agent 层实测 35 秒）。
//
// 实测发现：SDK 的 `Plugin` 接口只有 `Name/Start/Stop`，**没有 Configure**
// （那条"插件 Configure 需要 StatusProvider"的注释已过时）。
// 且唯一在 Start 阶段引 Status 的插件（healthcheck）是 **nil-guarded** 的。
// ⇒ 拆成"底层 7 项在这里注入、agent 相关的 2 项由 agent 模块**后补注入**"。
package pluginhost

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
)

// PluginMeta 是单个插件的**元数据**（缓存项）。
//
// ★ 为何不直接用 plugin.PluginManifest：缓存只保留"调度与展示"需要的字段，
// 且加一层让"元数据来源"可替换（将来若从 DB 而非文件读，上层不用改）。
type PluginMeta struct {
	Name string
	// RequiresFacilities 是声明的启动前置设施（空 = 等内核完全初始化）。
	RequiresFacilities []string
	// AutoStart：true=伴随启动；false=启用但不自动启动（等显式启用）。
	AutoStart bool
	// Disabled 来自 disabled_plugins 配置（持久禁用，最高优先级）。
	Disabled bool

	// 以下用于展示（pluginmgr 的列表/状态）
	Version     string
	Description string
	NameZh      string
	Tags        []string
	// ManifestPath 便于诊断（"这个插件的元数据从哪来的"）
	ManifestPath string
	// ScannedAt 记录扫描时刻（缓存新鲜度；也便于"元数据过期"排查）
	ScannedAt time.Time
}

// PluginCatalog 是插件**元数据缓存**。
//
// ## 为什么要有它（用户指出的设计）
//
// 原先元数据散落且**每次重扫**：
//
//	· cmd/homed 的 collectPluginSpecs（决定启动时机）
//	· pluginmgr 的 listPlugins（人看的列表）
//
// ⇒ 扫一次、缓存下来，供调度器 / pluginmgr / 显式启用路径共用。
//
// ## ★ 并发
//
// 调度在装配期（单线程语义），但 pluginmgr 的 HTTP 工具可能在运行期读它。
// 用 RWMutex 保护；读多写少（只在扫描时写）。
type PluginCatalog struct {
	mu    *rwMutex
	items map[string]*PluginMeta
	dir   string
}

// newCatalog 创建空缓存。
func newCatalog(dir string) *PluginCatalog {
	return &PluginCatalog{mu: newRWMutex(), items: map[string]*PluginMeta{}, dir: dir}
}

// Get 取单个插件的元数据。
func (c *PluginCatalog) Get(name string) (*PluginMeta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.items[name]
	return m, ok
}

// Names 返回全部插件名（升序，可复现）。
func (c *PluginCatalog) Names() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.items))
	for n := range c.items {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// All 返回全部元数据（按名字升序）。
func (c *PluginCatalog) All() []*PluginMeta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	names := make([]string, 0, len(c.items))
	for n := range c.items {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*PluginMeta, 0, len(names))
	for _, n := range names {
		out = append(out, c.items[n])
	}
	return out
}

// Dir 返回被扫描的插件目录。
func (c *PluginCatalog) Dir() string { return c.dir }

// Size 返回缓存条目数。
func (c *PluginCatalog) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// set 写入一条（扫描时用）。
func (c *PluginCatalog) set(m *PluginMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[m.Name] = m
}

// scanCatalog 扫插件目录，把元数据写进缓存。
//
// ## 容错策略
//
// 无 manifest / 解析失败 ⇒ **跳过该目录**（不是错误）：插件目录里可能有
// 非插件内容（README、临时备份、install 中途的半成品）。返回跳过列表
// 供日志说明"为什么某个目录没被识别成插件"。
func scanCatalog(dir string, isDisabled func(string) bool) (*PluginCatalog, []string) {
	c := newCatalog(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return c, []string{"读取插件目录失败: " + err.Error()}
	}
	var skipped []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		mft, err := plugin.ReadManifest(filepath.Join(dir, name))
		if err != nil {
			skipped = append(skipped, name)
			continue
		}
		display := mft.Name
		if display == "" {
			display = name
		}
		c.set(&PluginMeta{
			Name:               name, // ★ 用**目录名**：它与 LoadOneDir/注册表键一致
			RequiresFacilities: mft.RequiresFacilities,
			AutoStart:          mft.ShouldAutoStart(),
			Disabled:           isDisabled != nil && isDisabled(name),
			Version:            mft.Version,
			Description:        mft.Description,
			NameZh:             mft.NameZh,
			Tags:               mft.Tags,
			ManifestPath:       filepath.Join(dir, name, "plugin.json"),
			ScannedAt:          time.Now(),
		})
	}
	return c, skipped
}
