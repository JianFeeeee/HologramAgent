package core

// 工具调用**完整记录**的判据（2026-10-29 还技术债）。
//
// ## 为什么必须有
//
// 改动前的事件形状（生产数据实证）：
//
//	tools_used:   ["qq_get_message", "output_send__qq"]
//	tool_results: [{name, output}, {name, output}]     ← 只有 name/output
//
// 也就是说：**参数完全没有被记录**。后果不是"少了个字段"，而是三个能力失效：
//
//	① 回灌后无法知道"当时用什么参数调用的"，只能看到结果
//	② 段页式页化无法实现 —— toolcall 与其 result 配对成一页，
//	   call 侧只有名字的话那页是半空的
//	③ 同名工具的多次调用无法配对（两个 qq_get_history 分不清谁是谁）
//
// 本文件钉三件事：
//  ① 记录里含 CallID / Name / Arguments / RawArguments
//  ② ToolsUsed 与 ToolCalls **同步**维护（不能只写一份）
//  ③ 参数里的明文凭据必须过闸（新增字段不能绕过脱敏层）

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// ① 记录完整：四个字段都在。
func TestToolCallRecordCapturesArguments(t *testing.T) {
	f := &TaskFrame{}
	f.appendToolCallRecord(agentAPI.ToolCall{
		ID:           "call_1",
		Name:         "cmd_run",
		Arguments:    map[string]interface{}{"command": "ls -la", "timeout": float64(30)},
		RawArguments: `{"command":"ls -la","timeout":30}`,
	})

	if len(f.ToolCalls) != 1 {
		t.Fatalf("应记录 1 条调用，实际 %d", len(f.ToolCalls))
	}
	got := f.ToolCalls[0]
	if got.CallID != "call_1" || got.Name != "cmd_run" {
		t.Fatalf("配对键/名字丢失: %+v", got)
	}
	// ★ 这条是核心：参数必须留存
	if got.Arguments["command"] != "ls -la" {
		t.Fatalf("★ 参数未记录（这正是本次要还的技术债）: %+v", got.Arguments)
	}
	if !strings.Contains(got.RawArguments, "ls -la") {
		t.Fatalf("原始参数未记录: %q", got.RawArguments)
	}
}

// ② ToolsUsed 与 ToolCalls 必须同步。
func TestToolCallRecordKeepsToolsUsedInSync(t *testing.T) {
	f := &TaskFrame{}
	f.appendToolCallRecord(agentAPI.ToolCall{ID: "c1", Name: "a"})
	f.appendToolCallRecord(agentAPI.ToolCall{ID: "c2", Name: "b"})

	if len(f.ToolsUsed) != len(f.ToolCalls) {
		t.Fatalf("★ ToolsUsed(%d) 与 ToolCalls(%d) 不一致 —— 两份数据必须同处维护",
			len(f.ToolsUsed), len(f.ToolCalls))
	}
	for i := range f.ToolsUsed {
		if f.ToolsUsed[i] != f.ToolCalls[i].Name {
			t.Fatalf("第 %d 项不同步: ToolsUsed=%q ToolCalls.Name=%q",
				i, f.ToolsUsed[i], f.ToolCalls[i].Name)
		}
	}
}

// ③ 参数里的明文必须被脱敏（新增字段不能绕过这一层）。
//
// 这条用 scrubEventSecrets（Append 内的唯一咽喉）验证，
// 而不是验证 appendToolCallRecord —— 后者刻意不脱敏（见其注释：
// 多一个脱敏点就多一个会漏的地方）。
func TestToolCallArgumentsAreScrubbed(t *testing.T) {
	evt := &ContextEvent{
		ToolCalls: []ToolCallItem{{
			CallID: "c1", Name: "enc_shell",
			Arguments:    map[string]interface{}{"cmd": "curl -H 'Authorization: Bearer ghp_abcdefghijklmnopqrstuvwxyz0123456789'"},
			RawArguments: `{"cmd":"ghp_abcdefghijklmnopqrstuvwxyz0123456789"}`,
		}},
	}
	initScrubStore(t)
	scrubEventSecrets(evt)

	arg, _ := evt.ToolCalls[0].Arguments["cmd"].(string)
	if strings.Contains(arg, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("★ 参数里的明文未被脱敏: %q", arg)
	}
	if strings.Contains(evt.ToolCalls[0].RawArguments, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("★ 原始参数里的明文未被脱敏: %q", evt.ToolCalls[0].RawArguments)
	}
}

// initScrubStore 装一个启用状态的变量库，供脱敏判据使用。
//
// 单独封装而不是内联：脱敏依赖进程级单例（security.Global），
// 装配与清理成对出现才不会污染同包其他用例。
func initScrubStore(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if _, err := security.InitGlobal(security.Options{
		Path:    dir + "/v.json",
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { security.SetGlobal(nil) })
}
