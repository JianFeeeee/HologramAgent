package test

// internal/pluginview 的黑盒判据：事件输入 → Snapshot 输出。
// 只调 NewTerminalView/OnToolCall/OnTerminalOutput/Snapshot。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/pluginview"
)

// TestFreshViewIsEmpty：新视图的 Snapshot 为空。
func TestFreshViewIsEmpty(t *testing.T) {
	v := pluginview.NewTerminalView()
	terms, cmds := v.Snapshot()
	if len(terms) != 0 || len(cmds) != 0 {
		t.Fatalf("★ 新视图非空：terms=%d cmds=%d", len(terms), len(cmds))
	}
}

// TestCmdRunEventAppearsInSnapshot：cmd_run 事件必须进入命令历史。
func TestCmdRunEventAppearsInSnapshot(t *testing.T) {
	v := pluginview.NewTerminalView()
	v.OnToolCall(map[string]interface{}{
		"tool":   "cmd_run",
		"args":   map[string]interface{}{"command": "echo hello"},
		"status": "ok",
	})
	_, cmds := v.Snapshot()
	if len(cmds) != 1 {
		t.Fatalf("★ cmd_run 未进命令历史：%d 条", len(cmds))
	}
	if cmds[0].Command != "echo hello" {
		t.Fatalf("★ Command = %q，期望 echo hello", cmds[0].Command)
	}
}

// TestTerminalCreateThenOutput：创建终端事件 + 输出事件 → Snapshot 反映状态。
func TestTerminalCreateThenOutput(t *testing.T) {
	v := pluginview.NewTerminalView()
	v.OnToolCall(map[string]interface{}{
		"tool": "terminal_create",
		"args": map[string]interface{}{"id": "term-1", "command": "bash"},
	})
	terms, _ := v.Snapshot()
	if len(terms) != 1 || terms[0].ID != "term-1" {
		t.Fatalf("★ terminal_create 未出现在 Snapshot：%+v", terms)
	}
	v.OnTerminalOutput(map[string]interface{}{
		"terminal_id": "term-1", "output": "line-1\n", "running": true,
	})
	terms, _ = v.Snapshot()
	if len(terms) != 1 {
		t.Fatalf("★ 输出事件后终端数变了：%d", len(terms))
	}
	if terms[0].Output == "" {
		t.Fatal("★ 输出事件没进 Snapshot 的 Output")
	}
}

// TestUnknownToolIsIgnored：未知工具事件不得造出命令/终端。
func TestUnknownToolIsIgnored(t *testing.T) {
	v := pluginview.NewTerminalView()
	v.OnToolCall(map[string]interface{}{"tool": "some_plugin_tool", "args": map[string]interface{}{}})
	terms, cmds := v.Snapshot()
	if len(terms) != 0 || len(cmds) != 0 {
		t.Fatalf("★ 未知工具造出了记录：terms=%d cmds=%d", len(terms), len(cmds))
	}
}
