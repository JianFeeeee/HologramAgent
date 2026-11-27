package io

// inputch 是**最基本的输入路由单位**（设计见 docs/zh/resident-subagent-design.md §4）。
//
// 一个插件可以注册多个 inputch；每个 inputch 是彼此独立的路由单位：
// 可以被划给不同的 agent、可以分别限额。中断输入与排队输入**两类都从 inputch 进出**，
// 而"中断 vs 排队"是每条输入自己的类别 —— 不是 inputch 的属性。
//
// 本文件只放**登记表**（谁是注册者、划给了谁、容量多少、记忆策略是什么）。
// 路由本身发生在进内核之前：投递方决定"这条输入投给哪个 inputch"。

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

var (
	// ErrInputChannelUnknown 表示引用了未注册的 inputch。
	ErrInputChannelUnknown = errors.New("inputch 未注册")
	// ErrInputChannelNameEmpty 表示 inputch 名为空。
	ErrInputChannelNameEmpty = errors.New("inputch 名不能为空")
	// ErrAgentLacksCapability 表示**目标 agent**没有声明该通道能力（能力面拒划）。
	//
	// ★ 与 ErrChannelNotRealtime 是**两侧**的两个不同失败：那个是通道侧，
	//   这个是 agent 侧。分开后错误文案能直接指向该动哪一边。
	ErrAgentLacksCapability = errors.New("目标 agent 未声明该通道能力")

	// ErrChannelLacksCapability 表示该通道没有声明被要求的能力位。
	//
	// ★ 与 ErrAgentLacksCapability 是两侧校验的两个不同失败：
	//   一个是"通道承载不了"，一个是"agent 承接不了"。
	ErrChannelLacksCapability = errors.New("通道未声明所需能力位")

	// ErrChannelNotRealtime 是 ErrChannelLacksCapability 在 rt 场景下的具名别名。
	//
	// ★ 保留它是因为既有调用方与判据按名字断言；新代码一律用前者。
	ErrChannelNotRealtime = ErrChannelLacksCapability
)

// InputChannel 是一个 inputch 的完整登记记录。
type InputChannel struct {
	// Name 是路由单位 id（全局唯一，如 "qq"、"webui"、"qq/device-2"）。
	Name string `json:"name"`
	// Plugin 是注册它的插件名（"插件可注册多个 inputch"，归属可追溯）。
	Plugin string `json:"plugin,omitempty"`
	// Owner 是**被划给的 agent id**（"" = 未分配）。
	//
	// ★ 与 Orphaned 的组合决定"输入往哪去"（2026-11-17 起）：
	//
	//	Owner != ""            ⇒ 路由给那个 agent（驻留子/rtchild 或父）
	//	Owner == "" && !Orphaned ⇒ **未分配**：本内核（根）兜底处理
	//	Owner == "" &&  Orphaned ⇒ **悬空**：不归任何人，输入写进 stash
	//
	// 为什么需要区分后两者：原先 Owner 清空就是"悬空"，而路由对
	// Owner=="" 直接 return false ⇒ **输入静默回落给父处理**。
	// 于是"释放通道"实际等于"悄悄接回父"，而父以为已经放手了 ——
	// 同一个状态、两种相反的预期，日志里还看不到。用户 2026-11-17 指出
	// 「释放和挂起语义重复了」，根因就是这里没有"悬空"这个真实状态。
	Owner string `json:"owner,omitempty"`

	// Orphaned 标记该通道**已悬空**（原持有者已释放，还没决定交给谁）。
	//
	// 悬空期间到达的输入**写进 stash**（等接回/划拨时一并交出），
	// 而不是被根 agent 静默接走。这是"真悬空"与"悄悄回落"的唯一区别。
	Orphaned bool `json:"orphaned,omitempty"`
	// Capacity 是该 inputch 的队列容量（0 = 用内核默认值）。
	Capacity int `json:"capacity,omitempty"`
	// Output 是该 inputch 的默认回程输出通道（"" = 由来源/调用方决定）。
	//
	// 注意：这不是"内核路由"—— 输出仍然是 agent 的主动调用；这里只是登记
	// "这个 inputch 的回复默认该往哪个 outputch 走"的映射依据。
	Output string `json:"output,omitempty"`
	// Def 是记忆/上下文策略（沿用 ChannelDef：NoMemory / Cleaner / ContextPolicy）。
	// Cleaner 是函数，故本字段不可序列化（json:"-"）。
	Def ChannelDef `json:"-"`

	// Capabilities 是本通道的**能力位**（rt 流 / 文本 / 音频…）。
	//
	// ★ 为何不放进 Def：Def 是公开 SDK 的 ChannelDef 别名
	// （`type ChannelDef = pubsdk.ChannelDef`），往里面加字段等于改 SDK 契约；
	// 而能力位是**通道实例**的属性，属于内核侧的登记记录。
	//
	// ★ 它声明的是“本通道**能承载**什么”，不是“内核要产出什么”：
	// 例如 CapRT 表示“能承载实时流（新值作废旧值）”，CapAudio 表示
	// “本出口会把文本转成声音”——都是插件侧的实现能力声明。
	Capabilities OutputCapability `json:"capabilities,omitempty"`
}

// ChannelRegistry 是 inputch 的登记表。
//
// 它被设计成**可共享对象**（`*ChannelRegistry`）：根 agent 与它的驻留子共用同一份，
// 这样"划入/授权"才有意义；每个 IOManager 默认自带一份（向后兼容）。
type ChannelRegistry struct {
	mu       sync.RWMutex
	channels map[string]InputChannel
	// stash 是**挂在通道上**的暂存区：驻留子销毁时选 ResidualStash 的残余任务
	// 留在这里，等该通道被接回（Assign 给新 owner）时放出。
	//
	// ★ 为什么挂在通道而不是 agent：这些任务此刻**不属于任何 agent**
	// （子已销毁、父没接手）。挂在通道上才符合"暂存在对应通道"的语义，
	// 也让"谁接回通道谁拿到活"自动成立 —— 不需要额外的转交逻辑。
	stash map[string][]*InputEvent
	// outputTargets 把**输出通道**解析成"目标 agent 的哪个 inputch"。
	// 这是"输出可寻址到具体 agent"的依据（子→父、父→指定子）。
	outputTargets map[string]OutputTarget
}

// OutputTarget 是一个输出通道的投递目标。
type OutputTarget struct {
	// AgentID 是目标 agent（"" = 本 agent / 由传输层通道 device 自行处理）。
	AgentID string `json:"agent_id,omitempty"`
	// InputCh 是目标 agent 上接收它的 inputch（"" = 与输出通道同名）。
	InputCh string `json:"inputch,omitempty"`
}

// NewChannelRegistry 构造一个空的 inputch 登记表。
func NewChannelRegistry() *ChannelRegistry {
	return &ChannelRegistry{
		channels:      make(map[string]InputChannel),
		outputTargets: make(map[string]OutputTarget),
		stash:         make(map[string][]*InputEvent),
	}
}

// Register 登记/更新一个 inputch。
//
// 重复登记（插件重载）时**保留已有的 Owner/Capacity/Output**，只更新
// Plugin 与 Def —— 否则一次插件重载就会把父 agent 做的划分抹掉。
func (r *ChannelRegistry) Register(ch InputChannel) error {
	if ch.Name == "" {
		return ErrInputChannelNameEmpty
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.channels == nil {
		r.channels = make(map[string]InputChannel)
	}
	// ★ Def.Realtime ⇒ 映射到 Capabilities 的 CapRT 位（SDK 声明 → 内核能力位）。
	//
	// 为什么在这里映射而不是让插件直接填能力位：公开 SDK 的 ChannelDef 是
	// 插件能碰到的唯一结构（插件不依赖内核），能力位（OutputCapability）是
	// 内核侧类型。两侧各自表达同一件事，由登记表做**唯一一次**转换。
	// 判据 TestRTChannel_SDKRealtimeBecomesCapRT 钉它。
	if ch.Def.Realtime {
		ch.Capabilities |= CapRT
	}

	if old, ok := r.channels[ch.Name]; ok {
		if ch.Owner == "" {
			ch.Owner = old.Owner
		}
		if ch.Capacity == 0 {
			ch.Capacity = old.Capacity
		}
		if ch.Output == "" {
			ch.Output = old.Output
		}
		if ch.Plugin == "" {
			ch.Plugin = old.Plugin
		}
	}
	r.channels[ch.Name] = ch
	return nil
}

// Unregister 注销一个 inputch。
//
// 同时清理指向它的输出目标登记（outputTargets）：否则通道被注销后，
// ResolveOutputTarget 仍会把它解析成一个已不存在的 inputch。
// 两种指向都要清：显式绑定到该 inputch 的，以及“回退到同名通道”的
// （InputCh == "" 表示与输出通道同名）。
func (r *ChannelRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.channels, name)
	for out, t := range r.outputTargets {
		if t.InputCh == name || (t.InputCh == "" && out == name) {
			delete(r.outputTargets, out)
		}
	}
}

// Lookup 查询一个 inputch。
func (r *ChannelRegistry) Lookup(name string) (InputChannel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ch, ok := r.channels[name]
	return ch, ok
}

// AssignWithCapability 按**能力位**把 inputch 划给某个 agent。
//
// ## 两侧对称校验（docs §8.1.1，用户裁定 2026-11-20）
//
//	通道侧  ch.Capabilities.Supports(cap)   本通道能承载这种流吗
//	agent 侧 agentCap.Supports(cap)         这个 agent 能承接这种流吗
//
// ★ 本函数**不认识任何具体能力名**（不出现 CapRT）：调用方给位，
//
//	它做位与。加第三种能力（音频 / 图像 / 结构化）时本函数零改动。
//
// wantAgentCap 由调用方传入（通常是目标 agent 的能力位），因为 io 包
// 不认识 agent 类型 —— 与 `SetInputRouter` 同一纪律。
func (r *ChannelRegistry) AssignWithCapability(name, agentID string, capacity int,
	cap OutputCapability, agentCap OutputCapability) error {
	r.mu.RLock()
	ch, ok := r.channels[name]
	r.mu.RUnlock()
	if !ok {
		return ErrInputChannelUnknown
	}
	if !ch.Capabilities.Supports(cap) {
		return fmt.Errorf("通道划拨：%w（通道 %q 的 Capabilities 未含 %s）",
			ErrChannelLacksCapability, name, cap)
	}
	if !agentCap.Supports(cap) {
		return fmt.Errorf("通道划拨：%w（agent %q 的能力面未含 %s）",
			ErrAgentLacksCapability, agentID, cap)
	}
	return r.Assign(name, agentID, capacity)
}

// Assign 把一个 inputch **划给**某个 agent（可同时给定容量）。
//
// 语义（默认取值，见设计文档 R6）：**读写授权**，不转移所有权 ——
// 登记表仍记录 Plugin（谁注册的）与 Owner（划给了谁）两件事。
//
// ★ agentID 非空 ⇒ 同时清掉 Orphaned 标记（有人接手了，就不再悬空）。
func (r *ChannelRegistry) Assign(name, agentID string, capacity int) error {
	_, err := r.AssignAndDrainStash(name, agentID, capacity)
	return err
}

// Orphan 把一个 inputch 置为**悬空**：原持有者已释放，还没决定交给谁。
//
// 与 Assign(name, "", 0) 的差别是**真实存在的行为差异**（2026-11-17 前两者等价，
// 那是缺陷 —— 用户指出「释放和挂起语义重复了」）：
//
//	Assign(name, "", 0)  ⇒ 未分配：Owner 清空、无标记 ⇒ 输入由根 agent 兜底处理
//	Orphan(name)         ⇒ 悬空：Owner 清空 + Orphaned ⇒ 输入**写进 stash**，
//	                       谁接回/划拨谁拿到这批活
//
// 悬空期间**不丢输入**（写 stash），也不静默让别人处理。
func (r *ChannelRegistry) Orphan(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.channels[name]
	if !ok {
		return ErrInputChannelUnknown
	}
	ch.Owner = ""
	ch.Orphaned = true
	r.channels[name] = ch
	return nil
}

// IsOrphaned 报告某通道是否处于悬空状态（无该通道时返回 false）。
func (r *ChannelRegistry) IsOrphaned(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ch, ok := r.channels[name]
	return ok && ch.Orphaned
}

// AssignAndDrainStash 与 Assign 相同，但**同时返回该通道上的暂存任务**。
//
// ## 为什么需要它（用户 2026-10-30 要求）
//
// 「原本驻留子持有的通道也应该支持**挂起或接回**主 agent」。
//
// 挂起 = `Assign(name, "", 0)`（Owner 清空，通道仍登记但无人处理）；
// 接回 = `AssignAndDrainStash(name, newOwner, 0)`。
//
// 接回时必须把暂存任务一并交出：如果只改 Owner 不取暂存，
// 那批任务会永远躺在通道上 —— 通道看起来"已经接回来了"，
// 但活没人做，且没有任何报错。**这是本轮最容易埋的静默缺陷。**
func (r *ChannelRegistry) AssignAndDrainStash(name, agentID string, capacity int) ([]*InputEvent, error) {
	r.mu.Lock()
	ch, ok := r.channels[name]
	if !ok {
		r.mu.Unlock()
		return nil, ErrInputChannelUnknown
	}
	ch.Owner = agentID
	if agentID != "" {
		// 有人接手 ⇒ 不再悬空（否则它会一直"悬着"，输入永远只进 stash）。
		ch.Orphaned = false
	}
	if capacity > 0 {
		ch.Capacity = capacity
	}
	r.channels[name] = ch
	// 接回给某个具体 agent 时才释放暂存；挂起（agentID==""）时保持不动 ——
	// 挂起的语义正是"先放着，等有人接"。
	var drained []*InputEvent
	if agentID != "" {
		drained = r.stash[name]
		delete(r.stash, name)
	}
	r.mu.Unlock()
	return drained, nil
}

// List 返回全部已注册 inputch（按名字稳定排序）。
func (r *ChannelRegistry) List() []InputChannel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]InputChannel, 0, len(r.channels))
	for _, ch := range r.channels {
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListByOwner 返回划给某个 agent 的 inputch（owner == "" 时返回**未分配**的）。
func (r *ChannelRegistry) ListByOwner(agentID string) []InputChannel {
	var out []InputChannel
	for _, ch := range r.List() {
		if ch.Owner == agentID {
			out = append(out, ch)
		}
	}
	return out
}

// BindOutputTarget 登记"输出通道 → 目标 agent 的 inputch"的解析。
//
// 例：父把子用的输出通道 "to-child-1" 绑到 (child-1, "sub/in")，
// 于是子经该通道发出的消息会投进 child-1 的 sub/in。
func (r *ChannelRegistry) BindOutputTarget(output, agentID, inputCh string) error {
	if output == "" {
		return errors.New("输出通道名不能为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outputTargets == nil {
		r.outputTargets = make(map[string]OutputTarget)
	}
	r.outputTargets[output] = OutputTarget{AgentID: agentID, InputCh: inputCh}
	return nil
}

// UnbindOutputTarget 取消一个输出通道的目标登记。
//
// 与 BindOutputTarget 成对：输出通道可能是**随资源生灭**的（如远程设备
// 一台设备一个通道），设备掉线后必须解绑，否则 ResolveOutputTarget 会一直
// 把消息路由到一个已不存在的 agent/inputch 上——与 inputch 登记泄漏同一类问题。
// 未登记时是 no-op（幂等），方便清理路径无脑调用。
func (r *ChannelRegistry) UnbindOutputTarget(output string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.outputTargets, output)
}

// ResolveOutputTarget 解析一个输出通道的目标；未登记时 ok=false
// （意味着由传输层通道自行处理，如 qq/webui 这类 device 通道）。
func (r *ChannelRegistry) ResolveOutputTarget(output string) (OutputTarget, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.outputTargets[output]
	return t, ok
}

// ListOutputTargets 返回全部已登记的目标解析（按输出通道名排序）。
func (r *ChannelRegistry) ListOutputTargets() map[string]OutputTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]OutputTarget, len(r.outputTargets))
	for k, v := range r.outputTargets {
		out[k] = v
	}
	return out
}

// Count 返回已注册 inputch 数量。
func (r *ChannelRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.channels)
}

// Stash 把若干条输入事件**暂存到指定通道**（驻留子销毁时用）。
//
// 与"转回父队列"的区别：这些任务此刻不属于任何 agent —— 子已销毁、父没接手。
// 挂在通道上，于是"谁接回这条通道，谁就拿到这批活"自动成立，
// 不需要额外的转交逻辑。这正是用户要求的「暂存在对应通道」。
//
// 通道不存在时返回错误而不是静默丢弃：暂存到一条不存在的通道，
// 等于丢了一条还让人以为存住了。
func (r *ChannelRegistry) Stash(channel string, events []*InputEvent) error {
	if len(events) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.channels[channel]; !ok {
		return fmt.Errorf("通道 %s 不存在，无法暂存残余任务", channel)
	}
	r.stash[channel] = append(r.stash[channel], events...)
	return nil
}

// TakeStash 取走并清空某通道上的全部暂存任务。
//
// 取走即清空：调用方（Assign 的接回路径，或显式调用）负责把它们放出去。
// 返回值可能是 nil（无暂存）。
func (r *ChannelRegistry) TakeStash(channel string) []*InputEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	evs := r.stash[channel]
	delete(r.stash, channel) // 取出即清空，避免重复释放
	return evs
}

// StashLen 返回某通道暂存的条数（用于 ResidentInfo/控制面展示）。
//
// ★ 必须有：暂存如果不可见，它就和"丢弃"没有区别 ——
// 用户需要能查到"这批活还在通道上等着"。
func (r *ChannelRegistry) StashLen(channel string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.stash[channel])
}

// StashTotal 返回全部通道上的暂存总条数。
func (r *ChannelRegistry) StashTotal() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, evs := range r.stash {
		n += len(evs)
	}
	return n
}
