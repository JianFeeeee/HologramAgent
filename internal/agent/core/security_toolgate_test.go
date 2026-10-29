package core

// 安全策略闸的**接线**判据。
//
// ## 为什么需要这个文件（2026-10-29）
//
// `Policy.CheckTool` 此前是完美的机制 + 零个生产调用点：
//
//	CheckTool / CheckToolFor / ScanContent / CheckPath / CheckNetwork
//	  → 除了 security 包自己的测试，全仓 0 个调用方
//
// 更糟的是 `Agent.security` 字段**从未被赋值**（只有测试赋过），
// 所以 `securityDecisionFor` 恒走 nil 分支放行。两种故障叠加的结果是：
// 策略表存在、判定函数正确、日志里一条拒绝记录都没有 —— 看起来"策略已启用"。
//
// 本文件的判据钉的是**接线**，不是机制：
//   - 机制（deny 命中返回 !Allowed）：security/policy_test.go 已经钉过
//   - 接线（!Allowed 时工具真的不执行）：本文件钉
//
// ## 判据必须能红
//
// 如果把 `executeToolCallInner` 里的策略闸整段删掉，
// `TestToolDeniedByPolicyNeverExecutes` 必须失败。
// 这一点比"测试通过"更重要 —— 否则它只是又一份自我确认。

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// toolCallTo 构造一个最小可用的工具调用。
func toolCallTo(name string) agentAPI.ToolCall {
	return agentAPI.ToolCall{Name: name, Arguments: map[string]interface{}{}}
}

// policyRuntime 构造一个带工具黑名单的独立运行时（不碰全局单例）。
func policyRuntime(t *testing.T, deny ...string) *security.Runtime {
	t.Helper()
	rt := security.NewRuntime(nil)
	rt.SetPolicy(security.Policy{ToolDeny: deny})
	return rt
}

// ① 未注入安全运行时 ⇒ 全部放行（独立工具/测试的既有行为不变）。
func TestToolGateAbsentAllowsEverything(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.SetSecurity(nil)

	if d := a.securityDecisionFor("cmd_run"); !d.Allowed {
		t.Fatalf("未注入时应放行: %s", d.Reason)
	}
}

// ② SetSecurity 真的接上了（此前该字段只有测试赋过值）。
func TestSetSecurityWiresDecision(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.SetSecurity(policyRuntime(t, "cmd_*"))

	if d := a.securityDecisionFor("cmd_run"); d.Allowed {
		t.Fatal("SetSecurity 注入后 cmd_* 必须被拒 —— 否则注入通道是假的")
	}
	if d := a.securityDecisionFor("stablemem_recall"); !d.Allowed {
		t.Fatalf("未命中黑名单的工具必须放行: %s", d.Reason)
	}
}

// ③ ★ 核心判据：被策略拒绝的工具**不执行**，且返回结构化拒绝。
//
// "不执行" 通过让工具注册一个必被调用的探针来证明：
// 若闸门缺失，探针会被调用（hit=true），判据失败。
func TestToolDeniedByPolicyNeverExecutes(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())

	const toolName = "probe_tool"
	hit := false
	// 插件工具经 StageHost 注册（内置工具不走这条路，见 tooldefs.go 的说明）。
	if err := a.stageHost.RegisterTool(toolName, sdk.ToolDef{
		Name:        toolName,
		Description: "probe",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) {
		hit = true
		return "should not run", nil
	}); err != nil {
		t.Fatalf("注册探针工具失败: %v", err)
	}

	// 先证明探针本身可用（否则"没执行"可能只是工具不存在）。
	a.SetSecurity(nil)
	if out := a.executeToolCall(toolCallTo(toolName), "test"); !strings.Contains(out, "should not run") {
		t.Fatalf("前置条件不成立：策略未启用时探针应正常执行，实际返回 %q", out)
	}
	if !hit {
		t.Fatal("前置条件不成立：探针未被调用")
	}

	// 再打开黑名单 ⇒ 探针必须**一次都不再被调用**。
	hit = false
	a.SetSecurity(policyRuntime(t, toolName))
	out := a.executeToolCall(toolCallTo(toolName), "test")

	if hit {
		t.Fatal("★ 被策略拒绝的工具仍被执行了 —— 策略闸没接在实际执行链上")
	}
	if !strings.Contains(out, "not permitted") {
		t.Fatalf("拒绝必须以结构化文案回给模型（便于它换工具），实际 %q", out)
	}
}

// ④ 拒绝原因必须**可执行**：带上策略给的理由，而不是泛泛的 "denied"。
func TestToolDenialCarriesActionableReason(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.SetSecurity(policyRuntime(t, "cmd_*"))

	out := a.executeToolCall(toolCallTo("cmd_run"), "test")
	if !strings.Contains(out, "cmd_*") {
		t.Fatalf("拒绝文案应包含命中的黑名单条目（模型据此才知道边界），实际 %q", out)
	}
	if !strings.Contains(out, "denied") {
		t.Fatalf("应带 denied 原因码（与 unauthorized 区分），实际 %q", out)
	}
}
