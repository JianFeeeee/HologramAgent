package core

import (
	"path/filepath"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// outputTestDevice 是最小的输出通道替身（io 里输出通道就是 Device）。
type outputTestDevice struct {
	name string
	sent []map[string]interface{}
}

func (d *outputTestDevice) Name() string                                 { return d.name }
func (d *outputTestDevice) Type() agentIO.DeviceType                     { return agentIO.DeviceOutput }
func (d *outputTestDevice) Description() string                          { return "测试输出通道" }
func (d *outputTestDevice) Tools() []agentIO.ToolDef                     { return nil }
func (d *outputTestDevice) Start() error                                 { return nil }
func (d *outputTestDevice) Stop() error                                  { return nil }
func (d *outputTestDevice) OutputCapabilities() agentIO.OutputCapability { return agentIO.CapText }
func (d *outputTestDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{} }
func (d *outputTestDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	d.sent = append(d.sent, map[string]interface{}{"tool": tool, "args": args})
	return map[string]interface{}{"status": "sent"}, nil
}

func outputSendTool(name, payload string) agentAPI.ToolCall {
	return agentAPI.ToolCall{
		Name:      "output_send__" + name,
		Arguments: map[string]interface{}{"payload": payload, "type": "text"},
	}
}

// 驻留子必须能看见并使用**父**登记的输出通道。
//
// 现场缺陷（联调实录）：父侧通道装载完整、子侧 childIO 空壳 ——
// 子调 output_send__X 被 `GetChannelCapabilities` 判 0 ⇒
// 「通道 [X] 不存在或不可用。可用输出工具列表见 output_list_channels」，
// 而 output_list_channels 也是空的。根因是子的 io 是新建的、设备表为空，
// 而输出通道（io 的 Device）由插件登记在父的 io 上。
func TestResident_InheritsParentOutputChannels(t *testing.T) {
	parent, _, dir := newRootForResidents(t)
	defer parent.Stop()

	fake := &outputTestDevice{name: "fakeout"}
	other := &outputTestDevice{name: "other"}
	if err := parent.io.RegisterDevice(fake); err != nil {
		t.Fatal(err)
	}
	if err := parent.io.RegisterDevice(other); err != nil {
		t.Fatal(err)
	}

	if _, err := parent.SpawnResident(ResidentOptions{
		ID:         "r-out",
		TaskPrompt: "有情况就发到 fakeout",
		// 白名单只放行一个：验证"继承可见"不等于"绕过授权"
		AllowedOutputs: []string{"fakeout"},
		TempPath:       filepath.Join(dir, "residents", "r-out", "graph.db"),
	}); err != nil {
		t.Fatalf("创建驻留子失败: %v", err)
	}
	child := parent.residents["r-out"].agent

	// ① 看得见：修复前这里是 0（childIO 空壳）
	if caps := child.io.GetChannelCapabilities("fakeout"); caps == 0 {
		t.Fatal("驻留子看不见父的输出通道（childIO 空壳）")
	}
	// ② 发得出去：真走 dev.Execute("output", ...)
	if out := child.executeOutputSendTool(outputSendTool("fakeout", "子发来的消息")); out != "ok" {
		t.Fatalf("子发送应成功，得到 %q", out)
	}
	if len(fake.sent) != 1 {
		t.Fatalf("父通道应收到 1 次输出，得到 %d", len(fake.sent))
	}

	// ③ 授权闸不被回退绕过：白名单外的通道照样拒绝
	if out := child.executeOutputSendTool(outputSendTool("other", "越权")); !strings.Contains(out, "未授权") {
		t.Fatalf("白名单外的通道应被拒，得到 %q", out)
	}
	if len(other.sent) != 0 {
		t.Fatal("越权输出不应真的送达")
	}

	// ④ 工具面一致：子应生成 output_send__fakeout（含配套 _help），
	//    而**不生成**白名单外通道的工具 —— 模型看不到就不会去调。
	var names []string
	for _, td := range child.buildToolDefs() {
		entry, _ := td.(map[string]interface{})
		fn, _ := entry["function"].(map[string]interface{})
		if n, _ := fn["name"].(string); strings.HasPrefix(n, "output_send__") {
			names = append(names, n)
		}
	}
	has := func(want string) bool {
		for _, n := range names {
			if n == want {
				return true
			}
		}
		return false
	}
	if !has("output_send__fakeout") || !has("output_send__fakeout_help") {
		t.Fatalf("子缺少授权通道的输出工具，得到 %v", names)
	}
	for _, n := range names {
		if strings.HasPrefix(n, "output_send__other") {
			t.Fatalf("白名单外的通道不该生成工具，得到 %v", names)
		}
	}

	// ⑤ 实时性：父之后新登记的通道，子立刻可见（设备随资源生灭）
	late := &outputTestDevice{name: "late"}
	if err := parent.io.RegisterDevice(late); err != nil {
		t.Fatal(err)
	}
	if caps := child.io.GetChannelCapabilities("late"); caps == 0 {
		t.Fatal("父新登记的通道未实时反映到子（说明是快照而非回退）")
	}
}

// 默认授权（AllowedOutputs 空）= 完整授权：子用父的全部输出通道。
func TestResident_DefaultOutputsAreFull(t *testing.T) {
	parent, _, dir := newRootForResidents(t)
	defer parent.Stop()

	dev := &outputTestDevice{name: "anywhere"}
	if err := parent.io.RegisterDevice(dev); err != nil {
		t.Fatal(err)
	}
	if _, err := parent.SpawnResident(ResidentOptions{
		ID: "r-full", TaskPrompt: "待命",
		TempPath: filepath.Join(dir, "residents", "r-full", "graph.db"),
	}); err != nil {
		t.Fatalf("创建驻留子失败: %v", err)
	}
	child := parent.residents["r-full"].agent
	if out := child.executeOutputSendTool(outputSendTool("anywhere", "默认授权")); out != "ok" {
		t.Fatalf("默认应完整授权，得到 %q", out)
	}
}
