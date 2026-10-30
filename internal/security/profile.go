package security

// 插件**命名安全限式**（policy profile）：插件声明风格，inputch 触发时选一个。
//
// ## 用户模型（2026-10-26）
//
// > 插件可以声明自身安全限制策略，并在插件自身 inputch 触发时携带参数
// > 选择其中一种 —— 这样正好对应 QQ 插件内部 owner 与非 owner 的差别对待。
//
// 于是"谁能做什么"变成**插件自己声明的命名档位**：
//
//	qq 声明：  "owner"  → 允许开发类工具（cmd_* / files_write …）
//	          "guest"  → 只允许聊天类工具
//	          "group"  → 更窄（不得私发文件）
//
// 而每条 inputch 输入携带 `SecurityProfile: "qq.guest"`。
// 内核据此在该轮激活对应档位 —— **插件一行判定代码都不写**。
//
// ## 为什么档位必须由内核持有
//
// ① 插件崩了/被重载，档位不能跟着消失（否则挂掉插件即可绕过）
// ② 判定必须与全局策略、主体策略**同一条链**（否则又出现"两套语义"）
// ③ 拒绝文案要统一（模型看到的拒绝语义必须一致，否则它会反复重试）
//
// ## 档位不是"提权"
//
// 档位只在**收窄**方向生效：它可以追加 deny、追加敏感词、收紧沙箱，
// 但**不能**绕过全局黑名单或全局白名单（判据见下）。

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Profile 是插件声明的一个命名限式。
type Profile struct {
	// Name 是**全局唯一**的档位名，约定 `<插件名>.<档位>`（如 "qq.owner"）。
	//
	// ★ 强制前缀约定：拼错时能被立刻发现（`qq.ownr` 找不到 ⇒ 走默认档位
	// 而不是静默放行）。
	Name string
	// Owner 是声明它的插件（审计用；跨插件覆盖会被拒绝）。
	Owner string
	// Policy 是这一档的能力集合。
	Policy PrincipalPolicy
	// SensitiveWords 是这一档追加的敏感词。
	SensitiveWords []string
	// DisableNetwork 在这一档内禁止网络意图。
	DisableNetwork bool
	// Sandbox 覆盖沙箱约束（零值 = 沿用全局）。
	Sandbox SandboxPolicy
}

// profileRegistry 持有全部已声明档位（进程级，内核持有）。
type profileRegistry struct {
	mu     sync.RWMutex
	byName map[string]Profile
}

func newProfileRegistry() *profileRegistry {
	return &profileRegistry{byName: map[string]Profile{}}
}

// Declare 登记一个档位。
//
// 覆盖规则：**同一 Owner 可覆盖自己的档位**（便于插件更新），
// 但**不能覆盖别人的**（否则任何插件都能改写 QQ 的 owner 档 = 提权口子）。
func (r *profileRegistry) Declare(p Profile) error {
	if r == nil {
		return errNoRuntime
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return &policyErr{"security: profile name must not be empty"}
	}
	if p.Owner == "" {
		return &policyErr{"security: profile " + name + " must declare Owner (audit + overwrite protection)"}
	}
	// 约定 `<owner>.<档位>`：档位名必须带自己的插件名前缀。
	// 这样"跨插件覆盖"天然被名字挡住，而不是只靠下面的检查。
	if !strings.HasPrefix(name, p.Owner+".") {
		return &policyErr{fmt.Sprintf(
			"security: profile %q 必须以声明者前缀 %q 开头（防止跨插件覆盖）", name, p.Owner+".")}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.byName[name]; ok && old.Owner != p.Owner {
		return &policyErr{fmt.Sprintf(
			"security: profile %q 已由 %q 声明，%q 不能覆盖", name, old.Owner, p.Owner)}
	}
	r.byName[name] = p
	return nil
}

// Get 查询档位。
func (r *profileRegistry) Get(name string) (Profile, bool) {
	if r == nil {
		return Profile{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byName[name]
	return p, ok
}

// Names 返回全部档位名（状态面用，排序后返回更稳定）。
func (r *profileRegistry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	return out
}

// activeProfile 是一次已激活的档位（带 TTL）。
type activeProfile struct {
	name    string
	expires time.Time
}

// profileActivator 持有"当前活跃档位"。
//
// ★ 为什么是"当前活跃"而不是"每次调用都传"：
// 工具调用发生在 LLM 决定之后，此时插件已经不在调用栈上（异步通道、
// 多轮工具循环）。唯一可靠的传递方式是**内核在输入进来时记住**，
// 在该轮的工具判定里使用。
type profileActivator struct {
	mu     sync.RWMutex
	active activeProfile
}

// Activate 激活一个档位（带 TTL）。
func (a *profileActivator) Activate(name string, ttl time.Duration) {
	if a == nil || name == "" {
		return
	}
	if ttl <= 0 {
		ttl = scopeTTLDefault
	}
	a.mu.Lock()
	a.active = activeProfile{name: name, expires: time.Now().Add(ttl)}
	a.mu.Unlock()
}

// Current 返回当前活跃档位名（过期即空）。
func (a *profileActivator) Current() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	cur := a.active
	a.mu.RUnlock()
	if cur.name == "" || time.Now().After(cur.expires) {
		return ""
	}
	return cur.name
}

// Clear 清除活跃档位（一轮结束/换来源时调用）。
func (a *profileActivator) Clear() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.active = activeProfile{}
	a.mu.Unlock()
}

// ───────────────────── Runtime 上的对外入口 ─────────────────────

// DeclareProfile 是**插件声明限式**的入口（SDK 转发到这里）。
func (r *Runtime) DeclareProfile(p Profile) error {
	if r == nil || r.profiles == nil {
		return errNoRuntime
	}
	return r.profiles.Declare(p)
}

// Profiles 返回已声明档位名（诊断/状态面用）。
func (r *Runtime) Profiles() []string {
	if r == nil || r.profiles == nil {
		return nil
	}
	return r.profiles.Names()
}

// ActivateProfile 在 inputch 输入进来时激活插件选择的档位。
//
// 调用点：注入层拿到 `InjectOptions.SecurityProfile` 之后。
// ★ 未声明的档位**不静默放行**：返回错误，让注入方能看见配置错了
// （拼错档位名却默默用默认档，是安全事故的经典形态）。
func (r *Runtime) ActivateProfile(name string, ttl time.Duration) error {
	if r == nil || r.profiles == nil {
		return errNoRuntime
	}
	if name == "" {
		// 空 = 不使用档位（沿用全局策略）。这是**显式**选择，不是拼错。
		if r.active != nil {
			r.active.Clear()
		}
		return nil
	}
	if _, ok := r.profiles.Get(name); !ok {
		return &policyErr{fmt.Sprintf(
			"security: 未声明的安全档位 %q（已声明: %v）—— 拼错不会退化，直接报错", name, r.profiles.Names())}
	}
	if r.active == nil {
		return errNoRuntime
	}
	r.active.Activate(name, ttl)
	return nil
}

// ActiveProfile 返回当前活跃档位名（空 = 未使用档位）。
func (r *Runtime) ActiveProfile() string {
	if r == nil {
		return ""
	}
	return r.active.Current()
}

// ErrNoRuntimeForBridge 表示桥接层拿到 nil Runtime（装配顺序错误）。
var ErrNoRuntimeForBridge = &policyErr{"security: security runtime is not initialized"}

// TTLSeconds 把秒数转成 Duration（0 = 用默认 TTL）。
//
// 为什么桥接层用秒而不是 Duration：SDK 是跨语言的（Lua/Go/外部进程），
// 秒是唯一无歧义的单位。
func TTLSeconds(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// profilePolicy 取当前活跃档位的策略（无档位时返回零值 + false）。
func (r *Runtime) profilePolicy() (Profile, bool) {
	return r.profilePolicyByName(r.ActiveProfile())
}

// profilePolicyByName 按**指定**档位名取策略（per-agent 视图用）。
//
// ★ 与 profilePolicy 的分工：
//
//	profilePolicy        —— 读进程级 active（旧路径，单执行体场景）
//	profilePolicyByName  —— 由调用方给出档位（per-agent 视图必须走这条）
//
// 之所以要拆：per-agent 视图的档位是**它自己的** session/longtime，
// 不能去读 `Runtime.active` —— 那样多 agent 并发时仍会串档。
func (r *Runtime) profilePolicyByName(name string) (Profile, bool) {
	if name == "" || r == nil || r.profiles == nil {
		return Profile{}, false
	}
	return r.profiles.Get(name)
}
