package core

// 判据：**销毁路径真的撤销了子策略**（行为判据）。
//
// ## 为什么需要它（而不是只靠源码扫描）
//
// R2/R3 把"撤销策略"从两个停止函数搬进了骨架 `child.Registry.Cleanup`，
// 由各类型注入的 `StopDeps.RevokePolicy` 触发。这带来一个新的、**源码扫描
// 看不见**的风险：
//
//	StopDeps.RevokePolicy 漏填 ⇒ Cleanup 里那个 `if deps.RevokePolicy != nil`
//	                           直接跳过，静默不撤销
//
// 字符串 `revokeChildPolicy` 仍然出现在 `security_profile.go` 的定义体里，
// 所以源码扫描照样绿 —— 那正是结构判据的局限（engineering-lessons §1.1）。
//
// ## 判据语义
//
//	声明一条子策略 ⇒ 主体表里存在（前置）
//	走**真实销毁路径** ⇒ 主体表里消失（被测行为）
//
// 变红条件：
//   - rtStopDeps 漏填 RevokePolicy           ⇒ 红
//   - Cleanup 不调 deps.RevokePolicy         ⇒ 红
//   - teardownResidentWith 删掉撤销那一步    ⇒ 红

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// policyRTHost 造一个"父 agent + security 运行时 + 成对 rt 通道"。
//
// 复用 newRTHost 的注册方式，但多给一个 security 运行时（撤销策略要有地方落）。
func policyRTHost(t *testing.T) (*Agent, *security.Runtime) {
	t.Helper()
	rt := security.NewRuntime(nil)
	host, _ := newRTHost(t, &rtBlockingProvider{})
	host.security = rt
	return host, rt
}

// TestRTChild_StopRevokesPolicy 钉住 rt 的**单数**停止撤销策略。
func TestRTChild_StopRevokesPolicy(t *testing.T) {
	host, rt := policyRTHost(t)
	defer host.StopRTChildren()

	id := rtSpawn(t, host, "rp-1")
	if err := host.ProbeDeclareChildPolicy(id, nil, []string{"cmd_run"}); err != nil {
		t.Fatalf("声明子策略: %v", err)
	}
	if !rt.HasPrincipalPolicy(id) {
		t.Fatal("前置失败：声明后主体表里没有该子")
	}

	if err := host.StopRTChild(id); err != nil {
		t.Fatalf("StopRTChild: %v", err)
	}
	if rt.HasPrincipalPolicy(id) {
		t.Fatal("★ StopRTChild 之后主体策略仍在 —— StopDeps.RevokePolicy 可能漏填，" +
			"Cleanup 里的 nil 检查会静默跳过。同名重建将继承上一代父留的约束")
	}
}

// TestRTChildren_StopAllRevokesPolicy 钉住**复数**停止与单数对称。
//
// 这是 engineering-lessons.md §2.3「单数改了复数漏了」的行为版护栏：
// 源码扫描只能看到"函数里有没有那个字符串"，看不到"复数路径到底撤没撤"。
func TestRTChildren_StopAllRevokesPolicy(t *testing.T) {
	host, rt := policyRTHost(t)
	defer host.StopRTChildren()

	ids := []string{"rb-1", "rb-2", "rb-3"}
	for _, id := range ids {
		got := rtSpawn(t, host, id)
		if err := host.ProbeDeclareChildPolicy(got, nil, []string{"cmd_run"}); err != nil {
			t.Fatalf("声明 %s: %v", got, err)
		}
		if !rt.HasPrincipalPolicy(got) {
			t.Fatalf("前置失败：声明后主体表里没有 %s", got)
		}
	}

	if n := host.StopRTChildren(); n != len(ids) {
		t.Fatalf("StopRTChildren 停止 %d 个，期望 %d", n, len(ids))
	}
	for _, id := range ids {
		if rt.HasPrincipalPolicy(id) {
			t.Fatalf("★ StopRTChildren 之后主体策略仍在（%s）—— "+
				"复数路径与单数不对称（engineering-lessons §2.3 的形态）", id)
		}
	}
}

// TestResident_DestroyRevokesPolicy 钉住驻留子销毁撤销策略。
func TestResident_DestroyRevokesPolicy(t *testing.T) {
	parent, _, dir := newRootWith(t, &countingProvider{})
	rt := security.NewRuntime(nil)
	parent.security = rt

	const id = "res-rp"
	spawnTestResident(t, parent, dir, id)
	if err := parent.ProbeDeclareChildPolicy(id, nil, []string{"cmd_run"}); err != nil {
		t.Fatalf("声明子策略: %v", err)
	}
	if !rt.HasPrincipalPolicy(id) {
		t.Fatal("前置失败：声明后主体表里没有该驻留子")
	}

	if err := parent.DestroyResident(id); err != nil {
		t.Fatalf("DestroyResident: %v", err)
	}
	if rt.HasPrincipalPolicy(id) {
		t.Fatal("★ 驻留子销毁后主体策略仍在 —— 同名重建会继承上一代约束")
	}
}
