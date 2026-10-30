package security

// 主体（principal）授权模型：**声明式**能力授予。
//
// ## 为什么需要它（用户 2026-10-26 指出的现状）
//
// QQ 插件现在用 stage 管道自己实现了一套权限模型（`beforeToolcall`）：
// 判身份 → 查用户白名单 → 查"硬私有"工具 → 内部委派放行 → 写拒绝消息。
// 那是**在插件里执行安全策略**，后果实测有三类：
//
//	① 每个插件各写一套，判定必然漂移（QQ 拦了、其他入口没拦）
//	② 策略与被约束者在同一个进程里 —— 插件崩了/被替换，策略就没了
//	③ 拒绝语义散落（写 denial / 返回 error 各有各的写法）
//
// 正确形态：插件**声明**策略（谁可以调什么），内核**执行**策略。
//
// ## 与本文件已有内容的关系
//
//	Policy        全局静态策略（黑名单/白名单/沙箱）
//	ScopeOptions  轮次作用域（临时收紧）
//	PrincipalPolicy（本文件）**按主体**授予的能力集合
//
// 判定顺序始终是：全局 deny → 主体 deny → 轮次 deny → 白名单 → 放行。

import (
	"strings"
	"sync"
)

// Principal 是一次调用的**主体**：谁在请求。
type Principal struct {
	// ID 是主体标识（QQ 用户 id、群 id、驻留子 id、内部标记）。
	ID string
	// Kind 是主体类别（"user" / "group" / "internal" / "resident" …）。
	//
	// ★ 为什么不只看 ID：内部委派与用户请求的 ID 可能相同
	//（驻留子继承父的输入事件），但**权限模型完全不同** ——
	// 这正是 QQ 那条 `isInternalToolCall` 分支存在的原因。
	Kind string
	// Tags 是附加标签（如 "admin"、"trusted"），策略可按标签授予。
	Tags []string
}

// IsInternal 报告是否内部主体（agent 体系自己发起）。
func (p Principal) IsInternal() bool {
	return p.Kind == "internal" || p.Kind == "resident"
}

// PrincipalPolicy 是某个主体的能力策略。
type PrincipalPolicy struct {
	// AllowTools 是该主体**可用**的工具模式（空 = 不额外限制）。
	AllowTools []string
	// DenyTools 是该主体**禁用**的工具模式（优先于 AllowTools）。
	DenyTools []string
	// HardPrivateTools 是**永不外借**的工具模式（如 calendar_ / email_ / memory_）。
	//
	// ★ 与 DenyTools 的区别：DenyTools 是"这个主体不能用"，
	// HardPrivateTools 是"没有任何用户主体能用" —— 它防的是
	// 用户偷看私人数据，而内部执行体（父的委派）本就有完整权限。
	HardPrivateTools []string
}

// principalRegistry 持有各主体的策略（进程级，内核持有）。
type principalRegistry struct {
	mu    sync.RWMutex
	byID  map[string]PrincipalPolicy
	byTag map[string]PrincipalPolicy
}

func newPrincipalRegistry() *principalRegistry {
	return &principalRegistry{
		byID:  map[string]PrincipalPolicy{},
		byTag: map[string]PrincipalPolicy{},
	}
}

// SetByID 按主体 ID 声明策略。
func (r *principalRegistry) SetByID(id string, p PrincipalPolicy) {
	if r == nil || strings.TrimSpace(id) == "" {
		return
	}
	r.mu.Lock()
	r.byID[id] = p
	r.mu.Unlock()
}

// SetByTag 按标签声明策略（比逐 ID 声明更好维护）。
func (r *principalRegistry) SetByTag(tag string, p PrincipalPolicy) {
	if r == nil || strings.TrimSpace(tag) == "" {
		return
	}
	r.mu.Lock()
	r.byTag[tag] = p
	r.mu.Unlock()
}

// RemoveByID 撤销某个主体的策略声明。
//
// ## 为什么必须有它（2026-10-30）
//
// 主体 ID 对**子 agent** 来说是脚本给的（`ring_agents create id="bot1"`）。
// 子被回收后同名重建是常规操作，而若旧策略留在表里：
//
//	新一代用**上一代父当时留的约束**运行 —— 而那个父可能已经不在了。
//
// 这与今天修的 rtchild 留档串线（`rtClosedTranscripts` 以模型给的名字为键）
// 是**同一种形态**：以调用方给的名称为键的表，必须在生命周期结束时清理。
//
// 不清理还会单调增长（每建一个不同名字的子就多一条永不释放的 Polciy）。
func (r *principalRegistry) RemoveByID(id string) {
	if r == nil || strings.TrimSpace(id) == "" {
		return
	}
	r.mu.Lock()
	delete(r.byID, id)
	r.mu.Unlock()
}

// lookup 取出与主体相关的全部策略（ID + 每个标签）。
func (r *principalRegistry) lookup(p Principal) []PrincipalPolicy {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []PrincipalPolicy
	if pol, ok := r.byID[p.ID]; ok {
		out = append(out, pol)
	}
	for _, tag := range p.Tags {
		if pol, ok := r.byTag[tag]; ok {
			out = append(out, pol)
		}
	}
	return out
}

// DeclarePrincipalPolicy 是**插件声明主体策略**的入口（SDK 转发到这里）。
//
// scope 语义：
//
//	"id"  → 按主体 ID（一个具体的人/群/子 agent）
//	"tag" → 按标签（一类主体，如 "external"）
func (r *Runtime) DeclarePrincipalPolicy(scope, key string, p PrincipalPolicy) error {
	if r == nil || r.principals == nil {
		return errNoRuntime
	}
	switch scope {
	case "id":
		r.principals.SetByID(key, p)
	case "tag":
		r.principals.SetByTag(key, p)
	default:
		return &policyErr{"security: principal scope must be \"id\" or \"tag\", got " + scope}
	}
	return nil
}

// RevokePrincipalPolicy 撤销一个按 ID 声明的主体策略（合规性：子 agent 销毁时调）。
//
// 只支持 scope="id"：标签策略属于**部署级**声明（如 tag=external），
// 它们的生命周期不跟随某个具体执行体，撤销只能由声明方自己做。
func (r *Runtime) RevokePrincipalPolicy(scope, key string) error {
	if r == nil || r.principals == nil {
		return errNoRuntime
	}
	if scope != "id" {
		return &policyErr{"security: revoke only supports scope=\"id\" (tag policies are deployment-level), got " + scope}
	}
	r.principals.RemoveByID(key)
	return nil
}

// HasPrincipalPolicy 报告某 ID 是否已声明主体策略（诊断/状态面用）。
func (r *Runtime) HasPrincipalPolicy(id string) bool {
	if r == nil || r.principals == nil {
		return false
	}
	r.principals.mu.RLock()
	defer r.principals.mu.RUnlock()
	_, ok := r.principals.byID[id]
	return ok
}

// CheckToolFor 按**主体**判定工具是否可用。
//
// ★ 这是替代"插件在 stage 里自己拦"的入口：插件只声明策略，
// 判定与拒绝文案由内核统一产出（模型看到一致的拒绝语义）。
func (r *Runtime) CheckToolFor(p Principal, tool string) Decision {
	if r == nil {
		return allowed()
	}
	// ① 全局策略（deny 优先）。
	if d := r.CheckTool(tool); !d.Allowed {
		return d
	}
	pols := r.principals.lookup(p)
	if len(pols) == 0 {
		return allowed()
	}

	// ② 硬私有：任何**非内部**主体都不能用。
	// 内部执行体（父的委派）本就有完整权限 —— 再拦一次等于
	// "机器人自己派出去的活被自己拦住"（QQ 的实测教训）。
	for _, pol := range pols {
		if p.IsInternal() {
			break
		}
		for _, pat := range pol.HardPrivateTools {
			if matchTool(pat, tool) {
				return deny("tool %q is hard-private and unavailable to %s principals", tool, p.Kind)
			}
		}
	}

	// ③ 主体 deny。
	for _, pol := range pols {
		for _, pat := range pol.DenyTools {
			if matchTool(pat, tool) {
				return deny("tool %q is not permitted for this principal (deny %q)", tool, pat)
			}
		}
	}

	// ④ 主体 allow：只要求"至少有一个声明了 allow 的策略命中"。
	// 若所有策略都没声明 allow，则不做额外限制（沿用全局）。
	anyAllowDeclared := false
	allowHit := false
	for _, pol := range pols {
		if len(pol.AllowTools) == 0 {
			continue
		}
		anyAllowDeclared = true
		for _, pat := range pol.AllowTools {
			if matchTool(pat, tool) {
				allowHit = true
			}
		}
	}
	if anyAllowDeclared && !allowHit {
		return deny("tool %q is outside this principal's allowed set", tool)
	}
	return allowed()
}

// PrincipalPolicies 返回已声明策略数量（状态面用）。
func (r *Runtime) PrincipalPolicies() int {
	if r == nil || r.principals == nil {
		return 0
	}
	r.principals.mu.RLock()
	defer r.principals.mu.RUnlock()
	return len(r.principals.byID) + len(r.principals.byTag)
}

// errNoRuntime 表示安全运行时未装配。
var errNoRuntime = &policyErr{"security: runtime is not initialized"}

type policyErr struct{ msg string }

func (e *policyErr) Error() string { return e.msg }
