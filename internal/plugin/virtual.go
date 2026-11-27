package plugin

// virtual.go —— **按实例裁剪的虚拟插件管理器**（用户裁定 2026-11-27）。
//
// ## 它解决什么
//
// 插件的"存在"远不止工具：stage 回调 / IO 通道 / 提示词段 / skill / API。
// 改造前这些东西是**全局可见**的 —— 任何实例都能看见任何插件的一切。
// 想让某个子 agent"只活在 cli 与 files 的世界里"，内核没有表达能力。
//
// ## 为什么返回的是**同接口的对象**，不是一份清单
//
// 清单（`[]string`）只够渲染面用；执行面要的是"能不能用这个插件"。
// 两处各算一次就会漂移（渲染看得见、执行被拒）。所以让虚拟管理器
// **实现 `kernelapi.PluginHost` 同一接口**：调用方（基座、类型、渲染面）
// 原本怎么写，仍然怎么写，只是每条查询的结果已按该实例授权过滤。
//
// ## 复杂度内敛在模块内部
//
// 集合运算（`(全量 ∩ allow) ∪ add − deny`）在 `Grant`；
// 多实例/多读写的并发在真实 plghost 的那把 `r.mu`；
// 可见面变化走 `EventPluginsChanged`。
// 基座与类型**感知不到**这些，只持有一个 `kernelapi.PluginHost`。
//
// ## 底层唯一事实
//
// 虚拟管理器**不复制插件对象**，只持 `real` 指针 + 该实例的 agentID。
// 所有查询转给 real，再按授权裁。所以"底层事实上只有一个 plghost"成立。

import (
	"fmt"
	"sort"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// virtualHost 是一个实例视角下的插件管理器（实现 kernelapi.PluginHost）。
type virtualHost struct {
	real    *Registry
	agentID string

	// writable 表示本虚拟管理器是否允许**写操作**（Reload / Load 等）。
	//
	// 只有主 agent 的虚拟管理器可写：它是唯一调 admin_child 的实例
	// （`registry.Meta.MayOffload` 只给它）。子实例拿到的是只读视图 ——
	// 子 agent 不该能改全局插件状态，那是越权。
	writable bool
}

var _ kernelapi.PluginHost = (*virtualHost)(nil)

// VirtualPlugins 返回某实例视角下的虚拟插件管理器。
//
// 多次调用返回新对象，但代价极小（只持有两个字段，无快照）：
// 每次查询现算，所以插件增删/授权变更**立刻**反映出来。
//
// ★ 为什么不做快照：用户裁定走事件通知（`EventPluginsChanged`）+ 重拉。
//
//	快照会让"事件到了但表还是旧的"这种时序问题变成常态。
func (r *Registry) VirtualPlugins(agentID string, writable bool) kernelapi.PluginHost {
	if r == nil {
		return nil
	}
	return &virtualHost{real: r, agentID: agentID, writable: writable}
}

// grantOf 取本实例的授权（每次现查，不缓存）。
func (v *virtualHost) grantOf() Grant {
	if v == nil || v.real == nil {
		return Grant{}
	}
	g, _ := v.real.GrantOf(v.agentID)
	return g
}

// visible 报告某插件是否在本实例的可见表里。
//
// ★ 空 agentID（未声明授权的裸实例）= 不裁剪。这与存量 fail-open 契约一致。
func (v *virtualHost) visible(name string) bool {
	if v == nil || v.real == nil {
		return false
	}
	if v.agentID == "" {
		return true
	}
	return v.grantOf().PluginVisible(name)
}

// List 返回本实例可见的插件名（全量插件表按授权裁剪）。
func (v *virtualHost) List() []string {
	if v == nil || v.real == nil {
		return nil
	}
	all := v.real.List()
	if v.agentID == "" {
		return all
	}
	return v.grantOf().PluginTable(all)
}

// ListLoadedPlugins 返回本实例可见的**已加载**插件名。
func (v *virtualHost) ListLoadedPlugins() []string {
	if v == nil || v.real == nil {
		return nil
	}
	all := v.real.ListLoadedPlugins()
	if v.agentID == "" {
		return all
	}
	return v.grantOf().PluginTable(all)
}

// IsBuiltinPlugin 报告某插件是否为内置插件。
//
// ★ 不在可见表里 ⇒ **false**（而不是"去问 real"）：对不可见的插件报告
// 它的任何属性，等于把"它存在"这条信息泄露给不该看见它的实例。
func (v *virtualHost) IsBuiltinPlugin(name string) bool {
	if !v.visible(name) {
		return false
	}
	return v.real.IsBuiltinPlugin(name)
}

// AutoRestartEnabled 同上：不可见 ⇒ false。
func (v *virtualHost) AutoRestartEnabled(name string) bool {
	if !v.visible(name) {
		return false
	}
	return v.real.AutoRestartEnabled(name)
}

// Reload 全量重载（仅可写视图允许）。
func (v *virtualHost) Reload(dir string) (string, error) {
	if !v.writable {
		return "", fmt.Errorf("插件重载需要主 agent 权限（本实例是只读视图）")
	}
	return v.real.Reload(dir)
}

// ReloadOne 重载单个插件（仅可写视图允许，且目标必须可见）。
func (v *virtualHost) ReloadOne(name string) error {
	if !v.writable {
		return fmt.Errorf("插件重载需要主 agent 权限（本实例是只读视图）")
	}
	if !v.visible(name) {
		return fmt.Errorf("插件 %q 不在本实例的可见范围内", name)
	}
	return v.real.ReloadOne(name)
}

// LoadOneDir 加载单个插件目录（仅可写视图允许）。
func (v *virtualHost) LoadOneDir(dir, name string) error {
	if !v.writable {
		return fmt.Errorf("加载插件需要主 agent 权限（本实例是只读视图）")
	}
	return v.real.LoadOneDir(dir, name)
}

// LoadSkipping 批量加载（仅可写视图允许）。
func (v *virtualHost) LoadSkipping(dir string, skip map[string]bool) error {
	if !v.writable {
		return fmt.Errorf("加载插件需要主 agent 权限（本实例是只读视图）")
	}
	return v.real.LoadSkipping(dir, skip)
}

// SetLoadAllowlist **不**出现在虚拟管理器上（本方法只为实现接口而存在）。
//
// ★ 为什么明确拒绝：它是"允许加载哪些插件"的**进程级全局**开关（failback
// 受限启动用）。让某个实例经虚拟管理器改它，等于让实例改别人的世界 ——
// 那是越权，而且静默成功会让"我明明配了 failback 白名单"变得无法解释。
func (v *virtualHost) SetLoadAllowlist(names []string) {
	// 逆操作：非可写视图直接忽略；可写视图也**不**透传（它不是实例级能力）。
	_ = names
}

// VirtualList 是给基座用的便利口：返回本实例可见插件名（已排序）。
//
// ★ 为什么不叫 List：`List()` 已经是接口方法。这个是给"需要稳定顺序做
// 渲染/比对"的调用方用的（如提示词段按插件排序）。
func (v *virtualHost) VirtualList() []string {
	out := v.List()
	sort.Strings(out)
	return out
}
