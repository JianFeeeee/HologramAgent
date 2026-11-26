package io

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// ErrToolNotFound 表示「工具不存在」（未注册 / 所属插件已卸载或崩溃）。
//
// 存在的理由：工具是**动态注册**的（buildToolDefs 每轮重建、plgreload 即时生效、
// 插件崩溃后被摘除），因此「不存在」是运行期常态而非异常。
// 判别它必须**类型化**：此前 core 靠 strings.Contains(err, "not found in any plugin")
// 匹配错误文案，而插件的错误文案只要恰好含该子串就会被误判为「工具不存在」
// 并错误 fallback。errors.Is 才能精确区分「不存在」与「执行失败」——
// 二者对 on_error 的处置完全不同（前者工具没了，后者可 retry）。
var ErrToolNotFound = errors.New("工具不存在或未注册")

// ToolNotFound 返回一个包裹 ErrToolNotFound 的错误，带上工具名。
func ToolNotFound(name string) error {
	return fmt.Errorf("tool %s: %w", name, ErrToolNotFound)
}

// IsToolNotFound 报告 err 是否为「工具不存在」。
func IsToolNotFound(err error) bool { return errors.Is(err, ErrToolNotFound) }

// ChannelDef 描述通道在记忆计算层的行为，与 ToolDef.NoMemory/Cleaner 语义一致。
type ChannelDef = pubsdk.ChannelDef

type DeviceType int

const (
	DeviceInput  DeviceType = 0
	DeviceOutput DeviceType = 1
	DeviceIO     DeviceType = 2
)

// OutputCapability 定义通道支持的输出格式
type OutputCapability int

const (
	CapText       OutputCapability = 1 << iota // 文本
	CapFile                                    // 文件
	CapImage                                   // 图片
	CapAudio                                   // 音频
	CapStructured                              // 结构化数据（JSON/卡片）
	// CapRT 表示该通道**能承载 rt 流**（实时通话）。
	//
	// ★ 这是**通道**侧的声明，与 agent 是否为 rtchild 是两件事
	//   。rt 通道的划拨校验要两侧都满足：
	//     · 通道声明自己能承载 rt 流（本能力位）
	//     · 目标 agent 是 rtchild（AgentConfig.IsRealtime）
	//   校验落点是 AssignRealtime，而不是普通 Assign —— 计划原文：
	//   「标签不是准入门槛：不带 rt 标签的普通通道同样可以划给 rt agent」。
	//
	// ★ 它**不**表示"内核要产出音频"。outputch(rt) 只产出**文本**，
	//   怎么展示（含 TTS）是插件的活（用户裁定 R6）。
	CapRT
)

func (c OutputCapability) Supports(cap OutputCapability) bool {
	return c&cap != 0
}

// capabilityByName 是**能力名 ↔ 位**的唯一映射表。
//
// ★ 名字是插件侧的中立词（sdk 声明的 `Capabilities`），位是内核侧类型；
//
//	这张表是两者之间唯一的一次转换。加第三种能力只加一行常量 + 一行表项。
var capabilityByName = map[string]OutputCapability{
	"text":       CapText,
	"file":       CapFile,
	"image":      CapImage,
	"audio":      CapAudio,
	"structured": CapStructured,
	"rt":         CapRT,
}

// CapabilityByName 把中立能力名解析成能力位。
func CapabilityByName(name string) (OutputCapability, bool) {
	c, ok := capabilityByName[strings.ToLower(strings.TrimSpace(name))]
	return c, ok
}

// CapabilityNames 返回全部已知能力名（错误文案用，顺序稳定）。
func CapabilityNames() string {
	names := make([]string, 0, len(capabilityByName))
	for n := range capabilityByName {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (c OutputCapability) String() string {
	var flags []string
	if c&CapText != 0 {
		flags = append(flags, "text")
	}
	if c&CapFile != 0 {
		flags = append(flags, "file")
	}
	if c&CapImage != 0 {
		flags = append(flags, "image")
	}
	if c&CapAudio != 0 {
		flags = append(flags, "audio")
	}
	if c&CapStructured != 0 {
		flags = append(flags, "structured")
	}
	if c&CapRT != 0 {
		flags = append(flags, "rt")
	}
	return fmt.Sprintf("%v", flags)
}

type Device interface {
	Name() string
	Type() DeviceType
	Description() string
	Tools() []ToolDef
	Execute(tool string, args map[string]interface{}) (interface{}, error)
	Start() error
	Stop() error
	OutputCapabilities() OutputCapability
	ChannelDef() ChannelDef
}

type ToolHandler func(args map[string]interface{}) (interface{}, error)

type ToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
	Handler     ToolHandler            `json:"-"` // 可选：插件工具的直接处理器，Device 通过 Execute() 分发
	// ParallelSafe 与 SDK 的 ToolDef.ParallelSafe 同义：声明此设备工具可被
	// **并发执行**。零值 false = 不可并发（保守默认，见 SDK 注释）。
	ParallelSafe bool `json:"parallel_safe,omitempty"`
	// Serial 与 SDK 的 ToolDef.Serial 同义：声明此设备工具**必须**串行。
	// 判据优先级高于 ParallelSafe（显式声明不允许被任何默认值覆盖）。
	//
	// 对设备工具尤其重要：设备侧（C 实现）工具的并发安全性内核无从审核，
	// "没声明"可能只是因为那套接口里压根没这个字段 —— 显式给出
	// Serial:true 是设备作者唯一能表达"这里有隐含顺序约束"的途径。
	Serial bool `json:"serial,omitempty"`
}

type InputEvent struct {
	RequestID     string                 `json:"request_id"`
	Source        string                 `json:"source"`
	Type          string                 `json:"type"`
	Payload       map[string]interface{} `json:"payload"`
	ResponseCh    chan<- *OutputEvent    `json:"-"`
	OutputChannel string                 `json:"output_channel"` // 默认输出通道（不传则等于 Source）
}

type OutputEvent struct {
	RequestID     string                 `json:"request_id"`
	Target        string                 `json:"target"`
	Type          string                 `json:"type"`
	Payload       map[string]interface{} `json:"payload"`
	Done          bool                   `json:"done,omitempty"`
	OutputChannel string                 `json:"output_channel"` // 路由到此通道
}

type IOManager struct {
	mu         sync.RWMutex
	devices    map[string]Device
	inputCh    chan *InputEvent
	outputCh   chan *OutputEvent
	outputOnce sync.Once
	nextReqID  int64
	channelReg *ChannelRegistry

	// parent 是"上级 IOManager"（驻留子的轻量内核指向父的内核）。
	//
	// 为什么需要：**输出通道在 io 层就是 Device**，而它们是由插件登记在**父**的
	// io 上的。驻留子有自己的 IOManager（自己的输入入口、自己的 outputCh），
	// 若只看自己那张空表，`output_send__<通道>` 会被判"通道不存在或不可用"，
	// `output_list_channels` 是空的，`output_send__*` 工具也不会生成
	// —— 现场表现就是"驻留子不会说话/不会发消息"（联调实录：父侧通道装载完整、
	// 子侧 childIO 空壳）。
	//
	// 用**实时回退**而不是创建时复制快照：设备会随资源生灭（远程设备上线/掉线
	// 以分钟计），复制出来的表转瞬就过期。授权由各自的 AllowedOutputs 白名单把关，
	// 回退只解决"看得见"，不解决"能不能用"。
	parent *IOManager

	// priorityClamp 是**注入侧的授权夹取**（由装配点注入安全闸门）。
	//
	// ★ 为什么在 io 而不是 agent 的投递路径：夹取是"外部来源有没有 L4 这个
	//   能力"的**授权**判定。放在投递路径上会变成"每个类型各夹一次"，
	//   漏一处就是提权面；而所有注入都必经这里。
	//
	// nil ⇒ 不夹取（单测/独立工具；生产装配期由 kernelmod/agent 注入）。
	priorityClamp func(declared, source string) string

	// inputRouter 决定一条输入是否被"别的 agent"接管（返回 true = 已接管）。
	//
	// 为什么放在 io：inputch 是**最基本的输入路由单位**，而**路由发生在进内核之前**
	// （docs/zh/resident-subagent-design.md §4.1）。插件注入输入的收口就在这里，
	// 所以路由必须在这里生效 —— inputch 划给某个 agent 后，输入**只流向那个 agent**，
	// 本内核根本看不到它。io 层不认识 agent，路由器由内核注入
	// （见 core.Agent.routeInputByOwner）。
	inputRouter InputRouter

	// toolBlocks：插件工具注入多模态内容块，process.go 在下一条 tool message 时消费。
	// 用 interface{}[] 避免 import api.ContentBlock 导致的循环依赖。
	toolBlocksMu sync.Mutex
	// toolPendingBlocks 按 **call_id** 归档多模态块。
	//
	// 为何不用单槽：并行执行下（同批多个 tool_call 同时跑），单槽会让
	// 后执行的 ConsumeToolBlocks 抢走前一个工具注入的媒体 ⇒ 挂到错误的
	// tool 消息上。task.go 里"媒体必须紧跟自己的 toolMsg"那条结论
	// （三轮实测得出）会被直接破坏。
	toolPendingBlocks map[string][]interface{}
}

func NewIOManager() *IOManager {
	return &IOManager{
		devices:           make(map[string]Device),
		inputCh:           make(chan *InputEvent, 256),
		outputCh:          make(chan *OutputEvent, 256),
		channelReg:        NewChannelRegistry(),
		toolPendingBlocks: make(map[string][]interface{}),
	}
}

// InputRouter 是输入路由器的签名。
//
//	evt        待投递的输入事件（OutputChannel 即它的 inputch）
//	返回 true  = 已被别的 agent 接管，本内核不再处理
//
// ★ 没有"这是中断还是排队"这个参数：那是**接收方的排队语义**
// （见 kernelapi.InputEvent 的 priority 标签），路由只按归属搬运。
type InputRouter func(evt *InputEvent) bool

// SetPriorityClamp 注入优先级夹取（装配期一次；nil = 不夹取）。
func (m *IOManager) SetPriorityClamp(fn func(declared, source string) string) {
	m.mu.Lock()
	m.priorityClamp = fn
	m.mu.Unlock()
}

// SetInputRouter 注入输入路由器（nil = 不路由，行为与以前完全一致）。
func (m *IOManager) SetInputRouter(r InputRouter) {
	m.mu.Lock()
	m.inputRouter = r
	m.mu.Unlock()
}

// deliverInput 是**本内核**接收一条外部输入的收口：先按 inputch 归属路由，
// 被别的 agent 接管就不进本内核队列（划给子的 inputch，父不再收到 —— 这是「划拨」
// 的语义，不是"父也顺便看一眼"）。
func (m *IOManager) deliverInput(evt *InputEvent) {
	m.mu.RLock()
	router := m.inputRouter
	m.mu.RUnlock()
	if router != nil && router(evt) {
		return
	}
	m.pushLocal(evt)
}

// DeliverRouted 把**已被路由**的事件放进本内核队列（不再二次路由）。
// 由路由器实现调用：父把输入交给持有该 inputch 的子。
func (m *IOManager) DeliverRouted(evt *InputEvent) {
	m.pushLocal(evt)
}

// pushLocal 把所有输入放进**同一个通道**。
//
// ★ 这里曾经分 `interruptCh` / `inputCh` 两条：那是**主 agent 的排队语义**
// （中断要抢占、普通输入要排队），不是基座概念 —— rt 连队列都没有、
// worker 什么都没有。现在只有一条入口，`priority` 标签随事件走，
// 由**接收方**的投递口自己分拣（用户裁定：内核只带标签，不分拣）。
func (m *IOManager) pushLocal(evt *InputEvent) {
	m.inputCh <- evt
}

// SetParentIO 设置上级 IOManager（nil 表示无上级，行为与以前完全一致）。
// 见 parent 字段的说明：用于驻留子继承父的输出通道/设备视图。
func (m *IOManager) SetParentIO(p *IOManager) {
	m.mu.Lock()
	m.parent = p
	m.mu.Unlock()
}

// lookupDevice 查设备：自己的登记优先，其次回退到上级。
//
// 先在自己锁内取快照再查上级，**不跨锁调用**（避免锁序问题）。
func (m *IOManager) lookupDevice(name string) Device {
	m.mu.RLock()
	dev, ok := m.devices[name]
	parent := m.parent
	m.mu.RUnlock()
	if ok {
		return dev
	}
	if parent != nil {
		return parent.GetDevice(name)
	}
	return nil
}

func (m *IOManager) UnregisterDevice(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.devices, name)
}

func (m *IOManager) nextRequestID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextReqID++
	return fmt.Sprintf("req_%d_%d", time.Now().UnixNano(), m.nextReqID)
}

// AtomicSwapDevices 原子化替换全部 IO 设备
// 1. 新设备必须在调用前已完成 Start()
// 2. 调用后旧设备立即摘除，新请求走向新设备
// 3. 返回旧设备列表，由调用方负责 Stop()
func (m *IOManager) AtomicSwapDevices(newDevices map[string]Device) map[string]Device {
	m.mu.Lock()
	defer m.mu.Unlock()

	oldDevices := m.devices
	m.devices = newDevices

	return oldDevices
}

func (m *IOManager) RegisterDevice(dev Device) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.devices[dev.Name()]; ok {
		return fmt.Errorf("device %s already registered", dev.Name())
	}
	m.devices[dev.Name()] = dev
	return nil
}

func (m *IOManager) GetDevice(name string) Device {
	return m.lookupDevice(name)
}

func (m *IOManager) StartAll() error {
	m.mu.RLock()
	devices := make([]Device, 0, len(m.devices))
	for _, dev := range m.devices {
		devices = append(devices, dev)
	}
	m.mu.RUnlock()

	for _, dev := range devices {
		if err := dev.Start(); err != nil {
			return fmt.Errorf("start device %s: %w", dev.Name(), err)
		}
	}
	return nil
}

func (m *IOManager) StopAll() {
	m.mu.RLock()
	devices := make([]Device, 0, len(m.devices))
	for _, dev := range m.devices {
		devices = append(devices, dev)
	}
	m.mu.RUnlock()

	for _, dev := range devices {
		if err := dev.Stop(); err != nil {
			log.Printf("[io] stop device %s error: %v", dev.Name(), err)
		}
	}
}

func (m *IOManager) InjectInput(source string, eventType string, payload map[string]interface{}) {
	m.deliverInput(&InputEvent{
		RequestID:     m.nextRequestID(),
		Source:        source,
		Type:          eventType,
		Payload:       payload,
		OutputChannel: source,
	})
}

func (m *IOManager) InjectInputSync(source string, eventType string, payload map[string]interface{}) *OutputEvent {
	ch := make(chan *OutputEvent, 1)
	m.deliverInput(&InputEvent{
		RequestID:     m.nextRequestID(),
		Source:        source,
		Type:          eventType,
		Payload:       payload,
		ResponseCh:    ch,
		OutputChannel: source,
	})
	// 被路由走时，回答由持有该 inputch 的 agent 写进同一个 ResponseCh
	//（§4.3：同步输入的回程是事前定好的）——所以这里照常等待。
	return <-ch
}

// InjectInputTo 注入输入事件并指定输出通道
func (m *IOManager) InjectInputTo(source, outputChannel, eventType string, payload map[string]interface{}) {
	m.deliverInput(&InputEvent{
		RequestID:     m.nextRequestID(),
		Source:        source,
		Type:          eventType,
		Payload:       payload,
		OutputChannel: outputChannel,
	})
}

// InjectInputSyncTo 注入输入事件（同步等待）并指定输出通道
func (m *IOManager) InjectInputSyncTo(source, outputChannel, eventType string, payload map[string]interface{}) *OutputEvent {
	ch := make(chan *OutputEvent, 1)
	m.deliverInput(&InputEvent{
		RequestID:     m.nextRequestID(),
		Source:        source,
		Type:          eventType,
		Payload:       payload,
		ResponseCh:    ch,
		OutputChannel: outputChannel,
	})
	return <-ch
}

// InjectOptions 声明一次注入在记忆层与上下文层的表现。
//
// 零值 = 记入记忆 + 不裁剪上下文，与历史的三参数注入方法完全一致。
// 别名到公共 SDK 而非另建一套：内置插件与外部插件必须用同一套结构，
// 否则内核要认两种类型，而漏认会静默丢失标志位。
type InjectOptions = pubsdk.InjectOptions

// applyInjectOpts 把注入标志位写进事件 payload。
//
// 只在非零时写：零值与旧 payload 逐字节一致，事件订阅方与旧内核
// （不认识这些键）都不会受影响。
//
// 为什么不把标志位当独立参数传到底：eventloop 与各注入路径都按 payload 取字段
// （no_memory 本来就是这么走的），payload 是这里唯一已有的携带面。
func applyInjectOpts(payload map[string]interface{}, opts InjectOptions) {
	if opts.NoMemory {
		payload["no_memory"] = true
	}
	if opts.ContextPolicy != "" {
		payload["context_policy"] = opts.ContextPolicy
	}
	if opts.RecallPolicy != "" {
		payload["recall_policy"] = opts.RecallPolicy
	}
	if opts.ScenePolicy != "" {
		payload["scene_policy"] = opts.ScenePolicy
	}
	if opts.CleanerName != "" {
		payload["cleaner_name"] = opts.CleanerName
	}
	// priority 只对中断注入有意义；排队路径会忽略它（内核侧只读不写）。
	if opts.Priority != "" {
		payload["priority"] = opts.Priority
	}
	// ★ 插件声明的**命名安全限式**（可选）：本插件 inputch 触发时选一档。
	//
	// 语义（用户 2026-10-26）：插件声明 `qq.owner` / `qq.guest` 等档位，
	// 按发送者身份在这里带上档位名；内核在**本轮**激活它并据此判定工具
	// 与内容策略。空串 = 不使用档位（沿用内核默认策略），零行为变化。
	if opts.SecurityProfile != "" {
		payload["security_profile"] = opts.SecurityProfile
	}
	// ★ 本轮附加提示（插件陈述的"这一轮才成立的事实"）。
	//
	// 与 `content` 分开携带：content 是用户/来源说的话，turn_hint 是
	// 给模型的**系统侧说明**。混在一起会让模型分不清哪句是人说的，
	// 也让审计看不出哪些文字来自插件而非用户。
	if strings.TrimSpace(opts.TurnHint) != "" {
		payload["turn_hint"] = opts.TurnHint
	}
	// ★ 合并插件的 payload 扩展，但**不允许覆盖内核保留键**。
	//
	// 为什么必须拦：这些键是内核语义（`content` 是正文、`context_policy`
	// 决定裁剪、`priority` 决定抢占级别…）。允许插件覆盖意味着
	// 一个插件能伪造“这条注入不记记忆”或“这条要立即打断”——
	// 那是权限提升，不是便利。
	//
	// 拦的方式是**跳过**而不是拒绝整次注入：插件可能只想带一个业务字段，
	// 因顺手多写了一个保留键就让整条注入失败，代价太大。静默跳过 + 不报错
	// 会不会让调试变难？会在内核日志里看到，因为这是“插件想改内核语义”，
	// 属于应当被看见的事件（见下方 log）。
	for k, v := range opts.Payload {
		if IsReservedPayloadKey(k) {
			log.Printf("[io] injected payload tried to override kernel-reserved key %q (ignored) -- "+
				"this key is decided by kernel semantics and cannot be rewritten by plugins", k)
			continue
		}
		payload[k] = v
	}
}

// reservedPayloadKeys 是**内核语义键**，插件不得通过 InjectOptions.Payload 覆盖。
//
// 来源：实测 grep 全部 `Payload["..."]` 的读取点（internal/agent、internal/plugins），
// 再加注入路径自身写入的键。新增内核语义键时必须同步这个集合，
// 否则新键就成了新的提权面。
var reservedPayloadKeys = map[string]bool{
	// —— 注入路径自身写入的
	"no_memory":      true,
	"context_policy": true,
	"recall_policy":  true,
	"scene_policy":   true,
	"cleaner_name":   true,
	"priority":       true,
	"type":           true,
	"content":        true,
	// —— 内核读取的语义键（见 grep 结果）
	"interrupt":         true,
	"interrupt_source":  true,
	"interrupt_channel": true,
	"stop":              true,
	"command":           true,
	"scene":             true,
	"source":            true,
	"channel":           true,
	"media_blocks":      true,
	"media_type":        true,
}

// IsReservedPayloadKey 报告 k 是否为内核语义键。
//
// 导出给 core 用：emitResponse 把 payload 透传到 stageCtx.Extra 时，
// 也要用**同一份**保留字清单——否则两处清单会漂移，
// 一边拦一边放，等于没拦。
func IsReservedPayloadKey(k string) bool { return reservedPayloadKeys[k] }

func (m *IOManager) InjectInputOpts(source, eventType string, payload map[string]interface{}, opts InjectOptions) {
	applyInjectOpts(payload, opts)
	m.InjectInput(source, eventType, payload)
}

func (m *IOManager) InjectInputToOpts(source, outputChannel, eventType string, payload map[string]interface{}, opts InjectOptions) {
	applyInjectOpts(payload, opts)
	m.InjectInputTo(source, outputChannel, eventType, payload)
}

func (m *IOManager) InjectInputSyncToOpts(source, outputChannel, eventType string, payload map[string]interface{}, opts InjectOptions) *OutputEvent {
	applyInjectOpts(payload, opts)
	return m.InjectInputSyncTo(source, outputChannel, eventType, payload)
}

func (m *IOManager) InjectInterruptOpts(source, channel string, payload map[string]interface{}, opts InjectOptions) {
	applyInjectOpts(payload, opts)
	m.clampInterruptPriority(source, payload)
	m.InjectInterrupt(source, channel, payload)
}

// clampInterruptPriority 在**注入时刻**把声明的级别夹到允许范围。
//
// 特权来源（编译期内置插件 / 父 agent）由装配点给的判据决定；
// 判据本身在安全模块（ClampPriority），io 只把"谁在注入"这一事实递过去。
//
// ★ 失败方向是收紧：夹取函数缺失时**不夹**（单测/独立工具无闸门），
//
//	但生产装配必然注入它 —— 与"安全模块未加载 ⇒ 恒放行"同一口径。
func (m *IOManager) clampInterruptPriority(source string, payload map[string]interface{}) {
	if m == nil || payload == nil {
		return
	}
	raw, ok := payload["priority"].(string)
	if !ok || raw == "" {
		return
	}
	m.mu.RLock()
	clamp := m.priorityClamp
	m.mu.RUnlock()
	if clamp == nil {
		return
	}
	payload["priority"] = clamp(raw, source)
}

func (m *IOManager) InjectText(source string, text string) {
	m.InjectInput(source, "text", map[string]interface{}{
		"content": text,
	})
}

func (m *IOManager) InjectTextSync(source string, text string) *OutputEvent {
	return m.InjectInputSync(source, "text", map[string]interface{}{
		"content": text,
	})
}

// InjectTextTo 注入文本输入并指定输出通道
func (m *IOManager) InjectTextTo(source, outputChannel, text string) {
	m.InjectInputTo(source, outputChannel, "text", map[string]interface{}{
		"content": text,
	})
}

// InjectTextNoMemoryTo 注入文本输入（不产生记忆）并指定输出通道
func (m *IOManager) InjectTextNoMemoryTo(source, outputChannel, text string) {
	m.InjectInputTo(source, outputChannel, "text", map[string]interface{}{
		"content":   text,
		"no_memory": true,
	})
}

// InjectTextSyncNoMemoryTo 注入文本输入（同步等待，不产生记忆）并指定输出通道
func (m *IOManager) InjectTextSyncNoMemoryTo(source, outputChannel, text string) *OutputEvent {
	return m.InjectInputSyncTo(source, outputChannel, "text", map[string]interface{}{
		"content":   text,
		"no_memory": true,
	})
}

// InjectInterrupt 向中断通道发送输入
func (m *IOManager) InjectInterrupt(source, channel string, payload map[string]interface{}) {
	if payload == nil {
		payload = map[string]interface{}{}
	}
	evtType, _ := payload["type"].(string)
	m.deliverInput(&InputEvent{
		RequestID:     m.nextRequestID(),
		Source:        source,
		Type:          evtType,
		Payload:       payload,
		OutputChannel: channel,
	})
}

func (m *IOManager) InjectInterruptText(source, channel, text string) {
	m.InjectInterruptTextOpts(source, channel, text, InjectOptions{})
}

// InjectInterruptTextOpts 注入中断文本，并声明本次注入的记忆/裁剪行为。
//
// 中断也允许声明 ContextPolicyPrune：中断同样携带内容进入上下文。
func (m *IOManager) InjectInterruptTextOpts(source, channel, text string, opts InjectOptions) {
	m.InjectInterruptOpts(source, channel, map[string]interface{}{
		"type":    "text",
		"content": text,
	}, opts)
}

// InjectTextOpts 注入排队文本，并声明本次注入的记忆/裁剪行为。
func (m *IOManager) InjectTextOpts(source, channel, text string, opts InjectOptions) {
	m.InjectInputToOpts(source, channel, "text", map[string]interface{}{
		"content": text,
	}, opts)
}

// InjectTextSyncOpts 同步注入文本并声明记忆/裁剪行为。
func (m *IOManager) InjectTextSyncOpts(source, outputChannel, text string, opts InjectOptions) *OutputEvent {
	return m.InjectInputSyncToOpts(source, outputChannel, "text", map[string]interface{}{
		"content": text,
	}, opts)
}

// InjectTextSyncTo 注入文本输入（同步等待）并指定输出通道
func (m *IOManager) InjectTextSyncTo(source, outputChannel, text string) *OutputEvent {
	return m.InjectInputSyncTo(source, outputChannel, "text", map[string]interface{}{
		"content": text,
	})
}

// ---- 带标志位的注入（记忆/裁剪行为由调用点声明）----

// InjectInputMediaOpts 注入带媒体块的输入，并声明记忆/裁剪行为。
func (m *IOManager) InjectInputMediaOpts(source, outputChannel, text string, blocks []pubsdk.ContentBlock, opts InjectOptions) {
	m.InjectInputToOpts(source, outputChannel, "text", map[string]interface{}{
		"content":      text,
		"media_blocks": blocks,
	}, opts)
}

// InjectInputMediaSyncOpts 注入带媒体块的输入并同步等待回复，同时声明记忆/裁剪行为。
func (m *IOManager) InjectInputMediaSyncOpts(source, outputChannel, text string, blocks []pubsdk.ContentBlock, opts InjectOptions) *OutputEvent {
	return m.InjectInputSyncToOpts(source, outputChannel, "text", map[string]interface{}{
		"content":      text,
		"media_blocks": blocks,
	}, opts)
}

// InjectInterruptMediaOpts 注入带媒体块的中断，并声明记忆/裁剪行为。
func (m *IOManager) InjectInterruptMediaOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts InjectOptions) {
	m.InjectInterruptOpts(source, channel, map[string]interface{}{
		"type":         "text",
		"content":      text,
		"media_blocks": blocks,
	}, opts)
}

func (m *IOManager) EmitOutput(target string, outputType string, payload map[string]interface{}) {
	m.outputCh <- &OutputEvent{
		RequestID: "",
		Target:    target,
		Type:      outputType,
		Payload:   payload,
		Done:      true,
	}
}

// EmitOutputTo 通过指定输出通道发送
func (m *IOManager) EmitOutputTo(target, outputChannel, outputType string, payload map[string]interface{}) {
	m.outputCh <- &OutputEvent{
		RequestID:     "",
		Target:        target,
		Type:          outputType,
		Payload:       payload,
		Done:          true,
		OutputChannel: outputChannel,
	}
}

func (m *IOManager) EmitText(target string, text string) {
	m.EmitOutput(target, "text", map[string]interface{}{
		"content": text,
	})
}

// EmitTextTo 通过指定输出通道发送文本
func (m *IOManager) EmitTextTo(target, outputChannel, text string) {
	m.EmitOutputTo(target, outputChannel, "text", map[string]interface{}{
		"content": text,
	})
}

func (m *IOManager) InputChan() <-chan *InputEvent   { return m.inputCh }
func (m *IOManager) OutputChan() <-chan *OutputEvent { return m.outputCh }

// PendingInputs 返回**尚未被调度器取走**的输入条数（排队队列之外的那一段）。
//
// 为何需要单独一个口径：输入先进 inputCh，再由 pumpInbox 搬进调度器就绪队列。
// “停止”要统计的是“用户按下停止时还没被处理的消息”，而它们大多还躺在
// inputCh 里（调度器正忙于处理当前任务）。只数 sched.queue 会得到 0，
// 于是短路配额失效——实测就是这个现象：停止后排队消息照旧逐条跑完。
func (m *IOManager) PendingInputs() int {
	if m == nil {
		return 0
	}
	return len(m.inputCh)
}

// RegisterInputChannel 注册一个 inputch（不带插件归属，兼容旧调用）。
//
// inputch 是**最基本的输入路由单位**；一个插件可以注册多个。
// 新代码请用 RegisterInputChannelFrom 以便登记归属插件（可追溯）。
func (m *IOManager) RegisterInputChannel(name string, def ChannelDef) {
	_ = m.RegisterInputChannelFrom("", name, def)
}

// RegisterInputChannelFrom 注册一个 inputch 并登记归属插件。
func (m *IOManager) RegisterInputChannelFrom(plugin, name string, def ChannelDef) error {
	return m.channelReg.Register(InputChannel{Name: name, Plugin: plugin, Def: def})
}

// UnregisterInputChannel 注销一个 inputch。
func (m *IOManager) UnregisterInputChannel(name string) {
	m.channelReg.Unregister(name)
}

// AssignInputChannel 把一个 inputch 划给某个 agent（见 ChannelRegistry.Assign）。
func (m *IOManager) AssignInputChannel(name, agentID string, capacity int) error {
	return m.channelReg.Assign(name, agentID, capacity)
}

// SetChannelRegistry 注入一份**共享的**登记表（根 agent 与驻留子共用同一份）。
func (m *IOManager) SetChannelRegistry(r *ChannelRegistry) {
	if r == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.channelReg = r
}

// ChannelRegistry 返回底层登记表（只读用途；可直接读 Views）。
func (m *IOManager) ChannelRegistry() *ChannelRegistry { return m.channelReg }

// InputChannels 返回全部已注册 inputch（按名字排序）。
func (m *IOManager) InputChannels() []InputChannel { return m.channelReg.List() }

// LookupInputChannel 查询单个 inputch 的完整登记记录。
func (m *IOManager) LookupInputChannel(name string) (InputChannel, bool) {
	return m.channelReg.Lookup(name)
}

// GetInputChannelDef 查询 inputch 的记忆行为定义。
func (m *IOManager) GetInputChannelDef(name string) (ChannelDef, bool) {
	ch, ok := m.channelReg.Lookup(name)
	if !ok {
		return ChannelDef{}, false
	}
	return ch.Def, true
}

// ToolDefOf 按工具名取其声明（含 Parameters schema）。
// 用途：内核在执行前按 schema 预校验——没有它就只能校验到插件工具，
// 而设备/通道工具（cmd_run、files_write 等）会完全绕过校验。
func (m *IOManager) ToolDefOf(name string) (ToolDef, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, dev := range m.devices {
		for _, t := range dev.Tools() {
			if t.Name == name {
				return t, true
			}
		}
	}
	return ToolDef{}, false
}

func (m *IOManager) GetAllTools() []ToolDef {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var tools []ToolDef
	for _, dev := range m.devices {
		tools = append(tools, dev.Tools()...)
	}
	return tools
}

// DeviceOfTool 返回提供该工具的**设备/输出通道名**（设备类工具才有）。
//
// 用途：设备类工具（device_ctl_*/screensee/computeruse/...）需要按"目标设备"
// 做授权判断，调用方得先知道这个工具属于哪个设备通道。
func (m *IOManager) DeviceOfTool(name string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, dev := range m.devices {
		for _, t := range dev.Tools() {
			if t.Name == name {
				return dev.Name(), true
			}
		}
	}
	return "", false
}

// ExecuteTool 执行设备工具（无取消语义）。
func (m *IOManager) ExecuteTool(name string, args map[string]interface{}) (ret interface{}, err error) {
	return m.ExecuteToolCtx(context.Background(), name, args)
}

// ExecuteToolCtx 执行设备工具，并把 ctx 传给设备。
//
// ctx 同样不进 Device.Execute 的签名（那是插件作者的契约），而是**旁路**
// 放进 args（键与 core.executeCtxArgKey 一致）。内置插件若要消费它，
// 在自己的 handler 里取一次即可；不取就与今天完全一致。
func (m *IOManager) ExecuteToolCtx(ctx context.Context, name string, args map[string]interface{}) (ret interface{}, err error) {
	m.mu.RLock()
	type nameDevice struct {
		name string
		dev  Device
	}
	var candidates []nameDevice
	for _, dev := range m.devices {
		for _, t := range dev.Tools() {
			if t.Name == name {
				candidates = append(candidates, nameDevice{name: dev.Name(), dev: dev})
				break
			}
		}
	}
	m.mu.RUnlock()

	if len(candidates) == 0 {
		// 自己没这个设备工具 → 看上级（驻留子的设备工具都在父的 io 上）。
		//
		// ⚠️ 父的**执行失败**不得被吞成「工具不存在」：那会让 on_error 的
		// retry 失效（本该重试的失败被判为工具没了，整组被跳过）。
		// 因此只把父的「确实不存在」继续向上传递，其余错误如实上抛。
		m.mu.RLock()
		parent := m.parent
		m.mu.RUnlock()
		if parent != nil {
			ret, err := parent.ExecuteToolCtx(ctx, name, args)
			if err == nil {
				return ret, nil
			}
			if !IsToolNotFound(err) {
				return nil, err
			}
		}
		return nil, ToolNotFound(name)
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[io] tool %s execute panic: %v\n%s", name, r, debug.Stack())
			err = fmt.Errorf("tool %s execute panic: %v", name, r)
		}
	}()
	return candidates[0].dev.Execute(name, bindExecCtx(ctx, args))
}

func (m *IOManager) ListDevices() []Device {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]Device, 0, len(m.devices))
	for _, d := range m.devices {
		list = append(list, d)
	}
	return list
}

// ChannelInfo 返回 IOManager 中已注册的所有通道信息
type ChannelInfo struct {
	Name        string           `json:"name"`
	Type        DeviceType       `json:"type"`
	Description string           `json:"description"`
	Tools       []ToolDef        `json:"tools"`
	OutputCaps  OutputCapability `json:"output_capabilities"`
}

func (m *IOManager) ListChannels() []ChannelInfo {
	m.mu.RLock()
	own := make(map[string]Device, len(m.devices))
	for name, dev := range m.devices {
		own[name] = dev
	}
	parent := m.parent
	m.mu.RUnlock()

	// 自己的登记优先（子侧可覆盖/屏蔽同名通道），随后并入上级的可见通道。
	// 去重按**名字**：同名即视为同一个通道，不重复列举。
	seen := make(map[string]bool, len(own))
	var list []ChannelInfo
	appendDev := func(dev Device) {
		if seen[dev.Name()] {
			return
		}
		seen[dev.Name()] = true
		list = append(list, ChannelInfo{
			Name:        dev.Name(),
			Type:        dev.Type(),
			Description: dev.Description(),
			Tools:       dev.Tools(),
			OutputCaps:  dev.OutputCapabilities(),
		})
	}
	for _, dev := range own {
		appendDev(dev)
	}
	if parent != nil {
		for _, ch := range parent.ListChannels() {
			if seen[ch.Name] {
				continue
			}
			seen[ch.Name] = true
			list = append(list, ch)
		}
	}
	return list
}

func (m *IOManager) GetChannelCapabilities(channel string) OutputCapability {
	if dev := m.lookupDevice(channel); dev != nil {
		return dev.OutputCapabilities()
	}
	return 0
}

// SetToolBlocks 插件工具调用时注入多模态内容块（image_url/audio_url 等），
// 下一条 tool message 追加这些块到 content 数组（OpenAI 多模态格式）。
// SetToolBlocks 写入**当前调用**的多模态块（无 call_id 语境时的兼容入口）。
//
// ⚠️ 兼容语义：多模态插件（multimodal/plugin.go:136,246,320）调的是
// **无参** SetToolBlocks —— 那时内核还拿不到"当前是哪个 call"。
// 并行化后这条路径**不可靠**（无法区分同批多个工具），因此新增
// SetToolBlocksFor(callID, blocks) 供内核在执行前登记 call_id。
// 本方法保留给串行/单工具场景与存量调用方。
func (m *IOManager) SetToolBlocks(blocks []interface{}) {
	m.SetToolBlocksFor("", blocks)
}

// SetToolBlocksFor 按 call_id 归档多模态块 —— 并行安全的入口。
func (m *IOManager) SetToolBlocksFor(callID string, blocks []interface{}) {
	m.toolBlocksMu.Lock()
	defer m.toolBlocksMu.Unlock()
	if m.toolPendingBlocks == nil {
		m.toolPendingBlocks = make(map[string][]interface{})
	}
	m.toolPendingBlocks[callID] = blocks
}

// ConsumeToolBlocks 返回并清空 pending blocks（兼容入口，取 callID=""）。
func (m *IOManager) ConsumeToolBlocks() []interface{} {
	return m.ConsumeToolBlocksFor("")
}

// ConsumeToolBlocksFor 取走并清空**指定 call** 的块。
//
// 取走即消费（第二次返回空）：块被 ConsumeToolBlocksFor 拿走或
// ClearToolBlocks 清理后不再返回。
//
// ⚠️ 未知 callID 返回空且**不影响他人**的块 —— 这一点是并行下的关键：
// 若这里误取走别人的块，媒体会挂到错误的 tool 消息上。
func (m *IOManager) ConsumeToolBlocksFor(callID string) []interface{} {
	m.toolBlocksMu.Lock()
	defer m.toolBlocksMu.Unlock()
	blocks := m.toolPendingBlocks[callID]
	delete(m.toolPendingBlocks, callID)
	return blocks
}

// ClearToolBlocks 清理某个 call 的块（工具超时/取消时避免泄漏）。
func (m *IOManager) ClearToolBlocks(callID string) {
	m.toolBlocksMu.Lock()
	delete(m.toolPendingBlocks, callID)
	m.toolBlocksMu.Unlock()
}

// execCtxArgKey 是内核内部传递执行上下文的旁路键。
//
// ⚠️ 必须与 core.executeCtxArgKey 逐字一致（理由见 proc/execctx.go：
// 复制常量是为了避免 core ↔ io 的导入环，不一致的表现只是"取消不生效"）。
const execCtxArgKey = "\x00kernel.execute_ctx\x00"

// bindExecCtx 把 ctx 放到 args 里随调用下传。
//
// 为什么不改 Device.Execute 签名：那会让全部内置插件跟着改（同一个理由
// 见 StageHost.ExecuteToolCtx 的注释）。旁路键对不消费它的插件完全不可见——
// 但**必须**在真正下发给插件前剥掉，否则严格校验参数的插件会报未知参数。
// 剥除由各插件自己的入口决定：core 侧统一下发前已剥（见 StageHost），
// io 侧保持原样，因为设备插件直接读 args、不会做严格白名单校验。
// ★ 必须**复制**而不是原地写：设备调用在自己的 goroutine 里跑，超时/取消
// 路径下调用方会在 goroutine 仍存活时继续读同一份 args —— 原地写就是数据竞争
// （-race 实测复现，见 StageHost 侧 withExecuteCtx 的同类说明）。
func bindExecCtx(ctx context.Context, args map[string]interface{}) map[string]interface{} {
	if ctx == nil {
		return args
	}
	out := make(map[string]interface{}, len(args)+1)
	for k, v := range args {
		out[k] = v
	}
	out[execCtxArgKey] = ctx
	return out
}

// ChannelConsolidation 是「记忆整理」这类**自输入**的通道名。
//
// ★ 为什么归 io：它是**通道名**（IO 契约的一部分），不是某个类型的私有值。
//
//	状态机、子 agent 实现、事件循环三条线都要判它。
const ChannelConsolidation = "_consolidation_"

// PayloadKeyPriority 是优先级标记的键（"L1".."L4"）。
const PayloadKeyPriority = "priority"

// ───────────────────────── 标记（payload 键）契约 ─────────────────────────
//
// ★ 这些是**内核与注入方之间的契约**：键叫什么、取什么值由内核定义，
//   **怎么解释归类型**（docs §7）。同一个 priority="L4"：
//
//	主 agent  抢占：取消当前 LLM + 现场入栈
//	驻留子    高优先级入队（不挂起）
//	rt        任何输入都覆盖当前轮（它没有"级别"概念）
//	插件类型  插件自己的 Loop 定义
//
// 所以本文件只提供**取值原语**，不提供任何级别映射或中断判定。

// Marks 是输入事件 payload 的标记视图（本类型只读）。
type Marks map[string]interface{}

// MarksOf 取一条 payload 的标记（nil 安全）。
func MarksOf(payload map[string]interface{}) Marks {
	if payload == nil {
		return nil
	}
	return Marks(payload)
}

// Flag 读一个布尔标记（如 `stop` / `interrupt` / `kernel`）。
func (m Marks) Flag(key string) bool {
	if m == nil {
		return false
	}
	v, _ := m[key].(bool)
	return v
}

// Priority 返回**已授权**的优先级原始字符串（"L1".."L4"，空 = 非中断）。
//
// ★ 只有取值，没有映射：把 "L4" 翻成"第几级中断"是类型自己的策略。
//
//	L4→L3 的夹取发生在注入时刻（安全闸门），到这里字符串已经可信。
func (m Marks) Priority() string {
	if m == nil {
		return ""
	}
	s, _ := m[PayloadKeyPriority].(string)
	return s
}

// IsInterrupt 报告这条输入是否**声明了自己是中断**。
//
// ★ 这是"是否为中断"这个**事实**（注入方声明的），不是"该怎么处理"。
func (m Marks) IsInterrupt() bool {
	if m == nil {
		return false
	}
	if v, ok := m["interrupt"].(bool); ok && v {
		return true
	}
	return m.Priority() != ""
}

// StartOutputDispatch 启动**输出通道分发器**：把 `outputCh` 上的每条事件
// 送到 `OutputChannel` 指向的 device handler。
//
// ## 为什么它必须存在（2026-11-25 实测缺口）
//
// `EmitOutputTo` 只是把事件塞进 `outputCh`。这条链路上**没有人消费** ——
// 于是 docs §9.2「输出通道调用本身就是流式载体」这句话在实践中不成立：
// 事件进队列后原地蒸发，插件的通道 handler 一次都不被调。
//
// ## 为什么在 io 层而不是 kernelloop
//
// 分发是"IO 面的实现"，不是"推动机制"：它读的是本 IOManager 自己的
// device 表与自己的队列。`kernelloop.StartOutputConsumer` 只是**载体**
// （随循环生命周期、可秀回收），语义仍在这里。
//
// idempotent：重复启动只起一个 goroutine。
func (m *IOManager) StartOutputDispatch(stop <-chan struct{}) {
	if m == nil {
		return
	}
	m.outputOnce.Do(func() {
		go func() {
			for {
				select {
				case <-stop:
					return
				case evt, ok := <-m.outputCh:
					if !ok {
						return
					}
					m.dispatchOutput(evt)
				}
			}
		}()
	})
}

// dispatchOutput 把一条输出事件交给对应通道的 handler。
//
// ★ 找不到通道 ⇒ 响亮记日志，**不静默丢**：那通常意味着插件没注册
// (RegisterOutputChannel 失败) 或通道名拼错，静默丢弃会让"agent 说了话
// 但用户看不到"变成一个无从排查的现象。
func (m *IOManager) dispatchOutput(evt *OutputEvent) {
	if evt == nil {
		return
	}
	name := evt.OutputChannel
	if name == "" {
		name = evt.Target
	}
	if name == "" {
		log.Printf("[io] output event without channel (target=%q type=%q) — dropped", evt.Target, evt.Type)
		return
	}
	dev := m.lookupDevice(name)
	if dev == nil && evt.Target != "" && evt.Target != name {
		dev = m.lookupDevice(evt.Target)
	}
	if dev == nil {
		log.Printf("[io] no output device %q for output event (type=%q) — dropped "+
			"(plugin failed to RegisterOutputChannel?)", name, evt.Type)
		return
	}
	// ★ handler 的入参形状沿用既有约定：payload 字段**展平**到顶层
	//   （cli/webui 的 handler 都按 `args["payload"]` / `args["delta"]` /
	//   `args["type"]` 读）。这是插件侧的既有契约，不能改。
	args := make(map[string]interface{}, len(evt.Payload)+2)
	for k, v := range evt.Payload {
		args[k] = v
	}
	args["type"] = evt.Type
	if evt.Payload != nil {
		if p, ok := evt.Payload["payload"]; ok {
			args["payload"] = p
		} else if p, ok := evt.Payload["content"]; ok {
			args["payload"] = p
		}
	}
	if _, err := dev.Execute("output", args); err != nil {
		log.Printf("[io] output device %q handler failed: %v", name, err)
	}
}
