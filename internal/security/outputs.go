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
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// DeclareOutputs 登记某 agent 被授权使用的输出通道（父在创建子时声明）。
//
// ★ 2026-11-18（§3.2.2 五表合一）：**不再有 outputsByAgent 表**。
//
//	授权是 `AgentRecord.Facts.AllowedOutputs` 的一个字段，清理点只有
//	"记录消失"一处。本方法保留同名入口（调用方表达的是"声明这件事"），
//	落点改到记录表。
//
// 无记录表（裸 Runtime / 单测）⇒ 静默不做：判定侧同样 fail-open，
// 行为与改造前"未声明 = 完整授权"逐字一致。
func (r *Runtime) DeclareOutputs(agentID string, outputs []string) {
	if r == nil || agentID == "" {
		return
	}
	if reg, ok := r.writableRegistry(); ok {
		cp := make([]string, len(outputs))
		copy(cp, outputs)
		reg.Update(agentID, func(rec *model.AgentRecord) {
			rec.Facts.AllowedOutputs = cp
		})
	}
}

// ReleaseOutputs 撤销某 agent 的授权声明。
//
// ★ 现在它**什么都不用做**：记录被删除时授权随之消失（单一事实来源）。
//
//	保留入口是为了不动调用点语义（一处 Remove 覆盖全部事实）。
func (r *Runtime) ReleaseOutputs(agentID string) {
	// 授权随记录走：见 AgentRegistry.Remove（唯一清理点）。
	_ = agentID
}

// CheckOutput 判定某 agent 能否使用某个输出通道。
//
// `channel` 可以是普通通道名（`qq` / `webui`），也可以是设备形态
// （`device/<id>`）—— 两者是同一道闸的不同取值（改造前就是如此）。
func (r *Runtime) CheckOutput(agentID string, channel string) model.Decision {
	if r == nil {
		return model.Decision{Allowed: true}
	}
	allowed := r.factsFor(agentID).AllowedOutputs

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

// ───────────────────── 中断级别的授权夹取 ─────────────────────

// ClampPriority 把注入方声明的中断级别收到允许的范围（**授权**判定）。
//
// ## 为什么在安全模块，而不在 agent 的投递路径
//
// L4（"立即打断"）只有编译期内置插件与父 agent 可以声明。外部插件若拿到它，
// 就能抢占全部执行，而被打断方**无从分辨来源**。所以夹取必须是**注入侧的
// 授权**，与工具/通道授权同一层；放在每个 agent 的投递路径上会变成
// "每个实现各夹一次"，漏一处就是一个提权面。
//
// ## 语义
//
//	privileged=true  原样返回（内置插件 / 父 agent）
//	privileged=false 夹到 L3；L1/L2/L3 原样
//	未知字符串        返回 fallback "L1"（拼错不该被静默当成高级别）
func (r *Runtime) ClampPriority(declared string, privileged bool) string {
	if privileged {
		return declared
	}
	switch strings.ToUpper(strings.TrimSpace(declared)) {
	case "", "L1":
		return "L1"
	case "L2":
		return "L2"
	case "L3":
		return "L3"
	case "L4":
		// ★ 外部来源声明的 L4 一律夹到 L3：这不是"降级执行"，而是
		//   "它本来就没有 L4 这个能力"。
		return "L3"
	default:
		return "L1"
	}
}
