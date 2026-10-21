package core

// N1b：输出通道授权集合 + 输出通道 → 目标 agent 的 inputch 解析。
//
// 设计依据 docs/zh/resident-subagent-design.md §4.4（通道分配：不对称）与 R2
// （插件与工具由父授权，**默认完整授权**）。
//
// 三个过滤点必须一致，否则会出现"列表里看不到、但按名字还能调"的裂缝：
//   ① 工具表（不为未授权的通道生成 output_send__X）
//   ② 列表工具（output_list_channels 只列授权的）
//   ③ 调用点（凭名字直调也必须被拒 —— 纵深防御）

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// registerFakeOutput 注册一个假的输出通道（device 通道）。
func registerFakeOutput(t *testing.T, a *Agent, name string) {
	t.Helper()
	if err := a.io.RegisterDevice(&mockOutputDevice{name: name, caps: agentIO.CapText}); err != nil {
		t.Fatalf("注册测试通道 %s 失败: %v", name, err)
	}
}

func toolNames(a *Agent) []string {
	var names []string
	for _, t := range a.buildToolDefs() {
		m, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		fn, _ := m["function"].(map[string]interface{})
		if n, _ := fn["name"].(string); n != "" {
			names = append(names, n)
		}
	}
	return names
}

func hasTool(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// 默认（未配置白名单）= 完整授权：所有输出通道都能用。
func TestOutputGrant_DefaultIsFull(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	registerFakeOutput(t, a, "qq")
	registerFakeOutput(t, a, "webui")

	if !a.IsOutputAllowed("qq") || !a.IsOutputAllowed("webui") {
		t.Fatal("默认应为完整授权")
	}
	names := toolNames(a)
	if !hasTool(names, "output_send__qq") || !hasTool(names, "output_send__webui") {
		t.Fatalf("默认完整授权下应生成全部输出门，实际 %v", names)
	}
	if out := a.executeOutputListChannels(); !strings.Contains(out, "qq") || !strings.Contains(out, "webui") {
		t.Fatalf("默认完整授权下列表应含全部通道：\n%s", out)
	}
}

// 白名单收窄：三个过滤点必须一致。
func TestOutputGrant_NarrowedWhitelist(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.allowedOutputs = []string{"webui"} // 模拟父创建子时收窄
	registerFakeOutput(t, a, "qq")
	registerFakeOutput(t, a, "webui")

	if a.IsOutputAllowed("qq") {
		t.Fatal("白名单外的通道不应被授权")
	}
	if !a.IsOutputAllowed("webui") {
		t.Fatal("白名单内的通道应被授权")
	}

	// ① 工具表
	names := toolNames(a)
	if hasTool(names, "output_send__qq") {
		t.Fatalf("未授权的通道不该生成输出门工具：%v", names)
	}
	if !hasTool(names, "output_send__webui") {
		t.Fatalf("已授权的通道应生成输出门工具：%v", names)
	}

	// ② 列表工具
	out := a.executeOutputListChannels()
	if strings.Contains(out, "qq") {
		t.Fatalf("列表不应含未授权通道：\n%s", out)
	}
	if !strings.Contains(out, "webui") {
		t.Fatalf("列表应含已授权通道：\n%s", out)
	}

	// ③ 调用点（凭名字直调）
	got := a.executeOutputSendTool(agentAPI.ToolCall{
		ID: "c1", Name: "output_send__qq",
		Arguments: map[string]interface{}{"payload": "hi", "type": "text"},
	})
	if !strings.Contains(got, "未授权") {
		t.Fatalf("未授权的输出门必须被拒，实际 %q", got)
	}
}

// 输出通道 → 目标 agent 的 inputch 的解析（"输出可寻址到具体 agent"）。
func TestOutputTarget_Resolution(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	reg := a.io.ChannelRegistry()

	if err := reg.BindOutputTarget("to-child-1", "child-1", "sub/in"); err != nil {
		t.Fatal(err)
	}
	tgt, ok := a.ResolveOutputTarget("to-child-1")
	if !ok || tgt.AgentID != "child-1" || tgt.InputCh != "sub/in" {
		t.Fatalf("解析结果=%+v ok=%v", tgt, ok)
	}
	// 未登记的输出通道由传输层处理（如 qq/webui 这类 device 通道）。
	if _, ok := a.ResolveOutputTarget("qq"); ok {
		t.Fatal("未登记目标解析的输出通道不应解析出 agent")
	}
	if err := reg.BindOutputTarget("", "x", "y"); err == nil {
		t.Fatal("空输出通道名应报错")
	}

	// 列表工具在已登记时带出目标，便于模型知道"这条通道发给谁"。
	registerFakeOutput(t, a, "to-child-1")
	if out := a.executeOutputListChannels(); !strings.Contains(out, "child-1") {
		t.Fatalf("已登记目标的输出通道应在列表里标出目标：\n%s", out)
	}
}
