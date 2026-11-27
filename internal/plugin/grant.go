package plugin

// grant.go —— **per-agent 授权三档**的集合运算（唯一实现）。
//
// ## 为什么是表运算，不是判定链
//
// 改造前 allow/deny 是两条独立清单，各判一次（`security.Policy.CheckTool`
// 与 `agent.toolAllowedForRender` 两处逐字相同）：
//
//	deny 命中 ⇒ 拒绝；allow 非空且未命中 ⇒ 拒绝；都空 ⇒ 允许
//
// 那种形态**接不住 add**：allow 非空即"封闭白名单"，add 进去的项根本无所
// 安放（`({A} ) ∪ {B}` 里 B 不在 A 里 ⇒ 被拒）。而 add 是明确要求的能力
// （运行期为已授范围补一项）。
//
// 所以三档改成对**同一张表**的运算：
//
//	表 = (全量 ∩ Allow) ∪ Add − Deny
//
// Deny 是**删除操作**，作用于当前结果 ⇒ 它天然既能撤销 Allow 里的，也能
// 撤销 Add 里的，不需要区分来源。这正是"deny 面向表"与"deny 是否决票"
// 的区别：前者只声明 deny 时结果就是"全量 − deny"。
//
// ## 三档全空的语义
//
// 不裁剪（= 全量）。这与存量 fail-open 契约一致：父不为子声明就完整授权。

import (
	"sort"
	"strings"
)

// Grant 是一个 agent 实例的授权声明（插件 + 工具两组三档）。
//
// 零值合法：三档全空 = 不裁剪。
type Grant struct {
	AllowPlugins []string
	AddPlugins   []string
	DenyPlugins  []string

	AllowTools []string
	AddTools   []string
	DenyTools  []string
}

// IsZero 报告本授权是否为空（三档全空 ⇒ 调用方可走零成本路径）。
func (g Grant) IsZero() bool {
	return len(g.AllowPlugins) == 0 && len(g.AddPlugins) == 0 && len(g.DenyPlugins) == 0 &&
		len(g.AllowTools) == 0 && len(g.AddTools) == 0 && len(g.DenyTools) == 0
}

// PluginTable 按本授权把**全量插件名**裁成该实例可见的表。
func (g Grant) PluginTable(all []string) []string {
	return tableOf(all, g.AllowPlugins, g.AddPlugins, g.DenyPlugins)
}

// ToolTable 按本授权把**全量工具名**裁成该实例可用的表。
func (g Grant) ToolTable(all []string) []string {
	return tableOf(all, g.AllowTools, g.AddTools, g.DenyTools)
}

// PluginVisible 报告某插件在该实例的表里（名字不在表里 ⇒ 假）。
func (g Grant) PluginVisible(name string) bool {
	return visibleIn(name, g.AllowPlugins, g.AddPlugins, g.DenyPlugins)
}

// ToolVisible 报告某工具在该实例的表里。
func (g Grant) ToolVisible(name string) bool {
	return visibleIn(name, g.AllowTools, g.AddTools, g.DenyTools)
}

// tableOf 是表运算的唯一实现：`(all ∩ allow) ∪ add − deny`。
//
// ## 实现要点
//
//   - `allow` 空 ⇒ 起点是**全量**（不是空集）；否则起点是 `all ∩ allow`
//   - `add` 并进去（add 里的名字即使不在 all 里也保留：它可能是运行期才
//     注册的，或者是通配形如 `cmd_*`，all 里查不到不代表无效）
//   - `deny` 最后减，作用于当前结果
//
// ★ 为什么 add 不做 `all ∩ add`：`all` 是**当前快照**（插件可能还没加载完、
// 工具可能按需注册），把 add 项与快照取交会让"先授权、后注册"变成静默失效。
// 授权是**意图**，不依赖当下刚好有什么。
func tableOf(all, allow, add, deny []string) []string {
	// ① 起点
	var cur []string
	if len(allow) == 0 {
		cur = append(cur, all...)
	} else {
		// ★ allow 也要支持通配（`cmd_*`）：它与 deny/add 是同一套名字模式，
		//   只做精确集合匹配会让 `allowed_tools=["cmd_*"]` 静默变空。
		for _, n := range all {
			if anyHit(allow, n) {
				cur = append(cur, n)
			}
		}
	}

	// ② 并入 add（去重）
	seen := make(map[string]bool, len(cur)+len(add))
	out := make([]string, 0, len(cur)+len(add))
	for _, n := range cur {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range add {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		// add 支持通配：带 `*` 的项不是具体名字，展开成 all 里的命中项。
		if strings.HasSuffix(n, "*") {
			for _, cand := range all {
				if !seen[cand] && wildcardHit(n, cand) {
					seen[cand] = true
					out = append(out, cand)
				}
			}
			continue
		}
		seen[n] = true
		out = append(out, n)
	}

	// ③ 减去 deny
	if len(deny) == 0 {
		sort.Strings(out)
		return out
	}
	final := out[:0]
	for _, n := range out {
		if !anyHit(deny, n) {
			final = append(final, n)
		}
	}
	sort.Strings(final)
	return final
}

// visibleIn 回答单个名字是否在表运算结果里（不需要构造整张表）。
//
// 与 tableOf 必须**逐字等价** —— 两处判据漂移会出现"渲染看得见、执行被拒"
// 或反过来。所以它的逻辑与 tableOf 严格对应：
//
//	deny 命中              ⇒ 不在
//	allow 空               ⇒ 在（除非被 deny）
//	allow 非空且未命中：
//	    add 命中           ⇒ 在
//	    否则               ⇒ 不在
func visibleIn(name string, allow, add, deny []string) bool {
	if name == "" {
		return false
	}
	if anyHit(deny, name) {
		return false
	}
	if len(allow) == 0 {
		return true
	}
	if anyHit(allow, name) {
		return true
	}
	return anyHit(add, name)
}

// anyHit 报告名字是否命中任一模式（支持尾部 `*`）。
func anyHit(patterns []string, name string) bool {
	for _, p := range patterns {
		if wildcardHit(p, name) {
			return true
		}
	}
	return false
}

// wildcardHit 匹配一个名字模式：精确，或尾部 `*` 前缀。
//
// ★ 只支持尾部 `*`：中间的 `*` 会诱导写出难以推理的规则，而授权规则必须
// 一眼可判。与 `security.matchTool` / `agent.matchToolPattern` 同一约定。
func wildcardHit(pattern, name string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == name
}
