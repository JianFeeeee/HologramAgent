package agenttype

// construct.go —— **内核按 Spec 组装实例**（插件不碰构造细节）。
//
// ## 插件为什么不能自己造实例
//
// 造一个 agent 实例要碰这些东西：
//
//	agent.New(AgentConfig{IO, Memory, GraphOpener, Security, Agents, ...})
//	child.Registry / InstanceManager 登记
//	调度器装配（有队列 vs 无队列）
//	上下文实现（none / window / relevance）
//	记忆句柄（none / light / full）
//
// 全是内核实现。插件（哪怕同进程的内置插件）拿着这些就等于绕过判据 §1.2
// （"能不能只用 SDK 完整实现"）。所以插件只**声明要什么**，本文件按声明装。

import (
	"fmt"
	"strings"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

func init() {
	construct = constructImpl
	initialize = initializeImpl
	admin = adminImpl
	help = helpImpl
}

// constructImpl 按 Spec 建一个实例并登记到内核骨架。
//
// 复用与内置类型**同一套骨架**（`agent.EnsureInstanceManager` / `child.Registry`），
// 所以插件类型自动获得：持久化、admin_child_list、StopAll、安全策略声明。
func constructImpl(def pubsdk.AgentTypeDef, host registry.Host, arg map[string]interface{}) (string, error) {
	im, ok := host.(*agent.InstanceManager)
	if !ok || im == nil {
		return "", fmt.Errorf("agenttype: 宿主不是实例管理器")
	}
	root, _ := im.Root().(*agent.Agent)
	if root == nil {
		return "", fmt.Errorf("agenttype: 宿主实例管理器没有根 agent")
	}

	id := strings.TrimSpace(strOf(arg, "id"))
	if id == "" {
		return "", fmt.Errorf("agenttype: create 需要 id")
	}
	if _, dup := root.Children().Get(id); dup {
		return "", fmt.Errorf("agenttype: 实例 %s 已存在", id)
	}
	if def.NeedsInputCh && strings.TrimSpace(strOf(arg, "input_chs")) == "" {
		return "", fmt.Errorf("agenttype: 类型 %q 创建时需要 input_chs（不划通道它收不到任何输入）", def.Type)
	}

	// ★ 工作目录由内核分配（用户裁定 2026-11-19）。
	workDir := strings.TrimSpace(strOf(arg, registry.ArgWorkDir))

	cfg := agent.AgentConfig{
		ID:      types.AgentID(id),
		WorkDir: workDir,
		// ★ 只继承设施句柄：插件类型的子 agent 与内置子 agent 走同一条候选链，
		//   而它看不见源表（那是内核的事）。
		// ★ 子 agent 用**自己的 io**（共享通道登记表 + 父视图回退）：
		//   与内置驻留子/rt 同一做法，否则 output_send__* 与设备工具全不可见。
		KernelSource: string(root.ID()),
		ParentID:     string(root.ID()),
		AllowedTools: splitComma(strOf(arg, "allowed_tools")),
		DenyTools:    append(splitComma(strOf(arg, "deny_tools")), def.Spec.DenyTools...),
	}
	// 记忆形态（三档）。
	applyMemory(&cfg, string(def.Spec.Memory), root, workDir)
	// ★ 图记忆：子类型**不再**拿装配面。记忆能力来自主 agent 注册的
	//   `graph_recall` / `graph_commit` 工具（docs §6.4）。

	cfg.TypeName = def.Type
	a := agent.New(cfg)
	a.MountSurface(childIOOf(root), root.StageHost(), root.Gate(), root.AgentsRegistry(), nil, "", root.LLM())
	a.MountToolRegistry(root.ToolRegistry())
	// ★ 父声明类（系统提示词素材 / 优先模型源）：不是配置项，也不是设施句柄，
	//   是**父在创建这一刻传给子的**（docs §2 的 12 字段里没有它们）。
	//   与 MountDevice 同形：装配期写一次，运行期只读。
	a.SetSystemPrompt(root.SystemPrompt())
	a.SetPreferredProvider(agent.FirstNonEmpty(strOf(arg, "preferred_provider"), root.PreferredProvider()))

	// ★ **不在这里 initializeImpl**（2026-11-25）：那一步由
	// `registry.Instantiate` 统一在 construct 之后调（`Descriptor.Initialize`
	// → `initialize(def, instance)`）。在这里再调一次会让钩子装两遍。

	// 交给骨架登记（走内核统一的实例生命周期）。
	if err := root.Children().Add(newEntry(def, a, id)); err != nil {
		a.Stop()
		return "", fmt.Errorf("agenttype: 登记实例失败: %w", err)
	}
	// ★ 交出"刚造的那个" + 记创建参数（交接给内核，不是类型自己起运行期）。
	if im != nil {
		im.MarkCreated(a)
		im.Remember(def.Type, id, arg)
	}
	return fmt.Sprintf("已创建 %s 实例: %s", def.Type, id), nil
}

// initializeImpl 挂回调与零件。
func initializeImpl(def pubsdk.AgentTypeDef, instance any) error {
	a, ok := instance.(*agent.Agent)
	if !ok || a == nil {
		return fmt.Errorf("agenttype: 实例不是 *agent.Agent（装配错误）")
	}
	return installCallbacks(def, a)
}

func adminImpl(def pubsdk.AgentTypeDef, host registry.Host, action string, arg map[string]interface{}) (string, error) {
	im, ok := host.(*agent.InstanceManager)
	if !ok || im == nil {
		return "", fmt.Errorf("agenttype: 宿主不是实例管理器")
	}
	root, _ := im.Root().(*agent.Agent)
	if root == nil {
		return "", fmt.Errorf("agenttype: 宿主实例管理器没有根 agent")
	}
	id := strings.TrimSpace(strOf(arg, "id"))
	e, ok := root.Children().Get(id)
	if !ok {
		return "", fmt.Errorf("agenttype: 实例 %q 不存在", id)
	}
	a, _ := e.Agent().(*agent.Agent)
	if a == nil {
		return "", fmt.Errorf("agenttype: 实例 %q 的载体不是 agent", id)
	}
	if def.Admin == nil {
		return "", fmt.Errorf("agenttype: 类型 %q 没有声明 Admin（动作 %q 无处处理）", def.Type, action)
	}
	return def.Admin(newRuntime(a), action, arg)
}

func helpImpl(def pubsdk.AgentTypeDef, host registry.Host) string {
	if def.Help == nil {
		return fmt.Sprintf("%s：%s\n（该类型未声明 Help 正文）", def.Type, def.Description)
	}
	im, _ := host.(*agent.InstanceManager)
	var root *agent.Agent
	if im != nil {
		root, _ = im.Root().(*agent.Agent)
	}
	return def.Help(newRuntime(root))
}
