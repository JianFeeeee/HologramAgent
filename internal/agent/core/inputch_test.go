package core

// inputch 总览工具（单工具多视图）的验收。
//
// 需求：父 agent 能看到**所有已注册的 inputch**以及**它们的划分情况**。
// 构筑方式：单工具（input_channels）+ 多视图（view=all|mine|unassigned|by_agent|detail）。

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

func inputChToolCall(args map[string]interface{}) agentAPI.ToolCall {
	return agentAPI.ToolCall{ID: "ic1", Name: "input_channels", Arguments: args}
}

func TestInputChannelsTool_SingleToolMultipleViews(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	me := string(a.id)

	// 同一个插件注册多个 inputch（最基本的输入路由单位）；另一个插件再注册一个。
	if err := a.io.RegisterInputChannelFrom("qq", "qq", agentIO.ChannelDef{NoMemory: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.io.RegisterInputChannelFrom("qq", "qq/device-2", agentIO.ChannelDef{}); err != nil {
		t.Fatal(err)
	}
	if err := a.io.RegisterInputChannelFrom("sub", "sub/in", agentIO.ChannelDef{}); err != nil {
		t.Fatal(err)
	}
	// 划分：qq 与 sub/in 归本 agent，qq/device-2 留未分配。
	if err := a.io.AssignInputChannel("qq", me, 64); err != nil {
		t.Fatal(err)
	}
	if err := a.io.AssignInputChannel("sub/in", me, 0); err != nil {
		t.Fatal(err)
	}

	// view=all（默认）：全部已注册，且带归属插件。
	all := a.executeInputChannels(inputChToolCall(nil))
	for _, want := range []string{"qq", "qq/device-2", "sub/in", "插件 qq", "插件 sub", "无记忆"} {
		if !strings.Contains(all, want) {
			t.Fatalf("view=all 缺少 %q：\n%s", want, all)
		}
	}

	// view=mine：只有划给本 agent 的。
	mine := a.executeInputChannels(inputChToolCall(map[string]interface{}{"view": "mine"}))
	if !strings.Contains(mine, "qq") || !strings.Contains(mine, "sub/in") {
		t.Fatalf("view=mine 应含 qq 与 sub/in：\n%s", mine)
	}
	if strings.Contains(mine, "qq/device-2") {
		t.Fatalf("view=mine 不应含未分配的 qq/device-2：\n%s", mine)
	}

	// view=unassigned：只有尚未划出的。
	un := a.executeInputChannels(inputChToolCall(map[string]interface{}{"view": "unassigned"}))
	if !strings.Contains(un, "qq/device-2") {
		t.Fatalf("view=unassigned 应含 qq/device-2：\n%s", un)
	}
	if strings.Contains(un, "sub/in") {
		t.Fatalf("view=unassigned 不应含已划分的 sub/in：\n%s", un)
	}

	// view=by_agent：划分情况总览（按归属分组）。
	byAgent := a.executeInputChannels(inputChToolCall(map[string]interface{}{"view": "by_agent"}))
	if !strings.Contains(byAgent, me+":") {
		t.Fatalf("view=by_agent 应列出归属 %q：\n%s", me, byAgent)
	}
	if !strings.Contains(byAgent, "未分配") {
		t.Fatalf("view=by_agent 应列出未分配一组：\n%s", byAgent)
	}

	// view=detail：单个 inputch 的全字段（容量 / 策略 / 回程 / 归属）。
	detail := a.executeInputChannels(inputChToolCall(map[string]interface{}{"view": "detail", "name": "qq"}))
	for _, want := range []string{"inputch: qq", "注册插件: qq", "容量: 64", "无记忆", me} {
		if !strings.Contains(detail, want) {
			t.Fatalf("view=detail 缺少 %q：\n%s", want, detail)
		}
	}
	if miss := a.executeInputChannels(inputChToolCall(map[string]interface{}{"view": "detail", "name": "nope"})); !strings.Contains(miss, "未注册") {
		t.Fatalf("detail 查未注册的 inputch 应明确报错：%s", miss)
	}
	if noName := a.executeInputChannels(inputChToolCall(map[string]interface{}{"view": "detail"})); !strings.Contains(noName, "需要 name") {
		t.Fatalf("detail 缺 name 应提示：%s", noName)
	}

	// 未知视图必须报错并列出可用值（不要把拼错静默当成默认视图）。
	bad := a.executeInputChannels(inputChToolCall(map[string]interface{}{"view": "whatever"}))
	if !strings.Contains(bad, "未知 view") || !strings.Contains(bad, "by_agent") {
		t.Fatalf("未知 view 应报错并列出可用值：%s", bad)
	}
}

// 一个 inputch 都没有时应给出明确说明，而不是空串。
func TestInputChannelsTool_NoChannels(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	if out := a.executeInputChannels(inputChToolCall(nil)); !strings.Contains(out, "没有任何已注册") {
		t.Fatalf("空登记表应明确说明：%q", out)
	}
}
