package test

// extslot_exposure_test.go —— 判据：槽内工具的**暴露范围**由创建参数划定。
//
// docs §6.2 第三条硬规则：
//
//	主 agent 在 `admin_child` 创建子 agent 时，用参数**划定暴露哪些槽内工具**
//
// ## 为什么不是"另开一个机制"
//
// 槽内工具登记进的是**同一张工具表**（`registry.Tool`），于是它们自动受
// `allowed_tools` / `deny_tools` 两个创建参数约束 —— 那条约束本来就存在
// （工具渲染与执行闸门都读同一份 allow/deny 集合）。
//
// 本判据钉的是**这条链路对槽内工具同样成立**：如果有人给槽工具开后门
// （例如渲染时跳过 toolAllowSet），子 agent 就能看见父没划给它的能力。

import (
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// TestExtSlotToolsObeyChildAllowDeny：槽内工具受子实例的 allow/deny 约束。
//
// ★ 这是"创建参数划定暴露哪些槽内工具"的**唯一**执行点：
//
//	`Agent.RegisterExtSlot` 把工具登记进内核表（对所有 agent 可见），
//	而"哪个子看得见"由它自己的 `AllowedTools` / `DenyTools` 决定。
func TestExtSlotToolsObeyChildAllowDeny(t *testing.T) {
	iom := io.NewIOManager()
	root := newTypedAgent(t, agent.AgentConfig{ID: types.AgentID("root"), TypeName: agent.TypeNameMain}, iom)
	t.Cleanup(root.Stop)

	// 注册一个槽内工具（模拟主 agent 交出的 graph_recall）。
	if err := root.RegisterExtSlot([]registry.Tool{{
		Name:        "graph_recall",
		Description: "recall",
		Handle: func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
			return `{"ok":true}`
		},
	}}); err != nil {
		t.Fatalf("RegisterExtSlot: %v", err)
	}

	// ① 未声明的子：看得见槽工具（它在同一张表里）。
	noAllow := newTypedAgent(t, agent.AgentConfig{
		ID: types.AgentID("no-allow"), TypeName: agent.TypeNameMain, ParentID: "root",
	}, iom)
	t.Cleanup(noAllow.Stop)
	if !hasToolNamed(noAllow, "graph_recall") {
		t.Fatal("★ 不声明 allow 的子看不见槽工具 —— 槽工具没有被登记进同一张表")
	}

	// ② 声明了 allow（不含槽工具）：看不见。这是"父划定暴露哪些"的落点。
	restricted := newTypedAgent(t, agent.AgentConfig{
		ID: types.AgentID("restricted"), TypeName: agent.TypeNameMain, ParentID: "root",
		AllowedTools: []string{"read_*"},
	}, iom)
	t.Cleanup(restricted.Stop)
	if hasToolNamed(restricted, "graph_recall") {
		t.Fatal("★ 子声明了 allowed_tools 却仍看得见槽工具 —— 创建参数没有约束槽工具")
	}

	// ③ 显式 deny：看不见（deny 优先于 allow）。
	denied := newTypedAgent(t, agent.AgentConfig{
		ID: types.AgentID("denied"), TypeName: agent.TypeNameMain, ParentID: "root",
		DenyTools: []string{"graph_*"},
	}, iom)
	t.Cleanup(denied.Stop)
	if hasToolNamed(denied, "graph_recall") {
		t.Fatal("★ deny_tools 没有挡住槽工具")
	}

	// ④ **注册者自己看不见**（2026-11-25 用户裁定）。
	//
	// 槽工具是主 agent 交出去**给子 agent 用**的；它自己读图库走私有族
	// （`stablemem_recall`）。若它也能渲染到槽工具，同一能力在它的工具表里
	// 就有两套名字 —— 模型选哪个都是对的，选哪个也都不一致。
	//
	// ★ 这不是"主 agent 特殊"：`Side` 是**注册时由注册者声明的数据**
	//   （`SideChild` = 只在子侧渲染），与"你是哪个类型"无关。
	if hasToolNamed(root, "graph_recall") {
		t.Fatal("★ 槽工具的注册者（主 agent）自己也渲染到了它 —— " +
			"槽工具应声明 SideChild：交出去给子用，自己走私有族")
	}
}

// hasToolNamed 报告某个 agent 的**渲染清单**里是否有该工具。
func hasToolNamed(a *agent.Agent, name string) bool {
	if a == nil {
		return false
	}
	for _, d := range a.ToolDefinitions() {
		if d.Name == name {
			return true
		}
	}
	return false
}

// newTypedAgent 走**类型表的 Construct** 造实例（渲染面在那里装上）。
func newTypedAgent(t *testing.T, cfg agent.AgentConfig, iom *io.IOManager) *agent.Agent {
	t.Helper()
	d, ok := registry.Lookup(cfg.TypeName)
	if !ok {
		t.Fatalf("类型 %q 未注册", cfg.TypeName)
	}
	host := agent.NewRootHost(cfg)
	// ★ 走 `registry.Instantiate`（唯一造实例入口：construct + Initialize）。
	// 直接调 Construct 会跳过 Initialize ⇒ 实例缺调度器/上下文，
	// 而判据仍然绿（"建出来了"不等于"装配好了"）。
	if _, _, err := registry.Instantiate(d, host, map[string]interface{}{"cfg": cfg, "id": string(cfg.ID)}); err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	a, _ := host.Root().(*agent.Agent)
	if a == nil {
		t.Fatalf("类型 %q 没有交回根实例", cfg.TypeName)
	}
	a.MountSurface(iom, nil, nil, nil, "", nil)
	return a
}
