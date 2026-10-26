package core

// 输入携带 `security_profile` 时的**激活语义**判据。
//
// 用户模型（2026-10-26）：
//
// > 插件可以声明自身安全限制策略，并在插件自身 inputch 触发时携带参数
// > 选择其中一种 —— 正好对应 QQ 插件内部 owner 与非 owner 的差别对待。
//
// > 这是可选参数，不声明安全策略则使用内核默认安全策略。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// ① 没有携带 security_profile ⇒ 不激活任何档位（零行为变化）。
func TestNoSecurityProfileKeepsDefault(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	if got := a.activateSecurityProfile(map[string]interface{}{"content": "hi"}); got != "" {
		t.Fatalf("未携带档位时不应激活，实际 %q", got)
	}
	if d := a.securityDecisionFor("cmd_run"); !d.Allowed {
		t.Fatalf("未使用档位时应沿用默认策略: %s", d.Reason)
	}
}

// ② 声明档位 + 输入携带档位名 ⇒ 该轮工具判定按档位生效（owner/非 owner 差别）。
func TestSecurityProfileFromInputChangesToolDecision(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	// 用一个独立运行时（不依赖全局单例），模拟"security 模块已装配"。
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
	if got := a.activateSecurityProfile(map[string]interface{}{"security_profile": "qq.guest"}); got != "qq.guest" {
		t.Fatalf("应激活 qq.guest，实际 %q", got)
	}
	if d := a.securityDecisionFor("cmd_run"); d.Allowed {
		t.Fatal("guest 档激活后 cmd_* 必须被拒 —— 这是 owner 与非 owner 的差别")
	}
}

// ③ 携带**未声明**的档位名 ⇒ 不激活（回退默认），且不 panic。
//
// 判据只钉"不激活"这一半：日志侧由 security 模块保证响亮（返回 error）。
// 不在这里断言日志文案 —— 那会让判据绑死文案。
func TestUnknownSecurityProfileFallsBackWithoutPanic(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.security = security.NewRuntime(nil)
	if got := a.activateSecurityProfile(map[string]interface{}{"security_profile": "qq.typo"}); got != "" {
		t.Fatalf("未声明的档位不应被激活，实际 %q", got)
	}
}

// ④ 未装配安全模块（a.security == nil）⇒ 全部放行，且不 panic。
func TestSecurityDecisionWithoutRuntime(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	if d := a.securityDecisionFor("cmd_run"); !d.Allowed {
		t.Fatalf("未装配安全模块时应放行: %s", d.Reason)
	}
	if got := a.activateSecurityProfile(map[string]interface{}{"security_profile": "qq.owner"}); got != "" {
		t.Fatalf("未装配时不应激活，实际 %q", got)
	}
}
