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
	// 内核自带 skill 先落盘（不覆盖用户版本），再由下面的扫描统一加载。
	// 机制类内容住在 skill 里，系统提示词只引用、不复述（见 builtin.go 注释）。
	materializeBuiltinSkills(p.skillsDir)

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
		p.publishIndex()
		log.Printf("[skillmgr] registered via %s event: %s", detectEvent, filepath.Base(path))
	})

	// 启动全扫：兜底接管所有已存在的纯 SKILL 目录/单文件
	// （时序上 skillmgr(c<s) 先于 clawhubadapter 启动，此处先扫到的条目
	//   后续事件重复到达时 loadOne 幂等跳过）
	n := p.scanExisting()
	p.registerTools()
	// 首次声明：把当前索引推给内核的统一注册表。
	// 不调它的后果：模型看不到任何 skill 索引，而 skill_list 仍能列出
	// —— "工具能列但模型不知道"，是最难查的一类降级。
	p.publishIndex()
	log.Printf("[skillmgr] started, dir=%s, loaded %d native skills", p.skillsDir, n)
	return nil
}

func (p *Plugin) Stop() error {
	// 先撤声明再退订：顺序反过来的话，退订期间仍有事件在加载 skill，
	// 那部分会来不及进索引（虽然进程即将退出，但热重载路径下不退出）。
	p.unpublishIndex()
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

// skillSectionName 是本插件声明的系统提示词段名。
//
// 命名规则由 SDK 强制：`<owner>.<名字>`，owner 必须等于插件名。
// 因此别的插件无法覆盖本段（attempting 会被内核拒绝）。
const skillSectionName = "skillmgr.skills"

// skillSectionOrder 让技能索引排在**工具与技能使用说明**区段（100–199）。
//
// 为什么不是更靠前：索引里列的是工具名，模型要先知道"这轮有哪些工具"
// 再读"怎么用"才有意义。顺序约定见 sdk/systemprompt.go 的 PromptSection.Order。
const skillSectionOrder = 100

// buildIndex 生成已启用技能的精炼索引（名称 + 版本 + 一行描述）。
//
// 这是**声明内容**，由 publishIndex 推给内核；不再是让内核来 pull 的接口。
func (p *Plugin) buildIndex() string {
	const header = "[Skills] Usage manuals for plugins. Before calling a plugin's tool " +
		"(or when it fails), read the same-named skill with skill_info(\"skill-name\") " +
		"and follow it. Never guess from the name."

	entries := p.snapshot()
	var b strings.Builder
	for _, e := range entries {
		if !e.enabled {
			continue
		}
		desc := e.sk.Description()
		if desc == "" || desc == "---" {
			desc = "(no description)"
		}
		desc = p.truncateDesc(desc)
		fmt.Fprintf(&b, "%s v%s - %s\n", e.sk.Name(), e.sk.Version(), desc)
	}
	list := strings.TrimRight(b.String(), "\n")
	if list == "" {
		// 没有技能时**不声明**（而不是声明一段只有标题的空段）。
		return ""
	}
	return header + "\n\n" + list
}

// publishIndex 把当前技能索引声明到内核的**统一系统提示词注册表**。
//
// ## 为什么是"声明"而不是"让内核来问"
//
// 此前是内核定义 `SkillIndexProvider` 接口、skillmgr 实现它、bootstrap
// 手工把它接给根 agent。那条路有三个问题：
//
//  1. **双向耦合**：内核硬编码插件的产出，插件又 import 内核接口
//  2. **子 agent 漏接**：rtchild / 驻留子 / 工作式子各自构造，都要记得传；
//     漏传不报错也不崩，只是那个 agent 的提示词里一条 skill 都没有
//  3. **顺序不可控**：插件内容只能整块插在内核硬拼流程的某个固定位置
//
// 现在是声明式：插件声明自己的段，内核每轮重建时按 Order 渲染。
// 任何 agent（含未来新类型）读同一份注册表 —— **零接线**。
//
// ## 为什么每个变更点都要调
//
// 段内容是**静态字符串快照**（不是回调），所以技能集合变了必须重新声明。
// 漏调的症状是"新装的 skill 在提示词里看不到"，而 skill_list 工具却能列出
// —— 模型于是不知道有这个 skill 可用。这是本仓踩过的同类问题
// （built-in skill 不更新：机制在，但没推给读到的地方）。
//
// 内核侧同段重复声明是**覆盖**（幂等），所以无条件调用是安全的。
func (p *Plugin) publishIndex() {
	if p.sdk == nil {
		return
	}
	api := p.sdk.SystemPrompt()
	if api == nil {
		return
	}
	content := p.buildIndex()
	if err := api.DeclarePromptSection(sdk.PromptSection{
		Section:  skillSectionName,
		Owner:    p.name,
		Order:    skillSectionOrder,
		Content:  content,
		Volatile: true, // 内容随 skill 增删/启停变化（诊断用）
	}); err != nil {
		// 不静默：声明失败意味着模型看不到任何 skill 索引，
		// 而 skill_list 仍能列出 —— 这种"工具能列但模型不知道"最难查。
		log.Printf("[skillmgr] WARN: 系统提示词段声明失败（模型将看不到技能索引）: %v", err)
	}
}

// unpublishIndex 撤销本插件的声明（Stop / 停用时）。
//
// 不撤销的后果：插件已停用，但提示词里还留着"用 skill_info 读某 skill"，
// 模型于是去调一个不存在的工具。那比"没有说明"更糟。
func (p *Plugin) unpublishIndex() {
	if p.sdk == nil {
		return
	}
	if api := p.sdk.SystemPrompt(); api != nil {
		_ = api.RemovePromptSection(skillSectionName)
	}
}

// truncateDesc truncates a skill description to 80 runes and appends an ellipsis.
func truncateDesc(desc string) string {
	const maxRunes = 80
	r := []rune(desc)
	if len(r) <= maxRunes {
		return desc
	}
	return string(r[:maxRunes]) + "..."
}

// SkillDescriptions returns the full one-line descriptions of enabled skills.
// 供 agent 在有限的提示词空间里准确匹配技能；完整机制与操作步骤仍由 skill_info 按需读取。
// 描述应包括：适用问题/时机 + 该机制的关键区别，避免仅用含糊的目录名。
func (p *Plugin) SkillDescriptions() string {
	entries := p.snapshot()
	var b strings.Builder
	for _, e := range entries {
		if !e.enabled {
			continue
		}
		desc := strings.TrimSpace(e.sk.Description())
		if desc == "" || desc == "---" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", e.sk.Name(), desc)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (p *Plugin) truncateDesc(desc string) string {
	const maxRunes = 80
	r := []rune(desc)
	if len(r) <= maxRunes {
		return desc
	}
	return string(r[:maxRunes]) + "..."
}
