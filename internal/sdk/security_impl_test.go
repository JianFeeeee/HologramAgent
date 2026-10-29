package sdk

// SecurityAPI 内核实现的判据。
//
// ## 钉什么
//
// 声明面只有三件事可能出错，且三种都**静默**：
//
//	① Owner 被插件伪造 ⇒ 能声明别人的档位（qq.owner 全放行）
//	② 名字前缀不对   ⇒ 档位登记成功但在别的 owner 名下（永远命中不到）
//	③ 未装配时静默成功 ⇒ 插件以为生效了，实际没登记
//
// 本文件的判据对每一条都要求**响亮失败**。

import (
	"strings"
	"testing"

	internalSecurity "github.com/JianFeeeee/HologramAgent/internal/security"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// withRuntime 装一个干净的 Runtime 供测试，结束后恢复。
func withRuntime(t *testing.T) *internalSecurity.Runtime {
	t.Helper()
	prev := internalSecurity.RuntimeGlobal()
	rt := internalSecurity.NewRuntime(nil)
	internalSecurity.SetRuntimeGlobal(rt)
	t.Cleanup(func() { internalSecurity.SetRuntimeGlobal(prev) })
	return rt
}

// ① 正常声明：登记到 Runtime，且 Owner 是内核给的名字。
func TestSecurityDeclareProfileRegistersUnderRealOwner(t *testing.T) {
	rt := withRuntime(t)
	api := NewSecurityAPI("qq")

	if err := api.DeclareProfile(pubsdk.SecurityProfile{
		Name:       "qq.guest",
		DenyTools:  []string{"cmd_*"},
		AllowTools: []string{"qq_*"},
	}); err != nil {
		t.Fatalf("声明应成功: %v", err)
	}

	if !contains(rt.Profiles(), "qq.guest") {
		t.Fatalf("档位未登记，实际已有: %v", rt.Profiles())
	}
}

// ② ★ Owner 不可伪造：插件声明别人的档位必须报错。
//
// 若能声明 `qq.owner` 且给 AllowTools=["*"]，等于把 QQ 的 owner 档位
// 改成全放行 —— 非所有者立刻能调用所有工具。
func TestSecurityDeclareProfileRejectsForeignOwner(t *testing.T) {
	withRuntime(t)
	api := NewSecurityAPI("evil")

	err := api.DeclareProfile(pubsdk.SecurityProfile{
		Name:       "qq.owner",
		Owner:      "qq", // 冒名
		AllowTools: []string{"*"},
	})
	if err == nil {
		t.Fatal("★ 冒名声明必须被拒绝 —— 否则任何插件都能把 qq.owner 改成全放行")
	}
	if !strings.Contains(err.Error(), "evil") {
		t.Errorf("错误信息应指出真实插件名，实际: %v", err)
	}
}

// ③ 名字必须带本插件前缀（否则登记了也命中不到）。
func TestSecurityDeclareProfileRequiresOwnerPrefix(t *testing.T) {
	withRuntime(t)
	api := NewSecurityAPI("qq")

	for _, bad := range []string{"guest", "qqguest", "qqq.guest", ""} {
		if err := api.DeclareProfile(pubsdk.SecurityProfile{Name: bad}); err == nil {
			t.Errorf("名字 %q 应被拒绝（必须形如 qq.xxx）", bad)
		}
	}
}

// ④ 未装配 security 时必须报错，不能静默成功。
//
// 静默成功的后果：插件以为档位生效，直到注入时带 SecurityProfile
// 才报"未声明档位"，此时排查方向已跑到"名字拼错"上。
func TestSecurityDeclareProfileFailsLoudlyWhenUnassembled(t *testing.T) {
	prev := internalSecurity.RuntimeGlobal()
	internalSecurity.SetRuntimeGlobal(nil)
	t.Cleanup(func() { internalSecurity.SetRuntimeGlobal(prev) })

	api := NewSecurityAPI("qq")
	err := api.DeclareProfile(pubsdk.SecurityProfile{Name: "qq.guest"})
	if err == nil {
		t.Fatal("★ 安全模块未装配时必须报错 —— 静默成功会让插件误以为策略已生效")
	}
	if !strings.Contains(err.Error(), "not assembled") {
		t.Errorf("错误信息应说明未装配，实际: %v", err)
	}
}

// ⑤ 批量声明：任一失败即整体报错，且错误带下标便于定位。
func TestSecurityDeclareProfilesReportsIndex(t *testing.T) {
	withRuntime(t)
	api := NewSecurityAPI("qq")

	err := api.DeclareProfiles([]pubsdk.SecurityProfile{
		{Name: "qq.owner"},
		{Name: "bogus.name"}, // 第 1 个（0-based）非法
	})
	if err == nil {
		t.Fatal("批量声明中任一失败应整体报错")
	}
	if !strings.Contains(err.Error(), "profile[1]") {
		t.Errorf("错误应带失败下标，实际: %v", err)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
