package security

import "testing"

// ★★★ 判据：主体策略可**撤销**，且撤销后不影响其他主体。
//
// ## 为什么必须能撤销（2026-10-30）
//
// 主体 ID 对子 agent 来说是**调用方给的名字**（`create id="bot1"`）。
// 子被回收后同名重建是常规操作，而若旧策略留在表里：
//
//	新一代用上一代父当时留的约束运行 —— 而那个父可能已经不在了。
//
// 这与 rtchild 留档串线（`rtClosedTranscripts` 以模型给的名字为键、
// 全仓 0 处删除）是**同一种形态**：以调用方给的名称为键的表，
// 必须在生命周期结束时清理，否则既是语义错误也是单调增长。
func TestPrincipalPolicyCanBeRevoked(t *testing.T) {
	rt := NewRuntime(nil)
	if err := rt.DeclarePrincipalPolicy("id", "child-x", PrincipalPolicy{
		DenyTools: []string{"cmd_*"},
	}); err != nil {
		t.Fatal(err)
	}
	// 另声明一个，用来证明撤销不会误伤别人。
	if err := rt.DeclarePrincipalPolicy("id", "child-y", PrincipalPolicy{
		DenyTools: []string{"cmd_*"},
	}); err != nil {
		t.Fatal(err)
	}

	p := Principal{ID: "child-x", Kind: "resident"}
	if d := rt.CheckToolFor(p, "cmd_run"); d.Allowed {
		t.Fatal("前置：已声明的 deny 应生效")
	}

	if err := rt.RevokePrincipalPolicy("id", "child-x"); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if d := rt.CheckToolFor(p, "cmd_run"); !d.Allowed {
		t.Fatal("★ 撤销后仍被禁 —— 同名重建的子会继承上一代父留下的约束")
	}
	// 隔离：另一个主体的策略必须还在。
	if d := rt.CheckToolFor(Principal{ID: "child-y", Kind: "resident"}, "cmd_run"); d.Allowed {
		t.Fatal("★ 撤销 child-x 时误伤了 child-y —— 撤销必须只作用于指定 ID")
	}
}

// ★★★ 判据：撤销只支持 scope="id"（标签策略是部署级声明）。
//
// 允许撤销 tag 会让一个执行体的销毁顺手抹掉全站策略 —— 那是提权口子。
func TestRevokeRejectsTagScope(t *testing.T) {
	rt := NewRuntime(nil)
	if err := rt.DeclarePrincipalPolicy("tag", "external", PrincipalPolicy{
		HardPrivateTools: []string{"calendar_*"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := rt.RevokePrincipalPolicy("tag", "external"); err == nil {
		t.Fatal("★ 支持撤销 tag 策略 = 任何执行体销毁时都能抹掉全站策略")
	}
	// 原策略仍在。
	if d := rt.CheckToolFor(Principal{ID: "u1", Kind: "user", Tags: []string{"external"}}, "calendar_list"); d.Allowed {
		t.Fatal("tag 策略应仍然生效")
	}
}
