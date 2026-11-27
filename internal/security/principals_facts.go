package security

// principals_facts.go —— 主体策略改从 **agent 记录** 读（§3.2.2 五表合一）。
//
// ## 删掉的是什么
//
// 原 `principalRegistry` 有一张 `byID`：父在创建子时 `DeclareChildPolicy`
// 往里塞 `PrincipalPolicy{AllowTools, DenyTools}`，销毁时 `RevokeChildPolicy`
// 删掉。它与另外三张表（outputsByAgent / scopesByAgent / InstanceManager.recs）
// 描述的是**同一件事**：父为这个 agent 声明了什么。
//
// 现在这些声明全在 `AgentRecord.Facts` 一个地方：
//
//	AllowedOutputs  → CheckOutput
//	AllowedTools    → 本文件（主体 allow）
//	DenyTools       → 本文件（主体 deny，**优先**）
//	LongtimeProfile → AgentScope.EffectiveProfile
//
// ⇒ 清理点只剩"记录消失"一处。
//
// ## 保留的是什么
//
// `byTag` 仍是**部署级**声明（插件声明 tag=external 这类策略），
// 它的生命周期不跟随任何具体执行体，所以它继续住在 `principalRegistry`。

import (
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// factsPolicy 把一条 agent 记录的工具事实转成主体策略（零值 = 未声明）。
func factsPolicy(f model.AgentFacts) PrincipalPolicy {
	return PrincipalPolicy{AllowTools: f.AllowedTools, DenyTools: f.DenyTools}
}

// declareChildFacts 由父为子声明工具白/黑名单（写记录表）。
//
// 语义约束（security 保证）：声明只能**收窄** —— 全局 deny 在
// `checkToolForWithProfile` 的第 ① 步先判，所以父的黑名单对子依然有效。
func (r *Runtime) declareChildFacts(childID string, allow, deny []string) error {
	childID = strings.TrimSpace(childID)
	if childID == "" {
		return &policyErr{"security: declare child policy requires a non-empty child id"}
	}
	if len(allow) == 0 && len(deny) == 0 {
		return nil // 父选择不给额外限制
	}
	reg, ok := r.writableRegistry()
	if !ok {
		// 无记录表（裸 Runtime / 单测）：不给子声明 ⇒ 保持无限制，
		// 与改造前 `r == nil` 分支的行为逐字一致。
		return nil
	}
	cpAllow, cpDeny := clones(allow), clones(deny)
	reg.Update(childID, func(rec *model.AgentRecord) {
		rec.Facts.AllowedTools = cpAllow
		rec.Facts.DenyTools = cpDeny
	})
	return nil
}

// revokeChildFacts 撤销父为子声明的工具策略。
//
// ★ 现在它**什么都不用做**：记录消失时策略随之消失。保留函数是为了让
//
//	`child.StopDeps.RevokePolicy` 的调用点语义不变（一处 Remove 覆盖全部）。
func (r *Runtime) revokeChildFacts(childID string) {
	_ = strings.TrimSpace(childID)
}

func clones(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// lookup 取出与主体相关的全部策略（**记录事实** + 每个标签的部署级策略）。
//
// ★ 顺序有意义：记录事实在前（父为子声明的收窄），标签策略在后。
//
//	两者都是"声明"，判定侧不区分来源 —— 它们都只能收窄（全局 deny 先判）。
func (r *Runtime) lookup(p Principal) []PrincipalPolicy {
	if r == nil {
		return nil
	}
	var out []PrincipalPolicy
	// ① 记录表里的父声明事实（§3.2.2：byID 表已删）。
	if f, ok := r.agentFacts(p.ID); ok {
		if pol := factsPolicy(f); len(pol.AllowTools) > 0 || len(pol.DenyTools) > 0 {
			out = append(out, pol)
		}
	}
	// ② 部署级标签策略（生命周期不跟随执行体）。
	if r.principals == nil {
		return out
	}
	r.principals.mu.RLock()
	defer r.principals.mu.RUnlock()
	for _, tag := range p.Tags {
		if pol, ok := r.principals.byTag[tag]; ok {
			out = append(out, pol)
		}
	}
	return out
}
