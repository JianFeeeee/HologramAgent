package pluginview

import "testing"

// 这条用例锁死的是曾经的线上缺陷根因：EventToolCall 的 result 是 Go 的
// map 文本（map[cols:80 command:sleep 120 id:term_2 ...]），不是
// map[string]interface{}。旧代码断言成后者永远失败 → /terminals 恒空。
func TestMapFieldFromMapText(t *testing.T) {
	res := "map[cols:80 command:sleep 120 id:term_2 notify_mode:exit rows:24 status:created timeout:5m0s]"
	if got := mapFieldFromMapText(res, "id"); got != "term_2" {
		t.Fatalf("id = %q, want term_2", got)
	}
	// command 含空格：按空格切字段会把它切断，这里只要求拿到首段（与事件
	// 负载同源，command 的真实值另有 args 路径可拿，不靠 map 文本）。
	if got := mapFieldFromMapText(res, "cols"); got != "80" {
		t.Fatalf("cols = %q, want 80", got)
	}
	if got := mapFieldFromMapText(res, "notify_mode"); got != "exit" {
		t.Fatalf("notify_mode = %q, want exit", got)
	}
	if got := mapFieldFromMapText(res, "nope"); got != "" {
		t.Fatalf("missing key = %q, want empty", got)
	}
	if got := terminalIDFromMapText("not a map at all"); got != "" {
		t.Fatalf("garbage = %q, want empty", got)
	}
}

func TestTerminalRegistryLifecycle(t *testing.T) {
	r := NewTerminalView()

	// agent 路径：terminal_create 的 result 是 Go map 文本，要能回填 id。
	r.OnToolCall(map[string]interface{}{
		"tool":   "terminal_create",
		"args":   map[string]interface{}{"command": "sleep 120"},
		"result": "map[cols:80 command:sleep 120 id:term_7 status:created]",
		"status": "ok",
	})
	terms, _ := r.Snapshot()
	if len(terms) != 1 || terms[0].ID != "term_7" || !terms[0].Running {
		t.Fatalf("after create: %+v", terms)
	}

	// agentcli 输出事件：追加 output。
	r.OnTerminalOutput(map[string]interface{}{
		"terminal_id": "term_7", "output": "hello", "running": true,
	})
	terms, _ = r.Snapshot()
	if terms[0].Output != "hello" {
		t.Fatalf("output = %q", terms[0].Output)
	}

	// 生命周期事件：退出置 running=false。
	r.OnTerminalOutput(map[string]interface{}{
		"terminal_id": "term_7", "running": false,
	})
	terms, _ = r.Snapshot()
	if terms[0].Running {
		t.Fatalf("should be stopped: %+v", terms)
	}

	// CLI 直调路径：没有 EventToolCall，只有 terminal_output 生命周期事件，
	// 依然要能凭 command 字段建出条目（设备名不丢）。
	r.OnTerminalOutput(map[string]interface{}{
		"terminal_id": "term_9", "command": "top", "running": true,
	})
	terms, _ = r.Snapshot()
	var found bool
	for _, tm := range terms {
		if tm.ID == "term_9" && tm.Command == "top" && tm.Running {
			found = true
		}
	}
	if !found {
		t.Fatalf("direct-path terminal missing: %+v", terms)
	}

	// cmd_run 历史：只保留最近 maxCmdHistory 条。
	for i := 0; i < maxCmdHistory+10; i++ {
		r.OnToolCall(map[string]interface{}{
			"tool":   "cmd_run",
			"args":   map[string]interface{}{"command": "echo hi"},
			"status": "ok",
		})
	}
	_, cmds := r.Snapshot()
	if len(cmds) != maxCmdHistory {
		t.Fatalf("cmd history len = %d, want %d", len(cmds), maxCmdHistory)
	}
	if cmds[0].Command != "echo hi" || cmds[0].Status != "ok" {
		t.Fatalf("cmd exec = %+v", cmds[0])
	}
}

// 终端数超上限时要淘汰，不能无界增长。
func TestTerminalRegistryCap(t *testing.T) {
	r := NewTerminalView()
	for i := 0; i < maxTerminals+20; i++ {
		r.OnTerminalOutput(map[string]interface{}{
			"terminal_id": string(rune('a'+i%26)) + "-x", "running": true,
		})
	}
	terms, _ := r.Snapshot()
	if len(terms) > maxTerminals {
		t.Fatalf("terminals = %d, want <= %d", len(terms), maxTerminals)
	}
}
