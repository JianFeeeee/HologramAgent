package security

// 命名限式（profile）的判据。
//
// 用户模型（2026-10-26）：
//
// > 插件可以声明自身安全限制策略，并在插件自身 inputch 触发时携带参数
// > 选择其中一种 —— 正好对应 QQ 插件内部 owner 与非 owner 的差别对待。
//
// > 这是可选参数，不声明安全策略则使用内核默认安全策略。
//
// 所以每条判据都在问：**不声明时行为变了吗？声明后能绕过全局吗？**

import (
	"strings"
	"testing"
	"time"
)

// ① 不声明任何档位 ⇒ 一切沿用默认策略（存量插件零改动）。
func TestProfileAbsentUsesDefaultPolicy(t *testing.T) {
	r := NewRuntime(nil)
	if names := r.Profiles(); len(names) != 0 {
		t.Fatalf("新建运行时不应有档位: %v", names)
	}
	if got := r.ActiveProfile(); got != "" {
		t.Fatalf("未激活时活跃档位应为空，实际 %q", got)
	}
	for _, tool := range []string{"cmd_run", "files_read", "output_send__qq"} {
		if d := r.CheckTool(tool); !d.Allowed {
			t.Errorf("未声明档位时 %q 应放行（默认策略）: %s", tool, d.Reason)
		}
	}
}

// ② 声明并按名激活后，档位的 deny 生效。
func TestProfileDenyAppliesWhenActivated(t *testing.T) {
	r := NewRuntime(nil)
	if err := r.DeclareProfile(Profile{
		Name: "qq.guest", Owner: "qq",
		Policy: PrincipalPolicy{DenyTools: []string{"cmd_*", "files_write"}},
	}); err != nil {
		t.Fatal(err)
	}
	if d := r.CheckTool("cmd_run"); !d.Allowed {
		t.Fatalf("未激活档位时不应生效: %s", d.Reason)
	}
	if err := r.ActivateProfile("qq.guest", time.Minute); err != nil {
		t.Fatal(err)
	}
	if d := r.CheckTool("cmd_run"); d.Allowed {
		t.Fatal("激活 guest 档后 cmd_* 必须被拒 —— 这就是 owner/非 owner 的差别")
	}
	if d := r.CheckTool("files_read"); !d.Allowed {
		t.Fatalf("未列入 deny 的工具应放行: %s", d.Reason)
	}
}

// ③ 档位名必须带声明者前缀：防止跨插件覆盖（提权口子）。
func TestProfileNameMustBePrefixedByOwner(t *testing.T) {
	r := NewRuntime(nil)
	if err := r.DeclareProfile(Profile{Name: "owner", Owner: "qq"}); err == nil {
		t.Fatal("档位名不带插件前缀必须被拒 —— 否则任何插件都能声明 `owner`")
	}
	if err := r.DeclareProfile(Profile{Name: "qq.owner", Owner: "evil"}); err == nil {
		t.Fatal("别的插件声明 `qq.*` 必须被拒 —— 否则可以覆盖别人的 owner 档")
	}
}

// ④ 同一插件可以覆盖自己的档位（插件更新场景）。
func TestProfileOwnerCanOverwriteItsOwn(t *testing.T) {
	r := NewRuntime(nil)
	if err := r.DeclareProfile(Profile{Name: "qq.owner", Owner: "qq"}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeclareProfile(Profile{
		Name: "qq.owner", Owner: "qq",
		Policy: PrincipalPolicy{DenyTools: []string{"cmd_*"}},
	}); err != nil {
		t.Fatalf("自己覆盖自己应允许: %v", err)
	}
}

// ⑤ 激活**未声明**的档位必须报错，不能静默用默认档。
//
// 这是安全事故的经典形态：拼错档位名 → 静默回落到最宽松的策略。
func TestActivateUnknownProfileFailsLoudly(t *testing.T) {
	r := NewRuntime(nil)
	err := r.ActivateProfile("qq.ownr", time.Minute)
	if err == nil {
		t.Fatal("未声明的档位必须报错 —— 拼错不能静默放行")
	}
	if !strings.Contains(err.Error(), "qq.ownr") {
		t.Errorf("错误应指出具体档位名，实际: %v", err)
	}
}

// ⑥ 空档位名 = 显式"不用档位"（不是错误），并清掉之前激活的。
func TestEmptyProfileNameClearsActive(t *testing.T) {
	r := NewRuntime(nil)
	if err := r.DeclareProfile(Profile{
		Name: "qq.guest", Owner: "qq",
		Policy: PrincipalPolicy{DenyTools: []string{"cmd_*"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.ActivateProfile("qq.guest", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := r.ActivateProfile("", 0); err != nil {
		t.Fatalf("空档位名应是合法输入: %v", err)
	}
	if d := r.CheckTool("cmd_run"); !d.Allowed {
		t.Fatal("清除档位后应回到默认策略")
	}
}

// ⑦ 档位不得绕过**全局黑名单**（档位只能收窄）。
func TestProfileCannotBypassGlobalDenylist(t *testing.T) {
	r := NewRuntime(nil)
	r.SetPolicy(Policy{ToolDeny: []string{"cmd_rm_rf"}})
	if err := r.DeclareProfile(Profile{
		Name: "qq.owner", Owner: "qq",
		Policy: PrincipalPolicy{AllowTools: []string{"cmd_rm_rf"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.ActivateProfile("qq.owner", time.Minute); err != nil {
		t.Fatal(err)
	}
	if d := r.CheckTool("cmd_rm_rf"); d.Allowed {
		t.Fatal("档位的 allow 不得越过全局 blacklist —— 否则等于发提权口子")
	}
}

// ⑧ 档位可追加敏感词（内容审查），且随激活生效。
func TestProfileSensitiveWordsJoinContentScan(t *testing.T) {
	r := NewRuntime(nil)
	if err := r.DeclareProfile(Profile{
		Name: "qq.group", Owner: "qq",
		SensitiveWords: []string{"内部代号"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, blocked, _ := r.ScanContent(Inspection{Stage: "x"}, "这是内部代号"); blocked {
		t.Fatal("档位未激活时不应生效")
	}
	if err := r.ActivateProfile("qq.group", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, blocked, reason := r.ScanContent(Inspection{Stage: "x"}, "这是内部代号"); !blocked {
		t.Fatalf("档位敏感词应阻断，reason=%q", reason)
	}
}

// ⑨ 档位可收紧网络（guest 档不许出网）。
func TestProfileCanDisableNetwork(t *testing.T) {
	r := NewRuntime(nil)
	if err := r.DeclareProfile(Profile{Name: "qq.guest", Owner: "qq", DisableNetwork: true}); err != nil {
		t.Fatal(err)
	}
	if d := r.CheckNetwork(); !d.Allowed {
		t.Fatalf("档位未激活时不应限制网络: %s", d.Reason)
	}
	if err := r.ActivateProfile("qq.guest", time.Minute); err != nil {
		t.Fatal(err)
	}
	if d := r.CheckNetwork(); d.Allowed {
		t.Fatal("guest 档必须禁止网络")
	}
}

// ⑩ 档位自动过期：插件崩了不会留下永久放宽/收紧的幽灵策略。
func TestProfileExpires(t *testing.T) {
	r := NewRuntime(nil)
	if err := r.DeclareProfile(Profile{
		Name: "qq.guest", Owner: "qq",
		Policy: PrincipalPolicy{DenyTools: []string{"cmd_*"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.ActivateProfile("qq.guest", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if got := r.ActiveProfile(); got != "" {
		t.Fatalf("过期档位应失效，实际仍活跃 %q", got)
	}
}
