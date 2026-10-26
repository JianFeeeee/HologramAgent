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
func (a *Agent) securityDecisionFor(tool string) security.Decision {
	if a == nil || a.security == nil {
		return security.Decision{Allowed: true}
	}
	return a.security.CheckTool(tool)
}
