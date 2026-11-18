package agent

// toolfilter.go —— **渲染侧**的工具白/黑名单过滤（用户裁定 2026-11-18）。
//
// ## 为什么渲染也要过滤
//
// 此前 `allowedTools` / `denyTools` 只在**执行侧**拦（`security.DeclareChildPolicy`），
// 而 `tooldefs.go` 生成工具表时**零过滤** ⇒ 模型看得见父明确禁用的工具，
// 调一次被拒一次，白烧上下文与一轮往返。
//
// 用户裁定（【八】）：
//
//	范围（父声明的白名单） → **渲染**：进不进工具表 → 基座
//	闸门（CheckTool）      → **调用**：过不过     → 安全模块
//
// 两者是**两张表**，不是同一个判定的两次调用。
//
// ## 语义（与安全模块的主体策略逐字一致）
//
//	deny 命中          ⇒ 不渲染（deny 优先）
//	allow 非空且未命中  ⇒ 不渲染
//	两者都空           ⇒ 不额外限制
//
// 支持 `cmd_*` 形式的前缀通配（与 security 的 matchTool 同一约定）。

import "strings"

// splitToolPatterns 把白/黑名单规整成匹配用的形式（去空白、丢空串）。
func splitToolPatterns(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// toolAllowedForRender 报告某个工具名要不要**渲染进工具表**。
//
// ★ 它只回答"看不看得见"，不回答"能不能调"（后者是安全模块的闸门）。
func (a *Agent) toolAllowedForRender(name string) bool {
	if a == nil || name == "" {
		return true
	}
	for _, pat := range a.toolDenySet {
		if matchToolPattern(pat, name) {
			return false
		}
	}
	if len(a.toolAllowSet) == 0 {
		return true
	}
	for _, pat := range a.toolAllowSet {
		if matchToolPattern(pat, name) {
			return true
		}
	}
	return false
}

// matchToolPattern 匹配一个工具名模式（`*` 只在结尾作为前缀通配）。
//
// 与 `security.matchTool` 同一约定：只支持尾部 `*`，不做正则 ——
// 工具名是标识符，正则在这里只会带来误配与注入面。
func matchToolPattern(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	}
	return false
}

// filterToolDefsForRender 按 `toolAllowedForRender` 过滤工具表。
//
// ★ 为什么在**渲染出口**（而不是各生成分支里各判一次）：生成逻辑有六条
// 来源（io / stageHost / indexer / 知识 / 记忆族 / 注册表），在每处加一个
// if 就会漏一处 —— 而漏掉的那一处正是"模型看得见禁用工具"的入口。
//
// 白/黑名单都为空时**原样返回**（零成本，绝大多数 agent 是这种）。
func (a *Agent) filterToolDefsForRender(tools []interface{}) []interface{} {
	if a == nil || (len(a.toolAllowSet) == 0 && len(a.toolDenySet) == 0) {
		return tools
	}
	out := make([]interface{}, 0, len(tools))
	for _, raw := range tools {
		name := toolNameOfDef(raw)
		if name == "" || a.toolAllowedForRender(name) {
			out = append(out, raw)
		}
	}
	return out
}

// toolNameOfDef 从工具定义里取名字（两种形态都要认）。
//
// 形态：`map[string]interface{}{"function": {"name": ...}}`（LLM schema）
// 或带 `name` 的扁平结构。取不到 ⇒ 空串 ⇒ 调用方**保留**它（宁可多渲染一个，
// 也不要因为解析失败而让工具从表里消失）。
func toolNameOfDef(raw interface{}) string {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return ""
	}
	if fn, ok := m["function"].(map[string]interface{}); ok {
		if n, ok := fn["name"].(string); ok {
			return n
		}
	}
	if n, ok := m["name"].(string); ok {
		return n
	}
	return ""
}
