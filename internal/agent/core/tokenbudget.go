package core

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// TokenBudget 上下文 token 预算分配结果
type TokenBudget struct {
	MaxContext    int // 模型窗口上限
	TargetUsage   int // 目标使用量（受 maxTargetTokens 与 utilizationRate 共同约束）
	FixedTokens   int // 固定部分（system prompt base + tools + rules）
	MemoryTokens  int // memory context 可用预算
	ContextTokens int // 上下文事件可用预算
	Reserved      int // 预留（response 空间）
}

// EstimateTokens / TruncateByTokens 转发到编解码层统一出口。
//
// 本包内调用点很多（context.go / process.go / resident.go / tooldefs.go…），
// 而实现只有一份（api 包，C 化后可选走 C）。保留这两个同名转发，
// 是为了不把调用点全部改写成 api.EstimateTokens —— 那场改动对行为零收益，
// 却把「本包依赖 api」这件事铺得到处都是。

// EstimateTokens 粗略估算 token 数（转发到 api.EstimateTokens）。
func EstimateTokens(text string) int { return api.EstimateTokens(text) }

// TruncateByTokens 截断字符串至不超过 maxTokens 估计值（转发到 api.TruncateByTokens）。
func TruncateByTokens(s string, maxTokens int) string { return api.TruncateByTokens(s, maxTokens) }

// ContextTuning 是上下文预算的**可调参数**（零值 = 历史默认）。
//
// 为何要有它：这些阈值原本全部硬编码在 ComputeTokenBudget 里，
// 配置面板上一个都没有 —— 用户无法为不同窗口（如 200k）调优，
// 也无法做「同窗口下不同策略」的对照实验。
//
// ★ 两个常量叠加会合成一个**没人看得见的分段函数**：
//
//	窗口 ≤ 750000  → 工作面 = 窗口 × 0.8      （80%）
//	窗口 > 750000  → 工作面 = 600000          （利用率退化成 600000/窗口）
//
// 拐点位置由 0.8 与 600000 **共同**决定，改任一个都会挪动它。
// 现在拐点由 MaxTargetTokens 显式表达，而 MaxTargetTokens < 0 表示不封顶
// （利用率对所有窗口都真的等于 UtilizationPercent）。
//
// 零值语义（升级安全）：全部字段为 0 时，结果与硬编码时代**逐值相同**。
// 详见 context_tuning_test.go 的 TestContextTuningZeroValueEqualsLegacy。
type ContextTuning struct {
	// UtilizationPercent 是目标窗口利用率（百分比，1-100）。
	// 0 ⇒ 用历史默认 80。
	UtilizationPercent int
	// MaxTargetTokens 是**工作区间**的绝对上限（不是模型窗口）。
	//
	//	0   ⇒ 用历史默认 600000
	//	< 0 ⇒ 不封顶（工作面纯由 UtilizationPercent 跟随窗口）
	//	> 0 ⇒ 该值作为上限
	//
	// 为何不是模型窗口本身：标称窗口 ≠ 有效窗口。接近满窗口时注意力涣散，
	// 成本与延迟也随 prompt 线性上升。该源实测 990,034 token 仍能返回，
	// 但 600K 才是它的最优工作区间。
	MaxTargetTokens int
	// MemoryRatioPercent 是记忆上下文占「可用预算」的百分比（1-99）。
	//
	//	0      ⇒ 用历史默认：available / 3（整数除法，逐值不变）
	//	1-99   ⇒ available × pct / 100
	//
	// ⚠️ 0 特意不换算成 33%：实测 available/3 与 available×33/100 **不等价**
	//（1000 时是 333 vs 330），换算会让默认值不再等价。
	MemoryRatioPercent int
	// ProtectedCount 是裁剪时**无条件保留**的最近事件条数。
	//
	// 它决定「近处记忆」与「向量检索」的权：条数越大越不容易丢近处信息，
	// 越小越依赖检索准确度。0 ⇒ 用历史默认 10。
	ProtectedCount int
}

// 历史默认值（硬编码时代的常量，改这里等于改所有未配置实例的行为）。
const (
	defaultUtilizationPercent = 80
	defaultMaxTargetTokens    = 600000
	defaultProtectedCount     = 10
	// legacyMemoryDivisor 是记忆预算的历史除数（available/3）。
	legacyMemoryDivisor = 3
)

// ComputeTokenBudget 计算各部分的 token 预算（零值 tuning = 历史行为）。
//
// 保留这个签名是为了不改既有调用点；需要调参的调用方用 ComputeTokenBudgetTuned。
func ComputeTokenBudget(provider api.Provider, systemPromptBase string) TokenBudget {
	return ComputeTokenBudgetTuned(provider, systemPromptBase, ContextTuning{})
}

// computeTokenBudget 用本 agent 的配置调参计算上下文预算。
//
// 存在理由：ComputeTokenBudget 是包级函数、拿不到 agent 的配置；
// 若调用点各自传参很容易漏（本仓刚发现 core.agent.max_context_size
// 被注册进面板却从未被读取 —— 一个不报错的死配置）。
// 收成方法后，调用点只需 a.computeTokenBudget()。
func (a *Agent) computeTokenBudget() TokenBudget {
	return ComputeTokenBudgetTuned(a.provider, a.systemPrompt, a.ctxTuning)
}

// ComputeTokenBudgetTuned 按配置计算各部分的 token 预算。
//
// 固定部分（system prompt + tools + rules）优先保障，剩余按 MemoryRatioPercent
// 切给 memory context 与 context events。
func ComputeTokenBudgetTuned(provider api.Provider, systemPromptBase string, t ContextTuning) TokenBudget {
	// provider 可能为 nil（构造期、或 provider 尚未就绪/已被换掉）。
	// 之前直接调 provider.MaxContextTokens() 会 SIGSEGV —— 实测由contextTopK
	// 在测试里撞出来，但它同样会在「配置早于 provider 就绪」的启动序列上发生。
	var maxCtx int
	if provider != nil {
		maxCtx = provider.MaxContextTokens()
	}
	if maxCtx <= 0 {
		maxCtx = 32768
	}

	utilPct := t.UtilizationPercent
	if utilPct <= 0 {
		utilPct = defaultUtilizationPercent
	}
	if utilPct > 100 {
		utilPct = 100
	}
	// 整数运算：实测 maxCtx×80/100 与 int(float64(maxCtx)×0.8)
	// 在 42 万个窗口值上逐值相同，且不受浮点表示误差影响。
	targetUsage := maxCtx * utilPct / 100

	// 上限：0=历史默认（600000），负=不封顶。
	cap := t.MaxTargetTokens
	if cap == 0 {
		cap = defaultMaxTargetTokens
	}
	if cap > 0 && targetUsage > cap {
		targetUsage = cap
	}

	reserved := maxCtx - targetUsage
	if reserved < 0 {
		reserved = 0
	}

	fixedTokens := EstimateTokens(systemPromptBase)

	available := targetUsage - fixedTokens
	if available < 0 {
		available = 0
	}

	memTokens := available / legacyMemoryDivisor
	if t.MemoryRatioPercent > 0 {
		pct := t.MemoryRatioPercent
		if pct > 99 {
			pct = 99
		}
		memTokens = available * pct / 100
	}
	// 余数归 context：保证 mem + ctx == available（不因取整丢 token）。
	ctxTokens := available - memTokens

	return TokenBudget{
		MaxContext:    maxCtx,
		TargetUsage:   targetUsage,
		FixedTokens:   fixedTokens,
		MemoryTokens:  memTokens,
		ContextTokens: ctxTokens,
		Reserved:      reserved,
	}
}

// TruncateByTokens 已移至 api 包（编解码层统一出口，见 codec.go）。
// 上方已有同名转发，此处不再重复定义。
