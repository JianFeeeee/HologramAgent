package security

import "testing"

// ★★★ 判据：**两个 agent 的 session 档互不干扰**（2026-10-31 用户裁定）。
//
// ## 防的失败方式
//
// 收敛前档位挂在 `Runtime.active`（进程级单例）上，多 agent 并发时串档：
//
//	主 agent 轮次 A：注入 qq.guest  →  active = "qq.guest"
//	驻留子 轮次 B：注入 qq.owner    →  active = "qq.owner"   ← 覆盖 A
//	主 agent 轮次 A 继续执行工具     →  判定按 owner 走      ← 串档
//
// 这不是理论风险：驻留子与 rtchild 与主 agent 同进程并发。
func TestAgentScopesDoNotShareSessionProfile(t *testing.T) {
	rt := NewRuntime(nil)
	for _, p := range []Profile{
		{Name: "probe.owner", Owner: "probe"},
		{Name: "probe.guest", Owner: "probe",
			Policy: PrincipalPolicy{DenyTools: []string{"cmd_*"}}},
	} {
		if err := rt.DeclareProfile(p); err != nil {
			t.Fatal(err)
		}
	}

	main := rt.ForAgent("main", false)
	child := rt.ForAgent("kid", true)

	rm, err := main.BeginRound(RoundInput{AgentID: "main", Payload: map[string]interface{}{
		"security_profile": "probe.guest",
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer rm.End()

	rc, err := child.BeginRound(RoundInput{AgentID: "kid", IsChild: true, Payload: map[string]interface{}{
		"security_profile": "probe.owner",
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.End()

	// ① 各自的生效档位必须是自己的。
	if got := main.EffectiveProfile(); got != "probe.guest" {
		t.Fatalf("主 agent 的档位应是 probe.guest，实际 %q —— 被别的 agent 覆盖了", got)
	}
	if got := child.EffectiveProfile(); got != "probe.owner" {
		t.Fatalf("子 agent 的档位应是 probe.owner，实际 %q —— 被别的 agent 覆盖了", got)
	}

	// ② 行为上：主 agent 仍被 guest 收窄，子 agent 不受影响。
	if d := main.CheckTool("cmd_run"); d.Allowed {
		t.Fatal("★ 主 agent 的 guest 档没生效 —— 它被别的 agent 的 owner 档盖掉了")
	}
	if d := child.CheckTool("cmd_run"); !d.Allowed {
		t.Fatalf("子 agent 是 owner 档，不该被 guest 的收窄影响: %s", d.Reason)
	}
}

// ★★★ 判据：**longtimeProfile 跨轮保留**，sessionProfile 随轮结束清除。
//
// 这是用户裁定的两层语义：
//
//	sessionProfile  —— 属于这一轮，End 即清
//	longtimeProfile —— 属于 agent 的生命，跨轮保留
//
// 优先级：session > longtime（session 是"这一轮的事实"，必须能收窄）。
func TestLongtimeProfileSurvivesRounds(t *testing.T) {
	rt := NewRuntime(nil)
	for _, p := range []Profile{
		{Name: "probe.guest", Owner: "probe",
			Policy: PrincipalPolicy{DenyTools: []string{"cmd_*"}}},
		{Name: "probe.owner", Owner: "probe"},
	} {
		if err := rt.DeclareProfile(p); err != nil {
			t.Fatal(err)
		}
	}

	sc := rt.ForAgent("bot1", true)
	if err := sc.SetLongtimeProfile("probe.guest"); err != nil {
		t.Fatalf("设置 long-time 档失败: %v", err)
	}

	// 第一轮：没带 session 档 ⇒ 用 long-time。
	r1, _ := sc.BeginRound(RoundInput{AgentID: "bot1", IsChild: true})
	if got := r1.EffectiveProfile(); got != "probe.guest" {
		t.Fatalf("无 session 档时应落到 long-time 档，实际 %q", got)
	}
	r1.End()

	// 第二轮：仍然有 long-time（不能因为 End 就丢了）。
	r2, _ := sc.BeginRound(RoundInput{AgentID: "bot1", IsChild: true})
	defer r2.End()
	if got := r2.EffectiveProfile(); got != "probe.guest" {
		t.Fatalf("★ long-time 档在轮次结束后丢了（%q）—— 它应跨轮保留", got)
	}

	// 第三轮：带 session 档 ⇒ session 优先（必须能收窄或放宽）。
	r3, err := sc.BeginRound(RoundInput{AgentID: "bot1", IsChild: true,
		Payload: map[string]interface{}{"security_profile": "probe.owner"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := r3.EffectiveProfile(); got != "probe.owner" {
		t.Fatalf("session 档应优先于 long-time 档，实际 %q", got)
	}
	// ★ 反向最危险：若 longtime 优先，长期 owner 的 agent 上访客声明的
	// guest 会被吞掉 ⇒ 访客拿到 owner 权限。
	if d := r3.CheckTool("cmd_run"); !d.Allowed {
		t.Fatalf("★ session=owner 时 cmd_* 应放行；若被 long-time 的 guest 卡住，"+
			"说明优先级写反了（反之则更危险）: %s", d.Reason)
	}
	r3.End()

	// 归位：session 清了，long-time 还在。
	if got := sc.EffectiveProfile(); got != "probe.guest" {
		t.Fatalf("session 结束后应回落到 long-time 档，实际 %q", got)
	}
}

// ★★★ 判据：agent 释放后 long-time 档**不得残留**。
//
// agent/子 agent 回收后同名重建是常规操作（`create id="bot1"`）。
// 残留会让新一代继承上一代父留的约束 —— 与主体策略撤销同一纪律。
func TestReleaseAgentDropsLongtimeProfile(t *testing.T) {
	rt := NewRuntime(nil)
	if err := rt.DeclareProfile(Profile{
		Name: "probe.guest", Owner: "probe",
		Policy: PrincipalPolicy{DenyTools: []string{"cmd_*"}},
	}); err != nil {
		t.Fatal(err)
	}

	sc := rt.ForAgent("bot1", true)
	if err := sc.SetLongtimeProfile("probe.guest"); err != nil {
		t.Fatal(err)
	}
	rt.ReleaseAgent("bot1")

	// 同名重建：必须是干净的视图。
	fresh := rt.ForAgent("bot1", true)
	if got := fresh.LongtimeProfile(); got != "" {
		t.Fatalf("★ 同名重建的 agent 继承了上一代的 long-time 档 %q —— "+
			"新一代用上一代父留的约束运行", got)
	}
	if d := fresh.CheckTool("cmd_run"); !d.Allowed {
		t.Fatalf("重建后的 agent 应无收窄，实际被拒: %s", d.Reason)
	}
}

// ★★★ 判据：未声明的档位名必须报错，且**不沿用**上一个档。
func TestLongtimeProfileRejectsUndeclared(t *testing.T) {
	rt := NewRuntime(nil)
	sc := rt.ForAgent("bot1", false)
	if err := sc.SetLongtimeProfile("probe.typo"); err == nil {
		t.Fatal("★ 未声明的档位名被接受了 —— 拼写错误会静默变成'无档位'")
	}
	if got := sc.LongtimeProfile(); got != "" {
		t.Fatalf("设置失败后不应留下任何档位，实际 %q", got)
	}
}
