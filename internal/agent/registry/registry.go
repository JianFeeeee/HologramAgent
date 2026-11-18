// Package registry 是**子 agent 类型的注册表 + 管理调用分发**（内核侧）。
//
// ## 用户裁定（2026-11-18）
//
// > 「注册子 agent 类型的接口只对内置插件暴露，内置插件通过 eventbus，
// >    向 agent 基座（或者说是 agent 管理器）发布行为动态注册撤销新的 agent 类型」
//
// > 「是内核提供一个全参数化的 adminchild（type，meta，action，arg）调用，
// >    各个子 agent 注册各自的管理行为，并和 outputch 设计一样有 help 调用
// >    与 skill 指导主 agent 如何使用」
//
// > 「创建子 agent 执行在主 agent？不应该在内核吗？」
//
// ## 归属划分（本包的存在理由）
//
//	内核（本包）    类型注册表 · 通用动作（list/create/destroy/restart/send）
//	                 · admin_child 三件套的工具表生成 · 撤销语义
//	agent 类型模块  自己的 Meta · 自己的**类型独有**动作（compress/transcript/…）
//	                 · Construct（怎么装配一个本类型实例）
//	主 agent        纯调用方：模型看到 admin_child → 内核执行 → 结果回给它
//
// 所以「创建」不在主 agent 里：主 agent 只发一个 `action=create` 的请求，
// 内核按 type 查 Descriptor、调它的 Construct、登记实例、绑定上行通路。
//
// ## 为什么 Descriptor 里带函数值（方案 B）
//
// 内置插件与内核**同地址空间**（线程化运行），所以注册事件可以携带真正的
// Go 函数值；跨进程插件（lua/proc）拿不到这条路径 —— 它们本来也没有资格
// 注册 agent 类型（见 security 的 EventPolicy：只有内置插件能发注册事件）。
package registry

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Meta 是一个子 agent 类型的**静态描述**（供工具表、状态面、_help 使用）。
type Meta struct {
	// Type 是类型名（唯一键，也是 admin_child 的 type 参数取值）。
	Type string
	// Description 是一句话说明（进 admin_child 的 type 枚举说明）。
	Description string
	// NeedsInputCh 声明该类型**必须**划入至少一个 inputch 才能创建。
	//
	// 为什么由类型声明而不是内核硬编码：rt 必须成对（inputch+outputch），
	// 驻留子至少要一个 inputch，工作式子两个都不要 —— 这是类型语义。
	NeedsInputCh bool
	// NeedsOutputCh 声明该类型**必须**成对给出 outputch（rt 的成对契约）。
	NeedsOutputCh bool
}

// Action 是**类型独有**的管理动作声明（生成工具 schema 与 _help 用）。
//
// ★ 通用动作（list/create/destroy/restart/send）**不在这里声明**：
//
//	它们由内核统一实现，任何类型都可用。类型只声明它自己额外能做的事。
type Action struct {
	Name        string
	Description string
	// Args 是该动作接受的参数（JSON Schema properties）。可为空。
	Args map[string]interface{}
	// Required 是必填参数名。
	Required []string
}

// Descriptor 是一个子 agent 类型的完整注册项。
type Descriptor struct {
	Meta
	Actions []Action

	// Construct 装配**本类型**的一个实例。
	//
	// 参数 host 是内核侧的实例管理器（分配 id、查重、登记、绑定上行通路
	// 都在它那）；arg 是调用方给的参数。返回给模型看的结果文本。
	//
	// ★ 内核不在这里替类型做事：它只负责"按 type 找到这个函数并调用"。
	Construct func(host Host, arg map[string]interface{}) (string, error)

	// Admin 执行**类型独有**动作（Actions 里声明的那些）。
	// 通用动作由内核直接处理，不会走到这里。
	Admin func(host Host, action string, arg map[string]interface{}) (string, error)

	// Help 返回本类型的 `_help` 正文（技术性细节收敛在这里与 skill）。
	//
	// ★ 内核不承载类型的语义文档：与 outputch 的 `_help` 同一纪律。
	Help func(host Host) string
}

// ChildBrief 是实例的**对外简报**（list 动作与状态面共用）。
type ChildBrief struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	State    string                 `json:"state"`
	InputChs []string               `json:"input_chs,omitempty"`
	Extra    map[string]interface{} `json:"extra,omitempty"`
}

// Host 是**内核侧实例管理器**向类型暴露的最小面。
//
// ★ 类型拿到的是这个接口，不是"父 agent" —— 因为创建/销毁/登记都属于
//
//	实例管理，不属于某个 agent。父 agent 只是调用方。
type Host interface {
	// ListChildren 列出当前存活实例（可按 type 过滤，空 = 全部）。
	ListChildren(typeName string) []ChildBrief
	// GetChild 取单个实例简报。
	GetChild(id string) (ChildBrief, bool)
	// DestroyChild 销毁实例（类型 Dispose 由登记表骨架调用）。
	DestroyChild(id string, residual string) (string, error)
	// SendToChild 向实例注入一条输入（走它自己的 inputch，复用现有注入原语）。
	SendToChild(id, text string) (string, error)
	// OwnerID 是宿主（调用方）的标识 —— 用于模板化错误文案与审计。
	OwnerID() string
	// DataDir 是宿主的工作目录（类型的 Construct 需要它派生 temp 路径等）。
	DataDir() string
}

// ───────────────────────── 类型注册表 ─────────────────────────

var (
	mu     sync.RWMutex
	byType = map[string]Descriptor{}
	order  []string
)

// Register 登记一个类型（内置插件经 EventBus 收到的注册请求最终走到这里）。
//
// 同名重复登记被拒：两个实现声称同一类型意味着"到底哪个生效"取决于
// 调用顺序 —— 那是不可判定的故障（与 agent 的类型注册表同一纪律）。
func Register(d Descriptor) error {
	if strings.TrimSpace(d.Type) == "" {
		return fmt.Errorf("registry: 类型名不能为空")
	}
	if d.Construct == nil {
		return fmt.Errorf("registry: 类型 %q 缺少 Construct（没有它就创建不出实例）", d.Type)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := byType[d.Type]; dup {
		return fmt.Errorf("registry: 类型 %q 已注册", d.Type)
	}
	byType[d.Type] = d
	order = append(order, d.Type)
	return nil
}

// Unregister 撤销一个类型。
//
// ★ 撤销仍有存活实例的类型会被**拒绝**（返回错误并列出 id）：
//
//	实例的 Admin/Dispose 都在该 Descriptor 里，撤掉后它们立刻变成悬空调用。
//	要强制回收必须显式两步：先 admin_child(type, "destroy_all")，再撤销。
func Unregister(typeName string, alive []string) error {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := byType[typeName]; !ok {
		return fmt.Errorf("registry: 类型 %q 未注册", typeName)
	}
	if len(alive) > 0 {
		return fmt.Errorf("registry: 类型 %q 仍有 %d 个存活实例（%s），必须先销毁再撤销",
			typeName, len(alive), strings.Join(alive, ", "))
	}
	delete(byType, typeName)
	for i, t := range order {
		if t == typeName {
			order = append(order[:i], order[i+1:]...)
			break
		}
	}
	return nil
}

// Lookup 按类型名取 Descriptor。
func Lookup(typeName string) (Descriptor, bool) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := byType[typeName]
	return d, ok
}

// All 返回全部 Descriptor（按注册顺序，稳定输出）。
func All() []Descriptor {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Descriptor, 0, len(byType))
	for _, t := range order {
		if d, ok := byType[t]; ok {
			out = append(out, d)
		}
	}
	return out
}

// Types 返回全部已注册类型名。
func Types() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(order))
	out = append(out, order...)
	sort.Strings(out)
	return out
}

// ───────────────────────── 管理调用分发 ─────────────────────────

// 通用动作名（内核统一实现，任何类型都可用）。
const (
	ActionList       = "list"
	ActionCreate     = "create"
	ActionDestroy    = "destroy"
	ActionDestroyAll = "destroy_all"
	ActionRestart    = "restart"
	ActionSend       = "send"
	ActionInspect    = "inspect"
)

// genericActions 是内核统一实现的动作集（不进类型的 Actions 声明）。
var genericActions = []Action{
	{Name: ActionList, Description: "列出该类型的存活实例"},
	{Name: ActionCreate, Description: "创建一个实例（参数由该类型定义，见 _help）"},
	{Name: ActionInspect, Description: "查看单个实例的简报（不打断它）", Required: []string{"id"},
		Args: map[string]interface{}{"id": map[string]interface{}{"type": "string", "description": "实例 id"}}},
	{Name: ActionSend, Description: "向实例注入一条输入（对子而言是中断，取消其当前处理）", Required: []string{"id", "text"},
		Args: map[string]interface{}{
			"id":   map[string]interface{}{"type": "string", "description": "实例 id"},
			"text": map[string]interface{}{"type": "string", "description": "要注入的内容"},
		}},
	{Name: ActionDestroy, Description: "销毁单个实例", Required: []string{"id"},
		Args: map[string]interface{}{
			"id":       map[string]interface{}{"type": "string", "description": "实例 id"},
			"residual": map[string]interface{}{"type": "string", "enum": []string{"keep", "drop"}, "description": "未处理的残余任务处置：keep=转回你自己的队列（默认）/ drop=明确丢弃"},
		}},
	{Name: ActionDestroyAll, Description: "销毁该类型的全部实例（撤销类型前必须先做）"},
	{Name: ActionRestart, Description: "重启单个实例（停掉并重建，用于强行停止跑飞的实例）", Required: []string{"id"},
		Args: map[string]interface{}{"id": map[string]interface{}{"type": "string", "description": "实例 id"}}},
}

// GenericActions 返回内核统一动作集（工具表生成用）。
func GenericActions() []Action { return genericActions }

// Dispatch 执行一次管理调用。
//
// 通用动作在这里实现；类型独有动作转发给 Descriptor.Admin。
// 未知动作**响亮报错**并列出可用动作（不静默降级）。
func Dispatch(host Host, typeName, action string, arg map[string]interface{}) (string, error) {
	if host == nil {
		return "", fmt.Errorf("registry: 实例管理器未就绪")
	}
	action = strings.TrimSpace(action)
	if action == "" {
		action = ActionList
	}

	// list / destroy_all 可以不带 type（作用于全部已注册类型）。
	if typeName == "" {
		switch action {
		case ActionList:
			return listAll(host), nil
		case ActionDestroyAll:
			return destroyAll(host, ""), nil
		}
		return "", fmt.Errorf("registry: 动作 %q 需要指定 type（可用类型: %v）", action, Types())
	}

	d, ok := Lookup(typeName)
	if !ok {
		return "", fmt.Errorf("registry: 类型 %q 未注册（可用类型: %v）", typeName, Types())
	}

	switch action {
	case ActionList:
		return listOf(host, typeName), nil
	case ActionCreate:
		return d.Construct(host, arg)
	case ActionInspect:
		id := strOf(arg, "id")
		if id == "" {
			return "", fmt.Errorf("registry: inspect 需要 id")
		}
		b, ok := host.GetChild(id)
		if !ok {
			return "", fmt.Errorf("registry: 实例 %q 不存在", id)
		}
		return fmt.Sprintf("%s [%s] state=%s inputch=%v", b.ID, b.Type, b.State, b.InputChs), nil
	case ActionSend:
		id := strOf(arg, "id")
		text := strOf(arg, "text")
		if id == "" || text == "" {
			return "", fmt.Errorf("registry: send 需要 id 与 text")
		}
		return host.SendToChild(id, text)
	case ActionDestroy:
		id := strOf(arg, "id")
		if id == "" {
			return "", fmt.Errorf("registry: destroy 需要 id")
		}
		return host.DestroyChild(id, strOf(arg, "residual"))
	case ActionDestroyAll:
		return destroyAll(host, typeName), nil
	case ActionRestart:
		id := strOf(arg, "id")
		if id == "" {
			return "", fmt.Errorf("registry: restart 需要 id")
		}
		return restartChild(host, d, id)
	}

	// 类型独有动作。
	for _, a := range d.Actions {
		if a.Name == action {
			if d.Admin == nil {
				return "", fmt.Errorf("registry: 类型 %q 声明了动作 %q 但没有 Admin 实现", typeName, action)
			}
			return d.Admin(host, action, arg)
		}
	}
	return "", fmt.Errorf("registry: 类型 %q 没有动作 %q（可用: %v）",
		typeName, action, actionNames(d))
}

// restartChild 是**软重启**（用户裁定 2026-11-18：只重启这一个实例）。
//
// 语义：销毁该实例（不带残余转回，因为它可能已经跑飞）→ 用**同类型同参数**
// 重建。参数由 Host 保存（创建时记录），所以调用方只需要给 id。
func restartChild(host Host, d Descriptor, id string) (string, error) {
	if _, ok := host.GetChild(id); !ok {
		return "", fmt.Errorf("registry: 实例 %q 不存在", id)
	}
	if _, err := host.DestroyChild(id, "drop"); err != nil {
		return "", fmt.Errorf("registry: 重启 %q 失败于销毁阶段: %w", id, err)
	}
	if c, ok := host.(RestartHost); ok {
		return c.RecreateChild(d.Type, id)
	}
	return "", fmt.Errorf("registry: 实例管理器不支持重建（无法完成软重启）")
}

// RestartHost 是支持**软重启**的实例管理器（可选能力）。
//
// ★ 为什么拆成可选接口：不是所有宿主都能保存"创建参数"。做不到时
//
//	`restart` 必须**响亮失败**，而不是假装成功（那会让用户以为跑飞的
//	实例已经被停掉）。
type RestartHost interface {
	RecreateChild(typeName, id string) (string, error)
}

func listAll(host Host) string {
	var b strings.Builder
	types := Types()
	if len(types) == 0 {
		return "没有已注册的子 agent 类型。"
	}
	fmt.Fprintf(&b, "已注册子 agent 类型（%d 个）:", len(types))
	for _, t := range types {
		d, _ := Lookup(t)
		briefs := host.ListChildren(t)
		fmt.Fprintf(&b, "\n  - %s（%s）: %d 个实例", t, d.Description, len(briefs))
		for _, x := range briefs {
			fmt.Fprintf(&b, "\n      · %s [%s] inputch=%v", x.ID, x.State, x.InputChs)
		}
	}
	return b.String()
}

func listOf(host Host, typeName string) string {
	briefs := host.ListChildren(typeName)
	if len(briefs) == 0 {
		return fmt.Sprintf("当前没有 %s 类型的子 agent。", typeName)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s 实例（%d 个）:", typeName, len(briefs))
	for _, x := range briefs {
		fmt.Fprintf(&b, "\n  - %s [%s] inputch=%v", x.ID, x.State, x.InputChs)
	}
	return b.String()
}

func destroyAll(host Host, typeName string) string {
	briefs := host.ListChildren(typeName)
	if len(briefs) == 0 {
		return "没有可销毁的实例。"
	}
	var ok, failed int
	var errs []string
	for _, x := range briefs {
		if _, err := host.DestroyChild(x.ID, "drop"); err != nil {
			failed++
			errs = append(errs, fmt.Sprintf("%s: %v", x.ID, err))
			continue
		}
		ok++
	}
	if failed > 0 {
		return fmt.Sprintf("已销毁 %d 个，失败 %d 个：%s", ok, failed, strings.Join(errs, "; "))
	}
	return fmt.Sprintf("已销毁 %d 个实例。", ok)
}

func actionNames(d Descriptor) []string {
	out := make([]string, 0, len(genericActions)+len(d.Actions))
	for _, a := range genericActions {
		out = append(out, a.Name)
	}
	for _, a := range d.Actions {
		out = append(out, a.Name)
	}
	return out
}

func strOf(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return strings.TrimSpace(s)
}
