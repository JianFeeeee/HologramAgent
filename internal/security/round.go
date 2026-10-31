package security

// 轮次上下文（RoundContext）：**security 对外的输入接入点**。
//
// ## 为什么需要这一层（2026-10-31 收敛）
//
// 收敛前的形态：security 只暴露细粒度判定原语（`CheckTool` / `CheckToolFor` /
// `ActivateProfile`），于是**每个消费方都得自己理解策略语义**：
//
//	agent/core/security_profile.go 里曾这样写：
//
//	    name, _ := payload["security_profile"].(string)   // 自己抠字段
//	    a.security.ActivateProfile(name, 0)               // 再告诉 security
//	    if a.parentID != "" {                             // 自己判断走哪条链
//	        return a.security.CheckToolFor(Principal{Kind: "resident"}, tool)
//	    }
//	    return a.security.CheckTool(tool)
//
// 三个问题全出在这段：
//
//	① `security_profile` 是 security 自己的协议概念，解析却在外人手里
//	② “我是父还是子”是策略语义，判断却在 agent 手里
//	③ `Kind: "resident"` 是调用方**声明自己的类别** ⇒ 说自己是内部就享受
//	   内部豁免，那是提权口子
//
// ## 本文件确立的形态
//
//	调用方只提供**事实**（谁在跑、有没有父、原始载荷）
//	security 自己做**结论**（类别、档位、走哪条判定链）
//
// 对应 EventBus 那个已经跑通的范例：总线只交事件，guard 自己判。
//
//	events.Bus.Publish → Guard.AllowPublish(Publisher, EventType) → 判定在 guard 内
//
// ## 生命周期
//
//	rc, err := rt.BeginRound(RoundInput{...})   // 输入到达
//	defer rc.End()                              // 轮次结束（归位，防跨轮泄漏）
//	rc.CheckTool(name)                          // 本轮的判定（只含本轮的档位/主体）
//
// `End` 必须调：档位在 `Runtime` 上是**进程级状态**，不归位就会泄漏到下一轮
// （guest 的收窄误伤后续输入；方向相反就是提权）。

import (
	"fmt"
	"strings"
)

// RoundInput 是一次「输入到达」时调用方能提供的**全部事实**。
//
// ★ 这里刻意**不含 Kind**：类别由 security 自己从 AgentID/IsChild 推。
// 调用方给不出“我是内部主体”这种结论 —— 它只能说自己是谁。
type RoundInput struct {
	// AgentID 是执行本次输入的执行体标识（主 agent = "main"，子 = 子的 id）。
	AgentID string

	// IsChild 表示本次输入由**某个父 agent 派出的子执行体**处理。
	//
	// 这是调用方能提供的事实（驻留子/rtchild 自己知道有父），
	// 而“子属于哪一类主体、该不该豁免 HardPrivate”由 security 决定。
	IsChild bool

	// Payload 是**原始输入载荷**（`InputEvent.Payload`）。
	//
	// ★ security 自己从中解析 `security_profile` —— 那是 security 的协议，
	// 不该让每个消费方各写一遍解析。字段名定义见 `profileKey`。
	Payload map[string]interface{}

	// Source 是来源通道名（诊断与审计用，不参与判定）。
	Source string
}

// profileKey 是输入载荷里携带命名档位的键名。
//
// 与公开 SDK 的 `InjectOptions.SecurityProfile`（JSON 名 `security_profile`）
// **逐字一致** —— 那是插件与 security 之间的协议，解析权属于 security。
const profileKey = "security_profile"

// Round 是一轮的策略上下文句柄。
//
// 它把「本轮生效的档位」与「本轮的主体」绑在一起，使判定成为**纯函数式**的
// 一次调用，调用方不再需要知道任何组合规则。
type Round struct {
	rt *Runtime
	// scope 是**该 agent 独有的**策略执行视图（2026-10-31 起的正路）。
	//
	// 为 nil 表示走旧的进程级单例路径（Round.End 会恢复 Runtime.active）。
	scope *AgentScope
	// principal 是 security **自己判定**出来的主体（不是调用方声明的）。
	principal Principal
	// profile 是本轮生效的档位名（空 = 未使用档位）。
	profile string
	// restored 仅在 scope == nil 时使用：记录进程级 active 的进入值。
	//
	// ★ 为什么旧的进程级路径要"恢复"而不是"清空"：那时多个 agent
	// 共享一个 active，清空会把别处正在用的档抹掉。per-agent 视图
	// 没有这个问题（每个 agent 自己一份），所以走 scope 时直接清 session。
	restored string
	ended    bool
}

// CheckToolForAgent 按**执行体身份**判定工具是否可用（无轮次句柄时的入口）。
//
// ## 什么时候用它
//
// 工具执行链（`executeToolCallInner`）只持有执行体身份（id + 有无父），
// 不持有本轮句柄。此时由 security **自己**从事实推出主体类别，
// 而不是让调用方声明 `Kind` —— 后者是提权口子。
//
// 与 `BeginRound` + `Round.CheckTool` 的区别：
//
//	BeginRound 路线  —— 带档位（本轮激活的 profile 参与判定）
//	本函数         —— 不带档位处理，只看全局策略 + 主体策略
//
// ★ 为什么不用把档位也算进来：档位是**输入**携带的（`security_profile`），
// 它属于某一轮的事实。工具执行链上没有轮次上下文，硬塞一个“进程当前档”
// 才是不对的（那正是收敛前 `ActivateProfile` 全局单例的毛病）。
// 真正的修法是让执行链拿到本轮句柄 —— 见 future-plan。
func (r *Runtime) CheckToolForAgent(agentID string, isChild bool, tool string) Decision {
	if r == nil {
		return allowed()
	}
	return r.CheckToolFor(r.classifyPrincipal(RoundInput{
		AgentID: agentID,
		IsChild: isChild,
	}), tool)
}

// BeginRound 在**一条输入开始处理时**调用，由 security 完成：
//
//	① 判定主体类别（据 AgentID + IsChild，不信调用方声明）
//	② 从原始 Payload 解析 `security_profile`
//	③ 激活档位（未声明/拼错时**响亮降级**，不静默沿用上一轮的档）
//
// 返回的 `Round` 必须 `End()`（defer 即可）。
//
// ★ 为什么档位解析在这里而不是在调用方：
// 档位名是 `security` 的协议（`Profile.Name` 约定 `<owner>.<档位>`）。
// 把它交给调用方解析，等于让 N 个调用方各写一遍，且每写一遍都可能漏。
func (r *Runtime) BeginRound(in RoundInput) (*Round, error) {
	if r == nil {
		return &Round{}, nil // 未装配 security：零成本句柄
	}
	// ★ 正路：per-agent 视图（每个 agent 一份 session 档，互不干扰）。
	//
	// 2026-10-31 用户裁定「每个 agent 一个，分为 sessionProfile 和
	// longtimeProfile」后，这里的档位状态落在 `AgentScope` 上，
	// 不再写进程级 `Runtime.active`。
	sc := r.ForAgent(in.AgentID, in.IsChild)
	return sc.BeginRound(in)
}

// roundProfileName 从原始载荷里取档位名。
//
// 独立成小函数是为了让“解析规则只有一处”这件事在代码上可见：
// 任何调用方都不该再写 `payload["security_profile"]`。
func roundProfileName(payload map[string]interface{}) string {
	if payload == nil {
		return ""
	}
	s, _ := payload[profileKey].(string)
	return s
}

// classifyPrincipal 由 security **自己**判定主体类别。
//
// ## 判定规则（唯一事实来源）
//
//	IsChild        ⇒ KindSubAgent   （内核派出的子执行体）
//	AgentID == ""  ⇒ KindInternal   （内核自身发起的自循环任务）
//	其他            ⇒ KindUser      （经插件通道进来的终端用户）
//
// ★ 为什么"子"优先于"内核自身"：
// 子执行体也是内部委派（共享 HardPrivate 豁免），但它**可以被父收窄**。
// 若先判成 KindInternal，父在创建时声明的 Allow/Deny 就落不到它头上 ——
// 那正是收敛前 `Kind: "resident"` 掩盖的问题（它同时意味着
// “我是内部”和“我不受收窄”）。
func (r *Runtime) classifyPrincipal(in RoundInput) Principal {
	p := Principal{ID: in.AgentID}
	switch {
	case in.IsChild:
		p.Kind = KindSubAgent
	case strings.TrimSpace(in.AgentID) == "":
		p.Kind = KindInternal
	default:
		p.Kind = KindUser
	}
	return p
}

// Principal 返回本轮的主体（security 判定出的，只读）。
func (rc *Round) Principal() Principal {
	if rc == nil {
		return Principal{}
	}
	return rc.principal
}

// EffectiveProfile 返回本轮**实际生效**的档位名。
//
// per-agent 视图下它是 `session > longtime > 空` 的结果；
// 旧路径下等于 Round.Profile()。
func (rc *Round) EffectiveProfile() string {
	if rc == nil {
		return ""
	}
	if rc.scope != nil {
		return rc.scope.EffectiveProfile()
	}
	return rc.profile
}

// Profile 返回本轮生效的档位名（空 = 未使用档位）。
func (rc *Round) Profile() string {
	if rc == nil {
		return ""
	}
	return rc.profile
}

// CheckTool 判定本轮的某个工具是否可用。
//
// 这就是工具执行链**唯一**需要的调用：调用方不再需要知道
// “我是父还是子、该走 CheckTool 还是 CheckToolFor”。
func (rc *Round) CheckTool(name string) Decision {
	if rc == nil || rc.rt == nil {
		return allowed()
	}
	// per-agent 视图：用**本 agent 的**生效档位（session > longtime）。
	if rc.scope != nil {
		return rc.rt.checkToolWithProfile(rc.principal, rc.scope.EffectiveProfile(), name)
	}
	// 旧路径：进程级档位。
	return rc.rt.checkToolWithProfile(rc.principal, rc.rt.ActiveProfile(), name)
}

// End 结束本轮：恢复进入前的档位。
//
// 幂等：重复调用无副作用（defer + 显式调用并存是安全的）。
func (rc *Round) End() {
	if rc == nil || rc.ended {
		return
	}
	rc.ended = true

	// per-agent 视图：只清**自己的** session 档，longtime 档保留
	//（它是该 agent 的生命状态，不是本轮事实）。
	if rc.scope != nil {
		rc.scope.clearSession()
		return
	}

	// 旧的进程级路径：恢复进入前的值（那时多个 agent 共享一个 active）。
	if rc.rt == nil || rc.rt.active == nil {
		return
	}
	if rc.restored != "" {
		rc.rt.active.Activate(rc.restored, scopeTTLDefault)
		return
	}
	rc.rt.active.Clear()
}

// DeclareChildPolicy 让**父**为刚创建的子执行体声明主体策略。
//
// ## 为什么这个入口也在 security 里（而不是 agent 组装结构体）
//
// 收敛前 `agent/core` 自己拼 `PrincipalPolicy{AllowTools: …, DenyTools: …}`
// 字面量 —— 那是策略结构，属于 security 的领域。调用方只该说
// “这个子允许这些、禁止那些”，由 security 负责把语义落到正确的作用域。
//
// 语义约束（security 保证）：声明只能**收窄**。`CheckToolFor` 内部先跑
// `CheckTool`（全局 deny 优先），所以父的黑名单对子依然有效，子无法提权。
func (r *Runtime) DeclareChildPolicy(childID string, allow, deny []string) error {
	if r == nil {
		return nil // 未装配 security：不为子声明，保持无限制（与之前一致）
	}
	childID = strings.TrimSpace(childID)
	if childID == "" {
		return fmt.Errorf("security: declare child policy requires a non-empty child id")
	}
	if len(allow) == 0 && len(deny) == 0 {
		return nil // 父选择不给额外限制
	}
	return r.DeclarePrincipalPolicy("id", childID, PrincipalPolicy{
		AllowTools: allow,
		DenyTools:  deny,
	})
}

// RevokeChildPolicy 在子执行体销毁时撤销父为它声明的策略。
//
// ★ 必须撤销：主体 ID 是调用方给的名字，子回收后**同名重建**是常规操作。
// 旧策略留着 ⇒ 新一代用上一代父留的约束运行（且那个父可能已不存在），
// 主体表还会随 create/destroy 单调增长。
// 这与 rtchild 留档串线（`rtClosedTranscripts` 以模型给的名字为键）同一形态。
func (r *Runtime) RevokeChildPolicy(childID string) {
	if r == nil || strings.TrimSpace(childID) == "" {
		return
	}
	_ = r.RevokePrincipalPolicy("id", strings.TrimSpace(childID))
}

// ClassifyPrincipalForTest 暴露主体分类规则给跨包判据（**仅测试用**）。
//
// 为什么不写在 _test.go：判据住在 `kernelmod/all`（那里才有整机装配），
// 而 Go 的 _test.go 符号不跨包导出。带 ForTest 后缀是为了让
// "生产里调了它"在 code review 时一眼可见。
func (r *Runtime) ClassifyPrincipalForTest(in RoundInput) Principal {
	return r.classifyPrincipal(in)
}

// classifyScopePrincipal 按 per-agent 视图的事实判定主体类别。
//
// 与 classifyPrincipal 同一套规则（**唯一事实来源**），
// 只是入参来自 AgentScope 而不是 RoundInput。
func (r *Runtime) classifyScopePrincipal(agentID string, isChild bool) Principal {
	return r.classifyPrincipal(RoundInput{AgentID: agentID, IsChild: isChild})
}
