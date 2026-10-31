package security

// Runtime 是安全模块的**运行时对象**：横在 Core 与全部外部模块之间的边界。
//
// ## 形态（用户确认：显式代理）
//
// 业务模块看到的仍是它们熟悉的公共挂载点（IO / EventBus / 工具分发），
// 但那些挂载点在装配时被换成了本 Runtime 产出的代理：
//
//	Core ──▶ proxy.PrepareOutbound ──▶ 外部
//	Core ◀── proxy.ScrubInbound  ◀── 外部
//
// 因此"劫持"只是架构语义（便于各模块解耦），实现上是**显式代理**：
// 没有 monkey patch、没有反射替换、没有隐式全局 hook。
//
// ## 为什么必须有 Disabled 形态
//
// 安全模块可以被配置关掉（`core.security.enabled=false`）。若此时 Runtime
// 为 nil，每个调用点都得写 `if r != nil`；漏一个就是 panic 或绕过。
// 装一个 **Disabled Runtime**（所有方法零成本透传）后，调用点只有一条路径。
//
// ★ 这不是"降级放行"：用户显式关掉了这项能力，读配置就能看出来。
//   与"没装配好却假装安全"是两件事。

import (
	"fmt"
	"sync"
)

var (
	runtimeMu sync.RWMutex
	runtime   *Runtime
)

// Runtime 是安全运行时。
//
// 它把安全模块的三块能力合成**一个边界**：
//
//	gate    凭据防火墙（明文 ⇄ @{别名}）
//	policy  工具黑白名单 + 沙箱约束
//	content 敏感词 + 提示词注入预警
//	scopes  插件声明的**轮次作用域策略**（内核持有，按时过期）
type Runtime struct {
	gate    *Gate
	enabled bool

	// policy 是全局策略快照（启动/重载时替换）。
	policy Policy
	// content 是内容审查策略（敏感词 + 注入检测）。
	content ContentPolicy
	// scopes 是插件可声明的轮次作用域（内核持有，deny 优先）。
	scopes *ScopeRegistry
	// profiles 是**插件声明的命名限式**（`qq.owner` / `qq.guest` …）。
	//
	// ★ 可选：插件不声明任何档位时，一切判定沿用内核默认策略
	//（全局 policy + 主体策略 + 轮次作用域），行为与之前完全一致。
	profiles *profileRegistry
	// active 记录**当前活跃档位**（由 inputch 输入的 SecurityProfile 选择）。
	active *profileActivator
	// eventPolicy 是**事件总线授权表**（谁能发/订阅什么事件）。
	//
	// ★ 它与凭据闸门无关：`core.security.enabled=false` 关掉的是凭据替换，
	// 而"插件不许发 raw_input"这类授权始终生效 —— 所以 Disabled Runtime
	// 同样持有默认策略（见 NewDisabledRuntime）。
	eventPolicy EventPolicy
	// principals 是**按主体**的能力策略（插件声明，内核执行）。
	//
	// ★ 它替代的正是"插件自己在 stage 里拦工具"那种实现
	//（QQ 的 beforeToolcall）：插件只声明谁可以调什么，
	// 判定与拒绝文案由内核统一产出。
	principals *principalRegistry

	// scopesByAgent 是**每 agent 一个**策略执行视图（见 agentscope.go）。
	//
	// ★ 为何需要（2026-10-31 用户裁定）：每个 agent / 子 agent 一个实例，
	// 因为轮次档位（sessionProfile）是**该 agent 独有**的状态。
	// 放在 Runtime 单例上会让并发 agent 互相覆写（主 agent 的 guest
	// 被驻留子的 owner 盖掉，或反之）。
	//
	// ★ 为何不把整个 Runtime 复制一份：`profiles` / `principals` / `scopes`
	// 是**插件声明出来的注册表**，每个 agent 各持一份就会变成
	// “插件往 A 声明了档位，B 看不见”—— 那是最难查的一类缺陷。
	// 所以切分是「共享注册表 + per-agent 执行视图」。
	scopesByAgent map[string]*AgentScope
	scopeMu       sync.Mutex
}

// NewRuntime 用已装好的变量库构造运行时。
func NewRuntime(store *Store) *Runtime {
	if store == nil {
		return NewDisabledRuntime()
	}
	return &Runtime{
		gate:        NewGate(store),
		enabled:     true,
		policy:      DefaultPolicy(),
		eventPolicy: DefaultEventPolicy(),
		scopes:      NewScopeRegistry(),
		principals:  newPrincipalRegistry(),
		profiles:    newProfileRegistry(),
		active:      &profileActivator{},
	}
}

// NewDisabledRuntime 构造一个零成本透传的运行时（安全模块被显式关闭时用）。
func NewDisabledRuntime() *Runtime {
	// 即使禁用也装一个策略表：否则"关闭凭据防火墙"会连带让工具策略失效，
	// 而这是两件不同的事（用户可以只要策略、不要凭据替换）。
	return &Runtime{
		enabled:     false,
		policy:      DefaultPolicy(),
		eventPolicy: DefaultEventPolicy(),
		scopes:      NewScopeRegistry(),
		principals:  newPrincipalRegistry(),
		profiles:    newProfileRegistry(),
		active:      &profileActivator{},
	}
}

// RuntimeGlobal 取进程级运行时；未装配时返回 nil。
//
// ★ 返回 nil 而不是 Disabled：这只表示"安全模块还没装配"，
//
//	装配完成后（无论启用与否）一定非 nil。调用方拿 nil 应视为装配错误。
func RuntimeGlobal() *Runtime {
	runtimeMu.RLock()
	defer runtimeMu.RUnlock()
	return runtime
}

// SetRuntimeGlobal 装入运行时（幂等：重复装同一个实例无副作用）。
func SetRuntimeGlobal(r *Runtime) {
	runtimeMu.Lock()
	runtime = r
	runtimeMu.Unlock()
}

// Enabled 报告安全审查是否真的生效。
func (r *Runtime) Enabled() bool { return r != nil && r.enabled && r.gate != nil }

// Gate 返回边界闸门（未启用时为 nil）。
func (r *Runtime) Gate() *Gate {
	if r == nil {
		return nil
	}
	return r.gate
}

// PrepareOutbound 处理 Core → 外部的文本（明文换成 @{别名}）。
func (r *Runtime) PrepareOutbound(ins Inspection, text string) (string, int) {
	if r == nil || !r.enabled || r.gate == nil {
		return text, 0
	}
	return r.gate.PrepareOutbound(ins, text)
}

// ScrubInbound 处理外部 → Core 的文本（明文换成 @{别名}）。
//
// ★ 入向必须扫：入向内容直接进模型上下文（role=tool）。
//
//	只在"写记忆"时清洗是最坏组合 —— 记忆侧干净、当轮泄漏、事后查不到。
func (r *Runtime) ScrubInbound(ins Inspection, text string) (string, int) {
	if r == nil || !r.enabled || r.gate == nil {
		return text, 0
	}
	return r.gate.ScrubInbound(ins, text)
}

// FillForExecution 在真正交给外部执行前把 `@{别名}` 换回明文。
func (r *Runtime) FillForExecution(ins Inspection, text string) (string, int) {
	if r == nil || !r.enabled || r.gate == nil {
		return text, 0
	}
	return r.gate.FillForExecution(ins, text)
}

// Store 返回底层变量库（安全工具族 secret_* 需要元信息）。
func (r *Runtime) Store() *Store {
	if r == nil || r.gate == nil {
		return nil
	}
	return r.gate.Store()
}

// Count 返回变量条数（状态面用；未启用时为 0）。
func (r *Runtime) Count() int {
	if s := r.Store(); s != nil {
		return s.Count()
	}
	return 0
}

// ───────────────────── 策略（工具 / 内容 / 作用域） ─────────────────────

// SetPolicy 替换全局策略（配置重载时调用；快照式，不做增量）。
func (r *Runtime) SetPolicy(p Policy) {
	if r == nil {
		return
	}
	r.policy = p
}

// SetContentPolicy 替换内容审查策略（敏感词 + 注入检测）。
func (r *Runtime) SetContentPolicy(p ContentPolicy) {
	if r == nil {
		return
	}
	r.content = p
}

// Scopes 返回作用域登记表（插件声明入口；nil Runtime 时返回 nil）。
func (r *Runtime) Scopes() *ScopeRegistry {
	if r == nil {
		return nil
	}
	return r.scopes
}

// DeclareTurnPolicy 是**插件声明轮次策略**的入口（SDK 转发到这里）。
//
// 语义：声明在**本轮之后**生效，到期自动回收（内核持有，不依赖插件存活）。
func (r *Runtime) DeclareTurnPolicy(opts ScopeOptions) error {
	if r == nil || r.scopes == nil {
		return fmt.Errorf("security: security runtime is not initialized")
	}
	return r.scopes.Declare(opts)
}

// CheckTool 判定工具是否允许调用（含当前活跃档位）。
//
// 组合顺序（**deny 永远优先**，这是安全策略的基本直觉）：
//
//	全局 deny → 作用域 deny → 全局 allow（非空则必须命中）→ 作用域 allow
//
// ## 与 per-agent 视图的关系（2026-10-31 用户裁定）
//
// 本函数用 `Runtime.active`（**进程级**）上的档位 —— 新代码不要依赖它。
// 新调用方应走 `Runtime.ForAgent(id, isChild)` 拿 `*AgentScope`：
// 那里的档位是**该 agent 独有**的，不会与其他 agent 串。
//
// 保留本函数是为了：独立工具（memgc 等，进程内只有一个执行体）、判据、
// 以及尚未按 agent 切分的旧路径。
func (r *Runtime) CheckTool(name string) Decision {
	if r == nil {
		return allowed()
	}
	return r.checkToolWithProfile(Principal{}, r.ActiveProfile(), name)
}

// checkToolWithProfile 是工具判定的**唯一实现**：档位由调用方显式给定。
//
// `profileName` 为空 = 不使用档位（零影响）。
//
// ★ 为什么档位作参数、而不是读全局 `r.active`：
// per-agent 视图必须传入**它自己的**档位（session 或 longtime）。
// 若这里读 `r.active`，per-agent 视图就白做了 —— 那正是本轮要修的缺陷。
func (r *Runtime) checkToolWithProfile(p Principal, profileName, name string) Decision {
	if r == nil {
		return allowed()
	}
	if d := r.policy.CheckTool(name); !d.Allowed {
		return d
	}
	// ★ 活跃档位（可选）：插件没声明/没激活时这里零影响。
	// 档位只参与**收窄**：它的 deny / allow 与主体、全局同一条链，
	// 并且不能绕过全局黑名单（上面已先判过）。
	if prof, ok := r.profilePolicyByName(profileName); ok {
		for _, pat := range prof.Policy.DenyTools {
			if matchTool(pat, name) {
				return deny("tool %q is disabled by profile %q", name, prof.Name)
			}
		}
		if len(prof.Policy.AllowTools) > 0 && len(r.policy.ToolAllow) == 0 {
			hit := false
			for _, pat := range prof.Policy.AllowTools {
				if matchTool(pat, name) {
					hit = true
				}
			}
			if !hit {
				return deny("tool %q is outside profile %q allowed set", name, prof.Name)
			}
		}
	}
	// 作用域覆盖：插件的 deny 追加在全局之上。
	active := r.scopes.Active()
	if len(active) == 0 {
		return allowed()
	}
	for _, sc := range active {
		for _, pat := range sc.DenyTools {
			if matchTool(pat, name) {
				return deny("tool %q is disabled by plugin scope %q", name, sc.ID)
			}
		}
	}
	// 作用域 allow 只在"全局白名单为空"时才作为额外许可集合使用，
	// 否则会把全局白名单绕掉（作用域不该扩大权限）。
	if len(r.policy.ToolAllow) == 0 {
		allowedByScope := false
		for _, sc := range active {
			for _, pat := range sc.AllowTools {
				if matchTool(pat, name) {
					allowedByScope = true
				}
			}
		}
		// 有 allow 声明但一个都没命中 ⇒ 说明该作用域只允许更窄的集合。
		// 只要有作用域声明了 allow，就要求命中（否则等于没声明）。
		for _, sc := range active {
			if len(sc.AllowTools) > 0 && !allowedByScope {
				return deny("tool %q is not allowed by active plugin scope %q", name, sc.ID)
			}
		}
	}
	return allowed()
}

// CheckNetwork 判定网络意图是否允许（含作用域覆盖）。
func (r *Runtime) CheckNetwork() Decision {
	if r == nil {
		return allowed()
	}
	if d := r.policy.CheckNetwork(); !d.Allowed {
		return d
	}
	for _, sc := range r.scopes.Active() {
		if sc.DisableNetwork {
			return deny("network access is disabled by plugin scope %q", sc.ID)
		}
	}
	if prof, ok := r.profilePolicy(); ok && prof.DisableNetwork {
		return deny("network access is disabled by profile %q", prof.Name)
	}
	return allowed()
}

// CheckPath 判定路径是否被沙箱允许。
func (r *Runtime) CheckPath(direction, path string) Decision {
	if r == nil {
		return allowed()
	}
	return r.policy.CheckPath(direction, path)
}

// Content 返回当前内容审查策略（只读快照）。
func (r *Runtime) Content() ContentPolicy {
	if r == nil {
		return ContentPolicy{}
	}
	return r.content
}

// effectiveContent 把作用域里追加的敏感词并进基础策略。
func (r *Runtime) effectiveContent() ContentPolicy {
	p := r.content
	if prof, ok := r.profilePolicy(); ok && len(prof.SensitiveWords) > 0 {
		p.SensitiveWords = append(append([]string{}, p.SensitiveWords...), prof.SensitiveWords...)
	}
	for _, sc := range r.scopes.Active() {
		if len(sc.SensitiveWords) > 0 {
			p.SensitiveWords = append(append([]string{}, p.SensitiveWords...), sc.SensitiveWords...)
		}
	}
	return p
}

// ScanContent 审查一段文本，返回命中（凭据由 ScrubInbound 另行处理）。
//
// 返回值第二项表示"是否必须阻断"（敏感词，或策略要求阻断的注入迹象）。
func (r *Runtime) ScanContent(ins Inspection, text string) ([]Finding, bool, string) {
	if r == nil || text == "" {
		return nil, false, ""
	}
	p := r.effectiveContent()
	findings := p.Scan(text, ins.where())
	blocked, reason := p.BlockedBy(findings)
	return findings, blocked, reason
}
