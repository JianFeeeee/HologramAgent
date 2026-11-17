package security

// outputs.go —— **输出/设备授权**归安全模块（用户裁定 2026-11-17）。
//
// ## 用户原话
//
// > 「设备是否授权不应该安全模块负责吗？」
//
// 对。在此之前授权判定是 **agent 自己的**一个字段比较（`IsOutputAllowed`
// 遍历 `a.allowedOutputs`），于是：
//
//	· 授权判定分散在两处（agent 的工具闸 + SDK 的 ToolAPI.CanUse），
//	  它们靠"晚绑定闭包注入"维持一致（`SetDeviceAuthQuery`）
//	· 插件侧那条路径要绕一大圈（SDK 不能依赖 agent，只能注入查询函数）
//	· 审计面上看不到"某 agent 被授权了什么"（那是 agent 的私有字段）
//
// 现在：授权**事实**由 agent/父声明（`DeclareOutputs`），**判定**在安全模块
// （`CheckOutput`），与工具策略 / 路径策略 / 内容审查同一层。
//
// ## 语义（与改造前逐字一致，不改变任何行为）
//
//	空/nil 授权集合  ⇒ 完整授权（放行）
//	非空             ⇒ 白名单（只放行列出的通道）
//	未声明           ⇒ 放行（该 agent 没被声明过，与"空集合"同义）
//
// ★ fail-open 是存量契约的一部分：`device_id` 缺失、agent 未声明授权，
//   改造前都是放行。改语义会让现网插件集体失效，因此这里**只搬位置不改语义**。

import (
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// DeclareOutputs 登记某 agent 被授权使用的输出通道（父在创建子时声明）。
func (r *Runtime) DeclareOutputs(agentID string, outputs []string) {
	if r == nil || agentID == "" {
		return
	}
	r.outputsMu.Lock()
	defer r.outputsMu.Unlock()
	if r.outputsByAgent == nil {
		r.outputsByAgent = map[string][]string{}
	}
	// 拷贝一份：调用方后续改动切片不应该影响已声明的授权。
	cp := make([]string, len(outputs))
	copy(cp, outputs)
	r.outputsByAgent[agentID] = cp
}

// ReleaseOutputs 撤销某 agent 的授权声明（销毁时调用，防同名重建继承）。
func (r *Runtime) ReleaseOutputs(agentID string) {
	if r == nil || agentID == "" {
		return
	}
	r.outputsMu.Lock()
	defer r.outputsMu.Unlock()
	delete(r.outputsByAgent, agentID)
}

// CheckOutput 判定某 agent 能否使用某个输出通道。
//
// `channel` 可以是普通通道名（`qq` / `webui`），也可以是设备形态
// （`device/<id>`）—— 两者是同一道闸的不同取值（改造前就是如此）。
func (r *Runtime) CheckOutput(agentID string, channel string) model.Decision {
	if r == nil {
		return model.Decision{Allowed: true}
	}
	r.outputsMu.RLock()
	allowed := r.outputsByAgent[agentID]
	r.outputsMu.RUnlock()

	// 空集合 = 完整授权（存量契约：默认完整授权、父可收窄）。
	if len(allowed) == 0 {
		return model.Decision{Allowed: true}
	}
	for _, c := range allowed {
		if c == channel {
			return model.Decision{Allowed: true}
		}
	}
	return model.Decision{
		Allowed: false,
		Reason: "agent " + agentID + " is not authorized for output channel " + channel +
			" (see output_list_channels for the channels this agent may use)",
	}
}
