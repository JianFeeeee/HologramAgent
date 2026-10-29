package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// coreHandler 把插件发来的 RPC 调用路由到内核 PluginSDK。
//
// 这是 cabi/loader.go 那 51 个 case 体的**整块平移**（§3.2）：
// 参数解析、调用、错误处理逻辑不变，只把「整数 method id + 三个 C 字符串槽」
// 换成「method 名 + 结构化 JSON 参数」。语义不变，回归风险最小。
//
// 平移带来的直接改善：
//   - 参数不再挤进 s1/s2/s3 + i1/i2 五个固定槽（C ABI 的形状约束）
//   - 不需要 CORE_FREE_STRING：跨进程各自 GC
//   - 错误可携带结构化信息，不只是一个字符串
type coreHandler struct {
	// sdk 是内核为该插件构建的 PluginSDK（与内置插件同一类型）。
	sdk CoreSDK
	// name 是插件名，用于工具归属推断与日志。
	name string

	// host 持有被全部插件共享的 StageContext 段与锁仲裁（§3.3/§3.7）。
	// ❗ 必须是"全部插件共享一个 Host"——每插件一段会退化成副本模型。
	host *Host

	// owner 是本插件在共享槽池里的身份。内核用它校验 arena.free 的归属，
	// 并在插件退出时 ReclaimOwner 回收残留槽。
	owner uint32

	// process 是本插件的子进程句柄，用于按 CallID 找到对应的在途调用
	// （插件申请扩容块时要把块登记到那次调用名下。
	process *Process

	// locks 是 host.locks 的引用，供 stage.lock/unlock 路由。
	locks *lockRegistry

	// invokeTool/invokeCleaner/invokeStageFn/invokeOutput 反向调用插件（内核 → 插件）。
	// 由 Plugin 注入，注册回调时用它们构造 handler。
	invokeTool    func(name string, args map[string]interface{}) (interface{}, error)
	invokeCleaner func(params CleanerInvokeParams) (CleanerInvokeResult, error)
	invokeStageFn func(ctx context.Context, stage string, seq uint64) error
	invokeOutput  func(channel string, args map[string]interface{}) (interface{}, error)

	// evtRing 是事件环的订阅接口（实现由 internal/plugin 提供，避免循环依赖）。
	evtRing EvtRingSubscriber

	// caps 是本插件被授予的能力集（§3.8 权限梯度）。
	// nil 或 unrestricted 时不限制——存量插件未声明 capabilities，
	// 若按最小权限处理会让它们静默降级。
	caps *capabilitySet
}

// EvtRingSubscriber 是事件环订阅接口，由 internal/plugin.EventRing 实现。
// proc 包不依赖 internal/plugin，通过接口解耦。
// EvtRingSubscribe 返回一个取消函数（与 Bus.Subscribe 约定一致）。
type EvtRingSubscriber interface {
	EvtRingSubscribe(types []pubsdk.EventType) func()

	// EvtRingSubscribeTracked 与上面相同，但订阅会被登记、可在内核关停时统一退订。
	//
	// ★ 为什么需要单独的 tracked 版本：这些 handler 会写共享内存，而
	// Host.Close() 会 munmap 整块区域。若订阅不在关停前撤销，一条事件就会让
	// handler 写已解除映射的内存 ⇒ SIGSEGV（Bus.safeCall 的 recover 捕不到
	// runtime 致命错误）。详见 internal/plugin/evtring.go 的 unsubs 说明。
	EvtRingSubscribeTracked(types []pubsdk.EventType) func()
}

// evtCloser 是可关闭的事件环适配层（可选实现）。
//
// Host.Close 在 munmap 前调用它，撤掉全部写共享内存的 Bus handler。
type evtCloser interface{ Close() }

func (h *coreHandler) invokeStageWithCtx(ctx context.Context, stage string, seq uint64) error {
	if h.invokeStageFn == nil {
		return fmt.Errorf("插件 %s: stage 调用通道未就绪", h.name)
	}
	return h.invokeStageFn(ctx, stage, seq)
}

// CoreSDK 是 coreHandler 依赖的内核能力面。
//
// 定义为接口而非直接依赖 internal/sdk.PluginSDK，原因：
//  1. 避免 internal/plugin/proc → internal/sdk 的强耦合（后者已依赖 internal/plugin 的类型）
//  2. 单测可注入假实现，无需构造完整内核
//
// 方法集**刻意只包含外部插件应得的能力**——`Selftest`/`Supervisor`/`Tracker`/
// `Status`/`Adapter`/`Config`/`Tool`/`Indexer`/`OutputChan`/`Publish` 不在此列。
// 这正是把权限梯度从「C ABI 表达能力的意外产物」变成「显式声明并强制的策略」（§3.8）。
type CoreSDK interface {
	PluginName() string

	Settings() pubsdk.SettingsAPI
	// Secrets 是**全进程共享**的变量库（非 per-plugin）。
	Secrets() pubsdk.SecretsAPI
	// Security 是**策略声明面**（插件声明命名档位；判定由内核执行）。
	//
	// ★ 只声明、不判定：本接口刻意没有 CheckTool 之类的方法 ——
	// 那属于内核。插件自判会让策略分散、且插件崩溃即策略失效。
	Security() pubsdk.SecurityAPI
	Memory() pubsdk.MemoryAPI
	TextMemory() pubsdk.TextMemoryAPI
	DocMemory() pubsdk.DocMemoryAPI
	Knowledge() pubsdk.KnowledgeAPI
	LLM() pubsdk.LLMAPI
	Social() pubsdk.SocialAPI
	PluginMgr() pubsdk.PluginMgrAPI

	RegisterTool(name string, def pubsdk.ToolDef, handler pubsdk.ToolHandler) error
	RegisterStage(stage pubsdk.Stage, handler pubsdk.StageHandler, scope ...pubsdk.StageScope)
	RegisterPluginAPI(name string) error
	RegisterOutputChannel(name string, caps int, desc string, def pubsdk.ChannelDef, handler pubsdk.ToolHandler) error
	RegisterInputChannel(name string, def pubsdk.ChannelDef) error

	InjectText(source, channel, text string)
	InjectInterruptText(source, channel, text string)
	InjectTextNoMemory(source, channel, text string)
	InjectInputSync(source, channel, text string) string
	// 带媒体的注入：子进程插件也能主动发起一轮带图/音频的对话。
	InjectInputMedia(source, channel, text string, blocks []pubsdk.ContentBlock)
	InjectInputMediaSync(source, channel, text string, blocks []pubsdk.ContentBlock) string
	InjectInterruptMedia(source, channel, text string, blocks []pubsdk.ContentBlock)

	// 带标志位的注入：声明这一次注入是否记入记忆、是否据此裁剪上下文。
	// 上面的三参数方法是它们的零值糖。
	InjectTextOpts(source, channel, text string, opts pubsdk.InjectOptions)
	InjectInterruptTextOpts(source, channel, text string, opts pubsdk.InjectOptions)
	InjectInputSyncOpts(source, channel, text string, opts pubsdk.InjectOptions) string
	InjectInputMediaOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts pubsdk.InjectOptions)
	InjectInputMediaSyncOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts pubsdk.InjectOptions) string
	InjectInterruptMediaOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts pubsdk.InjectOptions)

	// SetToolBlocks 注入媒体块，内核在下一条 tool message 携带（§3.8）。
	SetToolBlocks(blocks []pubsdk.ContentBlock)

	SetAutoRestart(enabled bool)
}

// Handle 分派一次插件 → 内核的调用。
//
// 权限梯度在此强制（§3.8）：manifest 未声明的能力组被**明确拒绝**。
// 不静默忽略：C ABI 时代 case 23/24 返回成功但永远收不到事件
// （§1.3 的「给不了」而非「不给」），插件作者无从得知。
// Handle 分派一次插件 → 内核的调用。
//
// 权限梯度在此强制（§3.8）：manifest 未声明的能力组被**明确拒绝**。
// 不静默忽略：C ABI 时代 case 23/24 返回成功但永远收不到事件
// （§1.3 的「给不了」而非「不给」），插件作者无从得知。
//
// 体量上这里只做「method → 分部函数」一跳：原 51 个 case 的 550 行大 switch
// 已按协议面拆进同包 corehandler_register / _inject / _memory /
// _settings / _runtime。
func (h *coreHandler) Handle(method string, params json.RawMessage) (interface{}, error) {
	if ok, cap := h.caps.allows(method); !ok {
		return nil, errCapabilityDenied(h.name, method, cap)
	}

	switch method {
	case MethodToolRegister, MethodStageRegister, MethodOutputRegister, MethodAPIRegister,
		MethodInputRegister:
		return h.handleRegister(method, params)

	case MethodIOInjectText, MethodIOInjectInterrupt, MethodIOInjectTextNoMem,
		MethodIOInjectSync, MethodIOInjectMedia, MethodIOInjectMediaSync,
		MethodIOInjectInterruptMedia, MethodIOSetToolBlocks:
		return h.handleInject(method, params)

	case MethodMemoryRecall, MethodMemoryCommit, MethodMemoryIntrospect, MethodMemoryMerge,
		MethodMemoryPurge:
		return h.handleGraphMemory(method, params)

	case MethodDocQuery, MethodDocInsert, MethodDocInsertMedia, MethodDocRemove,
		MethodDocStats:
		return h.handleDocMemory(method, params)

	case MethodKnowledgeSearch, MethodKnowledgeAdd, MethodKnowledgeAddMedia, MethodKnowledgeList:
		return h.handleKnowledge(method, params)

	case MethodTextMemoryAppend:
		return h.handleTextMemory(method, params)

	case MethodSettingsGet, MethodSettingsSet, MethodSettingsRegisterDef,
		MethodSettingsSetSecret, MethodSettingsGetSecret,
		MethodSettingsGetCore, MethodSettingsSetCore, MethodSettingsListCore,
		MethodSettingsGetPlugin, MethodSettingsSetPlugin, MethodSettingsListPlugin,
		MethodSettingsList, MethodSettingsDefs, MethodSettingsDump, MethodSettingsPlugins,
		MethodSettingsDataDir:
		return h.handleSettings(method, params)

	case MethodSecurityDeclareProfile, MethodSecurityDeclareProfiles:
		return h.handleSecurity(method, params)

	case MethodLLMListSources, MethodLLMSetSource, MethodLLMCurrentSource:
		return h.handleLLM(method, params)

	case MethodSocialGetPerson, MethodSocialGetNetwork, MethodSocialGetTrait,
		MethodSocialGetRelation, MethodSocialListPersons:
		return h.handleSocial(method, params)

	case MethodLifecycleAutoRestart:
		return h.handleLifecycle(method, params)

	case MethodPluginReloadOne, MethodPluginListLoaded, MethodPluginIsDisabled,
		MethodPluginMgrStop, MethodPluginMgrStart, MethodPluginMgrIsRuntimeStopped:
		return h.handlePluginMgr(method, params)

	case MethodStageLock, MethodStageUnlock:
		return h.handleStageLocks(method, params)

	case MethodEventsSubscribe, MethodEventsUnsubscribe:
		return h.handleEvents(method, params)

	case MethodSecretsAvailable, MethodSecretsStore, MethodSecretsList,
		MethodSecretsForget, MethodSecretsFill, MethodSecretsEncrypt, MethodSecretsDecrypt:
		return h.secrets(method, params)

	case MethodArenaAlloc, MethodArenaFree:
		return h.handleArena(method, params)

	}

	return nil, fmt.Errorf("未知 method: %s", method)
}

// resolveText 从 injectParams 中提取 text：优先使用 TextRef（共享槽），
// 否则使用内联 Text。兼容新旧两种协议。
//
// 子进程分配自己的槽并在收到应答后释放，因此这里读到的一定是 busy 槽。
func (h *coreHandler) resolveText(p injectParams) string {
	if !p.TextRef.IsZero() {
		data, err := h.host.Arena().Read(p.TextRef, h.host.Generation())
		if err != nil {
			return ""
		}
		return string(data)
	}
	return p.Text
}

type injectParams struct {
	Source        string    `json:"source"`
	Channel       string    `json:"channel"`
	Text          string    `json:"text,omitempty"`
	TextRef       SharedRef `json:"text_ref,omitempty"`
	NoMemory      bool      `json:"no_memory,omitempty"`
	ContextPolicy string    `json:"context_policy,omitempty"`
	RecallPolicy  string    `json:"recall_policy,omitempty"`
	CleanerName   string    `json:"cleaner_name,omitempty"`
	// Priority 声明中断注入的优先级（L1..L3）；L4 内核独占，见 InjectOptions。
	Priority string `json:"priority,omitempty"`
}

// injectMediaParams 是带媒体注入/工具块注入的参数。
//
// blocks 优先经共享内存传递（BlocksRef）。旧的注释说“data URL 已是 base64
// 文本、再套一层二进制不会更小，所以走 JSON”——那只算了体积，漏了两件更重要
// 的事：① 内联时整份 base64 要在 RPC 报文里再编码/再拷贝一遍（一张本地生图
// 可达数 MB），② 内容本体不在共享段里，插件回调就无法就地改写，只能各自
// 持一份拷贝。共享内存的意义是后者。
//
// 没有 BlocksRef 时（直连 RPC 测试、arena 不可用）回退内联 Blocks。
type injectMediaParams struct {
	Source        string                `json:"source"`
	Channel       string                `json:"channel"`
	Text          string                `json:"text,omitempty"`
	Blocks        []pubsdk.ContentBlock `json:"blocks,omitempty"`
	BlocksRef     SharedRef             `json:"blocks_ref,omitempty"`
	NoMemory      bool                  `json:"no_memory,omitempty"`
	ContextPolicy string                `json:"context_policy,omitempty"`
	RecallPolicy  string                `json:"recall_policy,omitempty"`
	CleanerName   string                `json:"cleaner_name,omitempty"`
	Priority      string                `json:"priority,omitempty"`
}

// pubSdkInjectOpts 把 RPC 报文里的三个字段转成公开 SDK 的 InjectOptions。
//
// 单独提一个转换函数是为了让「默认值」只有一个出处：零值即记入记忆 + 不裁剪，
// 与旧三参数注入等价。
func pubSdkInjectOpts(noMemory bool, policy, recallPolicy, cleanerName, priority string) pubsdk.InjectOptions {
	return pubsdk.InjectOptions{
		NoMemory: noMemory, ContextPolicy: policy, RecallPolicy: recallPolicy,
		CleanerName: cleanerName,
		Priority:    clampExternalPriority(priority),
	}
}

// clampExternalPriority 把外部插件声明的优先级夹到 L1..L3。
//
// 走本桥的必然是外部插件（独立进程/动态库），它们**不是内核级插件**，
// 因此不能声明 L4——“立即打断”那类能力只属于编译期内置插件（如 WebUI 终止按钮）。
//
// 为什么在这里夹而不是只在内核里按 source 判：source 是插件自报的字段，
// 外部插件可以冒用 "webui" 之名；而本函数所在的位置能确知“这来自外部进程”。
// 内核侧的 isKernelLevelSource 是第二道闸（纵深防御）。
func clampExternalPriority(priority string) string {
	switch priority {
	case "L4", "l4":
		return pubsdk.PriorityL3
	default:
		return priority
	}
}

// validateContextPolicy 校验上下文策略取值，与 tool.register 同一套规则。
//
// 空串等价于 none（不裁剪）。非法值必须报错而不是当成 none：把拼写错误
// 静默降级成「不裁剪」会让调用方以为自己声明的裁剪在生效。
func validateContextPolicy(where, policy string) error {
	if !pubsdk.ValidContextPolicy(policy) {
		return fmt.Errorf("%s: context_policy 只允许 none/prune，实际 %q", where, policy)
	}
	return nil
}

// validateRecallPolicy 校验召回策略取值，与 context_policy 同一套规则。
func validateRecallPolicy(where, policy string) error {
	if !pubsdk.ValidRecallPolicy(policy) {
		return fmt.Errorf("%s: recall_policy 只允许 none/auto，实际 %q", where, policy)
	}
	return nil
}

// resolveJSONRef 若 ref 非零则从共享内存读取并 JSON 反序列化到 out；
// ref 为零时不动 out（调用方已填的内联值生效）。
//
// 供「大 payload 优先走共享内存、否则内联」的字段对共用。
func (h *coreHandler) resolveJSONRef(ref SharedRef, out interface{}) error {
	if ref.IsZero() {
		return nil
	}
	data, err := h.host.Arena().Read(ref, h.host.Generation())
	if err != nil {
		return fmt.Errorf("读取共享内容失败: %w", err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("解析共享内容失败: %w", err)
	}
	return nil
}

// resolveBlocks 取出媒体块：优先共享内存，否则内联。
func (h *coreHandler) resolveBlocks(p injectMediaParams) ([]pubsdk.ContentBlock, error) {
	if p.BlocksRef.IsZero() {
		return p.Blocks, nil
	}
	var blocks []pubsdk.ContentBlock
	if err := h.resolveJSONRef(p.BlocksRef, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func unmarshal(params json.RawMessage, out interface{}) error {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, out); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	return nil
}

func errUnavailable(what string) error {
	return fmt.Errorf("%s 能力在当前内核实例中不可用", what)
}

// cleanerProxy 把进程内函数式 Cleaner 恢复成内核侧透明代理。
// RPC 失败时返回原文：清洗是计算层优化，不能因插件暂时离线而丢失内容。
func (h *coreHandler) cleanerProxy(scope, name string, enabled bool) (func(string) string, error) {
	if !enabled {
		return nil, nil
	}
	if h.invokeCleaner == nil {
		return nil, fmt.Errorf("%s %s 声明 Cleaner，但清洗回调通道未就绪", scope, name)
	}
	return func(text string) string {
		out, err := h.invokeCleanerText(scope, name, text)
		if err != nil {
			return text
		}
		return out
	}, nil
}

// invokeCleanerText 完成一次 Cleaner 往返，使用与工具调用相同的 funccall 帧模型。
//
// 内核（caller）标定帧：输入段 + 结果预算段，插件在帧内写结果；
// 只有结果超出预算时插件才向内核申请扩容块（插件只申请，回收由内核做）。
func (h *coreHandler) invokeCleanerText(scope, name, text string) (string, error) {
	arena := h.host.Arena()
	gen := h.host.Generation()

	frame, err := arena.Alloc(OwnerHost, len(text)+cleanerResultBudget, gen)
	if err != nil {
		return "", fmt.Errorf("%s %s Cleaner 分配调用帧失败: %w", scope, name, err)
	}
	defer func() { _ = arena.Free(OwnerHost, frame) }()

	area, err := arena.Read(frame, gen)
	if err != nil {
		return "", err
	}
	copy(area[:len(text)], text)

	params := CleanerInvokeParams{
		Scope:    scope,
		Name:     name,
		Frame:    frame,
		InputLen: uint32(len(text)),
	}

	res, err := h.invokeCleaner(params)
	if err != nil {
		return "", err
	}

	// 插件申请了扩容块：内核负责归还（插件只会申请，回收由内核做）。
	if res.TextRef.IsZero() {
		return "", fmt.Errorf("%s %s Cleaner 未返回结果引用", scope, name)
	}
	// ★ owner 必须是插件自己（h.owner），理由同 plugin.freeExpand。
	if res.TextRef.Flags&sharedRefFlagExpand != 0 {
		defer func() {
			if err := arena.Free(h.owner, res.TextRef); err != nil {
				log.Printf("[proc] %s %s Cleaner expansion block return failed (arena will leak slowly): %v", scope, name, err)
			}
		}()
	}
	data, err := arena.Read(res.TextRef, gen)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ---- 共享内存的 syscall 风格接口（插件 RPC）----
//
// 共享内存是内部实现，不向插件开发者暴露；模板运行时在传输层调用它们，
// 公开 SDK 仍是普通字符串/Map。

// arenaAlloc 给本插件分配一块共享内存。
//
// 用途：结果超出内核标定帧的预算时，插件据此申请扩容块。
//
// callID 非 0 时把该块**登记到这次调用名下**，供取消超时时强制回收：
// 插件被取消后可能静默退出（正确行为），内核就再也看不到那个 ResultRef，
// 块会永久泄漏。有了归属，内核能在"取消也超时"时自己把账清了。
func (h *coreHandler) arenaAlloc(size uint32, callID uint64) (SharedRef, error) {
	ref, err := h.host.Arena().Alloc(h.owner, int(size), h.host.Generation())
	if err != nil {
		return SharedRef{}, err
	}
	if callID != 0 {
		// 登记到**那次调用**名下（不是另立一张表）：取消超时时由那次调用
		// 的收尾逻辑强制回收，见 Process.abandon / call.takeBlocks。
		if c := h.process.CallByID(callID); c != nil {
			c.trackBlock(ref)
		}
	}
	return ref, nil
}

// arenaFree 归还本插件申请的块。
//
// 内核会走块链校验 offset 确实是某个已分配块的数据起点、owner 匹配，
// 伪造引用不能改动分配器状态。
func (h *coreHandler) arenaFree(ref SharedRef) error {
	return h.host.Arena().Free(h.owner, ref)
}

// toolRegister 注册插件工具，handler 反向调用插件执行（原 case 1）。
func (h *coreHandler) toolRegister(params json.RawMessage) (interface{}, error) {
	var p struct {
		Name       string         `json:"name"`
		Def        pubsdk.ToolDef `json:"def"`
		HasCleaner bool           `json:"has_cleaner"`
	}
	if err := unmarshal(params, &p); err != nil {
		return nil, err
	}
	if p.Name == "" {
		return nil, fmt.Errorf("tool.register: 缺少 name")
	}
	if err := validateContextPolicy("tool.register", p.Def.ContextPolicy); err != nil {
		return nil, err
	}
	if err := validateRecallPolicy("tool.register", p.Def.RecallPolicy); err != nil {
		return nil, err
	}
	p.Def.Plugin = h.name
	// 函数本身不进 JSON；has_cleaner 只声明其存在，实际执行回到插件进程。
	cleaner, err := h.cleanerProxy(CleanerScopeTool, p.Name, p.HasCleaner)
	if err != nil {
		return nil, fmt.Errorf("tool.register: %w", err)
	}
	p.Def.Cleaner = cleaner

	name := p.Name
	return nil, h.sdk.RegisterTool(name, p.Def, func(args map[string]interface{}) (interface{}, error) {
		return h.invokeTool(name, args)
	})
}

// stageRegister 注册阶段处理器（原 case 2）。
//
// **与 C ABI 路径的本质差异**：这里不做「快照 → 副本 → 写回」。
// StageContext 的数据在共享段，插件直接在同一份状态上读改写，
// 由锁仲裁串行化——消除了副本模型的 lost update（§8.4 实测 35.8~36.8%）。
func (h *coreHandler) stageRegister(params json.RawMessage) (interface{}, error) {
	var p struct {
		Stage string `json:"stage"`
		Scope string `json:"scope"`
	}
	if err := unmarshal(params, &p); err != nil {
		return nil, err
	}
	if p.Stage == "" {
		return nil, fmt.Errorf("stage.register: 缺少 stage")
	}

	scope := pubsdk.StageScopeGlobal
	if p.Scope == "own_tools" {
		scope = pubsdk.StageScopeOwnTools
	}
	stage := p.Stage
	h.sdk.RegisterStage(pubsdk.Stage(stage), func(sc *pubsdk.StageContext) error {
		return h.runStage(stage, sc)
	}, scope)
	return nil, nil
}

// outputRegister 注册输出通道（原 case 3）。
//
// **与 C ABI 路径的本质差异**：可同步等真实结果。
// C ABI 下因 cgo 不可嵌套，只能异步 fire-and-forget，导致 output_send
// 永远返回成功（§9.4，现网 2 次消息发不出而模型以为成功）。
func (h *coreHandler) outputRegister(params json.RawMessage) (interface{}, error) {
	var p struct {
		Name       string            `json:"name"`
		Caps       int               `json:"caps"`
		Desc       string            `json:"desc"`
		Def        pubsdk.ChannelDef `json:"def"`
		HasCleaner bool              `json:"has_cleaner"`
	}
	if err := unmarshal(params, &p); err != nil {
		return nil, err
	}
	if p.Name == "" {
		return nil, fmt.Errorf("output.register: 缺少 name")
	}
	channel := p.Name
	cleaner, err := h.cleanerProxy(CleanerScopeOutput, channel, p.HasCleaner)
	if err != nil {
		return nil, fmt.Errorf("output.register: %w", err)
	}
	return nil, h.sdk.RegisterOutputChannel(channel, p.Caps, p.Desc,
		pubsdk.ChannelDef{NoMemory: p.Def.NoMemory, Cleaner: cleaner},
		func(args map[string]interface{}) (interface{}, error) {
			return h.invokeOutput(channel, args)
		})
}

func (h *coreHandler) settings(method string, params json.RawMessage) (interface{}, error) {
	sett := h.sdk.Settings()
	if sett == nil {
		return nil, errUnavailable("settings")
	}
	var p struct {
		Key    string           `json:"key"`
		Value  interface{}      `json:"value"`
		Prefix string           `json:"prefix"`
		Plugin string           `json:"plugin"`
		Def    pubsdk.ConfigDef `json:"def"`
	}
	if err := unmarshal(params, &p); err != nil {
		return nil, err
	}

	switch method {
	case MethodSettingsGet:
		v, err := sett.Get(p.Key)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"value": v}, nil
	case MethodSettingsSet:
		return nil, sett.Set(p.Key, p.Value)
	case MethodSettingsRegisterDef:
		sett.RegisterDef(p.Def)
		return nil, nil
	// ---- 密文读写（强制加密，不看键名）----
	//
	// 与 Set/Get 的区别：这两个**必然**走加密路径，用于插件用
	// `blob`/`payload`/`conn` 这类中性键名存凭据的场景 ——
	// 光靠键名模式猜不出它是敏感的。
	case MethodSettingsSetSecret:
		return nil, sett.SetSecret(p.Key, p.Value)
	case MethodSettingsGetSecret:
		v, err := sett.GetSecret(p.Key)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"value": v}, nil
	case MethodSettingsGetCore:
		v, err := sett.GetCore(p.Key)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"value": v}, nil
	case MethodSettingsSetCore:
		return nil, sett.SetCore(p.Key, p.Value)
	case MethodSettingsListCore:
		keys, err := sett.ListCore(p.Prefix)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"keys": orEmpty(keys)}, nil
	case MethodSettingsGetPlugin:
		v, err := sett.GetPlugin(p.Plugin, p.Key)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"value": v}, nil
	case MethodSettingsSetPlugin:
		return nil, sett.SetPlugin(p.Plugin, p.Key, p.Value)
	case MethodSettingsListPlugin:
		keys, err := sett.ListPlugin(p.Plugin, p.Prefix)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"keys": orEmpty(keys)}, nil
	case MethodSettingsList:
		keys, err := sett.List(p.Prefix)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"keys": orEmpty(keys)}, nil
	case MethodSettingsDefs:
		defs := sett.Defs(p.Prefix)
		if defs == nil {
			defs = []*pubsdk.ConfigDef{}
		}
		return map[string]interface{}{"defs": defs}, nil
	case MethodSettingsDump:
		return sett.Dump(), nil
	case MethodSettingsPlugins:
		return map[string]interface{}{"plugins": orEmpty(sett.Plugins())}, nil
	case MethodSettingsDataDir:
		return map[string]interface{}{"dir": sett.DataDir()}, nil
	}
	return nil, fmt.Errorf("未知 settings method: %s", method)
}

func (h *coreHandler) social(method string, params json.RawMessage) (interface{}, error) {
	social := h.sdk.Social()
	if social == nil {
		return nil, errUnavailable("social")
	}
	var p struct {
		Name  string `json:"name"`
		Trait string `json:"trait"`
		Depth int    `json:"depth"`
	}
	if err := unmarshal(params, &p); err != nil {
		return nil, err
	}

	switch method {
	case MethodSocialGetPerson:
		profile, err := social.GetPerson(p.Name)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"person": profile}, nil
	case MethodSocialGetNetwork:
		profiles, err := social.GetNetwork(p.Name, p.Depth)
		if err != nil {
			return nil, err
		}
		if profiles == nil {
			profiles = []*pubsdk.PersonProfile{}
		}
		return map[string]interface{}{"network": profiles}, nil
	case MethodSocialGetTrait:
		v, ok := social.GetTrait(p.Name, p.Trait)
		return map[string]interface{}{"value": v, "found": ok}, nil
	case MethodSocialGetRelation:
		rels, err := social.GetRelations(p.Name)
		if err != nil {
			return nil, err
		}
		if rels == nil {
			rels = []pubsdk.SocialRelation{}
		}
		return map[string]interface{}{"relations": rels}, nil
	case MethodSocialListPersons:
		names, err := social.ListPersons()
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"persons": orEmpty(names)}, nil
	}
	return nil, fmt.Errorf("未知 social method: %s", method)
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
