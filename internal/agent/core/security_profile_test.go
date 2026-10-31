package core

// 输入携带 `security_profile` 时的**激活语义**判据。
//
// 用户模型（2026-10-26）：
//
// > 插件可以声明自身安全限制策略，并在插件自身 inputch 触发时携带参数
// > 选择其中一种 —— 正好对应 QQ 插件内部 owner 与非 owner 的差别对待。
//
// > 这是可选参数，不声明安全策略则使用内核默认安全策略。
//
// ## 2026-10-31 重写（收敛后的形态）
//
// 本文件原先调用 `a.activateSecurityProfile(payload)` —— 一个**在 agent 里
// 解析 security 协议**的函数。收敛后该函数已删除：档位解析归 security
// （`security.BeginRound` 自己读 `payload["security_profile"]`）。
//
// 所以这里改用 `a.beginSecurityRound(payload, source)`，它走真实的生产入口，
// **不由判据手工解析字段**。这样判据钉的是「security 自己会解析吗」，
// 而不是「agent 有没有帮它解析」。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// ① 没有携带 security_profile ⇒ 不激活任何档位（零行为变化）。
func TestNoSecurityProfileKeepsDefault(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.security = security.NewRuntime(nil)

	round := a.beginSecurityRound(map[string]interface{}{"content": "hi"}, "probe")
	defer round.End()

	if got := round.Profile(); got != "" {
		t.Fatalf("未携带档位时不应激活，实际 %q", got)
	}
	if d := a.securityDecisionFor("cmd_run"); !d.Allowed {
		t.Fatalf("未使用档位时应沿用默认策略: %s", d.Reason)
	}
}

// ② 声明档位 + 输入携带档位名 ⇒ 该轮工具判定按档位生效（owner/非 owner 差别）。
func TestSecurityProfileFromInputChangesToolDecision(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	rt := security.NewRuntime(nil)
	if err := rt.DeclareProfile(security.Profile{
		Name: "qq.guest", Owner: "qq",
		Policy: security.PrincipalPolicy{DenyTools: []string{"cmd_*"}},
	}); err != nil {
		t.Fatal(err)
	}
	a.security = rt

	if d := a.securityDecisionFor("cmd_run"); !d.Allowed {
		t.Fatalf("激活前应放行: %s", d.Reason)
	}

	round := a.beginSecurityRound(map[string]interface{}{"security_profile": "qq.guest"}, "probe")
	defer round.End()

	if got := round.Profile(); got != "qq.guest" {
		t.Fatalf("★ security 应从 payload 自己解析出档位并激活 qq.guest，实际 %q —— "+
			"若这里是空，说明 BeginRound 没读 security_profile（解析又落回调用方了）", got)
	}
	// 判定走本轮句柄（生产上工具执行链用的是同一条判定语义）。
	if d := round.CheckTool("cmd_run"); d.Allowed {
		t.Fatal("guest 档激活后 cmd_* 必须被拒 —— 这是 owner 与非 owner 的差别")
	}
}

// ③ 携带**未声明**的档位名 ⇒ 不激活（回退默认），且**不沿用上一轮的档**。
//
// 两种失败都必须咬住：
//
//	· 静默沿用上一轮的 guest 档 ⇒ 拼错名却仍收窄（用户被无辜禁工具）
//	· 静默当作"无档"却继续用旧档 ⇒ 同上
func TestUnknownSecurityProfileFallsBackWithoutPanic(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	rt := security.NewRuntime(nil)
	if err := rt.DeclareProfile(security.Profile{
		Name: "qq.guest", Owner: "qq",
		Policy: security.PrincipalPolicy{DenyTools: []string{"cmd_*"}},
	}); err != nil {
		t.Fatal(err)
	}
	a.security = rt

	// 先开一个合法档位并**不结束**，模拟"进程里还挂着 guest 档"。
	prev := a.beginSecurityRound(map[string]interface{}{"security_profile": "qq.guest"}, "probe")
	if prev.Profile() != "qq.guest" {
		t.Fatalf("前置失败：合法档位未激活，实际 %q", prev.Profile())
	}

	// 再来一条拼错的。
	round := a.beginSecurityRound(map[string]interface{}{"security_profile": "qq.typo"}, "probe")
	defer round.End()

	if got := round.Profile(); got != "" {
		t.Fatalf("未声明的档位不应被激活，实际 %q", got)
	}
	if got := rt.ActiveProfile(); got != "" {
		t.Fatalf("★ 拼错的档位名必须在降级时**清掉**上一轮的档，实际仍是 %q —— "+
			"否则“配件名写错”会静默沿用旧档，用户被无辜收窄", got)
	}
	prev.End()
}

// ④ 未装配安全模块（a.security == nil）⇒ 全部放行，且不 panic。
func TestSecurityDecisionWithoutRuntime(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	if d := a.securityDecisionFor("cmd_run"); !d.Allowed {
		t.Fatalf("未装配安全模块时应放行: %s", d.Reason)
	}

	round := a.beginSecurityRound(map[string]interface{}{"security_profile": "qq.owner"}, "probe")
	defer round.End()
	if got := round.Profile(); got != "" {
		t.Fatalf("未装配时不应激活，实际 %q", got)
	}
}

// ⑤ ★ 档位必须**随轮次结束归位**，不泄漏到下一条输入。
//
// 这是收敛的附带收益：`End()` 恢复进入前的档位。没有它，
// 进程级 active 会让 guest 的收窄误伤后续输入（方向相反就是提权）。
func TestProfileRestoredAfterRoundEnd(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	rt := security.NewRuntime(nil)
	if err := rt.DeclareProfile(security.Profile{
		Name: "qq.guest", Owner: "qq",
		Policy: security.PrincipalPolicy{DenyTools: []string{"cmd_*"}},
	}); err != nil {
		t.Fatal(err)
	}
	a.security = rt

	func() {
		round := a.beginSecurityRound(map[string]interface{}{"security_profile": "qq.guest"}, "probe")
		defer round.End()
		if round.Profile() != "qq.guest" {
			t.Fatalf("前置：应激活 guest，实际 %q", round.Profile())
		}
	}()

	if got := rt.ActiveProfile(); got != "" {
		t.Fatalf("★ 轮次结束后档位应归位为空，实际 %q —— "+
			"进程级 active 泄漏到下一条输入，guest 的收窄会误伤后续输入", got)
	}
}
