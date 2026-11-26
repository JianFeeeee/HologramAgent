package mainagent

// tools_register.go —— 把**工具**登记到 registry（用户 2026-11-18 质问的修法）。
//
// ## 用户原话
//
// > 「为什么还是有硬编码的 rt，现有的回调注册式接口还不够用吗？」
//
// 答：接口够用。之前只把「子 agent 类型」注册掉了，工具表/schema/归属/分派
// 仍然是手抄字面量 —— 加一类子 agent 要回来改四处。现在**谁拥有那个能力，
// 谁就注册那个工具**：
//
//	工具名 + schema + 侧别 + 归属 + 执行体   全在这一张表里
//
// 于是下面这些地方不再有 `rt` / `resident_agents` 的字面量：
//
//	tooldefs.go        用 registry.ToolDefsForSide(side) 渲染
//	toolcall.go        用 registry.DispatchTool(...) 分派
//	toolroute/dispatch 用 registry.DispatchTool(...) 分派
//	plugins.go         用 registry.ToolOwner(name) 查归属
//
// ## 为什么用 init()
//
// 与类型注册同一纪律：装配期一次、失败即计划错误。工具表是**静态事实**，
// 在 init 期定死才不会出现"某轮工具表里有时有、有时没有"（本仓栽过）。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

func init() {
	// ── 通道类工具（`OutputGate` 标记）──
	//
	// ★ 它们原先只有**声明**（基座 iotooldefs.go）与**执行体**（基座
	//   output.go / inputch.go），没有登记点 ⇒ 想"剥掉主动输出工具"的
	//   类型（rtchild / worker）只能靠名字前缀猜。
	//   登记进来的收益：`registry.Tool.OutputGate` 成为判据，
	//   `agent.IsOutputGateTool` 一处生效，改名不再静默失效。
	for _, td := range []struct {
		name   string
		desc   string
		props  map[string]interface{}
		reqs   []string
		output bool
	}{
		{name: "output_list_channels",
			desc:   "列出所有可用输出通道及其能力（如 text/file/image/audio）和对应的输出门工具名称。",
			output: true},
		{name: "input_channels",
			desc: "查看 inputch（最基本的输入路由单位）：哪些已注册、谁注册的、各自划给了哪个 agent、容量与记忆策略。单工具多视图。",
			props: map[string]interface{}{
				"view": map[string]interface{}{
					"type": "string",
					"description": "all=全部已注册（默认）| mine=划给本 agent 的 | " +
						"unassigned=尚未划出的 | by_agent=按归属分组的划分总览 | detail=单个详情",
					"enum": []string{"all", "mine", "unassigned", "by_agent", "detail"},
				},
				"name": map[string]interface{}{
					"type":        "string",
					"description": "view=detail 时必填：inputch 名",
				},
			}},
	} {
		def := td
		if err := registry.RegisterTool(registry.Tool{
			Name:        def.name,
			Description: def.desc,
			Side:        registry.SideAny,
			Owner:       "", // 空 = 内核（执行体在基座 output.go / inputch.go）
			Properties:  def.props,
			Required:    def.reqs,
			OutputGate:  def.output,
			// 执行体在基座（output.go / inputch.go）—— 这里只登记声明与标记。
			DeclaredOnly: true,
		}); err != nil {
			panic("agent/tools: " + err.Error())
		}
	}

	// ★ 各类型**没有**父侧工具了：
	//   工作式子走 admin_child(type="worker", action="create")，
	//   子侧上报走 CallParent —— 旧的 spawn_child / child_result /
	//   notify_parent / inputch_note 已删（用户裁定：父子只有两条协议）。
	//   基座这里只留内核统一入口（admin_child 三件套）。

}

// registerTool 是 registerTool 的简写包装（失败即装配错误）。
func registerTool(name, desc string, side registry.Side, props map[string]interface{},
	required []string, h registry.ToolHandler) {
	if err := registry.RegisterTool(registry.Tool{
		Name: name, Description: desc, Side: side, Owner: "core",
		Properties: props, Required: required, Handle: h,
	}); err != nil {
		panic("agent/tools: " + err.Error())
	}
}

// hostAgent 从工具上下文取出 *Agent。
//
// ★ 注册者用闭包捕获 a 也可以，但那样注册必须在每个实例上重做一次，
//
//	而工具表是**包级静态表**。所以执行体在调用时才从上下文取宿主。
func hostAgent(c registry.ToolCtx) *agent.Agent {
	a, _ := c.Host.(*agent.Agent)
	_ = a
	return a
}

// toolPluginOf 是 registry 归属的兜底实现（供 plugins.go 用）。
//
// 空归属 ⇒ 调用方继续走自己的前缀规则。
func toolPluginOf(name string) string {
	return registry.ToolOwner(name)
}

// sideOf 返回本 agent 的工具侧别（注册表按它取表）。
//
// ★ 这是唯一一处"基座把层级事实翻译成侧别"的地方，而且它读的是
//
//	`isChildAgent()`（层级数据事实），不是类型名。类型模块自己不再参与。
func (m *Module) sideOf() registry.Side {
	if m.a.IsChildAgent() {
		return registry.SideChild
	}
	return registry.SideParent
}
