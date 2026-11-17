package agent

// type_main.go —— 主 agent 的**实例化调用**（用户裁定 2026-11-17）。
//
// > 「同时每个 agent 都有对应的实例化调用，没有被调用就没有这个 agent 的实例，
// >    内核启动后自动实例化一个主 agent」
//
// 本文件只登记"主 agent 怎么被造出来"：基座 `New` + **显式挂上标准实现**。
//
// ★ 为什么放在 agent 包而不是新建 internal/agent/main：
// 主 agent 的实现（上下文裁剪 / 蒸馏 / 归档 / 合并 / 复审 / 工具面）目前仍在
// 本包（24000 行待拆）。先由本包登记类型，等第 5/6 类搬包完成后，只需把
// `mainFactory` 整体挪进 `internal/agent/main` 并在那里 init 注册 —— 调用点
// （`kernelmod/agent` 的 `Instantiate(TypeNameMain, …)`）一行都不用改。
//
// 这就是"注册表 + 实例化调用"相对"硬编码构造"的直接收益。

func init() {
	// 主 agent：相关性裁剪（ContextBudget 由 New 从构造参数带上）
	// + 标准同步 loop + 同步 toolcall + 标准 stage。
	_ = RegisterType(TypeNameMain, func(cfg AgentConfig) (*Agent, error) {
		a := New(cfg)
		h := a.hooks
		// ── 七公共面：主 agent 的**标准实现**在这里一次性挂齐 ──
		// 上下文管理：相关性裁剪 + 动态预算（RelevanceContext 的策略）。
		if h.ContextBudget == nil {
			h.ContextBudget = ContextBudgetRelevance
		}
		if h.OverBudget == nil {
			h.OverBudget = OverBudgetRatio
		}
		if h.Prune == nil {
			h.Prune = PruneByQuery
			h.EnforceOverflow = EnforceOverflowStandard
		}
		h.Loop = LoopStandard
		if h.ToolRunner == nil {
			h.ToolRunner = SynchronousToolRunner{}
		}
		if h.RunStage == nil {
			h.RunStage = RunStageStandard
		}
		a.SetHooks(h)
		if err := a.Validate(); err != nil {
			return nil, err
		}
		return a, nil
	})
}
