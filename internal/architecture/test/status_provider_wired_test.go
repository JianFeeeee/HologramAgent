package test

// 「状态面 provider 必须注入」的判据（2026-11-19 实测回归）。
//
// ## 事故
//
// 重构 agent 模块时，`wirePluginSDK` 里那行 `SetStatusProvider(a)` 被改成
// `_ = a`，理由是"状态面由类型注册 RegisterStatusSection 就够了"。
//
// 混淆了两件事：
//
//	RegisterStatusSection  类型往**段表**里挂自己那段快照
//	SetStatusProvider      注册表能**取到整个状态**（GetKernelStatus）
//
// 插件的 `s.Status().GetKernelStatus()` 走第二条。不注入 ⇒ cli 的
// `/status` `/kernel` `/runtime` 三个命令全部返回
// "status provider not available"（1:1 对照旧内核时现场复现）。
//
// ## 判据为什么是源码级
//
// 行为层要真起内核 + 起插件 + 敲 CLI 才能验，代价不划算；而这条约束
// 一旦被改回去，症状是"某个命令报一句英文"，最难查的那类。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// TestStatusProviderIsInjected：**装配之后**状态面可读（非零值、不 panic）。
//
// ★ 黑盒（2026-11-25 改写）：观测装配产物的**公开访问器**，不读装配点源码。
//
// 原实现用 `os.ReadFile("kernelmod/agent/mod.go")` + 正则钉
// `reg.SetStatusProvider(` 这一行。它抓不到真正会死人的形态：
// 调了、但传进去的东西什么都不返回。而那条正则换个写法（比如拆成两行）
// 也会误红 —— 判据在测**怎么写代码**，不是**行为对不对**。
func TestStatusProviderIsInjected(t *testing.T) {
	a := newTypedAgent(t, agent.AgentConfig{
		ID: "status-wired", TypeName: agent.TypeNameMain,
	}, io.NewIOManager())
	t.Cleanup(a.Stop)

	ks := a.GetKernelStatus()
	if ks == nil {
		t.Fatal("★ 装配后 GetKernelStatus() 为 nil —— " +
			"插件的 /status /kernel /runtime 会全部报 status provider not available")
	}
	// 最少要有身份与运行期信息，否则"可读"是假的（空结构体也是非 nil）。
	if ks.AgentID == "" {
		t.Fatal("★ 状态面无 AgentID —— 提供了个空壳，等于没接")
	}
	if ks.StartTime == "" {
		t.Fatal("★ 状态面无 StartTime —— 运行期段没接上")
	}
}
