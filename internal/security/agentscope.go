package security

// per-agent 策略作用域：**每个 agent 一个实例**。
//
// ## 为什么必须 per-agent（2026-10-31 用户裁定）
//
// 收敛前 `Runtime` 是进程级单例，而「当前活跃档位」（`active`）挂在它上面。
// 多 agent 并发时档位会串：
//
//	主 agent 轮次 A：注入 qq.guest  →  active = "qq.guest"
//	驻留子 轮次 B：注入 qq.owner    →  active = "qq.owner"     ← 覆盖
//	主 agent 轮次 A 继续执行工具     →  判定按 qq.owner 走        ← 串档
//
// 这不是理论风险：驻留子与 rtchild 与主 agent **同进程并发**（见 plan.md 十一）。
//
// ## 但**不能**整份复制 Runtime
//
// `profiles` / `principals` / `scopes` 是**插件声明出来的注册表**。
// 若每个 agent 各持一份：
//
//	插件往 A 声明了 qq.guest ⇒ B 看不见 ⇒ B 上注入 qq.guest 报"未声明"
//
// 那是最难查的一类缺陷（"时灵时不灵"）。所以切分必须是：
//
//	共享（进程级 1 份）          per-agent（N 份）
//	─────────────────────      ─────────────────────
//	gate / policy / content      sessionProfile  ← 轮次级，End 即清
//	profiles / principals        longtimeProfile ← 跨轮保留
//	scopes / eventPolicy         roundActive     ← 本轮正在生效的那个
//
// ## 两层档位（用户裁定的语义）
//
//	sessionProfile  —— 属于**这一轮**。输入携带它，`End()` 就清掉。
//	                   典型：一条 QQ 消息来自访客 ⇒ 这一轮按 guest 判定。
//	                   ★ 严格 per-round：同一用户下一条消息要重新声明。
//
//	longtimeProfile —— 属于**这个 agent 的一段生命**。设置后跨轮保留，
//	                   直到显式改掉或 agent 销毁。
//	                   典型：把这个驻留子长期派驻为"访客权限"。
//
// 优先级（`roundActive` 的计算）：session > longtime > 无。
//
//	session 是"这一轮的事实"，它必须能盖住 long-term 的默认值 ——
//	否则访客发一条消息也无法在长期为 owner 的 agent 上收窄权限。
//
//	反向（longtime 盖 session）会造成**提权**：长期档是 owner，
//	访客这一轮声明的 guest 被忽略 ⇒ 访客拿到 owner 权限。

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// AgentScope 是**一个 agent 的策略执行视图**。
//
// 它由 `Runtime.ForAgent` 创建，持有该 agent 独有的轮次状态，
// 同时共享 `Runtime` 上的全部注册表与全局基线。
//
// ★ 生命周期跟随 agent：创建时 `ForAgent`，销毁时 `ReleaseAgent`。
// 不释放不会泄漏内存（视图只持有几个字符串），但**残留的 longtime 档位**
// 会让同名重建的 agent 继承上一代的约束 —— 与主体策略的撤销同一形态。
type AgentScope struct {
	rt      *Runtime
	agentID string
	isChild bool

	mu sync.RWMutex
	// sessionProfile 是本轮输入声明的档位（End 清空）。
	sessionProfile string
	// longtimeProfile 是跨轮保留的档位（显式设置/清除）。
	longtimeProfile string
	// sessionExpires 是 session 档位的 TTL 到期时刻（0 = 不过期）。
	//
	// 为什么 session 也要 TTL：输入到达后若轮次异常终止（panic/取消），
	// `End()` 可能没跑到。TTL 是兜底 —— 避免档位永远卡在某一轮的值上。
	sessionExpires time.Time
}

// ForAgent 为一个 agent 创建策略执行视图（幂等：同一 id 返回同一个）。
//
// 幂等很重要：`BeginRound` 在每条输入上都会走一遍，若每次新建视图，
// `longtimeProfile` 会在下一轮凭空消失。
func (r *Runtime) ForAgent(agentID string, isChild bool) *AgentScope {
	if r == nil {
		return nil
	}
	r.scopeMu.Lock()
	defer r.scopeMu.Unlock()
	if r.scopesByAgent == nil {
		r.scopesByAgent = map[string]*AgentScope{}
	}
	if sc, ok := r.scopesByAgent[agentID]; ok {
		return sc
	}
	sc := &AgentScope{rt: r, agentID: agentID, isChild: isChild}
	r.scopesByAgent[agentID] = sc
	return sc
}

// ReleaseAgent 销毁一个 agent 的视图（含它的 longtime 档位）。
//
// ★ 必须调：agent/子 agent 回收后同名重建是常规操作（`create id="bot1"`），
// 残留的 longtime 档会让新一代继承上一代父留的约束。
// 与 `RevokePrincipalPolicy` 同一纪律。
func (r *Runtime) ReleaseAgent(agentID string) {
	if r == nil {
		return
	}
	r.scopeMu.Lock()
	delete(r.scopesByAgent, agentID)
	r.scopeMu.Unlock()
}

// BeginRound 开始一轮：由 security 自己从原始载荷解析 session 档位。
//
// 与 `Runtime.BeginRound` 的区别：这个版本的状态落在 **per-agent 视图**上，
// 不污染其他 agent。
func (sc *AgentScope) BeginRound(in RoundInput) (*Round, error) {
	if sc == nil || sc.rt == nil {
		return &Round{}, nil
	}
	round := &Round{
		rt:        sc.rt,
		scope:     sc,
		principal: sc.rt.classifyPrincipal(in),
	}

	name := strings.TrimSpace(roundProfileName(in.Payload))
	if name == "" {
		// 不带档位 = 这一轮显式不使用 session 档。
		// ★ 只清 session，**不动** longtime —— 后者是 agent 的生命状态。
		// 所以这里 effective 仍可能是 longtime 档。
		sc.clearSession()
		round.profile = sc.EffectiveProfile()
		return round, nil
	}

	if err := sc.setSession(name); err != nil {
		// 响亮但不断流；同时**清掉**上一轮的 session（不能沿用旧档）。
		sc.clearSession()
		log.Printf("[security] agent %q round profile %q rejected: %v "+
			"(falling back to long-term profile %q / default policy)",
			sc.agentID, name, err, sc.LongtimeProfile())
		round.profile = sc.EffectiveProfile() // 降级后可能是 longtime 档
		return round, err
	}
	// Round.profile 报告**本轮生效的档位**（session 优先，其次 longtime）。
	round.profile = sc.EffectiveProfile()
	return round, nil
}

// setSession 设置本轮档位（校验档位已声明）。
func (sc *AgentScope) setSession(name string) error {
	if _, ok := sc.rt.profiles.Get(name); !ok {
		return &policyErr{fmt.Sprintf(
			"security: 未声明的安全档位 %q（已声明: %v）—— 拼错不会退化，直接报错",
			name, sc.rt.profiles.Names())}
	}
	sc.mu.Lock()
	sc.sessionProfile = name
	sc.sessionExpires = time.Now().Add(scopeTTLDefault)
	sc.mu.Unlock()
	return nil
}

// clearSession 清除本轮档位（不影响 longtime）。
func (sc *AgentScope) clearSession() {
	if sc == nil {
		return
	}
	sc.mu.Lock()
	sc.sessionProfile = ""
	sc.sessionExpires = time.Time{}
	sc.mu.Unlock()
}

// SetLongtimeProfile 设置跨轮保留的档位（空串 = 清除）。
//
// 用途：把一个驻留子**长期**派驻为某个权限档（如"访客权限的数字助手"），
// 不必每条输入都声明。
//
// 校验与 session 一致：未声明的档位名返回错误，不静默接受。
func (sc *AgentScope) SetLongtimeProfile(name string) error {
	if sc == nil || sc.rt == nil {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		sc.mu.Lock()
		sc.longtimeProfile = ""
		sc.mu.Unlock()
		return nil
	}
	if _, ok := sc.rt.profiles.Get(name); !ok {
		return &policyErr{fmt.Sprintf(
			"security: 未声明的安全档位 %q（已声明: %v）", name, sc.rt.profiles.Names())}
	}
	sc.mu.Lock()
	sc.longtimeProfile = name
	sc.mu.Unlock()
	return nil
}

// LongtimeProfile 返回跨轮档位名（空 = 未设置）。
func (sc *AgentScope) LongtimeProfile() string {
	if sc == nil {
		return ""
	}
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.longtimeProfile
}

// SessionProfile 返回本轮档位名（空 = 本轮未声明；已过期视为空）。
func (sc *AgentScope) SessionProfile() string {
	if sc == nil {
		return ""
	}
	sc.mu.RLock()
	name, exp := sc.sessionProfile, sc.sessionExpires
	sc.mu.RUnlock()
	if name == "" {
		return ""
	}
	if !exp.IsZero() && time.Now().After(exp) {
		return "" // TTL 兜底：End 没跑到也不至于永远卡住
	}
	return name
}

// EffectiveProfile 返回本轮**实际生效**的档位：session > longtime > 空。
//
// ★ 这个优先级本身是安全语义，写在这里而不是调用方：
//
//	session 是"这一轮的事实"，必须能盖住 long-term 默认值 ——
//	否则访客发一条消息也无法在长期为 owner 的 agent 上收窄权限。
//
//	反向（longtime 盖 session）就是提权：长期档 owner 会把
//	访客这一轮声明的 guest 吞掉 ⇒ 访客拿到 owner 权限。
func (sc *AgentScope) EffectiveProfile() string {
	if s := sc.SessionProfile(); s != "" {
		return s
	}
	return sc.LongtimeProfile()
}

// CheckTool 判定某工具在**本 agent 当前状态**下是否可用。
//
// 它把三件事合成一次调用，调用方不需要知道任何组合规则：
//
//	① 全局基线（policy）
//	② 生效档位（session 优先，其次 longtime）
//	③ 主体策略（父为子声明的 Allow/Deny）
func (sc *AgentScope) CheckTool(tool string) Decision {
	if sc == nil || sc.rt == nil {
		return allowed()
	}
	return sc.rt.checkToolForWithProfile(sc.rt.classifyScopePrincipal(sc.agentID, sc.isChild),
		sc.EffectiveProfile(), tool)
}
