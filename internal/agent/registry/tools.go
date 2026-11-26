package registry

// tools.go —— **工具注册表**（与类型注册表并列的第二张表）。
//
// ## 为什么需要它（用户 2026-11-18 质问）
//
// > 「为什么还是有硬编码的 rt，现有的回调注册式接口还不够用吗？」
//
// 答：接口够用，是我**没把它用在这些地方**。上一轮只把「子 agent 类型」
// 注册掉了，工具名/schema/归属/分派仍然是手抄字面量，于是残留：
//
//	tooldefs.go      spawn_child / child_result / notify_parent / inputch_note
//	toolcall.go      同名四个分支
//	toolroute/dispatch.go   resident_agents / notify_parent / inputch_note
//	plugins.go       rt_transcript / rt_children / resident_agents / …
//
// 每一份都是「再打一遍类型名」，加一类子 agent 就要回来改四处。这正是
// 本设计要消灭的嵌合：**谁拥有那个能力，谁就注册那个工具**。
//
// ## 三张表的分工
//
//	类型注册表（registry.go）      type → Descriptor（Construct/Admin/Help）
//	工具注册表（本文件）            工具名 → Tool（schema + 侧别 + 归属 + handler）
//	实例管理器（agent/instances）   id → 存活实例
//
// 工具表是**数据 + 一个函数值**，因此：
//
//	· tooldefs 不再写 if，而是 `ToolDefsFor(side)` 拿表渲染
//	· toolroute 不再写 case，而是 `DispatchTool(...)` 先问表
//	· plugins.go 的归属不再写 string，而是 `ToolOwner(name)`
//
// ## 侧别（Side）为什么是工具属性
//
// 同一个类型在父侧贡献工具（admin_child 的入口、spawn_child），在子侧
// 贡献另一些（notify_parent、inputch_note）。若把"谁能看到什么工具"
// 写成基座里的 `if isChildAgent()`，基座又认识了类型语义。
// 侧别是**注册时声明的事实**，基座只做「按我的侧别取表」。

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// Side 是工具在**哪一侧**可见。
type Side int

const (
	// SideAny 两侧都可见（内核通用工具）。
	SideAny Side = iota
	// SideParent 只在父侧可见（管理子 agent 的入口）。
	SideParent
	// SideChild 只在子侧可见（向上汇报、写处理表）。
	SideChild
)

// ToolCtx 是工具执行时随调用一起传下来的上下文。
//
// ★ 为什么不是 (host, channel, turnScenes) 三个裸参数：三个裸参数在不同
//
//	工具间用法不同（只有 spawn_child 用 channel），位置参数会让"第几个
//	是什么"变成隐性约定。结构体让未用字段显式为 nil/空。
type ToolCtx struct {
	// Host 是执行宿主。由**注册者与调用方**约定具体类型（registry 不解释）：
	// 内置工具注册者拿到的是 `*agent.Agent`。
	Host any
	// Channel 是本次工具调用的输出通道名（spawn_child 需要它授权子）。
	Channel string
	// TurnScenes 是本轮场景标签（记忆族工具用，其余忽略）。
	TurnScenes []string
}

// ToolHandler 执行一个工具，返回给模型看的文本。
type ToolHandler func(c ToolCtx, tc api.ToolCall) string

// Tool 是工具注册表的一项。
type Tool struct {
	// Name 是工具名（对模型可见，也是分派键）。
	Name string
	// Description 是给模型的说明（**一句话**；技术细节进 `_help` 或 skill）。
	Description string
	// Side 声明该工具在哪一侧可见（默认 SideAny）。
	Side Side
	// Owner 是归属的插件名（内置工具一律 "core"；插件工具由插件注册）。
	Owner string
	// Properties 是 JSON Schema properties。
	Properties map[string]interface{}
	// Required 是必填参数名。
	Required []string
	// Parallel 声明该工具可被并发执行（已核实只读、无共享写）。
	Parallel bool
	// NoMemory 声明该工具的输出不参与记忆计算。
	NoMemory bool
	// Timeout 声明单次执行的最长时长（如 "3m"；留空 = 内核默认）。
	Timeout string
	// Handle 是执行体。
	Handle ToolHandler
	// Slot 声明这是**通用扩展槽**交出的工具（docs §6）。
	//
	// ★ 它决定「谁执行」：槽工具的钩子在注册时已交出（`Handle`），
	//   执行一律走**内核执行设施** —— 与插件工具完全同一条链。
	//   归属（`Owner`）只用于溯源与"创建参数划定暴露范围"，**不**表示
	//   "由主 agent 的 agentloop 执行"。
	//
	//   缺这个标记的后果（实测）：主 agent 的 `ownsTool` 按 Owner 命中
	//   自己 ⇒ 判成"类型私有" ⇒ 落进私有族分岔 ⇒ 那里没有槽工具的分支
	//   ⇒ 报「归属声明缺失（DeclareOwnership 未登记）」，槽工具永远执行不了。
	Slot bool
	// OwnershipOnly 声明"**只登记归属，声明不在本表里**"。
	//
	// 与 DeclaredOnly 是两件事，别混：
	//
	//	DeclaredOnly   声明**在**表里（Description/Properties 有值），执行体在别处
	//	OwnershipOnly  声明**不在**表里（由拥有者自己的渲染面产出），
	//	               本项只让 `OwnerOf(name)` 查得到归属
	//
	// 用途：`llm_*` / `secret_*` / `plgreload` / `get_plugin_tools` 的 schema
	// 由**类型**渲染（它知道 `core.llm.*` 的配置与门控），而执行体在基座。
	// 把它们的 schema 也搬进表里 ⇒ 两个渲染源 ⇒ 同一工具名出现两次 ⇒
	// 上游硬拒绝（`Tool names must be unique`）。实测发生过。
	OwnershipOnly bool
	// DeclaredOnly 声明"只登记声明，执行体在基座的设施里"。
	//
	// 用于通道类工具（output_list_channels / input_channels）：它们的执行在
	// output.go / inputch.go，属内核设施。登记它们只为**归属与标记可见**，
	// 不为执行 —— 强制要求 Handle 会逼出一个假的执行体。
	DeclaredOnly bool
	// OutputGate 声明本工具属于**主动输出门**（output_send__* / output_list_channels 这类
	// 会让模型显式点名通道的工具）。
	//
	// ★ 为什么是一个显式标记而不是"看名字前缀"：rtchild 的输出走 outputch 的流式管线，
	//   与"模型显式点名通道"是两套语义，两套同时存在会分叉。原先靠
	//   `strings.HasPrefix(name, "output_send__")` 判 —— 改名字就静默失效。
	//   标记由拥有者在自己登记时声明，判据读标记，不读名字。
	OutputGate bool
}

var (
	toolMu    sync.RWMutex
	toolTable = map[string]Tool{}
)

// RegisterTool 登记一个工具（装配期一次，通常在包 init 里）。
//
// 同名重复登记被拒：两个模块声称同一工具名意味着"到底哪个生效"取决于
// 调用顺序 —— 与类型注册表同一纪律。
func RegisterTool(t Tool) error {
	if strings.TrimSpace(t.Name) == "" {
		return errf("registry: 工具名不能为空")
	}
	// Handle 可为 nil 仅当登记的是**纯声明**工具（执行体在基座，
	// 如 output_list_channels / input_channels —— 它们的执行在 output.go /
	// inputch.go，属内核设施，不该为一个名字再造一份执行体）。
	if t.Handle == nil && !t.OutputGate && !t.DeclaredOnly && !t.OwnershipOnly {
		return errf("registry: 工具 %q 缺少 Handle", t.Name)
	}
	if t.Owner == "" {
		t.Owner = "core"
	}
	toolMu.Lock()
	defer toolMu.Unlock()
	if _, dup := toolTable[t.Name]; dup {
		return errf("registry: 工具 %q 已注册", t.Name)
	}
	toolTable[t.Name] = t
	return nil
}

// RegisterToolReplace 与 RegisterTool 相同，但**同名覆盖**。
//
// ★ 只给"槽内工具"用：多个主 agent 实例交的是同一能力（`graph_recall`
//
//	就���主 agent 的图记忆召回），后注册者赢是唯一合理语义。
//	普通工具（插件 / 内核族）仍走 RegisterTool：同名重复是计划错误。
func RegisterToolReplace(t Tool) error {
	if strings.TrimSpace(t.Name) == "" {
		return errf("registry: 工具名不能为空")
	}
	if t.Handle == nil && !t.OutputGate && !t.DeclaredOnly && !t.OwnershipOnly {
		return errf("registry: 工具 %q 缺少 Handle", t.Name)
	}
	if t.Owner == "" {
		t.Owner = "core"
	}
	toolMu.Lock()
	defer toolMu.Unlock()
	toolTable[t.Name] = t
	return nil
}

// LookupTool 按名字取工具项。
func LookupTool(name string) (Tool, bool) {
	toolMu.RLock()
	defer toolMu.RUnlock()
	t, ok := toolTable[name]
	return t, ok
}

// ToolsFor 返回某一侧可见的全部工具（按名字排序，输出稳定）。
//
// side=SideAny 的过滤语义：**父侧看得到 SideAny+SideParent**，
// **子侧看得到 SideAny+SideChild**。返回值不含"对方那一侧"的工具。
func ToolsFor(side Side) []Tool {
	toolMu.RLock()
	defer toolMu.RUnlock()
	out := make([]Tool, 0, len(toolTable))
	for _, t := range toolTable {
		switch t.Side {
		case SideAny:
			out = append(out, t)
		default:
			if t.Side == side {
				out = append(out, t)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ToolOwner 返回工具的归属（未注册 ⇒ 空，调用方决定兜底）。
func ToolOwner(name string) string {
	toolMu.RLock()
	defer toolMu.RUnlock()
	if t, ok := toolTable[name]; ok {
		return t.Owner
	}
	return ""
}

// DispatchTool 尝试用注册表执行一个工具。
//
// 返回 (结果, 是否已处理)。handled=false 表示"不是注册表里的工具"，
// 调用方必须继续走自己的路径 —— **绝不吞掉未知工具**（插件工具会消失）。
func DispatchTool(c ToolCtx, tc api.ToolCall) (string, bool) {
	t, ok := LookupTool(tc.Name)
	if !ok {
		return "", false
	}
	// ★ `DeclaredOnly` 的工具**只登记声明**（执行体在别处，如基座的
	//   output.go / inputch.go）。这里必须返回 `handled=false` 让调用方
	//   继续往下路由 —— 直接调 nil Handle 会 panic，而"panic 在被 recover
	//   之后变成一条莫名的工具失败"是最难查的形态。
	if t.Handle == nil {
		return "", false
	}
	return t.Handle(c, tc), true
}

// ToolNames 返回全部已注册工具名（判据与状态面用）。
func ToolNames() []string {
	toolMu.RLock()
	defer toolMu.RUnlock()
	out := make([]string, 0, len(toolTable))
	for n := range toolTable {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// errf 是本文件内部用的错误构造（避免为了一个 fmt.Errorf 反复 import）。
func errf(format string, args ...any) error { return fmt.Errorf(format, args...) }

// ToolDef 是**工具声明的纯数据形态**（生成 schema 用，不含执行体）。
//
// ★ 与 Tool 的分工：Tool 是注册表项（含 Handle，执行侧用）；
// ToolDef 是"要发给模型的那份声明"（渲染侧用）。分开是因为有一批工具
// 的声明由**表生成**（admin_child 三件套随类型增删而变），它们不该
// 被当成静态注册项 —— 否则类型撤销后那条声明还挂着。
type ToolDef struct {
	Name        string
	Description string
	Properties  map[string]interface{}
	Required    []string
	Parallel    bool
	NoMemory    bool
	Timeout     string
}

// ───────────────────────── 归属声明 ─────────────────────────
//
// 私有工具的 **schema 由类型渲染、执行体在类型里**，但它们仍然是"谁拥有的"
// 这条事实的一部分 —— 而分岔判据（`ownsTool`）只能靠这条事实。
//
// ★ 为什么不进 toolTable：`Tool` 要求 `Handle`，而私有工具的执行体签名是
//   `(*类型, ToolCall) string`（它读类型的私有状态），注册表的 `ToolCtx.Host`
//   拿不到那个形状。把 Handle 造出来只会得到一层假适配。
//
// 所以分成两张表：
//
//	toolTable     名字 → Tool（含执行体）      内核可执行的工具
//	ownerTable    名字/前缀 → Owner            谁拥有它（含不可由内核执行的）

// 归属声明表：精确名 → Owner。
var (
	ownMu    sync.RWMutex
	ownerTab = map[string]string{}
)

// DeclareOwnership 声明一个**精确工具名**的归属（不含执行体）。
//
// 用于"schema 与执行体都在类型里、但基座需要知道它归谁"的工具。
// 与 RegisterTool 的 `Owner` 字段是两个入口、同一张事实：
// 内核可执行的工具走 RegisterTool，类型私有的走这里。
func DeclareOwnership(name, owner string) error {
	if strings.TrimSpace(name) == "" {
		return errf("registry: 归属声明的工具名不能为空")
	}
	if strings.TrimSpace(owner) == "" {
		return errf("registry: 归属声明的 owner 不能为空")
	}
	ownMu.Lock()
	defer ownMu.Unlock()
	if prev, dup := ownerTab[name]; dup && prev != owner {
		// 两个类型声称同一个工具名：与类型注册表同一纪律，响亮失败。
		return errf("registry: 工具 %q 的归属已被 %q 声明", name, prev)
	}
	ownerTab[name] = owner
	return nil
}

// OwnerOf 返回某工具的归属（先查归属声明表，再查工具表）。
//
// 空表示"没有归属声明" ⇒ 调用方按"不是我该执行的"处理（交给内核）。
func OwnerOf(name string) string {
	ownMu.RLock()
	if o := ownerTab[name]; o != "" {
		ownMu.RUnlock()
		return o
	}
	ownMu.RUnlock()
	toolMu.RLock()
	defer toolMu.RUnlock()
	if tool, ok := toolTable[name]; ok {
		return tool.Owner
	}
	return ""
}

// HasOwnership 报告某工具是否已声明归属（诊断用）。
func HasOwnership(name string) bool { return OwnerOf(name) != "" }
