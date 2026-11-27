package io

// view.go —— **按实例裁剪的虚拟 IOManager**（用户裁定 2026-11-27）。
//
// # 与虚拟插件管理器（plugin.virtualHost）同构
//
//	plugin:  真实 Registry（唯一事实） → virtualHost{real, agentID, grant}
//	io:      真实 IOManager（唯一事实）→ IOView{real, agentID, allowed}
//
// 三条共同纪律：
//
//	① `real` 是唯一事实 —— 视图**不复制**设备/通道，只持指针
//	② 查询转给 real，再按本实例的归属/授权过滤
//	③ 事实层（IOManager 本身）**一行不改** —— 视图是另一个对象
//
// 第 ③ 条是最容易违反的一条：改造过程中一度往 IOManager 里加
// `real` / `viewAgentID` 字段，那等于把"事实"和"视图"揉进同一个结构，
// 于是"这到底是内核那张表，还是某个实例的视角"再也说不清。
//
// # 它取代的是什么
//
// 改造前子实例的建法是「空 IOManager + 共享父的 ChannelRegistry +
// SetParentIO(父)」：两个口子都让子**看见父的全部**。于是"通道归属 /
// 输出授权"的隔离只剩执行闸，可见面早已全开 —— 模型看见一个必然被拒的通道。
//
// # 可见性判定
//
//	入站通道   Owner == 本实例（seeAll 时全部 —— 根实例是未分配通道的兜底持有者）
//	出站设备   本实例的输出授权（空 = 完整授权，存量 fail-open 契约）

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// IOView 是一个实例视角的 IO 读面。
type IOView struct {
	real    *IOManager
	agentID string
	// seeAll 表示可见全部通道（根实例 / 诊断面）。
	seeAll bool
	// allowed 是输出授权（空 = 完整授权）。
	allowed []string
}

// VirtualIO 返回某实例视角的虚拟 IO 管理器。
//
// 与 `Registry.VirtualPlugins` 同一签名形态：多次调用返回新对象（只两个字段，
// 无快照），所以设备增删/授权变更**立刻**反映出来。
func (m *IOManager) VirtualIO(agentID string, seeAll bool, allowedOutputs []string) *IOView {
	if m == nil {
		return nil
	}
	return &IOView{
		real:    m,
		agentID: agentID,
		seeAll:  seeAll,
		allowed: append([]string(nil), allowedOutputs...),
	}
}

// Real 返回底层真实 IOManager（执行侧要它——执行不是"查询"，不裁剪）。
func (v *IOView) Real() *IOManager {
	if v == nil {
		return nil
	}
	return v.real
}

// ───────────────────────── 出站：设备与授权 ─────────────────────────

// OutputAllowed 报告本实例能否向该通道输出（空授权 = 完整授权）。
func (v *IOView) OutputAllowed(channel string) bool {
	if v == nil {
		return false
	}
	if len(v.allowed) == 0 {
		return true
	}
	for _, p := range v.allowed {
		if channelPatternHit(p, channel) {
			return true
		}
	}
	return false
}

// GetDevice 取本实例可见的设备（未授权 ⇒ nil）。
func (v *IOView) GetDevice(name string) Device {
	if v == nil || v.real == nil || name == "" {
		return nil
	}
	if !v.OutputAllowed(name) {
		return nil
	}
	return v.real.GetDevice(name)
}

// ListChannels 返回本实例可见的设备清单（按授权过滤、按名字排序）。
func (v *IOView) ListChannels() []ChannelInfo {
	if v == nil || v.real == nil {
		return nil
	}
	all := v.real.ListChannels()
	if len(v.allowed) == 0 {
		sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
		return all
	}
	out := make([]ChannelInfo, 0, len(all))
	for _, ch := range all {
		if v.OutputAllowed(ch.Name) {
			out = append(out, ch)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GetAllTools 返回本实例可见的设备工具（按通道可见性过滤、按工具名去重）。
//
// ★ 这是"能执行"与"看得见"必须一致的那一处：只给执行不给渲染 = 模型想
// 不到调；只给渲染不给执行 = 调一次拒一次。
func (v *IOView) GetAllTools() []ToolDef {
	seen := map[string]bool{}
	var out []ToolDef
	for _, ch := range v.ListChannels() {
		for _, td := range ch.Tools {
			if seen[td.Name] {
				continue
			}
			seen[td.Name] = true
			out = append(out, td)
		}
	}
	return out
}

// GetChannelCapabilities 返回某通道的能力位（不可见 ⇒ 0）。
func (v *IOView) GetChannelCapabilities(channel string) OutputCapability {
	if v == nil || v.real == nil || !v.OutputAllowed(channel) {
		return 0
	}
	return v.real.GetChannelCapabilities(channel)
}

// ExecuteToolCtx 执行设备工具（**先过授权，再转给 real**）。
//
// ★ 这是与 plghost 的 `Reload` 同一纪律：视图不只是"看得见"的过滤器，
//
//	它同时是**权限边界**。直接透传等于让视图只做渲染过滤、执行全放行 ——
//	那就回到了改造前"看见一个必然被拒的工具"的老问题，只是方向反了：
//	现在变成"没看见也能调"。
//
// 授权判据是**工具所属的通道**（`output_send__<通道>` 的通道名），所以先反
// 查它属于哪条通道，再判 `OutputAllowed`。
func (v *IOView) ExecuteToolCtx(ctx context.Context, name string, args map[string]interface{}) (interface{}, error) {
	if v == nil || v.real == nil {
		return nil, fmt.Errorf("virtual io: 未装配真实 IOManager")
	}
	if ch, ok := v.channelOfTool(name); ok && !v.OutputAllowed(ch) {
		return nil, fmt.Errorf("通道 [%s] 未授权给本实例（可用通道见 output_list_channels）", ch)
	}
	return v.real.ExecuteToolCtx(ctx, name, args)
}

// channelOfTool 反查某设备工具属于哪条通道（找不到 ⇒ ok=false）。
//
// ★ 找不到即放行：设备工具可能由不声明通道的 device 提供（诊断类），
//
//	把它们一律拒掉会让"未知来源工具"变成永久不可用。
func (v *IOView) channelOfTool(tool string) (string, bool) {
	if v == nil || v.real == nil || tool == "" {
		return "", false
	}
	for _, ch := range v.real.ListChannels() {
		for _, td := range ch.Tools {
			if td.Name == tool {
				return ch.Name, true
			}
		}
	}
	return "", false
}

// ───────────────────────── 入站：通道归属 ─────────────────────────

// InputChannels 返回本实例可见的 inputch（按名字排序）。
func (v *IOView) InputChannels() []InputChannel {
	if v == nil || v.real == nil {
		return nil
	}
	all := v.real.InputChannels()
	if v.seeAll || v.agentID == "" {
		return all
	}
	out := make([]InputChannel, 0, len(all))
	for _, ch := range all {
		if ch.Owner == v.agentID {
			out = append(out, ch)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LookupInputChannel 取单条通道（不可见 ⇒ ok=false）。
//
// ★ 不可见时返回 false 而不是"去问 real"：对不可见的通道报告任何属性，
// 等于把"它存在、归谁"泄露给不该看见它的实例。
func (v *IOView) LookupInputChannel(name string) (InputChannel, bool) {
	if v == nil || v.real == nil || name == "" {
		return InputChannel{}, false
	}
	ch, ok := v.real.LookupInputChannel(name)
	if !ok {
		return InputChannel{}, false
	}
	if v.seeAll || v.agentID == "" || ch.Owner == v.agentID {
		return ch, true
	}
	return InputChannel{}, false
}

// GetInputChannelDef 取某通道的记忆/上下文策略（不可见 ⇒ ok=false）。
func (v *IOView) GetInputChannelDef(name string) (ChannelDef, bool) {
	ch, ok := v.LookupInputChannel(name)
	if !ok {
		return ChannelDef{}, false
	}
	return ch.Def, true
}

// channelPatternHit 匹配通道名模式（精确 或 尾部 `*`），与授权面同一约定。
func channelPatternHit(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == name
}
