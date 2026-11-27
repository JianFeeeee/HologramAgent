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

	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
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
	// MayOffload 声明该类型**可以**为转投拉起新实例。
	//
	// 只给主 agent（文档 §8.2）：让子再去拉孙子会形成无界增殖。
	MayOffload bool
	// AgentCap 是本类型**能承接的通道能力位**（能力面，文档 §8.1.1）。
	//
	// ★ 它是**数据声明**，不是行为：内核与基座只做位掩码比较，
	//   不认识任何一个具体能力名（`rt` 只是 io 包里的一个位）。
	//   加第三种能力（音频 / 图像 / 结构化）时，本字段与基座代码都不用改。
	AgentCap io.OutputCapability
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
	// ⚠️ **不要直接调它** —— 用 `Instantiate(d, host, arg)`：后者在它之后
	//   统一跑 `Initialize` 并负责回滚。直接调 Construct 会跳过 Initialize，
	//   症状是"实例建好了但调度器/上下文全没装"（四个类型各自踩过一次）。
	//
	// ★ 为什么私有（2026-11-25）：装配有两个阶段——"把实例造出来"与
	// "把它的运行时服务装进去"。此前**每个类型自己决定怎么衔接**：
	//
	//	主 agent    在 `New()` 里自己调 `Runtime.Initialize`
	//	resident    在自己的 Construct 里 `registry.Lookup(kind)` 再调 `.Initialize`
	//	rtchild     同上
	//	worker      根本不调（没有 Initialize 字段）
	//	插件类型    由内核 `kernelmod/agenttype` 调
	//
	// 于是"所有 agent 对内核一致"只在 construct 这一层成立，Initialize
	// 这一层完全不统一 —— 类型为了让自己能工作，必须知道"注册表怎么查"，
	// 这是倒挂。收成一个入口之后，两阶段都由 registry 保证。
	//
	// 参数 host 是内核侧的实例管理器（分配 id、查重、登记、绑定上行通路
	// 都在它那）；arg 是调用方给的参数。返回给模型看的结果文本。
	//
	// ★ 内核不在这里替类型做事：它只负责"按 type 找到这个函数并调用"。
	Construct func(host Host, arg map[string]interface{}) (string, error)

	// Initialize 装配**本类型实例**的运行时服务（基座不替任何类型决定它需要什么）。
	//
	// ## 为什么签名是 any 而不是具体 agent 类型
	//
	// registry 是纯注册表，**不 import agent 包**（那会让注册表变成基座的一部分）。
	// 所以实例以 `any` 传入，由**类型模块自己断言**回它的载体类型 ——
	// 与基座的 `impl any` 同一纪律：契约边界上不出现具体实现类型。
	//
	// ## 为什么它与 Construct 分开
	//
	// Construct 回答"怎么把一个实例装配出来"（父的决定 + 通道划拨 + 登记），
	// Initialize 回答"这个实例跑起来需要哪些服务"。两者失败点不同、时机不同：
	// Construct 失败 ⇒ 实例没建起来；Initialize 失败 ⇒ 实例建起来了但跑不了，
	// 必须回滚掉（否则留下一个静默待机的僵尸）。
	Initialize func(instance any) error

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
	// DestroyChild 销毁实例。residual / channels 是**销毁时的声明**
	// （残余任务处置 / 通道去向），由基座执行，类型只声明。
	DestroyChild(id string, residual string, channels string) (string, error)
	// SendToChild 向实例注入一条输入（走它自己的 inputch，复用现有注入原语）。
	SendToChild(id, text string) (string, error)
	// OwnerID 是宿主（调用方）的标识 —— 用于模板化错误文案与审计。
	OwnerID() string
	// DataDir 是宿主的工作目录（类型的 Construct 需要它派生 temp 路径等）。
	DataDir() string
	// WorkDirOf 返回某个实例的**工作数据目录**（内核派生，类型只消费）。
	//
	// ★ 类型**不许**自己拼 `<data>/residents/<id>/graph.db` 这类路径：
	//   目录是内核统一的资源分配（一条记录 = 一个实例 = 一个目录），
	//   改布局只改内核一处，加类型不必再发明一套目录规则。
	WorkDirOf(typeName, id string) string
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
	// Construct 是**唯一**的实例化能力：主 agent 与子 agent 都经它
	// （文档 §7.1「一张表管五件事」，主 agent 不是特例）。
	if d.Construct == nil {
		return fmt.Errorf("registry: 类型 %q 缺少装配能力（没有它就创建不出实例）", d.Type)
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

// Instantiate 是**造实例的唯一入口**（两阶段一起做完）。
//
//	① construct   把实例装配出来（父的决定 + 通道划拨 + 登记）
//	② Initialize  把它的运行时服务装进去（失败必须回滚，否则留下僵尸）
//
// ★ 为什么必须由 registry 保证顺序（而不是各类型自己调）：
// Initialize 失败时实例已经建好（可能已 children.Add、已 Assign inputch），
// 回滚的责任在**谁造的**。此前四种类型各自决定去哪一步、失败怎么收，
// 于是"Initialize 失败留僵尸"在一条路径上被修过、另一条没修。
//
// ★ 主 agent 不是特例：它也走这里（`Construct` 里交回根实例，Initialize
// 照常对它跑）。「所有 agent 对内核一致」于是真正落在代码上。
func Instantiate(d Descriptor, host Host, arg map[string]interface{}) (string, func(), error) {
	if d.Construct == nil {
		return "", nil, fmt.Errorf("registry: 类型 %q 没有装配能力", d.Type)
	}
	out, err := d.Construct(host, arg)
	if err != nil {
		return "", nil, err
	}
	// ★ 实例参数是 `any`：注册表不 import agent 包（那会让注册表变成
	//   基座的一部分）。由宿主经 `NewlyCreated` 交出刚造的那个。
	instance := NewlyCreated(host)
	if instance == nil {
		// 没有"刚造的实例"可取 ⇒ 本宿主不支持两阶段（裁剪版内核 / 判据）。
		// 明确跳过，不猜。
		return out, nil, nil
	}
	if d.Initialize == nil {
		return out, nil, nil
	}
	if err := d.Initialize(instance); err != nil {
		// ★ 回滚责任在造的人：调用方拿不到实例（构造失败语义一致）。
		if r, ok := host.(RollbackHost); ok {
			r.RollbackConstruct(d.Type, strOf(arg, "id"))
		}
		return "", nil, fmt.Errorf("registry: 类型 %q 的 Initialize 失败（实例已回滚）: %w", d.Type, err)
	}
	// ★ 这里**只做装配，不做启动**（2026-11-25）。
	//
	// `Start` 需要一个前置：**面已挂好**（IO / 阶段管道 / 安全闸）。
	// 面由装配点在实例建好后挂（`MountSurface`），所以启动必须排在它之后，
	// 不能塞进 Instantiate —— 否则运行期一起来就撞 nil IO。
	//
	// 启动由内核在**面挂完之后**调（`StartInstance`）；本函数返回的实例
	// 由调用方决定何时起（装配点/实例管理器各在自己的时机调）。
	return out, func() { /* 已成功，无回滚动作 */ }, nil
}

// StartInstance 由**内核**在"面已挂好"之后调用（docs §10）。
//
// 顺序不可换：`Instantiate`（construct + Initialize）→ 装配点挂面
// （`MountSurface` 等）→ 本函数。把 Start 塞进 Instantiate 会让 IO/
// StageHost 还是 nil 时运行期就起来了（实测 panic）。
func StartInstance(instance any) {
	if st, ok := instance.(Starter); ok && st != nil {
		st.Start()
	}
}

// Starter 是"能起自己运行期"的实例（*agent.Agent 满足它）。
//
// ★ 注册表不 import agent 包，所以经这个窄接口调用。
type Starter interface {
	Start()
}

// NewlyCreated 由宿主实现：返回**刚刚由 construct 造出来的**实例（any）。
//
// ★ 为什么是独立接口而不是塞进 Host：只有需要两阶段装配的宿主才实现它
//
//	（实例管理器有"刚造那个"的概念；判据里的假宿主没有）。
type NewlyCreatedHost interface {
	NewlyCreated() any
}

// NewlyCreated 取宿主刚造出来的实例（未支持 ⇒ nil）。
func NewlyCreated(host Host) any {
	if h, ok := host.(NewlyCreatedHost); ok {
		return h.NewlyCreated()
	}
	return nil
}

// RollbackHost 是支持**回滚已构造实例**的宿主（可变可选能力）。
type RollbackHost interface {
	RollbackConstruct(typeName, id string)
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
			"residual": map[string]interface{}{"type": "string", "enum": []string{"keep", "stash", "drop"}, "description": "未处理的残余任务处置：keep=投回你的队列（默认，父接手）/ stash=退回原通道暂存（接回该通道即释放）/ drop=明确丢弃（逐条留痕，异步通道用户收不到回复）"},
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
		// ★ 工作目录由**内核**在建实例之前分配好（用户裁定 2026-11-19）：
		//   「内核持有的 agent 表里就有 agent 的工作数据目录」。
		//   调用方给的 work_dir 一律覆盖 —— 目录不是调用方的参数。
		if wd := host.WorkDirOf(typeName, strOf(arg, "id")); wd != "" {
			arg[ArgWorkDir] = wd
		}
		// ★ 统一两阶段（construct → Initialize）。此前这里直接调 `d.Construct`，
		//   于是 `admin_child(create)` 造出来的实例**没有 Initialize** ——
		//   插件的运行期钩子没挂、Loop 永远不跑，而创建动作返回成功。
		out, _, err := Instantiate(d, host, arg)
		if err != nil {
			return "", err
		}
		// ★ 面由 construct 挂好 ⇒ 现在起运行期（顺序见 StartInstance 注释）。
		if im, ok := host.(NewlyCreatedHost); ok {
			StartInstance(im.NewlyCreated())
		}
		return out, nil
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
		return host.DestroyChild(id, strOf(arg, "residual"), strOf(arg, "channels"))
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
// restartChild 完成一次**软重启**（停掉某实例并用它的创建参数重建）。
//
// ★ 整个动作委托给宿主的 `RecreateChild`，**不在本包拆成“先销毁再重建”**
//
//	（2026-11-26 实测缺陷）。
//
// 原先这里的分步写法：
//
//	DestroyChild(id, "drop")   → 末尾 Forget(id) —— 创建参数快照被删
//	RecreateChild(type, id)    → 读快照 —— 已经不在了
//	⇒ 「实例 %q 没有创建参数快照（无法重建）」
//
// 于是**软重启永远不可能成功**，而报错指向“没有快照”，看起来像记录没写。
//
// 重启是一个原子语义（“换上新的同参实例”），快照的取用归宿主：
// 它才知道自己的销毁链会清理什么。
func restartChild(host Host, d Descriptor, id string) (string, error) {
	if _, ok := host.GetChild(id); !ok {
		return "", fmt.Errorf("registry: 实例 %q 不存在", id)
	}
	c, ok := host.(RestartHost)
	if !ok {
		return "", fmt.Errorf("registry: 实例管理器不支持重建（无法完成软重启）")
	}
	return c.RecreateChild(d.Type, id)
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
		if _, err := host.DestroyChild(x.ID, "drop", "release"); err != nil {
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
