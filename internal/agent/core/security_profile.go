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

// securityDecisionFor 用当前活跃档位 + 主体判定某工具是否可用。
//
// 工具执行前的统一检查点：调用方拿到 !Allowed 时应把 Reason 原样回给模型
// （统一拒绝语义 —— 模型据此能修正，而不是反复重试同一调用）。
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
//	  → 子 agent（resident/rtchild）继承 a.security
//	  → 本函数 → CheckTool
//
// 判据钉的是**装配**（kernelmod/all 的整机判据），不是本函数被手动调用后的行为。
func (a *Agent) securityDecisionFor(tool string) security.Decision {
	if a == nil || a.security == nil {
		return security.Decision{Allowed: true}
	}
	return a.security.CheckTool(tool)
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
