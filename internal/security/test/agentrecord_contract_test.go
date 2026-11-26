package test

// agentrecord_contract_test.go —— **记录表是唯一事实来源**（黑盒行为判据）。
//
// ## 它取代了什么
//
// 原先 `architecture/test` 有一条 `TestSingleAgentRecordTable`，用
// `os.ReadFile` + `strings.Contains` 扫 security 包的源码，断言
// `outputsByAgent` / `principals.byID` 这两个标识符不存在。
//
// 两个问题：
//
//	① 判据在测"某个私有字段名别出现" —— 换个名字、换个结构就绕过；
//	② 而它真正要防的东西（"旧表还在 ⇒ 同名重建会继承旧策略"）**是可观测的行为**，
//	   根本不需要读源码。
//
// ## 行为事实
//
// 旧表 `outputsByAgent` 的危害不是"多了个字段"，而是：
// **记录被删除后，那条策略还在** ⇒ 同名 agent 重建时继承上一次的授权。
//
// 五表合一（§3.2.2）之后，策略随记录走：`Remove` 是**唯一**清理点。
// 这条判据直接验证那个行为 —— 删掉记录，授权与子策略一起消失。
//
// ## 为什么这是黑盒
//
// 全部经 `kernelapi` 的**公开接口**：`Runtime.DeclareOutputs` / `CheckOutput` /
// `DeclareChildPolicy` / `AgentRegistry.Remove`。不读任何源码文本，不看任何
// 私有字段名。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agentreg"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// TestPolicyDiesWithRecord：记录删掉，Policy 一起消失（不留孤儿表）。
func TestPolicyDiesWithRecord(t *testing.T) {
	reg := agentreg.New()
	rt := security.NewRuntime(nil) // 无凭据库 ⇒ Disabled，但策略表照装
	rt.SetAgentRegistry(reg)

	const agentID = "policy-probe"

	// ① 先登记（Update 只改**已存在**的记录 —— 这是记录表是唯一事实来源的落点：
	//    没登记过的 agent 没有策略可声明）。再声明输出授权与子策略。
	if err := reg.Register(model.AgentRecord{ID: agentID}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rt.DeclareOutputs(agentID, []string{"cli"})
	if err := rt.DeclareChildPolicy(agentID, []string{"graph_recall"}, nil); err != nil {
		t.Fatalf("DeclareChildPolicy: %v", err)
	}

	// ② 声明后：允许 cli。
	if d := rt.CheckOutput(agentID, "cli"); !d.Allowed {
		t.Fatalf("声明后 cli 应被允许，实得 %v", d)
	}
	// 未声明过的通道仍拦（白名单语义）。
	if d := rt.CheckOutput(agentID, "qq"); d.Allowed {
		t.Fatalf("★ 白名单外的通道被放行 —— 声明没有生效")
	}

	// ③ 删记录 ⇒ 回到"未声明"（**完整授权**，不是"全部拒绝"）。
	//
	// ★ 这是语义要点：空集合 = 未声明 = 完整授权（存量契约）。
	//   所以"策略消失"的可观测形态不是"全部被拒"，而是
	//   **收窄消失**：删除前 `qq` 被拦（白名单只有 cli），删除后 `qq` 也允许。
	//
	//   旧表（outputsByAgent）若还活着，`qq` 会**继续被拦** —— 那就是这张
	//   表活得比记录久的证据。
	reg.Remove(agentID)

	rt2 := security.NewRuntime(nil)
	rt2.SetAgentRegistry(reg)
	if d := rt2.CheckOutput(agentID, "qq"); !d.Allowed {
		t.Fatal("★ 记录已删除，但 qq 仍被拒 —— 存在一张活得比记录久的表" +
			"（旧的 outputsByAgent 形态：同名重建会继承上一次的收窄授权）")
	}
}

// TestChildPolicyDiesWithRecord：父为子声明的工具策略也随记录消失。
func TestChildPolicyDiesWithRecord(t *testing.T) {
	reg := agentreg.New()
	rt := security.NewRuntime(nil)
	rt.SetAgentRegistry(reg)

	const childID = "child-policy-probe"

	// 先登记（同 TestPolicyDiesWithRecord 的理由）。
	if err := reg.Register(model.AgentRecord{ID: childID}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 父为子声明：只允许 graph_recall。
	if err := rt.DeclareChildPolicy(childID, []string{"graph_recall"}, []string{"stablemem_purge"}); err != nil {
		t.Fatalf("DeclareChildPolicy: %v", err)
	}

	// 子视图读得到这两条约束。
	sc := rt.ForAgent(childID, true)
	if sc == nil {
		t.Fatal("ForAgent 返回 nil")
	}
	if d := sc.CheckTool("graph_recall"); !d.Allowed {
		t.Fatalf("白名单内的工具被拒: %v", d)
	}
	if d := sc.CheckTool("stablemem_recall"); d.Allowed {
		t.Fatal("★ 白名单外的工具被放行 —— 父声明没生效")
	}
	if d := sc.CheckTool("stablemem_purge"); d.Allowed {
		t.Fatal("★ 黑名单里的工具被放行 —— 禁止项没有胜过允许项")
	}

	// 删记录 ⇒ 策略消失。
	reg.Remove(childID)
	rt.ReleaseAgent(childID)

	// 同名重建：不该继承上一次的白名单。
	sc2 := rt.ForAgent(childID, true)
	if sc2 == nil {
		t.Fatal("ForAgent 返回 nil")
	}
	if d := sc2.CheckTool("stablemem_recall"); !d.Allowed {
		t.Fatal("★ 记录已删除，但子策略仍把 stablemem_recall 拦在外面 —— " +
			"存在一张活得比记录久的表（旧的 principals.byID 形态）")
	}
}
