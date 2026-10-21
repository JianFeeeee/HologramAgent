package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// 回归：工具「不存在」必须用**类型化哨兵**判别，不得依赖错误文案匹配。
//
// 现状（toolcall.go:104）：
//
//	if result, err := a.stageHost.ExecuteTool(...); err == nil { ... }
//	else if !strings.Contains(err.Error(), "not found in any plugin") { ... }
//
// 这是**约定**不是契约：插件的错误文案只要恰好含 "not found in any plugin"
// 这个子串，就会被误判成「工具不存在」而错误地 fallback 到 io 路径。
//
// 动态注册（plgreload / 插件崩溃 / 卸载）让这条路径比静态场景更常走，
// 因此判别必须精确。
func TestToolNotFoundIsTypeableNotStringMatched(t *testing.T) {
	// ① 内核自己产生的「不存在」必须可被 errors.Is 判别
	err := toolNotFound("qq_get_message")
	if !errors.Is(err, agentIO.ErrToolNotFound) {
		t.Fatalf("内核的 not-found 错误必须包裹 ErrToolNotFound，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "qq_get_message") {
		t.Errorf("错误文案应含工具名，实际: %v", err)
	}

	// ② 插件自定义错误即使**恰好含** "not found in any plugin" 子串，
	//    也不得被判为「工具不存在」—— 这正是字符串匹配的缺陷。
	fake := fmt.Errorf("plugin internal: device not found in any plugin table (busy)")
	if isToolNotFound(fake) {
		t.Errorf("含诱饵子串的插件错误被误判为工具不存在: %v", fake)
	}

	// ③ 包裹后仍能穿透 fmt.Errorf %w
	wrapped := fmt.Errorf("工具 %s 执行失败: %w", "x", toolNotFound("y"))
	if !errors.Is(wrapped, agentIO.ErrToolNotFound) {
		t.Errorf("%%w 包裹后应仍可判别，实际: %v", wrapped)
	}

	// ④ 普通执行失败不得被判为 not found
	if isToolNotFound(errors.New("connection refused")) {
		t.Errorf("普通执行失败被误判为工具不存在")
	}
}

// 回归：执行期遇到「工具不存在」时，必须**如实报告**而不是静默 fallback。
//
// 场景：stageHost 抛 not-found，io 也没有 ⇒ 最终错误必须仍是 not-found，
// 且文案要让模型看出是"工具不存在/插件可能没加载"，而不是含糊的"执行失败"。
func TestExecuteToolCallReportsMissingToolClearly(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())

	got := a.executeToolCall(agentAPI.ToolCall{
		ID: "c1", Name: "definitely_no_such_tool", Arguments: map[string]interface{}{},
	}, "cli")

	if !strings.Contains(got, "definitely_no_such_tool") {
		t.Fatalf("错误文案应含工具名，实际: %s", got)
	}
	// 模型需要能据此判断该做什么（换名字 / 查 get_plugin_tools / 加载插件）
	if !strings.Contains(got, "不存在") && !strings.Contains(got, "未注册") && !strings.Contains(got, "not found") {
		t.Errorf("错误文案应明示『不存在/未注册』，实际: %s", got)
	}
}
