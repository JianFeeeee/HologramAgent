package test

// 「族处理器必须交给基座」的判据（2026-11-20 实测回归）。
//
// ## 事故
//
// 内核执行设施的 `dispatch` 按序问四处，第 ② 处是 `toolroute.Dispatch`
// （**族处理器**形态的工具）。而 `SetBuiltinTools` 全仓**零调用**
// ⇒ `BuiltinTools()` 恒 nil ⇒ 那一路是死的。
//
// 实测后果（真二进制）：
//
//	output_send__<通道>   异步通道（qq/wechat）上**唯一**能对用户说话的路径
//	                      ⇒ 全部落空 ⇒ 用户永远收不到回复，模型以为自己答过了
//	output_list_channels  → 兜底报错「不存在或未注册」
//	llm_list_sources      → 同上
//	secret_list           → 同上
//	get_plugin_tools      → 同上
//
// ## 判据为什么是源码级
//
// 行为层要真起内核 + 真发一条异步消息才能观察到（QQ/微信这类通道没有
// 同步回执）；而 `BuiltinTools()` 恒 nil 是**编译期就成立**的事实，
// 一条源码判据比一次端到端跑便宜得多。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// TestBuiltinHandlersAreInstalled：**装配之后** `BuiltinTools()` 必须非 nil。
//
// ★ 黑盒（2026-11-25 改写）：观测装配产物的**公开访问器**，不读源码文本。
//
// 原实现用 `os.ReadFile("mainagent.go")` + 正则钉 `mm.installBuiltinHandlers()`
// 这一行。它能抓到"调用被注释掉"，但代价是：
//
//	· 换个装配顺序（把调用挪进另一个文件/函数）就误红；
//	· 反过来，"调了但内部早 return"它看不见 —— 那才是真正会死人的形态。
//
// 现在直接看结果：主 agent 经 `registry.Instantiate`（生产同一入口）装配后，
// `BuiltinTools()` 必须有内容。少一个族 ⇒ `toolroute.Dispatch` 少一条路
// （`output_send__*` / `output_llm__*` / `secret_*` / `get_plugin_tools` 全不可执行）。
func TestBuiltinHandlersAreInstalled(t *testing.T) {
	a := newTypedAgent(t, agent.AgentConfig{
		ID: "builtin-wired", TypeName: agent.TypeNameMain,
	}, io.NewIOManager())
	t.Cleanup(a.Stop)

	if a.BuiltinTools() == nil {
		t.Fatal("★ 装配后 BuiltinTools() 为 nil —— " +
			"内核执行设施的族处理器那一路（② toolroute.Dispatch）是死的：" +
			"output_send__* / output_list_channels / llm_* / secret_* / get_plugin_tools 全部不可执行")
	}
}

// TestBuiltinHandlersCoverTheInterface 已删除（2026-11-25）。
//
// 它读 `builtin_handlers.go` 源码、比对 11 个方法名。而它自己的注释已经
// 写明："少一个方法 ⇒ 接口不满足 ⇒ 编译不过"。**编译期保证不需要测试** ——
// 那条判据只是把编译器已经做完的事用 grep 再做一遍，并且会误伤重构
// （方法改名/挪文件就红，而接口仍然满足）。
