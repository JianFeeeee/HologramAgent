package security

// 把 **SDK 的声明面** 桥接到内核的 security.Runtime。
//
// 分工（用户 2026-10-26 的模型）：
//
//	插件（子进程/动态库）
//	  │  DeclareProfile(...) / InjectOptions.SecurityProfile = "qq.guest"
//	  ▼
//	proc 桥 / SDK 转发
//	  │
//	  ▼
//	security.Runtime.DeclareProfile / ActivateProfile   ← 内核持有与执行
//
// 为什么必须由内核持有：插件崩溃或被重载时，声明不能跟着消失
//（否则挂掉插件即可绕过策略）；且判定要与全局/主体策略走同一条链。

import (
	sec "github.com/JianFeeeee/HologramAgent/internal/security"
)

// DeclareProfile 把一个插件声明的命名限式登记进内核运行时。
//
// 语义与 sec.Profile 一致：名字必须是 `<owner>.<档位>`，同一 owner 可覆盖自己。
func DeclareProfile(rt *sec.Runtime, p sec.Profile) error {
	if rt == nil {
		return sec.ErrNoRuntimeForBridge
	}
	return rt.DeclareProfile(p)
}

// ActivateProfile 在 inputch 输入进来时激活插件选择的档位。
//
// 调用点约定：注入层拿到 `InjectOptions.SecurityProfile` 后调用一次。
// 空串 = 显式不使用档位（沿用内核默认策略），这是**可选参数**的缺省语义。
func ActivateProfile(rt *sec.Runtime, name string, ttlSeconds int) error {
	if rt == nil {
		return sec.ErrNoRuntimeForBridge
	}
	return rt.ActivateProfile(name, sec.TTLSeconds(ttlSeconds))
}
