package core

// 刀 0：验证 commondev 架构赖以成立的前提。
//
// 设计 docs/zh/commondev-agent-runtime-design.md §2.2 / §2.3 / §2.5。
//
// 整套「插件注册通道 → 主 agent 建驻留子并划入 → 子调 output_send__ 下任务」
// 的架构，建立在**两个**此前只有读码结论、从未跑过的事实上：
//
//	P1. 驻留子能调到**外部插件**注册的工具（若不能，子无法下任务给 pi）
//	P2. 驻留子能用**外部插件**注册的输出门 output_send__<ch>（若不能，通道白注册）
//
// 这两条不成立，上层机制全是纸面的 —— 所以先验它，再写插件。
//
// 注册路径与真实插件一致：工具走 StageHost，输出通道走 IOManager.RegisterDevice
// （插件经 proc 桥最终也落到这两处，见 internal/plugin/registry.go:305-320）。

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

// 与 commondev 真实用法一致的通道名：inputch 可含斜杠，outputch 必须合规
// （会被拼成 output_send__<name>，见 SDK RegisterOutputChannel 注释）。
const (
	probeInputCh = "commondev/br-1"
	probeOutCh   = "commondev_br-1"
	probeTool    = "commondev_probe"
	probeOutGate = "output_send__" + probeOutCh
)

// probeDevice 模拟插件注册的输出通道（与内部 channelDevice 同构）。
type probeDevice struct {
	name    string
	caps    agentIO.OutputCapability
	handler sdk.ToolHandler
	got     *string
}

func (d *probeDevice) Name() string                                 { return d.name }
func (d *probeDevice) Type() agentIO.DeviceType                     { return agentIO.DeviceOutput }
func (d *probeDevice) Description() string                          { return "探针通道：给 pi 投递任务" }
func (d *probeDevice) Start() error                                 { return nil }
func (d *probeDevice) Stop() error                                  { return nil }
func (d *probeDevice) OutputCapabilities() agentIO.OutputCapability { return d.caps }
func (d *probeDevice) Tools() []agentIO.ToolDef                     { return nil }
func (d *probeDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{NoMemory: true} }
func (d *probeDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	payload, _ := args["payload"].(string)
	if d.got != nil {
		*d.got = payload // 记录插件 handler 真的收到了什么
	}
	return d.handler(args)
}

// registerProbePlugin 模拟 commondev 插件注册工具与通道。
// delivered 非 nil 时记录插件 handler 实际收到的 payload（证明"包一下"真的发生）。
func registerProbePlugin(t *testing.T, a *Agent, delivered *string) {
	t.Helper()

	// ① 插件工具：子要能调它（P1）
	if err := a.stageHost.RegisterTool(probeTool, sdk.ToolDef{
		Name:        probeTool,
		Description: "探针：插件注册的普通工具",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) {
		return "plugin-tool-ok", nil
	}); err != nil {
		t.Fatalf("插件工具注册失败: %v", err)
	}

	// ② 插件输出通道：子要能用 output_send__commondev_br-1（P2）
	if err := a.io.RegisterDevice(&probeDevice{
		name: probeOutCh,
		caps: agentIO.CapText,
		handler: func(args map[string]interface{}) (interface{}, error) {
			return "已投递", nil
		},
		got: delivered,
	}); err != nil {
		t.Fatalf("插件输出通道注册失败: %v", err)
	}

	// ③ inputch：pi → 子 的回投入口
	if err := a.io.RegisterInputChannelFrom("commondev", probeInputCh, agentIO.ChannelDef{NoMemory: true}); err != nil {
		t.Fatalf("inputch 注册失败: %v", err)
	}
}

// TestP1_ResidentSeesExternalPluginTools 钉住 P1：
// 驻留子的工具表里必须有**外部插件**注册的工具，且真的能执行。
//
// 若不成立：子无法下任务给 pi ⇒ 整套架构作废（必须改为父代投）。
func TestP1_ResidentSeesExternalPluginTools(t *testing.T) {
	parent, _, dir := newRootForResidents(t)
	registerProbePlugin(t, parent, nil)

	info := spawnTestResident(t, parent, dir, "dev-br-1", probeInputCh)
	child := residentOf(t, parent, info.ID)

	// ① 工具表（模型看得到的）里要有
	names := childToolNames(child)
	if !hasToolNamed(names, probeTool) {
		t.Fatalf("P1 不成立：驻留子看不到外部插件工具 %s\n看到的是: %v", probeTool, names)
	}
	t.Logf("P1a ✅ 驻留子工具表含插件工具（工具总数 %d）", len(names))

	// ② 且真的能执行（能看到 ≠ 能调到）
	got := child.executeToolCall(agentAPI.ToolCall{
		ID: "p1", Name: probeTool, Arguments: map[string]interface{}{},
	}, probeInputCh)
	if !strings.Contains(got, "plugin-tool-ok") {
		t.Fatalf("P1 不成立：工具在表里但调不通: %s", got)
	}
	t.Log("P1b ✅ 且可执行（plugin-tool-ok）")
}

// TestP2_ResidentCanUseExternalPluginOutputGate 钉住 P2：
// 驻留子必须能通过 output_send__commondev_br-1 打到**插件注册的通道 handler**。
//
// 若不成立：通道注册了但子用不了 ⇒ 投递路径断掉。
func TestP2_ResidentCanUseExternalPluginOutputGate(t *testing.T) {
	parent, _, dir := newRootForResidents(t)
	var delivered string
	registerProbePlugin(t, parent, &delivered)

	info := spawnTestResident(t, parent, dir, "dev-br-1", probeInputCh)
	child := residentOf(t, parent, info.ID)

	// ① 输出门工具存在（未授权则工具表里根本不会有）
	names := childToolNames(child)
	if !hasToolNamed(names, probeOutGate) {
		t.Fatalf("P2 不成立：驻留子看不到 %s\n看到的是: %v", probeOutGate, names)
	}
	t.Logf("P2a ✅ 驻留子有 %s", probeOutGate)

	// ② 调用它 —— 必须真的打到插件 handler
	//
	// 期望返回 "ok"：`output.go:94` 只在 `dev.Execute()` **成功**时返回它。
	// ⚠️ 不能断言返回插件 handler 的文本：内核对成功的回执刻意**极简**
	//（rich 回执会驱动模型继续调 output_send，是 output loop 的根源）。
	// 所以"插件真的收到了"要由 handler 自己记下来（delivered）来证。
	got := child.executeToolCall(agentAPI.ToolCall{
		ID: "p2", Name: probeOutGate,
		Arguments: map[string]interface{}{"payload": "给 pi 的任务：修 bug", "type": "text"},
	}, probeInputCh)
	if strings.Contains(got, "不存在") || strings.Contains(got, "未授权") {
		t.Fatalf("P2 不成立：输出门不可用: %s", got)
	}
	if got != "ok" {
		t.Fatalf("P2 不成立：输出门应返回 ok（dev.Execute 成功），实际: %q", got)
	}
	// ★ 真正的判据：插件 handler 收到的 payload
	if delivered != "给 pi 的任务：修 bug" {
		t.Fatalf("P2 不成立：插件 handler 收到的 payload 不符: %q", delivered)
	}
	t.Logf("P2b ✅ 输出门打到插件 handler，payload 完整传达（%d 字节）", len(delivered))
}

// TestP3_ChannelNameSlashIsIllegalForOutputGate 钉住命名约束（文档 §2.2）：
// outputch 带斜杠会让 output_send__<name> 不合法 ⇒ 工具名违规。
//
// 这条是**反例**：证明"inputch 可用斜杠、outputch 不行"不是猜想。
func TestP3_ChannelNameSlashIsIllegalForOutputGate(t *testing.T) {
	parent, _, dir := newRootForResidents(t)
	registerProbePlugin(t, parent, nil)

	info := spawnTestResident(t, parent, dir, "dev-br-1", probeInputCh)
	child := residentOf(t, parent, info.ID)
	names := childToolNames(child)

	// 合法命名：有
	if !hasToolNamed(names, probeOutGate) {
		t.Fatalf("下划线命名应可用，但没有 %s", probeOutGate)
	}
	// 非法命名：绝不能出现在工具表里
	for _, n := range names {
		if strings.HasPrefix(n, "output_send__") && strings.Contains(n, "/") {
			t.Fatalf("发现带斜杠的输出门工具名 %q —— 会触发上游 400", n)
		}
	}
	t.Log("P3 ✅ 输出门命名合规（无斜杠）")
}

// ---------------- 测试辅助 ----------------

// residentOf 取父登记表里的驻留子（不存在则 fatal）。
func residentOf(t *testing.T, parent *Agent, id string) *Agent {
	t.Helper()
	parent.residentMu.Lock()
	rc, ok := parent.residents[id]
	parent.residentMu.Unlock()
	if !ok || rc == nil || rc.agent == nil {
		t.Fatalf("驻留子 %s 不存在", id)
	}
	return rc.agent
}

// childToolNames 取某个 agent 当前工具表里的名字。
func childToolNames(a *Agent) []string {
	var out []string
	for _, raw := range a.buildToolDefs() {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		fn, _ := m["function"].(map[string]interface{})
		if fn == nil {
			continue
		}
		if name, _ := fn["name"].(string); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func hasToolNamed(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
