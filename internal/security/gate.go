package security

// 安全闸门（Security Gate）：**所有跨模块数据流的必经点**。
//
// ## 为什么它不是一个"调用方要记得调"的辅助函数
//
// 用户裁定（2026-10-26）：
//
// > 安全模块不是独立的模块吗？core 发出的请求和 device 返回的请求都会
// > 经过安全模块审查，通过才能进行下一步 —— 相当于安全模块就是横在
// > 其他所有模块与 core 之间的一堵墙。
//
// 因此本文件定义的是**边界契约**：出向（Core → 外部）与入向（外部 → Core）
// 两个方向、各自一个入口。谁想跨过这条边界，就只能走这两个函数 ——
// 而不是各自决定"要不要顺手扫一下"。
//
// ## 实现形态：显式代理，不是运行时魔法
//
// 落地方式是**显式代理**（KernelState 里换掉公共挂载点，见
// internal/kernelmod/security）。这里只提供闸门本身：
//
//	出向：PrepareOutbound —— 拦截明文（换成 @{别名}），保留别名回填能力
//	入向：ScrubInbound    —— 把返回内容里的明文换成 @{别名}
//
// ## 与 Store 的分工
//
// Store 是"变量库"（别名、加密、回填、替换）。
// Gate 是"边界"（方向、来源、放行/拒绝/改写、审计）。
// 上层只认 Gate；Store 的细节不外泄给业务模块。

import (
	"log"
)

// Direction 是数据流方向。
type Direction string

const (
	// DirectionOutbound 是 Core → 外部（工具参数、载荷、设备命令、插件调用）。
	DirectionOutbound Direction = "outbound"
	// DirectionInbound 是外部 → Core（工具结果、设备回执、事件载荷）。
	DirectionInbound Direction = "inbound"
)

// Inspection 描述一次过境的数据属于谁、往哪去。
//
// ★ 只放**定位信息**，不放内容：内容走参数，避免这里成为明文副本。
type Inspection struct {
	Direction Direction
	// Stage 是过境点的短名（"tool_args" / "tool_result" / "plugin_result" …），
	// 用于审计与日志定位（不是给模型的文案）。
	Stage string
	// Tool / Channel / AgentID / Source 是定位信息，缺省时为空。
	Tool    string
	Channel string
	AgentID string
	Source  string
}

// where 返回审计用的位置串（形如 "tool_args:coldcontext_query"）。
func (i Inspection) where() string {
	w := i.Stage
	if i.Tool != "" {
		w += ":" + i.Tool
	} else if i.Channel != "" {
		w += ":" + i.Channel
	}
	return w
}

// Gate 是安全边界的**唯一入口**。
//
// 未启用安全模块时（Store == nil）两个方法都是**零成本透传**：
// 这不是"降级放行"，而是"安全能力未装配"——配置关掉时用户明确选择不用它。
type Gate struct {
	store *Store
}

// NewGate 构造闸门。store 为 nil 表示未启用（所有方法透传）。
func NewGate(store *Store) *Gate { return &Gate{store: store} }

// Store 返回底层变量库（只给 security 模块内部与显式需要元信息的工具用）。
func (g *Gate) Store() *Store {
	if g == nil {
		return nil
	}
	return g.store
}

// Enabled 报告闸门是否真的在拦。
func (g *Gate) Enabled() bool { return g != nil && g.store != nil }

// PrepareOutbound 处理 Core → 外部的文本。
//
// 语义：把**明文凭据**换成 `@{别名}` 交给外部，而不是把明文发出去。
// 外部若需要真实值，由 Fill* 在真正调用插件/设备前回填
// （对外部保持"它看到的是明文"的既有语义，对 LLM 保持"只有别名"）。
//
// 返回 (改写后文本, 拦截事件数)。未启用时原样返回。
func (g *Gate) PrepareOutbound(ins Inspection, text string) (string, int) {
	if !g.Enabled() || text == "" {
		return text, 0
	}
	out, evts := g.store.Replace(text, ins.where(), Patterns)
	for _, e := range evts {
		log.Printf("[security] %s blocked %s (pattern %s, %d bytes, merged=%v)",
			ins.where(), e.Alias, e.Kind, e.Len, e.Merged)
	}
	return out, len(evts)
}

// ScrubInbound 处理外部 → Core 的文本。
//
// ★ 为什么入向也必须扫：入向内容会直接进入模型上下文（role=tool）。
// 只在"写记忆"时清洗是**最坏的组合**——记忆侧干净、当轮泄漏、事后查不到
// （该缺陷在 agent/core 的 toolcall 里有实测记录）。
//
// 返回 (清洗后文本, 拦截事件数)。未启用时原样返回。
func (g *Gate) ScrubInbound(ins Inspection, text string) (string, int) {
	if !g.Enabled() || text == "" {
		return text, 0
	}
	out, evts := g.store.Replace(text, ins.where(), Patterns)
	for _, e := range evts {
		log.Printf("[security] %s scrubbed %s (pattern %s, %d bytes, merged=%v)",
			ins.where(), e.Alias, e.Kind, e.Len, e.Merged)
	}
	return out, len(evts)
}

// FillForExecution 在**真正交给外部执行前**把 `@{别名}` 换回明文。
//
// 语义边界：别名只出现在 LLM 侧；外部（插件/设备）看到真实值且对替换无感知。
// 返回 (回填后文本, 回填处数)。
func (g *Gate) FillForExecution(ins Inspection, text string) (string, int) {
	if !g.Enabled() || text == "" {
		return text, 0
	}
	out, n, used := g.store.Fill(text)
	if n > 0 {
		log.Printf("[security] %s backfilled %d place(s) (aliases %v)", ins.where(), n, used)
	}
	return out, n
}
