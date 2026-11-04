package promptsec

// 插件**声明式系统提示词段**：插件只声明内容，内核每轮重建时渲染。
//
// ## 用户模型（2026-11-04）
//
// > 每个插件都可以声明 systemprompt，可运行时更新，内核每轮重建。
//
// 于是"某个插件想告诉模型怎么用自己的工具"变成**插件自己声明的一段文本**：
//
//	skillmgr 声明 "skillmgr.skills" → 技能索引 + "调工具前先读同名 skill"
//	qq       声明 "qq.identity"    → 身份绑定规则
//	其他插件 声明 "<自己>.<段名>"  → 各自的说明
//
// 而内核只做两件事：**存**（按 section 覆盖）与**渲染**（每轮按 Order 拼进
// system prompt）。内核不认识 skill、不认识 qq，也不判内容。
//
// ## 为什么必须由内核持有（不能是 Agent 字段）
//
// 本仓已有一处栽在这上面：技能索引曾经是 `Agent.skillIndex` 字段，
// 由 `cmd/homed/bootstrap.go` 手工接线到**根 agent**。于是 rtchild、驻留子
// 各自 `New(AgentConfig{...})` 时都得记得再传一遍 —— 漏传不报错、不崩，
// 只是"那个 agent 看不到任何 skill"，然后模型凭工具名瞎猜。
//
// 生产症状：A2A 分诊助手（驻留子）手上没有 cmd_run（cmd 插件按需拉起），
// 也看不到 cmd-tools skill 里"先 plugin_start cmd"的说明，于是把
// terminal_create 当成一次性命令执行器，一条命令开一个终端。
//
// 教训：**能力归属内核，插件只声明**。放在 Registry 里，任何 agent
// （含未来新类型）零改动自动可见；放在 Agent 字段里，每个构造点都是一根
// 要记得接的线 —— 那正是"洋葱"退化成"莲藕"的形态。
//
// ## 幂等语义（用户明确要求"可运行时更新"）
//
// 同一 Section 重复声明 ⇒ **覆盖**，不是追加。
//
// 这条不是风格问题，是正确性问题：`stepPrepare` 在一次任务里可能被重入
// （上下文裁剪后 `f.Step = StepPrepare`），而 system prompt 每轮重建。
// 若声明是追加，索引会越堆越多，最终挤掉真正的对话历史。
// 用 map 而不是 slice，让"多份"在结构上不可能发生。

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Section 是插件声明的一段系统提示词内容。
type Section struct {
	// Section 是**全局唯一**的段名，约定 `<插件名>.<名字>`（如 "skillmgr.skills"）。
	//
	// ★ 强制前缀约定（与 security 的 Profile.Name 同型）：拼错能被立刻发现，
	// 且插件之间不可能互相覆盖。
	Section string

	// Owner 是声明它的插件名。必须与 Section 的前缀一致。
	Owner string

	// Order 决定渲染顺序（小的在前）。同 Order 按 Section 字典序，保证稳定。
	Order int

	// Content 是正文。**空串表示移除该段**（运行时撤销声明）。
	Content string

	// Volatile 表示内容会频繁变化（如技能索引随 skill 增删变化）。
	//
	// 仅用于诊断输出（状态面能看到哪些段每轮都在变），不参与渲染决策。
	// 存在的理由：技能索引是本类声明里唯一"随时可能变"的段，
	// 出问题时需要能一眼区分"这段是静态说明"还是"这段每轮重算"。
	Volatile bool
}

// Registry 是**内核级**的声明表：一个进程一份，所有 agent 共享。
//
// ★ 并发：pluginmgr 在插件 Start 期间声明，agent 每轮渲染读取。
// 读多写少，用 RWMutex；渲染时先拷贝快照再排序，不在持锁期做字符串拼接。
type Registry struct {
	mu       sync.RWMutex
	sections map[string]Section
}

// NewRegistry 建一个空的声明表。
func NewRegistry() *Registry {
	return &Registry{sections: make(map[string]Section)}
}

// Declare 声明/更新一段。同 Section 覆盖（幂等）。
//
// 返回 error 的情形：
//
//	· 段名不符合 `<owner>.<名字>` 形式
//	· Owner 与段名前缀不一致（试图声明别人的段）
//
// ★ 不做"静默接受"：拼错段名却悄悄生效，是这个仓库反复吃过亏的
// 一类故障（参见 security 档位未声明即报错的理由）。
func (r *Registry) Declare(s Section) error {
	if r == nil {
		return fmt.Errorf("promptsec: registry 未初始化")
	}
	owner := strings.TrimSpace(s.Owner)
	section := strings.TrimSpace(s.Section)
	if owner == "" {
		return fmt.Errorf("promptsec: Owner 不能为空")
	}
	if section == "" {
		return fmt.Errorf("promptsec: Section 不能为空")
	}
	prefix, _, ok := strings.Cut(section, ".")
	if !ok || prefix == "" || !strings.HasPrefix(section, owner+".") {
		return fmt.Errorf("promptsec: 段名 %q 必须形如 %q.<名字>（Owner=%q）",
			section, owner, owner)
	}
	if len(section) <= len(owner)+1 {
		return fmt.Errorf("promptsec: 段名 %q 缺少名字部分", section)
	}

	s.Owner = owner
	s.Section = section

	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(s.Content) == "" {
		// 空正文 = 撤销。保持"map 里没有该键"的语义。
		delete(r.sections, section)
		return nil
	}
	r.sections[section] = s
	return nil
}

// Remove 撤销一段声明（等价于 Declare 传空 Content）。
//
// 单独提供是因为"撤销"在插件 Stop / 禁用时是常见动作，写成空串容易漏。
func (r *Registry) Remove(section string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sections, section)
}

// RemoveOwner 撤销某插件的**全部**声明（插件停用/卸载时调用）。
//
// ★ 为什么必须有：插件停用后它的说明还在提示词里，模型会去调一个
// 已不存在的工具 —— 这比"没有说明"更糟。单个 Remove 需要插件自己
// 记住声明过哪些段，插件崩了就没机会记住。
func (r *Registry) RemoveOwner(owner string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.sections {
		if v.Owner == owner {
			delete(r.sections, k)
		}
	}
}

// Snapshot 返回全部声明段的**副本**，按 Order 升序（同 Order 按 Section 字典序）。
//
// 与 Render 的分工：
//
//	Render   = 自己拼成最终文本（注册表独占的场景）
//	Snapshot = 交出各段，由调用方与**别的来源**合并（内核的当轮段）
//
// ★ 为什么必须能交出各段而不能只给拼好的字符串：内核自己也有十几块
// 系统提示词（人格卡、记忆说明、输出规则…），它们要能与插件段按 Order
// 交错排序。只给拼好的字符串，插件段就只能整块插在某个固定位置。
func (r *Registry) Snapshot() []Section {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	list := make([]Section, 0, len(r.sections))
	for _, s := range r.sections {
		list = append(list, s)
	}
	r.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool {
		if list[i].Order != list[j].Order {
			return list[i].Order < list[j].Order
		}
		return list[i].Section < list[j].Section
	})
	return list
}

// Render 渲染全部声明段，按 Order 升序（同 Order 按 Section 字典序）。
//
// 无声明时返回空串——调用点用 `if s != ""` 判断，不要自己判 nil。
func (r *Registry) Render() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	list := make([]Section, 0, len(r.sections))
	for _, s := range r.sections {
		list = append(list, s)
	}
	r.mu.RUnlock()

	if len(list) == 0 {
		return ""
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Order != list[j].Order {
			return list[i].Order < list[j].Order
		}
		return list[i].Section < list[j].Section
	})

	var b strings.Builder
	for i, s := range list {
		if i > 0 {
			_, _ = b.WriteString("\n\n")
		}
		_, _ = b.WriteString(s.Content)
	}
	return b.String()
}

// Names 返回全部段名（诊断/状态面用）。
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.sections))
	for k := range r.sections {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
