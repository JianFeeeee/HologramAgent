package agent

// surface.go —— 基座对外的**最小构造面**（用户裁定 2026-11-18，§6.3）。
//
// ## 34 → 5
//
// 收敛前这里有 34 个访问器：`Provider` / `ProviderManager` / `DataDir` /
// `Sched` / `Sub` / `CancelTools` / `ToolMu` / `Impl` … 它们全都是
// **`AgentConfig` 公有字段的再导出**或**类型自己的状态**。
//
// 问题不是"多几个方法"，而是它们把基座的**内部结构**变成了类型可以依赖的
// 契约：任何一次字段搬家都得同步改 N 个类型模块。
//
// 现在只剩五个（分布在三个文件里）：
//
//	Config()   构造参数（归一化后，类型初始化时读一次）
//	Hooks()    槽位表（类型工厂"取—改—挂回"）
//	SetHooks() 挂回槽位
//	Ctx()      取消域（事件循环与类型共用同一个）
//	Cancel()   取消函数
//
// 其余一律**删掉**。类型的状态走它自己的实现体（`Impl`→删，改由类型模块
// 自己的闭包/结构持有）；基座字段走 `Config()`。

import "context"

// Ctx 返回取消域。
func (a *Agent) Ctx() context.Context {
	if a == nil {
		return context.Background()
	}
	return a.ctx
}

// Cancel 返回取消函数。
func (a *Agent) Cancel() context.CancelFunc { return a.cancel }
