package core

// agent 侧与安全模块的**接线点**。
//
// ## 边界（2026-10-31 收敛后的形态）
//
// security 是**外围内核到核心内核之间的防火墙**：它自己提供接口、自己做判定。
// agent 只负责两件事：
//
//	① 提供**事实**：谁在跑、有没有父、原始输入载荷
//	② 在输入开始/结束时各调一次 security 的轮次入口
//
// ★ 本文件**不得**再出现任何策略语义：
//
//	✗ 不该解析 `security_profile`（那是 security 的协议）
//	✗ 不该判断“我该走 CheckTool 还是 CheckToolFor”（那是策略决策）
//	✗ 不该声明自己是哪一类主体（Kind 由 security 自己推）
//	✗ 不该拼 `PrincipalPolicy{...}` 字面量（那是策略结构）
//
// 这些全部已收回到 `internal/security`：
//
//	security.RoundInput   —— 调用方能提供的全部事实（无 Kind）
//	security.BeginRound   —— security 解析档位 + 判定主体类别
//	security.Round        —— 本轮句柄，只暴露 CheckTool / End
//	security.DeclareChildPolicy / RevokeChildPolicy —— 子策略的生命周期
//
// ## 为什么这样收（教训）
//
// 收敛前本文件有 214 行，其中绝大部分是策略逻辑。它带来三个后果：
//
//	· 三条输入路径（主 agent / 驻留子 / rtchild）各自得接一遍，接漏一条就静默失效
//	· 判据只能钉住“接线之后行为对”，钉不住“有没有接线”
//	· `Kind: "resident"` 让调用方**声明自己的类别** ⇒ 提权口子
//
// 对应的成功范例就在树里：`events.Bus.SetGuard` + `security.AsEventGuard` ——
// 总线只交事件，guard 自己判，总线一行策略都没有。

import (
	"log"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// beginSecurityRound 在一条输入开始处理时调用：
// 把**原始事实**交给 security，拿回本轮句柄。
//
// 调用方必须在轮次结束时 `End()`（defer）。未装配 security 时返回一个
// 零成本句柄（所有判定放行），与收敛前行为一致。
//
// ★ 为什么由 agent 调而不是 io 层调：轮次的生命周期由 agent 的
// `prepareInputTask` 界定（那里才知道一条输入何时真正开始/结束）。
// security 不依赖 agent，所以只能由 agent 主动把边界告诉它。
func (a *Agent) beginSecurityRound(payload map[string]interface{}, source string) *security.Round {
	if a == nil || a.security == nil {
		return nil
	}
	rc, err := a.security.BeginRound(security.RoundInput{
		AgentID: string(a.id),
		// IsChild 是**事实**（有父就是子），不是类别结论。
		IsChild: a.parentID != "",
		Payload: payload,
		Source:  source,
	})
	if err != nil {
		// security 内部已记录原因并把档位归位。这里不阻断输入：
		// 注入是 fire-and-forget，插件看不到内核错误，丢输入比降级更糟。
		// 但**必须留痕**，否则“档位一直没生效”会完全静默。
		a.securityRoundDegraded(err)
	}
	return rc
}

// securityRoundDegraded 记录“本轮策略降级”这一事实。
//
// 独立成方法是为了让降级有一个**可被检索的日志锚点**：
// 收敛前这条链断了整整一轮发布，而没有任何一行日志提示。
func (a *Agent) securityRoundDegraded(err error) {
	if a == nil {
		return
	}
	log.Printf("[security] round policy degraded for agent %q: %v "+
		"(continuing with the default global policy)", a.id, err)
}

// securityDecisionFor 判定某工具是否可用（**兼容入口**）。
//
// ## 它现在只做转发，不含任何策略
//
// 收敛前这里写着两条分支（父走 CheckTool、子走 CheckToolFor）并硬编码
// `Kind: "resident"`。现在那两条判断都在 security 内部
// （`classifyPrincipal`），本函数退化成一次裸转发。
//
// ★ 保留它的理由：工具执行链（`executeToolCallInner`）需要一次判定，
// 而那里只持有 `*Agent`、不持有轮次句柄。走 security 的**默认轮次**
// （按 `AgentID` + 有无父自动分类）等价于原来“父/子分流”的行为，
// 但语义只写在 security 一处。
func (a *Agent) securityDecisionFor(tool string) security.Decision {
	if a == nil || a.security == nil {
		return security.Decision{Allowed: true}
	}
	// ★ 走本 agent 的**执行视图**（session 档 > longtime 档 > 全局基线）。
	//
	// 这是 2026-10-31「每个 agent 一个实例」的落地：视图由 security 按
	// agentID 持有，本 agent 的 sessionProfile 不会污染别的 agent。
	return a.securityScope().CheckTool(tool)
}

// securityScope 返回（并缓存）本 agent 的策略执行视图。
//
// 为什么要缓存：`ForAgent` 是幂等的，但每条输入/每次工具调用都取一次会
// 反复加锁；而视图本身在整个 agent 生命周期内不变。
func (a *Agent) securityScope() *security.AgentScope {
	if a == nil || a.security == nil {
		return nil
	}
	a.securityScopeOnce.Do(func() {
		a.securityScopeView = a.security.ForAgent(string(a.id), a.parentID != "")
	})
	return a.securityScopeView
}

// ReleaseSecurityScope 在 agent 销毁时释放它的策略视图。
//
// ★ 必须调：视图里存着 **longtimeProfile**（跨轮保留的档位）。
// agent/子 agent 回收后同名重建是常规操作，残留的 long-time 档会让
// 新一代继承上一代父留的约束 —— 与 `RevokePrincipalPolicy` 同一纪律。
func (a *Agent) ReleaseSecurityScope() {
	if a == nil || a.security == nil {
		return
	}
	a.security.ReleaseAgent(string(a.id))
}

// declareChildPolicy 让父为刚创建的子执行体声明主体策略。
//
// 这里只做转发：策略结构的组装与作用域落点由 security 负责
// （`security.DeclareChildPolicy`）。
func (a *Agent) declareChildPolicy(childID string, allow, deny []string) error {
	if a == nil || a.security == nil {
		return nil
	}
	return a.security.DeclareChildPolicy(childID, allow, deny)
}

// revokeChildPolicy 在子执行体销毁时撤销父为它声明的策略。
func (a *Agent) revokeChildPolicy(childID string) {
	if a == nil || a.security == nil {
		return
	}
	a.security.RevokeChildPolicy(childID)
}

// SetSecurity 注入安全运行时（**仅限测试与独立工具**）。
//
// ## 不要在生产装配里调它
//
// 生产路径是 `AgentConfig.Security`（构造期落位），因为那一条由依赖图
// 保证 `ks.Security` 已就绪（`kernelmod/agent` 的 `Requires()` 声明了
// `FacSecurity`）。用 SetSecurity 事后补会造出一个图看不见的隐式依赖 ——
// 正是本仓反复踩的那个坑。
//
// 保留它是因为进程内判据需要一个「只换安全运行时」的开关，
// 而它们不跑整机装配。
//
// ## 为什么允许 nil
//
// 测试与独立工具（memgc 等）不需要安全模块；传 nil 表示显式不启用。
func (a *Agent) SetSecurity(rt *security.Runtime) {
	if a == nil {
		return
	}
	a.security = rt
}

// ───────────────────── 判据专用探针（非生产接口） ─────────────────────
//
// 下面几个函数只为 `internal/kernelmod/all/security_wiring_test.go` 服务。
//
// ## 为什么不写在 _test.go 里
//
// 判据住在 `kernelmod/all` 包（那里才有真实登记表与整机装配），
// 而 Go 的 `_test.go` 符号不跨包导出 ⇒ 跨包判据必须有一个导出面。
//
// ## 为什么叫 Probe*
//
// 这是**测试专用**接口。生产代码不应调用它们 —— 需要判定时用
// `securityDecisionFor`（工具执行链上的那条）。带 Probe 前缀是为了让
// "有人在生产里调了它"在 code review 时一眼可见。

// ProbeSecurityRuntime 返回本 agent 持有的安全运行时（判据用）。
func (a *Agent) ProbeSecurityRuntime() *security.Runtime {
	if a == nil {
		return nil
	}
	return a.security
}

// ProbeSecurityDecision 判定工具可用性（判据用）。
//
// 它走的就是生产路径（securityDecisionFor），不是另写一份逻辑。
func (a *Agent) ProbeSecurityDecision(tool string) security.Decision {
	return a.securityDecisionFor(tool)
}

// ProbeSecurityRound 按真实输入路径开一轮，返回句柄（判据用）。
//
// 判据用它验证「档位由 security 自己从 payload 解析」这条链真的通了，
// 而不需要直接调 security 的内部方法。
func (a *Agent) ProbeSecurityRound(payload map[string]interface{}, source string) *security.Round {
	return a.beginSecurityRound(payload, source)
}

// ProbeDeclareChildPolicy 是 declareChildPolicy 的导出包装（判据用）。
func (a *Agent) ProbeDeclareChildPolicy(childID string, allow, deny []string) error {
	return a.declareChildPolicy(childID, allow, deny)
}

// ProbeRevokeChildPolicy 是 revokeChildPolicy 的导出包装（判据用）。
func (a *Agent) ProbeRevokeChildPolicy(childID string) {
	a.revokeChildPolicy(childID)
}
