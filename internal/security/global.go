package security

// 进程级单例与两个入向填充入口。
//
// ## 依赖方向（重要）
//
//	internal/security        ← 纯逻辑，不 import 任何 agent 包
//	    ↑
//	internal/agent/api       provider 装饰器（出向改写）
//	internal/agent/core      工具参数/载荷填充（入向回填）
//
// 是 DAG，**没有 import 环**。所以装饰器可以直接用内核的 Provider 接口，
// 不必做接口投影。（初版误以为会成环而写了一层投影接口，已删除。）
//
// ## 为何单例必须是进程级
//
// 已核：主 / 子 / 驻留子 agent 共享同一个 ProviderManager
//（`resident.go:185`；manager 全进程只建一份 `bootstrap.go:249`）。
// 所以变量库挂在 provider 层天然就是进程级 —— **不需要跨 agent 同步**。

import ()

// ★ 2026-11-04：进程级单例 `global *Store` 已删除。
//
// ## 为什么删
//
// 它绕开了内核的**统一挂载点**（`kstate.State.Security`）。变量库只有一个
// 事实来源：security 模块产出的 Runtime，经 Assembly 传进
// `AgentConfig.Security` / `ProviderManager.SetSecretStore` /
// `plugin.Registry.SetSecretStore`，各消费方从自己的注入面取。
//
// ## 单例的危害（不是"多一个全局变量"这么轻）
//
// 本 agent 的 `a.security` 与 `security.Global()` 是**两个独立变量**。
// 正常装配下它们恰好指向同一份数据，但"恰好"不是保证：
//
//   · 测试替身：测试装单例、被测代码用 agent 字段 ⇒ 测试测的是单例
//   · 多 Runtime：两个内核实例（或热重载）时单例只有一份，另一方被覆盖
//   · 装配顺序：单例装晚了，先注册的 provider 就没被装饰（静默漏一条出网路径）
//
// 最后一条曾真实存在：`ProviderManager.Register` 在注册时读单例套装饰器，
// 而 llm 模块原本**不声明** FacSecurity 依赖 —— 顺序正确纯属巧合。
// 现在 llm 模块声明了 FacSecurity，且注入必须早于 Register。

// ---------------- 入向填充（插件无感知的关键） ----------------

// ★ 2026-11-04：`FillToolArgs` / `FillPayload` 已删除（死代码）。
//
// 它们读 `Global()` 单例做入向回填，但**生产调用者为 0**：
// `core.StageHost.ExecuteToolCtx` 已直接用注入的 Store 做同一件事
// （见 stages.go 的 `h.secretStore().FillMap(args)`）。
//
// 只留一个测试调用它们 ⇒ 测的是没人用的代码路径，绿了也不代表任何
// 生产行为正确（engineering-lessons §1.1「判据要钉真实路径」）。

// MustStore 构造并加载一个变量库（装配/测试用），失败直接返回 error。
//
// ★ 存在的理由（2026-11-04）：包级单例删除后，"New + Load" 这对操作在
// 装配代码与十几个测试里重复出现。抽成函数让调用点保持一行，
// 也避免各人各写一遍（漏 Load 的症状是"库看起来在、但读不到已有条目"）。
//
// 注意：本函数**不**注册任何全局单例 —— 返回值由调用方持有并注入。
func MustStore(opt Options) (*Store, error) {
	s := New(opt)
	if err := s.Load(); err != nil {
		return nil, err
	}
	return s, nil
}
