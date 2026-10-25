// Package skillmgr 实现原生 SKILL 插件管理器。
//
// 职责边界：
//   - 本插件是 native skill（SKILL.md / skill.json 指令文档型插件）的唯一归属者，
//     负责其全生命周期：加载、卸载、启停、生成（模板）、导出（.skm 包）、安装。
//   - clawhubadapter（OpenClaw 兼容层）扫描 skills 目录时发现纯 SKILL 条目后
//     发布 skill_detected 事件移交本插件注册；sidecar/OC plugin 不归本插件管。
//
// skill 的运行模型：SKILL.md 是给 LLM 的指令文档，agent 通过 skill_info 读取全文后
// 用 cmd_run 等工具按文档执行；skill 不注册可调用 handler 工具。
package skillmgr

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

const (
	// SkillFileName 标准 skill 主文件名
	SkillFileName = "SKILL.md"
	// MetaFileName 可选元数据文件（覆盖 SKILL.md frontmatter 字段）
	MetaFileName = "skill.json"
	// PackExt 导出包扩展名
	PackExt = ".skm"
	// detectEvent 兼容层移交事件类型
	detectEvent = events.EventSkillDetected
)

func init() {
	plugin.RegisterPluginMeta("skillmgr", "技能管理器", "Skill Manager")
	plugin.RegisterFactory("skillmgr", func(name string, cfg map[string]interface{}) (sdk.Plugin, error) {
		dir := ""
		if dataDir, ok := cfg["data_dir"].(string); ok && dataDir != "" {
			dir = filepath.Join(dataDir, "skills")
		}
		return New(name, dir), nil
	})
}

// SkillEntry 一个已加载的 native skill 及其实时状态。
type SkillEntry struct {
	sk      *plugin.SKILLPlugin
	path    string
	enabled bool
}

type Plugin struct {
	name      string
	skillsDir string

	mu     sync.RWMutex
	skills map[string]*SkillEntry // name -> entry
	sdk    *sdk.PluginSDK

	unsub func() // skill_detected 订阅注销
}

func New(name, skillsDir string) *Plugin {
	return &Plugin{
		name:      name,
		skillsDir: skillsDir,
		skills:    map[string]*SkillEntry{},
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s

	// 默认目录：内核 data_dir 下 skills 目录（与配置 core.daemon.data_dir 对齐）
	if p.skillsDir == "" {
		if v, _ := s.Settings().GetCore("daemon.data_dir"); v != nil {
			if dir, ok := v.(string); ok && dir != "" {
				p.skillsDir = filepath.Join(dir, "skills")
			}
		}
	}
	if err := os.MkdirAll(p.skillsDir, 0755); err != nil {
		return fmt.Errorf("mkdir skills dir: %w", err)
	}

	// 订阅兼容层移交事件（clawhubadapter 发现纯 SKILL 后发布）
	p.unsub = s.Subscribe(detectEvent, func(evt *events.Event) {
		path, _ := evt.Payload["path"].(string)
		if path == "" {
			return
		}
		if _, err := p.loadOne(path); err != nil {
			log.Printf("[skillmgr] detected skill %s: %v", path, err)
			return
		}
		log.Printf("[skillmgr] registered via %s event: %s", detectEvent, filepath.Base(path))
	})

	// 启动全扫：兜底接管所有已存在的纯 SKILL 目录/单文件
	// （时序上 skillmgr(c<s) 先于 clawhubadapter 启动，此处先扫到的条目
	//   后续事件重复到达时 loadOne 幂等跳过）
	n := p.scanExisting()
	p.registerTools()
	log.Printf("[skillmgr] started, dir=%s, loaded %d native skills", p.skillsDir, n)
	return nil
}

func (p *Plugin) Stop() error {
	if p.unsub != nil {
		p.unsub()
		p.unsub = nil
	}
	return nil
}

// scanExisting 扫描 skills 目录加载全部纯 SKILL 条目。返回加载数量。
func (p *Plugin) scanExisting() int {
	entries, err := os.ReadDir(p.skillsDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || e.Name() == "node_modules" {
			continue
		}
		path := filepath.Join(p.skillsDir, e.Name())
		if !isNativeSkillPath(path) {
			continue
		}
		if _, err := p.loadOne(path); err != nil {
			log.Printf("[skillmgr] scan skip %s: %v", e.Name(), err)
			continue
		}
		count++
	}
	return count
}

// isNativeSkillPath 判断路径是否为纯 SKILL 类型（非 sidecar 非 OC plugin）。
// sidecar: main.js/main.py；OC plugin: openclaw.plugin.json 或带 openclaw 扩展的 package.json。
func isNativeSkillPath(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !info.IsDir() {
		return strings.HasSuffix(strings.ToLower(path), ".md")
	}
	// 目录内含 sidecar / OC plugin 标志文件的归兼容层管
	for _, flag := range []string{"main.js", "main.py", "openclaw.plugin.json"} {
		if fileExists(filepath.Join(path, flag)) {
			return false
		}
	}
	if hasOCExtensions(filepath.Join(path, "package.json")) {
		return false
	}
	// 纯 skill：有 SKILL.md 或 skill.json 即认
	return fileExists(filepath.Join(path, SkillFileName)) ||
		fileExists(filepath.Join(path, MetaFileName))
}

// hasOCExtensions 检查 package.json 是否带 openclaw 扩展声明（与 clawhubadapter 同语义）。
func hasOCExtensions(pkgPath string) bool {
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return false
	}
	var pkg struct {
		Openclaw json.RawMessage `json:"openclaw"`
	}
	return json.Unmarshal(data, &pkg) == nil && len(pkg.Openclaw) > 0
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// loadOne 从路径加载单个 skill 并入表（幂等：已存在且未变更则跳过）。
func (p *Plugin) loadOne(path string) (*SkillEntry, error) {
	sk, err := plugin.LoadSKILL(path)
	if err != nil {
		return nil, err
	}
	name := sk.Name()
	if name == "" || name == "." {
		return nil, fmt.Errorf("skill at %s has empty name", path)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if old, ok := p.skills[name]; ok {
		// 幂等：同路径同内容不重复加载
		if old.path == path {
			return old, nil
		}
		// 重名不同路径：拒绝并提示
		p.mu.Unlock()
		return nil, fmt.Errorf("skill name conflict: %q already loaded from %s", name, old.path)
	}
	entry := &SkillEntry{sk: sk, path: path, enabled: true}
	p.skills[name] = entry
	return entry, nil
}

// removeOne 从表中移除 skill（不动磁盘）。
func (p *Plugin) removeOne(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.skills[name]; !ok {
		return false
	}
	delete(p.skills, name)
	return true
}

// snapshot 返回排序后的 skill 名列表。
func (p *Plugin) snapshot() []*SkillEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	names := make([]string, 0, len(p.skills))
	for n := range p.skills {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*SkillEntry, 0, len(names))
	for _, n := range names {
		out = append(out, p.skills[n])
	}
	return out
}

func (p *Plugin) get(name string) (*SkillEntry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.skills[name]
	return e, ok
}

// SkillIndex 实现 agentCore.SkillIndexProvider：返回已启用技能的精炼索引。
// 每次 LLM 调用都会执行，必须轻量（只读锁 + 字符串拼接，无 IO）。
func (p *Plugin) SkillIndex() string {
	entries := p.snapshot()
	var b strings.Builder
	for _, e := range entries {
		if !e.enabled {
			continue
		}
		desc := e.sk.Description()
		if desc == "" || desc == "---" {
			desc = "(无描述)"
		}
		// ★ 截断必须按**字符**（rune）而不是字节。
		//
		// 原实现 `desc[:80]` 按字节切，而中文占 3 字节 ⇒ 切点常落在
		// 多字节字符中间，产生**非法 UTF-8**。后果实测：
		//
		//   seq-tools v - 把反复出现的同类工具操作编成「序列」存起来，以后一次�
		//
		// 模型看到的是乱码尾部，且"什么时候该用"那部分整段丢失 ——
		// 于是它根本不知道这个 skill 何时适用（e2e 实测：该用 seq 的
		// 多对象重复场景，模型改去调 file-tools）。
		//
		// 本仓 25 个 skill 里有 8 个（32%）踩过这个截断。
		// 判据见 TestSkillIndexTruncatesByRune。
		desc = truncateDesc(desc)
		fmt.Fprintf(&b, "%s v%s - %s\n", e.sk.Name(), e.sk.Version(), desc)
	}
	return strings.TrimRight(b.String(), "\n")
}

// truncateDesc 把 skill 描述截断到 80 个**字符**（rune），超出加省略号。
//
// 必须是字符而不是字节：中文占 3 字节，按字节切会落在多字节字符中间，
// 产生非法 UTF-8。见 TestSkillIndexTruncatesByRune 的真实缺陷说明。
func truncateDesc(desc string) string {
	const maxRunes = 80
	r := []rune(desc)
	if len(r) <= maxRunes {
		return desc
	}
	return string(r[:maxRunes]) + "..."
}
