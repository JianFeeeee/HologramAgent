package agent

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
// 于是下面这些地方不再有 `rt` / `resident_agents` / `spawn_child` 的字面量：
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
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

func init() {
	// ── 父侧：子 agent 管理面（内核统一入口）──
	//
	// ★ 三个工具名固定、type 走参数：类型是运行期可增删的（内置插件经
	//   EventBus 注册/撤销），工具名固定模型才不必每次重学一套表。
	for _, td := range registry.ChildAdminTools() {
		def := td
		if err := registry.RegisterTool(registry.Tool{
			Name:        def.Name,
			Description: def.Description,
			Side:        registry.SideParent,
			Owner:       "core",
			Properties:  def.Properties,
			Required:    def.Required,
			Parallel:    def.Parallel,
			NoMemory:    def.NoMemory,
			Timeout:     def.Timeout,
			Handle: func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
				a := hostAgent(c)
				if a == nil {
					return "内核装配错误：工具上下文里没有 agent"
				}
				return a.runChildAdminTool(def.Name, tc)
			},
		}); err != nil {
			panic("agent/tools: " + err.Error())
		}
	}

	// ── 父侧：工作式子（异步子 agent）──
	//
	// ★ 工作式子不进 children 登记表（它没有 io/通道，结果是幂等缓存），
	//   所以它不是 admin_child 管的类型；两个工具自成一组。
	registerTool("spawn_child",
		"启动一个异步子 Agent 执行独立任务，不阻塞当前对话。完成后系统自动通知你。"+
			"多个互不依赖的子任务可在同一轮里一次 spawn 多个。",
		registry.SideParent,
		map[string]interface{}{
			"task": map[string]interface{}{
				"type": "string", "description": "要子 Agent 完成的任务描述（要完整、含必要背景）",
			},
			"max_turns": map[string]interface{}{
				"type": "integer", "description": "子 Agent 最大工具轮数（默认 5，范围 1-30）",
			},
		}, []string{"task"},
		func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
			a := hostAgent(c)
			if a == nil {
				return "内核装配错误：工具上下文里没有 agent"
			}
			return spawnChildFn(a, tc, c.Channel)
		})

	registerTool("child_result",
		"查询异步子 Agent 的执行结果（收到完成通知后调用，只需查询一次）。",
		registry.SideParent,
		map[string]interface{}{
			"task_id": map[string]interface{}{
				"type": "string", "description": "spawn_child 返回的任务 ID，如 child_1",
			},
		}, []string{"task_id"},
		func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
			a := hostAgent(c)
			if a == nil {
				return "内核装配错误：工具上下文里没有 agent"
			}
			return childResultFn(a, tc)
		})

	// ── 子侧：向上汇报 + 写处理表 ──
	//
	// 侧别由**注册声明**，不是基座里的 `if a.isChildAgent()` ——
	// 后者会让基座重新认识"谁是子"这个类型结论。
	registerTool("notify_parent",
		"向主 agent 汇报（以 L3 中断投给它）。用于主动报告进展/结论，而不是等它来问。",
		registry.SideChild,
		map[string]interface{}{
			"text": map[string]interface{}{"type": "string", "description": "汇报内容"},
		}, []string{"text"},
		func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
			return hostAgent(c).ExecuteNotifyParent(tc)
		})

	registerTool("inputch_note",
		"为**本轮** inputch 主动写入处理信息（主 agent 会查这张表判断你的进度）。"+
			"写了就不会再被系统自动记录；不写则本轮结束时系统自动写。",
		registry.SideChild,
		map[string]interface{}{
			"text": map[string]interface{}{"type": "string", "description": "本轮处理信息摘要"},
		}, []string{"text"},
		func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
			return hostAgent(c).ExecuteInputchNote(tc)
		})
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
func hostAgent(c registry.ToolCtx) *Agent {
	a, _ := c.Host.(*Agent)
	return a
}

// runChildAdminTool 分派 admin_child 三件套里的某一个。
func (a *Agent) runChildAdminTool(name string, tc agentAPI.ToolCall) string {
	switch name {
	case "admin_child":
		return a.executeAdminChild(tc)
	case "admin_child_help":
		return a.executeAdminChildHelp(tc)
	case "admin_child_list":
		return a.ExecuteAdminChildList()
	}
	return "未知管理工具: " + name
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
func (a *Agent) sideOf() registry.Side {
	if a.isChildAgent() {
		return registry.SideChild
	}
	return registry.SideParent
}

// registryToolDefs 把注册表里**本侧可见**的工具渲染成模型 schema。
//
// ★ 渲染走 toolDefWith（而不是自己拼 map）：并行/超时/不进记忆这几项
//
//	声明必须同时登记到包级聚合表，否则 textForVector 查询会"时有时无"
//	（见 toolDefs.go 里那段注释的教训）。
func (a *Agent) registryToolDefs() []interface{} {
	side := a.sideOf()
	items := registry.ToolsFor(side)
	out := make([]interface{}, 0, len(items))
	for _, t := range items {
		opts := toolDefOptions{parallel: t.Parallel, noMemory: t.NoMemory, timeout: t.Timeout}
		out = append(out, toolDefWith(t.Name, t.Description, t.Properties, t.Required, opts))
	}
	// 表生成型声明（随类型增删而变，不能当静态注册项）。
	if !a.isChildAgent() {
		for _, d := range registry.ChildAdminTools() {
			opts := toolDefOptions{parallel: d.Parallel, noMemory: d.NoMemory, timeout: d.Timeout}
			out = append(out, toolDefWith(d.Name, d.Description, d.Properties, d.Required, opts))
		}
	}
	return out
}
