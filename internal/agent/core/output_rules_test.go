package core

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// 设计口径：输出是 agent 的**主动调用** —— 收到一次输入后，可以往任意（已授权的）
// 通道发**任意多次**（分段播报、先回执后结论、同时通知多个通道都合法）。
//
// 这条判据钉住的是"提示词里不得出现输出次数限制"。此前 `tooldefs.go` 里写着
// 「每轮对话通常只需调用一次 output_send__{通道名} 即可完成回复」—— 一条凭空的限制，
// 会让模型自己收起合理的多次输出（用户现场指出）。
func TestSystemPromptDoesNotRestrictOutputCount(t *testing.T) {
	parent, _, _ := newRootForResidents(t)
	defer parent.Stop()

	prompt := parent.buildSystemPrompt("", "你好")
	banned := []string{
		"只需调用一次",
		"只能调用一次",
		"通常只需调用",
		"不要重复发送",
	}
	for _, b := range banned {
		if strings.Contains(prompt, b) {
			t.Fatalf("系统提示词里仍有输出次数限制 %q —— 设计上次数不限", b)
		}
	}
	if !strings.Contains(prompt, "输出次数与目标通道由你自己决定") {
		t.Fatal("系统提示词应明确「输出次数与目标通道由你自己决定」")
	}
	if !strings.Contains(prompt, "没有任何「一轮只能发一次」的限制") {
		t.Fatal("系统提示词应显式否认「一轮只能发一次」")
	}
}

// 输出工具的 type 可省略，缺省按 text 处理（判据该拦的是"不知道发什么"，
// 不是"没写众所周知的默认值"）。
func TestOutputSendTypeDefaultsToText(t *testing.T) {
	parent, _, _ := newRootForResidents(t)
	defer parent.Stop()

	dev := &outputTestDevice{name: "fakeout"}
	if err := parent.io.RegisterDevice(dev); err != nil {
		t.Fatal(err)
	}

	out := parent.executeOutputSendTool(agentAPI.ToolCall{
		Name:      "output_send__fakeout",
		Arguments: map[string]interface{}{"payload": "只给 payload，不给 type"},
	})
	if out != "ok" {
		t.Fatalf("省略 type 时应默认 text 并发送成功，得到 %q", out)
	}
	if len(dev.sent) != 1 {
		t.Fatalf("通道应收到 1 次输出，得到 %d", len(dev.sent))
	}
	if args, _ := dev.sent[0]["args"].(map[string]interface{}); args["type"] != "text" {
		t.Fatalf("缺省类型应为 text，实际 %v", args["type"])
	}

	// 工具 schema：required 只应含 payload
	var found bool
	for _, td := range parent.buildToolDefs() {
		entry, _ := td.(map[string]interface{})
		fn, _ := entry["function"].(map[string]interface{})
		if n, _ := fn["name"].(string); n != "output_send__fakeout" {
			continue
		}
		found = true
		params, _ := fn["parameters"].(map[string]interface{})
		req, _ := params["required"].([]string)
		if len(req) != 1 || req[0] != "payload" {
			t.Fatalf("output_send 的 required 应只有 payload，实际 %v", req)
		}
	}
	if !found {
		t.Fatal("未生成 output_send__fakeout 工具")
	}
}

// 空 payload 仍应被拦（这条判据是对的：不知道发什么不能放过）。
func TestOutputSendStillRequiresPayload(t *testing.T) {
	parent, _, _ := newRootForResidents(t)
	defer parent.Stop()

	if err := parent.io.RegisterDevice(&outputTestDevice{name: "fakeout"}); err != nil {
		t.Fatal(err)
	}
	out := parent.executeOutputSendTool(agentAPI.ToolCall{
		Name:      "output_send__fakeout",
		Arguments: map[string]interface{}{"type": "text"},
	})
	if !strings.Contains(out, "payload 不能为空") {
		t.Fatalf("空 payload 应被拦，得到 %q", out)
	}
}
