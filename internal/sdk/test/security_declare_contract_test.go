package test

// internal/sdk 的**策略声明面**判据 —— 面向接口的黑盒。
//
// 入口：`sdk.NewSecurityAPI(pluginName, rt)` → `DeclareProfile(p)`，
// 断言只看返回的 error 与 `rt.Profiles()` 的可观测结果。
//
// ## 原判据的缺口
//
// `public_contract_test.go` 覆盖的是反代鉴权归一化 / Host 标签 / Config Get-Put /
// 内置反代登记表。**策略声明面零覆盖** —— 而它是权限梯度的落点：
// 插件声明 `qq.owner` 档位，内核据此决定工具放行。
//
// 声明面失效的后果与"权限太松"同型但更难发现：插件写了档位、内核从没登记，
// 于是那份声明**静默不生效**，档位不限制任何东西。

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// profileNames 读出当前已登记的档位名。
func profileNames(rt *security.Runtime) []string { return rt.Profiles() }

// hasProfile 判断某档位是否已登记。
func hasProfile(rt *security.Runtime, name string) bool {
	for _, n := range profileNames(rt) {
		if n == name {
			return true
		}
	}
	return false
}

// ---- 判据 1：正常声明必须可观测 ----

// TestDeclareProfileIsObservable：插件声明自己名下的档位，必须能被查到。
//
// 最小闭环：声明 → 登记 → 可查询。这条不通过，后面所有权限行为都无从谈起。
func TestDeclareProfileIsObservable(t *testing.T) {
	rt := security.NewRuntime(nil)
	api := sdk.NewSecurityAPI("qq", rt)

	if err := api.DeclareProfile(pubsdk.SecurityProfile{Name: "qq.guest"}); err != nil {
		t.Fatalf("声明 qq.guest 失败: %v", err)
	}
	if !hasProfile(rt, "qq.guest") {
		t.Fatalf("★ 声明后查不到 qq.guest（现有 %v）—— 声明静默不生效", profileNames(rt))
	}
}

// ---- 判据 2：不许声明别人的档位（安全边界）----

// TestDeclareRejectsForeignOwner：Owner 填成别人时必须显式报错。
//
// ★ 为什么这是安全边界而不是参数校验：档位名全局唯一，Runtime 拒绝跨 owner
// 覆盖。若允许插件自报 owner，任何拿到声明面的插件都能声明 `qq.owner` 并给
// `AllowTools:["*"]` —— 直接把别人的档位改成全放行。
//
// 而且必须**报错**而不是静默改写：静默改会让作者以为自己声明成功了。
func TestDeclareRejectsForeignOwner(t *testing.T) {
	rt := security.NewRuntime(nil)
	api := sdk.NewSecurityAPI("pluginA", rt)

	// Owner 明确写成别人
	err := api.DeclareProfile(pubsdk.SecurityProfile{Name: "qq.owner", Owner: "qq"})
	if err == nil {
		t.Fatal("★ 插件 pluginA 声明 Owner=qq 的档位成功了 —— 越权面")
	}
	if !strings.Contains(err.Error(), "pluginA") || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("★ 报错没说清身份冲突（作者无法自助修复）：%v", err)
	}

	// 名字前缀不属于自己（即使不填 Owner）。
	//
	// 注：这一条**单独删掉 sdk 层的前缀检查不会变红** —— `security.ProfileRegistry`
	// 自己还有一道 `<owner>.` 前缀校验。两道闸互为兜底，属纵深防御；
	// 本判据钉的是"越权声明一定被拒"这个可观察后果，不钉由哪一层拒。
	if err := api.DeclareProfile(pubsdk.SecurityProfile{Name: "qq.staff"}); err == nil {
		t.Fatal("★ 插件 pluginA 声明 qq.staff 成功了 —— 前缀约束没生效")
	}

	// 两种越权尝试都不得留下登记
	if hasProfile(rt, "qq.owner") || hasProfile(rt, "qq.staff") {
		t.Fatalf("★ 越权声明留下了登记：%v", profileNames(rt))
	}
}

// ---- 判据 3：安全模块缺席时不得静默 ----

// TestDeclareFailsLoudlyWithoutRuntime：安全运行时为 nil 时必须报错。
//
// 这条对应"内核没装安全模块"的部署形态。若静默返回 nil，插件作者会以为
// 档位已经生效 —— 而实际上它不存在，所有档位约束全部落空。
//
// 源码注释里明确写了这个理由（"the profile would be silently unregistered"）。
func TestDeclareFailsLoudlyWithoutRuntime(t *testing.T) {
	api := sdk.NewSecurityAPI("qq", nil)
	err := api.DeclareProfile(pubsdk.SecurityProfile{Name: "qq.guest"})
	if err == nil {
		t.Fatal("★ 安全运行时为 nil 时声明返回成功 —— 档位从未登记，作者却以为生效了")
	}
	if !strings.Contains(err.Error(), "security") {
		t.Fatalf("★ 错误没指出是安全模块缺席：%v", err)
	}
}
