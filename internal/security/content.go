package security

// 内容审查 + **轮次作用域策略**。
//
// ## 范围（用户裁定 2026-10-26）
//
// > 包括敏感词过滤与提示词注入预警这些，都应该是安全模块负责并对外提供
// > 接口，插件可以通过接口声明某个轮次下的敏感词与工具禁用与否等方式。
//
// 所以安全模块不只是"凭据防火墙"，而是**策略中枢**：
//
//	① 内容审查：凭据（已有）+ 敏感词 + 提示词注入迹象
//	② 工具策略：全局黑白名单（policy.go）+ **轮次作用域覆盖**（本文件）
//	③ 对外接口：插件通过 SDK 声明"这一轮禁用什么、追加哪些敏感词"
//
// ## 为什么"轮次作用域"必须由内核持有而不是插件自己记
//
// 插件是子进程（或动态库），它的生命周期与本轮不同：插件可能在轮次结束前
// 就崩溃/被重载，而**策略不能因为声明者消失就失效**（否则挂掉插件即可绕过）。
// 所以：插件只**声明**，内核持有并按时清理。

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// FindingKind 是内容审查命中的类别。
type FindingKind string

const (
	// FindingSecret 是凭据（由 Store.Replace 处理：换成 @{别名}）。
	FindingSecret FindingKind = "secret"
	// FindingSensitiveWord 是敏感词命中（由配置/插件声明）。
	FindingSensitiveWord FindingKind = "sensitive_word"
	// FindingInjection 是提示词注入迹象。
	//
	// ★ 语义是**预警**，不是"一定恶意"：正常对话也可能引用
	// "ignore previous instructions"。所以它默认只**标记**并记事件，
	// 是否阻断由策略决定（BlockInjection）。
	FindingInjection FindingKind = "prompt_injection"
)

// Finding 是一次审查命中。
//
// ★ 绝不携带命中原文：它会被写进日志/事件/状态面，
// 而"把敏感词原文记进日志"本身就是泄漏（与 S1 同一纪律）。
type Finding struct {
	Kind   FindingKind `json:"kind"`
	Where  string      `json:"where"`
	Rule   string      `json:"rule,omitempty"` // 命中的规则/词条标识（不是原文）
	Offset int         `json:"offset,omitempty"`
	Length int         `json:"length,omitempty"`
	At     string      `json:"at,omitempty"`
}

// ContentPolicy 是内容审查配置。
type ContentPolicy struct {
	// SensitiveWords 是敏感词表（空 = 不审查）。
	SensitiveWords []string
	// DetectInjection 开启提示词注入迹象检测。
	DetectInjection bool
	// BlockInjection 为 true 时把注入迹象当**阻断**而不是预警。
	BlockInjection bool
}

// injectionMarkers 是提示词注入的**迹象**（不是完整分类器）。
//
// ★ 为什么用迹象而不是"智能判定"：
//   - 可解释：命中时能给出是哪一条，模型/人能据此修正
//   - 可判定：不会因为"模型觉得可疑"而随机误伤
//   - 可演进：词条可配置，检测逻辑不变
//
// 这些片段对应最常见的几类注入形态：忽略先前指令、扮演其他角色、
// 索取系统提示、伪造更高优先级。
var injectionMarkers = []struct {
	Rule    string
	Markers []string
}{
	{"ignore_previous", []string{
		"ignore previous", "ignore all previous", "disregard previous",
		"forget your instructions", "忽略之前", "忽略上述", "无视前面的",
	}},
	{"role_hijack", []string{
		"you are now", "act as", "pretend to be", "从现在开始你是",
		"你现在是", "扮演",
	}},
	{"prompt_exfil", []string{
		"reveal your system prompt", "print your instructions",
		"show me your system prompt", "输出你的系统提示", "打印你的提示词",
		"repeat the text above",
	}},
	{"authority_spoof", []string{
		"system: you must", "developer mode", "sudo mode",
		"admin override", "越狱",
	}},
}

// Scan 审查一段文本，返回命中列表（**不含原文**）。
func (p ContentPolicy) Scan(text, where string) []Finding {
	if text == "" {
		return nil
	}
	var out []Finding
	lower := strings.ToLower(text)

	// ① 敏感词：大小写不敏感的子串匹配。
	// 为什么不做正则：敏感词表是**数据**（可被配置/插件追加），
	// 让数据变成正则等于把注入面开给配置者。
	for _, w := range p.SensitiveWords {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if idx := strings.Index(lower, strings.ToLower(w)); idx >= 0 {
			out = append(out, Finding{
				Kind: FindingSensitiveWord, Where: where,
				Rule: "sensitive:" + ruleID(w), Offset: idx, Length: len(w),
				At: nowISO(),
			})
		}
	}

	// ② 注入迹象。
	if p.DetectInjection {
		for _, m := range injectionMarkers {
			for _, marker := range m.Markers {
				if idx := strings.Index(lower, strings.ToLower(marker)); idx >= 0 {
					out = append(out, Finding{
						Kind: FindingInjection, Where: where,
						Rule: m.Rule, Offset: idx, Length: len(marker),
						At: nowISO(),
					})
					break // 同一规则只报一次，避免刷屏
				}
			}
		}
	}
	return out
}

// ruleID 给敏感词一个**不可逆**的标识（日志里不出现原文）。
func ruleID(word string) string {
	// 用长度 + 首字符类别做粗标识即可：目的是"能对照是哪条规则"，
	// 而不是"能还原出词"。
	return fmt.Sprintf("len%d", len([]rune(word)))
}

// BlockedBy 报告命中里是否有"必须阻断"的项。
func (p ContentPolicy) BlockedBy(findings []Finding) (bool, string) {
	for _, f := range findings {
		switch f.Kind {
		case FindingSensitiveWord:
			// 敏感词默认即阻断：它是配置者明确列出的禁用内容。
			return true, "sensitive word (rule " + f.Rule + ")"
		case FindingInjection:
			if p.BlockInjection {
				return true, "prompt injection marker (" + f.Rule + ")"
			}
		}
	}
	return false, ""
}

// ───────────────────────── 轮次作用域策略 ─────────────────────────

// ScopeOptions 是**插件可声明**的轮次作用域策略。
//
// 用途（用户举的例子）：某个轮次下追加敏感词、禁用部分工具。
type ScopeOptions struct {
	// ID 是作用域标识（同一 ID 重复声明 = 覆盖，便于插件更新自己的策略）。
	ID string
	// DenyTools / AllowTools 追加到全局策略之上（**deny 仍然优先**）。
	DenyTools  []string
	AllowTools []string
	// SensitiveWords 追加到内容审查词表。
	SensitiveWords []string
	// DisableNetwork 在本作用域内禁止网络意图。
	DisableNetwork bool
	// TTL 是存活时间（0 = 用默认 10 分钟）。
	//
	// ★ 必须有 TTL：插件崩溃/被重载时不会有人来注销，
	// 没 TTL 就会留下永久生效的幽灵策略。内核自己负责过期回收。
	TTL time.Duration
}

// scopeTTLDefault 是作用域默认存活时间。
//
// 为什么是分钟级而不是"轮次结束即删"：内核并不总能知道"这一轮"何时结束
// （异步通道、驻留子、抢占挂起都会让轮次边界模糊）。分钟级 TTL 是
// **可解释的上界**，且远大于一轮的合理时长。
const scopeTTLDefault = 10 * time.Minute

// scopeEntry 是已登记的作用域。
type scopeEntry struct {
	opts    ScopeOptions
	expires time.Time
}

// ScopeRegistry 持有全部活跃作用域（进程级，内核持有）。
type ScopeRegistry struct {
	mu     sync.RWMutex
	scopes map[string]scopeEntry
}

// NewScopeRegistry 构造空登记表。
func NewScopeRegistry() *ScopeRegistry {
	return &ScopeRegistry{scopes: map[string]scopeEntry{}}
}

// Declare 登记/覆盖一个作用域策略。
func (r *ScopeRegistry) Declare(opts ScopeOptions) error {
	if strings.TrimSpace(opts.ID) == "" {
		return fmt.Errorf("security: scope id must not be empty")
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = scopeTTLDefault
	}
	r.mu.Lock()
	r.scopes[opts.ID] = scopeEntry{opts: opts, expires: time.Now().Add(ttl)}
	r.mu.Unlock()
	return nil
}

// Revoke 注销一个作用域。
func (r *ScopeRegistry) Revoke(id string) {
	r.mu.Lock()
	delete(r.scopes, id)
	r.mu.Unlock()
}

// Active 返回当前有效的作用域（顺带清理过期项）。
//
// ★ 清理放在读路径上（惰性）：没有后台 goroutine 就不会有"忘了停"的问题，
// 而策略数量是个人规模的小集合，惰性清理足够。
func (r *ScopeRegistry) Active() []ScopeOptions {
	if r == nil {
		return nil
	}
	now := time.Now()
	r.mu.Lock()
	out := make([]ScopeOptions, 0, len(r.scopes))
	for id, e := range r.scopes {
		if now.After(e.expires) {
			delete(r.scopes, id)
			continue
		}
		out = append(out, e.opts)
	}
	r.mu.Unlock()
	return out
}
