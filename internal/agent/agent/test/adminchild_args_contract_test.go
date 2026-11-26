package test

// admin_child 的**参数契约**判据（用户裁定 2026-11-18 / P-5）。
//
// 契约：顶层**只有四个参数**
//
//	type    子 agent 类型
//	action  动作
//	meta    附加元数据（透传给类型的 Admin，内核不解释）
//	arg     **所有**业务键（id / text / residual / create 参数）
//
// 曾经的实现为了让"模型常常直接写 id"而把顶层 id / text / residual 也收进来。
// 那是把契约按模型的习惯改 —— 后果是同一个动作有两种写法，而两种写法的
// 参数合并顺序成了隐性约定。本判据钉住"顶层多写 ⇒ 明确拒绝且可执行"。

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// TestIdInsideArgWorks：正确写法（id 在 arg 里）必须工作。
func TestIdInsideArgWorks(t *testing.T) {
	h := newChildHarness(t)
	if _, err := h.adminChild(t, "resident", "create", map[string]interface{}{
		"id": "r1", "input_chs": "inbox",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	out := h.agent.ExecuteAdminChild(agentAPI.ToolCall{
		Name:      "admin_child",
		Arguments: map[string]interface{}{"type": "resident", "action": "inspect", "arg": map[string]interface{}{"id": "r1"}},
	})
	if !strings.Contains(out, "r1") {
		t.Fatalf("★ id 放进 arg 却不生效: %s", out)
	}
}

// TestTopLevelIdIsRejected：顶层写 id 必须**明确拒绝**（不静默兼容）。
//
// ★ 静默忽略比报错更糟：模型会以为自己写对了，然后反复试同一个错。
func TestTopLevelIdIsRejected(t *testing.T) {
	h := newChildHarness(t)
	out := h.agent.ExecuteAdminChild(agentAPI.ToolCall{
		Name:      "admin_child",
		Arguments: map[string]interface{}{"type": "resident", "action": "inspect", "id": "r1"},
	})
	if !strings.Contains(out, "顶层只接受") {
		t.Fatalf("★ 顶层多写的 id 被静默接受了: %s", out)
	}
	// 文案必须**可执行**：给出正确写法的例子。
	if !strings.Contains(out, `arg={"id":"r1"`) {
		t.Fatalf("★ 拒绝理由不可执行（没给出正确写法示例）: %s", out)
	}
}

// TestTopLevelTextAndResidualAreRejected：send 的 text、destroy 的 residual 同理。
func TestTopLevelTextAndResidualAreRejected(t *testing.T) {
	h := newChildHarness(t)
	for _, key := range []string{"text", "residual"} {
		out := h.agent.ExecuteAdminChild(agentAPI.ToolCall{
			Name:      "admin_child",
			Arguments: map[string]interface{}{"type": "resident", "action": "send", key: "x"},
		})
		if !strings.Contains(out, "顶层只接受") {
			t.Fatalf("★ 顶层多写的 %q 被静默接受了: %s", key, out)
		}
	}
}
