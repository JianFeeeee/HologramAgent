package io

import (
	"errors"
	"fmt"
	"log"
	"runtime/debug"
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
)

func (c OutputCapability) Supports(cap OutputCapability) bool {
	return c&cap != 0
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
	mu          sync.RWMutex
	devices     map[string]Device
	inputCh     chan *InputEvent
	interruptCh chan *InputEvent
	outputCh    chan *OutputEvent
	nextReqID   int64
	channelReg  *ChannelRegistry

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
		interruptCh:       make(chan *InputEvent, 64),
		outputCh:          make(chan *OutputEvent, 256),
		channelReg:        NewChannelRegistry(),
		toolPendingBlocks: make(map[string][]interface{}),
	}
}

// InputRouter 是输入路由器的签名。
//
//	evt        待投递的输入事件（OutputChannel 即它的 inputch）
//	isInterrupt 该输入是中断还是排队（两者都要按归属路由）
//	返回 true  = 已被别的 agent 接管，本内核不再处理
type InputRouter func(evt *InputEvent, isInterrupt bool) bool

// SetInputRouter 注入输入路由器（nil = 不路由，行为与以前完全一致）。
func (m *IOManager) SetInputRouter(r InputRouter) {
	m.mu.Lock()
	m.inputRouter = r
	m.mu.Unlock()
}

// deliverInput 是**本内核**接收一条外部输入的收口：先按 inputch 归属路由，
// 被别的 agent 接管就不进本内核队列（划给子的 inputch，父不再收到 —— 这是「划拨」
// 的语义，不是"父也顺便看一眼"）。
func (m *IOManager) deliverInput(evt *InputEvent, isInterrupt bool) {
	m.mu.RLock()
	router := m.inputRouter
	m.mu.RUnlock()
	if router != nil && router(evt, isInterrupt) {
		return
	}
	m.pushLocal(evt, isInterrupt)
}

// DeliverRouted 把**已被路由**的事件放进本内核队列（不再二次路由）。
// 由路由器实现调用：父把输入交给持有该 inputch 的子。
func (m *IOManager) DeliverRouted(evt *InputEvent, isInterrupt bool) {
	m.pushLocal(evt, isInterrupt)
}

func (m *IOManager) pushLocal(evt *InputEvent, isInterrupt bool) {
	if isInterrupt {
		m.interruptCh <- evt
		return
	}
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
	}, false)
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
	}, false)
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
	}, false)
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
	}, false)
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
			log.Printf("[io] 注入 payload 试图覆盖内核保留键 %q（已忽略）——"+
				"该键由内核语义决定，插件不可改写", k)
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
	m.InjectInterrupt(source, channel, payload)
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
	}, true)
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

func (m *IOManager) InputInterruptChan() <-chan *InputEvent { return m.interruptCh }

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

func (m *IOManager) ExecuteTool(name string, args map[string]interface{}) (ret interface{}, err error) {
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
			ret, err := parent.ExecuteTool(name, args)
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
	return candidates[0].dev.Execute(name, args)
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

// Microphone
type Microphone struct {
	name       string
	sampleRate int
	io         *IOManager
}

func NewMicrophone(name string, sampleRate int, io *IOManager) *Microphone {
	return &Microphone{name: name, sampleRate: sampleRate, io: io}
}

func (d *Microphone) Name() string                         { return d.name }
func (d *Microphone) Type() DeviceType                     { return DeviceInput }
func (d *Microphone) OutputCapabilities() OutputCapability { return 0 } // 纯输入
func (d *Microphone) Description() string {
	return fmt.Sprintf("麦克风 (%s, %dHz)", d.name, d.sampleRate)
}
func (d *Microphone) Start() error           { return nil }
func (d *Microphone) Stop() error            { return nil }
func (d *Microphone) ChannelDef() ChannelDef { return ChannelDef{} }

func (d *Microphone) Tools() []ToolDef {
	return []ToolDef{{
		Name:        d.name + "_capture",
		Description: fmt.Sprintf("从 %s 录制音频", d.name),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"duration": map[string]interface{}{"type": "number", "description": "录制时长（秒）", "default": 3},
			},
		},
	}}
}

func (d *Microphone) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	return map[string]interface{}{"device": d.name, "status": "recorded", "format": "wav", "sample_rate": d.sampleRate}, nil
}

// Speaker
type Speaker struct {
	name string
	io   *IOManager
}

func NewSpeaker(name string, io *IOManager) *Speaker {
	return &Speaker{name: name, io: io}
}

func (d *Speaker) Name() string                         { return d.name }
func (d *Speaker) Type() DeviceType                     { return DeviceOutput }
func (d *Speaker) OutputCapabilities() OutputCapability { return CapText | CapAudio }
func (d *Speaker) Description() string                  { return fmt.Sprintf("扬声器 (%s)", d.name) }
func (d *Speaker) Start() error                         { return nil }
func (d *Speaker) Stop() error                          { return nil }
func (d *Speaker) ChannelDef() ChannelDef               { return ChannelDef{} }

func (d *Speaker) Tools() []ToolDef {
	return []ToolDef{{
		Name:        d.name + "_speak",
		Description: fmt.Sprintf("通过 %s 播放语音", d.name),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"text": map[string]interface{}{"type": "string", "description": "播放文本"},
			},
			"required": []string{"text"},
		},
	}}
}

func (d *Speaker) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	text, _ := args["text"].(string)
	return map[string]interface{}{"device": d.name, "status": "playing", "text": text}, nil
}

// Camera
type Camera struct {
	name string
	io   *IOManager
}

func NewCamera(name string, io *IOManager) *Camera {
	return &Camera{name: name, io: io}
}

func (d *Camera) Name() string                         { return d.name }
func (d *Camera) Type() DeviceType                     { return DeviceInput }
func (d *Camera) OutputCapabilities() OutputCapability { return CapImage } // 可返回图片
func (d *Camera) Description() string                  { return fmt.Sprintf("摄像头 (%s)", d.name) }
func (d *Camera) Start() error                         { return nil }
func (d *Camera) Stop() error                          { return nil }
func (d *Camera) ChannelDef() ChannelDef               { return ChannelDef{} }

func (d *Camera) Tools() []ToolDef {
	return []ToolDef{
		{
			Name:        d.name + "_capture",
			Description: fmt.Sprintf("使用 %s 拍照", d.name),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"quality": map[string]interface{}{"type": "integer", "description": "质量1-100", "default": 90},
				},
			},
		},
		{
			Name:        d.name + "_stream",
			Description: fmt.Sprintf("控制 %s 视频流", d.name),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"action": map[string]interface{}{"type": "string", "enum": []interface{}{"start", "stop"}},
				},
				"required": []string{"action"},
			},
		},
	}
}

func (d *Camera) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	return map[string]interface{}{"device": d.name, "status": "captured"}, nil
}

// RobotArm
type RobotArm struct {
	name string
	io   *IOManager
}

func NewRobotArm(name string, io *IOManager) *RobotArm {
	return &RobotArm{name: name, io: io}
}

func (d *RobotArm) Name() string                         { return d.name }
func (d *RobotArm) Type() DeviceType                     { return DeviceIO }
func (d *RobotArm) OutputCapabilities() OutputCapability { return CapStructured }
func (d *RobotArm) Description() string                  { return fmt.Sprintf("机械臂 (%s)", d.name) }
func (d *RobotArm) Start() error                         { return nil }
func (d *RobotArm) Stop() error                          { return nil }
func (d *RobotArm) ChannelDef() ChannelDef               { return ChannelDef{} }

func (d *RobotArm) Tools() []ToolDef {
	return []ToolDef{
		{
			Name:        d.name + "_move",
			Description: fmt.Sprintf("移动 %s 到坐标", d.name),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"x": map[string]interface{}{"type": "number", "description": "X 轴"},
					"y": map[string]interface{}{"type": "number", "description": "Y 轴"},
					"z": map[string]interface{}{"type": "number", "description": "Z 轴"},
				},
				"required": []string{"x", "y", "z"},
			},
		},
		{
			Name:        d.name + "_grip",
			Description: fmt.Sprintf("控制 %s 夹爪", d.name),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"action": map[string]interface{}{"type": "string", "enum": []interface{}{"open", "close"}},
				},
				"required": []string{"action"},
			},
		},
	}
}

func (d *RobotArm) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	return map[string]interface{}{"device": d.name, "tool": tool, "status": "executed"}, nil
}

// GPIODevice
type GPIODevice struct {
	name string
	pins []int
	io   *IOManager
}

func NewGPIODevice(name string, pins []int, io *IOManager) *GPIODevice {
	return &GPIODevice{name: name, pins: pins, io: io}
}

func (d *GPIODevice) Name() string                         { return d.name }
func (d *GPIODevice) Type() DeviceType                     { return DeviceIO }
func (d *GPIODevice) OutputCapabilities() OutputCapability { return CapStructured }
func (d *GPIODevice) Description() string                  { return "GPIO 通用引脚" }
func (d *GPIODevice) Start() error                         { return nil }
func (d *GPIODevice) Stop() error                          { return nil }
func (d *GPIODevice) ChannelDef() ChannelDef               { return ChannelDef{} }

func (d *GPIODevice) Tools() []ToolDef {
	return []ToolDef{
		{
			Name:        d.name + "_gpio_write",
			Description: "设置引脚电平",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pin":   map[string]interface{}{"type": "integer"},
					"value": map[string]interface{}{"type": "integer", "enum": []interface{}{0, 1}},
				},
				"required": []string{"pin", "value"},
			},
		},
		{
			Name:        d.name + "_gpio_read",
			Description: "读取引脚电平",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pin": map[string]interface{}{"type": "integer"},
				},
				"required": []string{"pin"},
			},
		},
	}
}

func (d *GPIODevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	return map[string]interface{}{"device": d.name, "tool": tool, "status": "ok"}, nil
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
