// Package knowledgeplugin 是**知识库内部插件**（用户裁定 2026-11-18）。
//
// ## 为什么它是插件，不是内核模块
//
// 用户原话：
//
//	「knowledge 不应该作为一个内核模块，而是应该作为一个内部插件，因为它
//	  并不必须依附内核内部接口实现，而是可以通过 sdk 完美实现功能」
//	「同时移出内核后相关计算要它自己实现」
//	「kbtree 直接合入知识库插件」
//
// 判据（§0 第 11 条）：**能不能只用 SDK 完整实现？**
//
//	能 ⇒ 内部插件，**且它的计算也归它自己**
//
// 知识库的每一件事都满足：存储（文件 + 索引）、检索（稠密 + 词法 + 标识符）、
// 分类树、导入、媒体关联、对外 HTTP —— 全是纯计算与纯 IO，没有一件必须读
// 内核状态或被 eventloop 推动。所以它整体搬出内核。
//
// ## 它现在拥有什么（都在这个包里）
//
//	store.go    存储 + 检索 + 分类树 + 导入 + 媒体关联（原 internal/knowledge）
//	tokens.go   分词 / 关键词 —— **自己算**（原调 memory.*）
//	tools.go    knowledge_* 工具族（**自己注册**并声明 NoMemory/ParallelSafe）
//	http.go     读写 HTTP 面（原 kbtree 5 条只读路由 + 可选写面）
//	scope.go    暴露范围过滤
//	plugin.go   本文件：生命周期 + 配置 + 状态段
//
// ## 它不再向内核要什么
//
//	× FacKnowledge                （设施已删）
//	× kernelapi.Knowledge 接口    （已删）
//	× sdk.KnowledgeAPI 13 方法版  （已删）
//	× memory.TokenizeWords 等     （自己实现）
//	× mainState.knowledge 句柄    （已删）
package knowledgeplugin

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// PluginName 是本插件的名字（内置插件、编译期工厂）。
const PluginName = "knowledge"

// Plugin 是知识库插件。
type Plugin struct {
	name  string
	host  Host
	store *Store
	http  *HTTPServer
}

// New 构造插件（未 Start 前不碰磁盘）。
func New(name string) *Plugin { return &Plugin{name: name} }

// Name 实现 sdk.Plugin。
func (p *Plugin) Name() string { return p.name }

// Start 装配存储、注册工具、起 HTTP 面。
//
// ★ 这里是知识的**唯一装配点**：它自己开 store、自己注册工具、自己注册
// 状态段、自己起 HTTP —— 内核一行都不参与。
func (p *Plugin) Start(s Host) error {
	if s == nil {
		return nil
	}
	p.host = s

	// ── ① 配置（先声明（设置页可见），再读当前值）──
	declareSettings(s)
	cfg := newPluginConfig(s)

	// ── ② 存储（数据目录 = <dataDir>/knowledge）──
	root := cfg.rootDir
	if root == "" {
		// 默认落在宿主的数据根下：内核不再知道"知识库目录叫什么"。
		root = filepath.Join(cfg.dataDir, "knowledge")
	}
	// ★ **先迁移再扫盘**（原 kernelmod/knowledge 的顺序约束，见 migrate.go）。
	// 目录不存在时先建：首启没有目录不该让迁移"扫描失败"（那是噪声日志）。
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("knowledge: 建数据目录 %s: %w", root, err)
	}
	migrateRoot(root)
	store := NewStore(root)
	if err := store.Start(); err != nil {
		return err
	}
	p.store = store

	// ── ③ 多模态向量空间（**经 SDK 能力**，不是内核设施）──
	if mm := s.Multimodal(); mm != nil && mm.Loaded() {
		store.SetDenseSpace(mm)
		built, skipped := store.ReindexDense()
		if built > 0 || skipped > 0 {
			log.Printf("[knowledge] multimodal dense index: built %d, skipped %d (rest hit cache)", built, skipped)
		}
	}

	// ── ④ 工具族（插件自己注册并声明 NoMemory / ParallelSafe）──
	if n, err := RegisterTools(s, s.Media(), store); err != nil {
		return err
	} else {
		log.Printf("[knowledge] registered %d tool(s)", n)
	}

	// ── ⑤ 状态段（**谁持有数据谁注册**：知识库的统计由插件报，不由主 agent）──
	if st := s.Status(); st != nil {
		st.RegisterSection("knowledge", func() any {
			return map[string]any{"available": true, "stats": store.Stats()}
		})
	}

	// ── ⑥ HTTP 面（读默认开、写默认关）──
	if cfg.listenAddr != "" {
		srv := NewHTTPServer(store, cfg.expose, cfg.listenAddr, cfg.token, cfg.allowWrite)
		if err := srv.Start(); err != nil {
			// 端口占用不该让整个知识库不可用：工具面仍然工作。
			log.Printf("[knowledge] http face disabled: %v", err)
		} else {
			p.http = srv
		}
	}
	return nil
}

// Store 返回本插件的存储（装配侧据此构造对外的能力视图）。
//
// ★ 只在装配侧用：插件自己不需要它（内部直接调方法）。
func (p *Plugin) Store() *Store { return p.store }

// Stop 停 HTTP 面并落盘（幂等）。
func (p *Plugin) Stop() error {
	if p.http != nil {
		if err := p.http.Stop(); err != nil {
			log.Printf("[knowledge] http stop: %v", err)
		}
		p.http = nil
	}
	if p.store != nil {
		p.store.Stop()
		p.store = nil
	}
	return nil
}

// ── 配置 ──

// pluginConfig 是本插件的配置快照（读一次，Start 期间不变）。
type pluginConfig struct {
	dataDir    string
	rootDir    string
	listenAddr string
	token      string
	expose     string
	allowWrite bool
}

// newPluginConfig 从 SDK 配置面读取（缺失一律落到安全默认值）。
//
// ★ 为什么全走 `s.Config()`：插件不能读内核配置文件（那是内核的实现细节）。
//
//	`s.Config()` 是插件侧唯一的配置面 —— 运维在设置页改的就是它。
func newPluginConfig(s Host) pluginConfig {
	cfg := pluginConfig{
		listenAddr: defaultAddr, // 默认仅本机；改成对外地址需显式配置
		allowWrite: false,       // 写面默认关闭
	}
	set := s.Settings()
	if set == nil {
		return cfg
	}
	cfg.dataDir = set.DataDir()
	if cfg.dataDir == "" {
		cfg.dataDir = "."
	}
	get := func(key, def string) string {
		if v, err := set.Get(key); err == nil {
			if str, ok := v.(string); ok {
				if str = strings.TrimSpace(str); str != "" {
					return str
				}
			}
		}
		return def
	}
	cfg.rootDir = get("root_dir", "")
	cfg.listenAddr = get("listen_addr", defaultAddr)
	cfg.token = get("token", "")
	cfg.expose = get("expose_categories", "")
	if v, err := set.Get("allow_remote_write"); err == nil {
		if b, ok := v.(bool); ok {
			cfg.allowWrite = b
		}
	}
	return cfg
}

// declareSettings 声明本插件的配置项（设置页据此渲染，无需硬编码）。
//
// ★ 配置归插件：listen_addr/token/expose 是知识库自己的运行参数，
//
//	内核不该知道它们叫什么（endpoint 暴露面属于模块自身）。
func declareSettings(s Host) {
	set := s.Settings()
	if set == nil {
		return
	}
	set.RegisterDef(ConfigDef{
		Key: "root_dir", Default: "", Type: "string",
		DisplayName: "数据目录", Category: PluginName,
		Description: "知识库根目录；留空则用 <data>/knowledge",
	})
	set.RegisterDef(ConfigDef{
		Key: "listen_addr", Default: defaultAddr, Type: "string",
		DisplayName: "HTTP 监听地址", Category: PluginName,
		Description: "知识库 HTTP 面监听地址（默认 127.0.0.1:9892，仅本机）。改为对外地址前请确认 token 分发方式",
	})
	set.RegisterDef(ConfigDef{
		Key: "token", Default: "", Type: "password",
		DisplayName: "访问令牌", Category: PluginName, Secret: true,
		Description: "外部 agent 访问本服务所需的令牌；留空则启动时随机生成（仅本次运行有效）",
	})
	set.RegisterDef(ConfigDef{
		Key: "expose_categories", Default: "", Type: "string",
		DisplayName: "暴露范围", Category: PluginName,
		Description: "逗号分隔的分类路径，只暴露这些分类及其子树。留空=全部可见。前缀按路径分段匹配",
	})
	set.RegisterDef(ConfigDef{
		Key: "allow_remote_write", Default: false, Type: "bool",
		DisplayName: "允许远程写入", Category: PluginName,
		Description: "打开后 HTTP 面提供 POST /write。默认关闭：写入要决定分类归属与媒体处理，交给内核工具做",
	})
}
