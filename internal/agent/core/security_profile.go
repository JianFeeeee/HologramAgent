package core

// 插件声明限式的**生效点**：inputch 输入进来时激活，轮次内据此判定。
//
// ## 为什么在这里（而不是在工具调用时）
//
// 插件在注入时带上 `security_profile`，那一刻是**唯一**能确定
// "这条输入属于哪个身份档位"的时刻。等 LLM 决定调工具时，插件早已
// 不在调用栈上（异步通道 + 多轮工具循环），档位信息就断了。
//
// 所以：输入到达 → 激活档位（带 TTL）→ 该轮的工具/内容判定都读它。
//
// ## 缺省语义
//
// 没有携带 `security_profile` = **不使用档位**（沿用内核默认策略）。
// 这是可选参数：不声明档位的插件行为与之前完全一致。
//
// 携带了**未声明**的档位名 ⇒ 记日志并回退到默认策略，但**不改写输入**：
// 注入是 fire-and-forget，插件看不到内核错误；至少要在日志里留下
// 「你声明的档位没生效」的痕迹（与 cleaner_name 同一纪律）。

import (
	"log"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// activateSecurityProfile 按输入 payload 激活插件声明的安全档位。
//
// 返回被激活的档位名（空 = 未使用档位），仅供日志/诊断。
func (a *Agent) activateSecurityProfile(payload map[string]interface{}) string {
	if a == nil || a.security == nil || payload == nil {
		return ""
	}
	name, _ := payload["security_profile"].(string)
	if name == "" {
		return ""
	}
	if err := a.security.ActivateProfile(name, 0); err != nil {
		// 响亮但不断流：拼错档位名不该让整条输入丢失，
		// 但也绝不能静默 —— 那等于悄悄降级到默认策略。
		log.Printf("[agent] input declared security_profile=%q but activation failed: %v", name, err)
		return ""
	}
	return name
}

// securityDecisionFor 判定某工具是否可用。
//
// ## 两条路径（为什么不能只有一条）
//
//	普通 agent（主 agent）  → Runtime.CheckTool(tool)
//		全局策略 + 活跃档位 + 轮次作用域。它代表**内核自己**，不受“某个主体”约束。
//
//	子 agent（resident/rtchild）→ Runtime.CheckToolFor(Principal{self}, tool)
//		额外叠加**父在创建它时为它声明的主体策略**（见 SpawnResident /
//		SpawnRTChild 里的 DeclarePrincipalPolicy）。
//
// ★ 为什么子不能只走 CheckTool：那等于“子继承父的**全部**权限”。
// 正确模型是 **父为子声明策略**（用户 2026-10-30 裁定）：
// 父在创建时决定这个子能做什么，内核持有并执行，子不能自己扩大。
// 而 CheckToolFor 内部**已经**先调了 CheckTool（全局 deny 优先），
// 所以不存在“主体策略绕过全局黑名单”的缺口。
//
// ## 历史（两轮都错，值得记住）
//
// ★ 第一轮（2026-10-29）的诊断是「`a.security` 从未被赋值」，于是补了
// `SetSecurity()`。但**生产装配链从来没调它** —— 唯一的调用方是 4 条测试。
// 那 4 条判据全绿，因为它们在测试里**手动**调了 `a.SetSecurity(...)`：
// 判据自己把缺失的那根线补上了，所以永远绿。
//
// ★ 第二轮（2026-10-30）才看清根因：`kernelmod/agent` 的 `Requires()`
// **没有声明 `FacSecurity`**，依赖图就没有理由把 security 的产物交给
// agent（只有声明了的 device 拿到了）。所以这不是"少一行赋值"，
// 而是"模块边界没连上"。
//
// 现在的链路：
//
//	security.Init → ks.Security
//	  → buildAgent 传 AgentConfig.Security
//	  → New() 写 a.security
//	  → 子 agent（resident/rtchild）继承同一个 Runtime 实例
//	  → 但**权限**不继承：父在创建时为每个子单独声明主体策略
//	  → 本函数按“自己是不是子”选对应判定入口
//
// 判据钉的是**装配**（kernelmod/all 的整机判据）与**创建时的策略声明**，
// 不是本函数被手动调用后的行为。
func (a *Agent) securityDecisionFor(tool string) security.Decision {
	if a == nil || a.security == nil {
		return security.Decision{Allowed: true}
	}
	// 子 agent：叠加父为它声明的主体策略。
	//
	// ★ 主体 Kind 用 "resident"（IsInternal() 为真）：
	//   · 它确实属于内部执行体（父的委派），所以 HardPrivateTools 不拦它
	//     （那条防的是**用户主体**偷看私人数据，内部委派本就有完整权限）
	//   · 但父声明的 DenyTools / AllowTools **仍然生效**（都是收窄方向）
	//   · 若父没为它声明任何策略（lookup 为空）⇒ 行为与 CheckTool 一致
	if a.parentID != "" {
		return a.security.CheckToolFor(security.Principal{
			ID:   string(a.id),
			Kind: "resident",
		}, tool)
	}
	return a.security.CheckTool(tool)
}

// declareChildPolicy 让**父**为刚创建的子 agent 声明主体策略。
//
// ## 为什么必须在创建点、由父做（用户裁定 2026-10-30）
//
// 子 agent 没有配置中心句柄（轻量内核刻意不接），也没有“自己决定权限”的
// 合理性 —— 权限只能来自委派者。这与 `PreferredProvider` 同一道理：
// “哪个源/多少权限”是**委派策略**，本来就该在创建点决定。
//
// ## 收窄的方向
//
// 声明只能**收窄**：
//
//	DenyTools   追加禁止（父自己能用、但不给这个子用）
//	AllowTools  白名单（非空时，子只能用这些）
//	HardPrivateTools  由主体模型处理（内部主体不被它拦）
//
// 它**不能扩大**：CheckToolFor 内部先跑 CheckTool（全局 deny 优先），
// 所以父的黑名单对子依然有效。
//
// childID 未声明策略时返回 nil（合法：父选择不给额外限制）。
func (a *Agent) declareChildPolicy(childID string, allow, deny []string) error {
	if a == nil || a.security == nil {
		// 父自己没有安全运行时（独立工具/单测）⇒ 不为子声明，保持无限制。
		return nil
	}
	if len(allow) == 0 && len(deny) == 0 {
		return nil
	}
	return a.security.DeclarePrincipalPolicy("id", childID, security.PrincipalPolicy{
		AllowTools: allow,
		DenyTools:  deny,
	})
}

// revokeChildPolicy 在子 agent 销毁时撤销父为它声明的策略。
//
// ★ 不撤销的后果（与今天修的 rtchild 留档串线同型）：
// 主体 ID 是调用方给的名字，子回收后**同名重建**是常规操作，
// 而旧策略留在表里 => 新一代用上一代父留的约束运行。
// 且表会随 create/destroy 单调增长。
func (a *Agent) revokeChildPolicy(childID string) {
	if a == nil || a.security == nil || childID == "" {
		return
	}
	_ = a.security.RevokePrincipalPolicy("id", childID)
}

// SetSecurity 注入安全运行时（**仅限测试与独立工具**）。
//
// ## 不要在生产装配里调它
//
// 生产路径是 `AgentConfig.Security`（构造期落位），因为那一条由依赖图
// 保证 `ks.Security` 已就绪。用 SetSecurity 事后补会造出一个图看不见的
// 隐式依赖 —— 正是本仓反复踩的那个坑。
//
// 保留它是因为 4 条进程内判据需要一个"只换安全运行时"的开关，
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

// ProbeSecurityDecision 按**本 agent 的身份**（是否子 agent）判定工具可用性（判据用）。
//
// 它走的就是生产路径（securityDecisionFor），不是另写一份逻辑。
func (a *Agent) ProbeSecurityDecision(tool string) security.Decision {
	return a.securityDecisionFor(tool)
}

// ProbeDeclareChildPolicy 是 declareChildPolicy 的导出包装（判据用）。
func (a *Agent) ProbeDeclareChildPolicy(childID string, allow, deny []string) error {
	return a.declareChildPolicy(childID, allow, deny)
}

// ProbeRevokeChildPolicy 是 revokeChildPolicy 的导出包装（判据用）。
func (a *Agent) ProbeRevokeChildPolicy(childID string) {
	a.revokeChildPolicy(childID)
}
