package core

// 安全模块的**面向 agent 的工具族**（文档 C8）。
//
// ## 为什么需要这几个工具
//
// 用户拍板的模型是：
//
//	变量 = 内核全局库；**不自动淘汰**，但 agent 可以**主动提交/删除**。
//
// 「不自动淘汰」意味着泄漏控制不能靠超时，只能靠 agent（或人）主动删。
// 那就必须有工具能做这两件事 —— 否则「可主动删除」只是文档里的一句空话。
//
// ## ★ 设计红线：**永不返回明文**
//
// 这几个工具**只能引用别名，不能读回明文**（设计文档 S1）。一旦提供
// 「取回明文」的接口，模型就能把明文写回上下文，整层防护作废 ——
// 因为它会把明文交给 LLM，而那正是要避免的。
//
// 所以：
//   · `secret_submit` 接受明文（这是唯一入口，且必须如此才能登记）
//   · `secret_list` 只给元信息（别名、来源、命中次数）
//   · `secret_forget` 按别名删除
//   · **没有** `secret_get`
//
// ## 关于 secret_submit 的明文暴露面（文档 T10）
//
// 提交时明文会经过 context（模型输出的参数）。这是**已知且可接受**的：
// 提交的目的是「让内核记住这个凭据」，而模型本来就是从这里知道的。
// 真正要防的是「凭据被送到 LLM 之外或写进持久化」—— 那由
// memory_scrub 与 provider 装饰器覆盖。
//
// 更安全的替代路径（`${ENV}` / `@file:`）留给后续（T10），
// 当前先提供最直接的形态。

import (
	"fmt"
	"sort"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/security"
)

func (a *Agent) executeSecretTool(tc agentAPI.ToolCall) string {
	s := security.Global()
	if s == nil {
		return "安全模块未启用（core.security.enabled=false），变量库不可用"
	}
	switch tc.Name {
	case "secret_submit":
		return a.execSecretSubmit(s, tc)
	case "secret_list":
		return a.execSecretList(s, tc)
	case "secret_forget":
		return a.execSecretForget(s, tc)
	}
	return "未知的 secret 工具: " + tc.Name
}

// execSecretSubmit 主动登记一条凭据，返回其别名。
//
// 用途：agent 知道后面要用某个凭据（如刚读到的 token），
// 先登记它，后续在工具参数里用 @{别名} 引用。
func (a *Agent) execSecretSubmit(s *security.Store, tc agentAPI.ToolCall) string {
	value, _ := tc.Arguments["value"].(string)
	if strings.TrimSpace(value) == "" {
		return "value 不能为空"
	}
	note, _ := tc.Arguments["note"].(string)
	alias, err := s.Submit(value, note)
	if err != nil {
		// 无密钥等情况：**响亮失败**，不静默存明文。
		return fmt.Sprintf("登记失败（未以明文降级存储）: %v", err)
	}
	// ★ 只回别名，绝不回明文 —— 否则这个工具就成了"明文读回接口"。
	return fmt.Sprintf("已登记为变量 @{%s}。后续调用请用 @{%s} 引用，不要直接写明文。", alias, alias)
}

// execSecretList 列出变量元信息（**不含明文**）。
func (a *Agent) execSecretList(s *security.Store, tc agentAPI.ToolCall) string {
	infos := s.List()
	if len(infos) == 0 {
		return "变量库为空。用 secret_submit 登记凭据。"
	}
	// 稳定排序：便于人/模型对照（map 遍历顺序是随机的）。
	sort.Slice(infos, func(i, j int) bool { return infos[i].Alias < infos[j].Alias })

	var b strings.Builder
	fmt.Fprintf(&b, "共 %d 个变量（只显示元信息，明文不可读回）：\n", len(infos))
	for _, in := range infos {
		fmt.Fprintf(&b, "  @{%s}", in.Alias)
		if in.Note != "" {
			fmt.Fprintf(&b, "  %s", in.Note)
		}
		fmt.Fprintf(&b, "  [来源 %s, 命中 %d]", in.Source, in.Hits)
		if in.CreatedAt != "" {
			fmt.Fprintf(&b, "  建于 %s", in.CreatedAt)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\n在工具参数里用 @{别名} 引用即可（内核会换回真实值，插件无感知）。")
	return b.String()
}

// execSecretForget 按别名删除一个变量。
func (a *Agent) execSecretForget(s *security.Store, tc agentAPI.ToolCall) string {
	alias, _ := tc.Arguments["alias"].(string)
	alias = strings.TrimSpace(alias)
	// 宽容一点：模型可能把 @{sec_xxx} 整个传进来
	alias = strings.TrimPrefix(alias, "@{")
	alias = strings.TrimSuffix(alias, "}")
	if alias == "" {
		return "alias 不能为空"
	}
	if err := s.Forget(alias); err != nil {
		if err == security.ErrUnknownAlias {
			return fmt.Sprintf("未找到变量 %s（用 secret_list 查看现有变量）", alias)
		}
		return fmt.Sprintf("删除失败: %v", err)
	}
	return fmt.Sprintf("已删除 %s。该别名不再可用。", alias)
}

// secretToolDefs 返回这几个工具的定义（供 buildToolDefs 注入）。
//
// 直接用 toolDef 构造，与其它内置工具同形 —— 免得两处各写一份 schema。
func secretToolDefs() []interface{} {
	return []interface{}{
		toolDef("secret_submit",
			"登记一条凭据（token/密钥/口令）到内核变量库，返回别名。之后在工具参数里用 @{别名} 引用它 —— 内核会在调用插件前把别名换回真实值，插件完全无感知。用于：你刚读到一个凭据、后面要用它，但不想让明文反复出现在对话里。注意：明文只应在本次登记时出现一次。",
			map[string]interface{}{
				"value": map[string]interface{}{
					"type":        "string",
					"description": "凭据原文（只需在这里出现一次）",
				},
				"note": map[string]interface{}{
					"type":        "string",
					"description": "可选：给人看的用途说明（如「Gitea 推送令牌」）",
				},
			}, "value"),
		toolDef("secret_list",
			"列出内核变量库里的全部变量（只给别名与元信息，**不含明文** —— 明文无法读回，这是刻意设计）。用于查看有哪些可用变量、某变量是否已登记。",
			map[string]interface{}{}),
		toolDef("secret_forget",
			"删除一个变量（按别名）。变量库**不会自动淘汰** —— 泄漏控制靠人/agent 主动删，所以凭据用完后应主动删除。",
			map[string]interface{}{
				"alias": map[string]interface{}{
					"type":        "string",
					"description": "要删除的别名（sec_xxx 或 @{sec_xxx} 均可）",
				},
			}, "alias"),
	}
}
