package proc

import (
	"fmt"
	"sort"
	"strings"
)

// 权限梯度：外部插件可调用哪些内核 method（§3.8）。
//
// 迁移前，「外部插件拿不到 Selftest/Supervisor/Tracker」是 C ABI 表达能力的
// **意外产物**——C 结构体不好传函数指针，于是这些能力自然到不了插件侧。
// 那是运气，不是策略：任何人给 dispatch 加个 case 就能捅穿。
//
// 迁移后要变成**显式声明并强制的策略**，分三道闸：
//
//  1. 类型层（internal/plugin/proc_core.go）：procCore 用命名字段持有内核 SDK，
//     不嵌入 —— 未在收窄面显式写出的方法根本不存在，编译期就拿不到。
//  2. 能力集（本文件）：method 划入 capability 组，manifest 未声明的组被拒。
//  3. RPC 边界：被拒时返回**明确错误**而非静默忽略——插件作者能立刻知道
//     「这个能力没给我」，而不是调用成功但什么也没发生。
//
// 第 3 条针对的是一类真实故障：C ABI 时代 case 23/24（事件订阅）是空实现，
// 返回成功但永远收不到事件（§1.3 的「给不了」而非「不给」）。

// Capability 是一组相关 method 的权限单元。
//
// 粒度选择：按**能力域**而非单个 method 划分。逐 method 授权看似更精细，
// 但插件作者要在 manifest 里列 60 个名字，且内核加 method 时所有 manifest 都得改。
type Capability string

const (
	// CapCore 是无需声明即可用的基础能力：注册自身工具/阶段/通道、
	// 读写自己的配置、共享段锁仲裁。没有这些插件无法工作。
	CapCore Capability = "core"

	// CapIO 注入输入到 agent 主循环（可影响对话流）。
	CapIO Capability = "io"

	// CapMemory 图记忆读写。
	CapMemory Capability = "memory"

	// CapDocMemory 文档记忆读写。
	CapDocMemory Capability = "doc_memory"

	// CapKnowledge 知识库读写。
	CapKnowledge Capability = "knowledge"

	// CapTextMemory 文本记忆追加。
	CapTextMemory Capability = "text_memory"

	// CapLLM 切换 LLM 源（影响全局行为）。
	CapLLM Capability = "llm"

	// CapSocial 社交图读取。
	CapSocial Capability = "social"

	// CapEvents 订阅内核事件。
	CapEvents Capability = "events"

	// CapPluginMgr 管理其他插件（重载/查询禁用状态）。
	//
	// 这是**最敏感**的一组：能重载其他插件意味着能间接影响它们的状态。
	CapPluginMgr Capability = "plugin_mgr"

	// CapKernelControl 控制**内核本身**的生死（重启/关机）。
	//
	// 这是整个能力表里**最危险**的一档：它不改变某个插件的状态、
	// 也不改配置，而是让**整个服务**停下来。
	//
	// 与 CapPluginMgr 的区别（别混）：
	//   CapPluginMgr     能重载**别的插件** —— 影响一个插件
	//   CapKernelControl 能重启/关机**内核** —— 影响全部插件 + 全部用户
	//
	// 关机之后服务不会自己回来（systemd Restart=always 由 guard 显式
	// systemctl stop 压制），必须人工介入。因此默认**不授予**任何插件，
	// 必须由 manifest 显式声明。
	CapKernelControl Capability = "kernel_control"

	// CapCrossPluginSettings 读写**其他插件**的配置与内核核心配置。
	//
	// 与 CapCore 里的「读写自己的配置」区分开：跨插件配置读写能改别人的行为，
	// 核心配置读写能改内核行为。
	CapCrossPluginSettings Capability = "settings_cross"
)

// methodCapability 把每个 method 映射到所需能力。
//
// ❗ 新增 method 时必须在此登记，否则 capabilityOf 返回 CapCore
// （最宽松），等于绕过权限检查。checkAllMethodsClassified 测试守着这一点。
var methodCapability = map[string]Capability{
	// ---- 基础能力（无需声明）----
	MethodHandshake:      CapCore,
	MethodToolRegister:   CapCore,
	MethodStageRegister:  CapCore,
	MethodOutputRegister: CapCore,
	MethodAPIRegister:    CapCore,
	MethodInputRegister:  CapCore,
	MethodStageLock:      CapCore,
	MethodStageUnlock:    CapCore,
	// 共享槽池申请/归还：内部传输层能力，等同于核心基础能力。
	MethodArenaAlloc: CapCore,
	MethodArenaFree:  CapCore,
	// 调用取消：**内核 → 插件**的单向通知，插件不需要声明任何能力。
	//
	// 归 CapCore 的理由：它是传输层的收尾动作（内核放弃某次调用时告知对端），
	// 不是插件"申请"的能力。若不登记，capabilityOf 会回落 CapCore —— 结果一样，
	// 但那是**未分类**的默认放行，此后加能力分组时没人会想起它（判据
	// TestCapability_AllMethodsClassified 就是防这个）。
	MethodCallCancel: CapCore,
	// 自身配置读写与元信息属基础能力
	MethodSettingsGet:         CapCore,
	MethodSettingsSet:         CapCore,
	MethodSettingsSetSecret:   CapCore,
	MethodSettingsGetSecret:   CapCore,
	MethodSettingsList:        CapCore,
	MethodSettingsDefs:        CapCore,
	MethodSettingsRegisterDef: CapCore,
	MethodSettingsDataDir:     CapCore,
	MethodSecretsAvailable:    CapCore,
	MethodSecretsStore:        CapCore,
	MethodSecretsList:         CapCore,
	MethodSecretsForget:       CapCore,
	MethodSecretsFill:         CapCore,
	MethodSecretsEncrypt:      CapCore,
	MethodSecretsDecrypt:      CapCore,
	// 策略声明是 CapCore：任何插件都只能声明**自己名下**的档位
	//（Owner 由内核填），所以它不构成越权面；限制在"声明别人的档位"
	// 那一层由 internal/sdk/security_impl.go 拒绝。
	MethodSecurityDeclareProfile:  CapCore,
	MethodSecurityDeclareProfiles: CapCore,
	// 生命周期自述（插件声明自己是否可自动重启）
	MethodLifecycleAutoRestart: CapCore,
	// 多模态内容块注入是工具返回值的一部分，不越权
	MethodIOSetToolBlocks: CapCore,

	// ---- IO 注入 ----
	MethodIOInjectText:      CapIO,
	MethodIOInjectInterrupt: CapIO,
	MethodIOInjectTextNoMem: CapIO,
	MethodIOInjectSync:      CapIO,
	// 带媒体的注入与纯文本注入同一权限组：能不能发起一轮对话是 IO 能力，
	// 带不带图不改变这个判断。
	MethodIOInjectMedia:          CapIO,
	MethodIOInjectMediaSync:      CapIO,
	MethodIOInjectInterruptMedia: CapIO,

	// ---- 图记忆 ----
	MethodMemoryRecall:     CapMemory,
	MethodMemoryCommit:     CapMemory,
	MethodMemoryIntrospect: CapMemory,
	MethodMemoryMerge:      CapMemory,
	MethodMemoryPurge:      CapMemory,

	// ---- 文档记忆 ----
	MethodDocQuery:  CapDocMemory,
	MethodDocInsert: CapDocMemory,
	MethodDocRemove: CapDocMemory,
	MethodDocStats:  CapDocMemory,
	// 带媒体写入与普通写入同权限：都是往文档记忆里写东西。
	MethodDocInsertMedia: CapDocMemory,

	// ---- 知识库 ----
	MethodKnowledgeSearch:   CapKnowledge,
	MethodKnowledgeAdd:      CapKnowledge,
	MethodKnowledgeAddMedia: CapKnowledge,
	MethodKnowledgeList:     CapKnowledge,

	// ---- 文本记忆 ----
	MethodTextMemoryAppend: CapTextMemory,

	// ---- LLM ----
	MethodLLMListSources:   CapLLM,
	MethodLLMSetSource:     CapLLM,
	MethodLLMCurrentSource: CapLLM,

	// ---- 社交图 ----
	MethodSocialGetPerson:   CapSocial,
	MethodSocialGetNetwork:  CapSocial,
	MethodSocialGetTrait:    CapSocial,
	MethodSocialGetRelation: CapSocial,
	MethodSocialListPersons: CapSocial,

	// ---- 事件 ----
	MethodEventsSubscribe:   CapEvents,
	MethodEventsUnsubscribe: CapEvents,

	// ---- 插件管理 ----
	MethodPluginReloadOne:  CapPluginMgr,
	MethodPluginListLoaded: CapPluginMgr,
	MethodPluginIsDisabled: CapPluginMgr,
	MethodPluginMgrStop:    CapPluginMgr,
	MethodPluginMgrStart:   CapPluginMgr,
	// 内核生命周期控制：单独一档，见 CapKernelControl 的注释。
	MethodKernelRestart:             CapKernelControl,
	MethodKernelShutdown:            CapKernelControl,
	MethodPluginMgrIsRuntimeStopped: CapPluginMgr,

	// ---- 跨插件 / 核心配置 ----
	MethodSettingsGetCore:    CapCrossPluginSettings,
	MethodSettingsSetCore:    CapCrossPluginSettings,
	MethodSettingsListCore:   CapCrossPluginSettings,
	MethodSettingsGetPlugin:  CapCrossPluginSettings,
	MethodSettingsSetPlugin:  CapCrossPluginSettings,
	MethodSettingsListPlugin: CapCrossPluginSettings,
	MethodSettingsDump:       CapCrossPluginSettings,
	MethodSettingsPlugins:    CapCrossPluginSettings,
}

// withheldCapabilities 是**刻意不提供给外部插件**的内核内部机制（§3.8 最后一行）。
//
// 这些没有对应的 method 常量——不是"忘了加"，是决定不加。
// 列在这里是为了让决策可见：读代码的人能看到边界在哪，而不是从
// 「protocol.go 里没有」这个负面事实去推断。
//
// 类型层已经挡住了（procCore 不暴露这些访问器），本表是文档 + 测试锚点。
var withheldCapabilities = map[string]string{
	"SelftestAPI":   "虚拟实例自检 —— 能构造内核实例，等于绕过全部权限边界",
	"SupervisorAPI": "进程监管 —— 能启停 worker，等于控制内核生命周期",
	"TrackerAPI":    "变更追踪 —— 内核 overlay 文件系统的内部机制",
	"StatusAPI":     "内核状态面 —— 暴露内部运行时细节",
	"AdapterAPI":    "LLM 适配器管理 —— 能改写请求/响应链路",
	"ConfigAPI":     "内核配置对象 —— 与 settings 的受控读写不同，这是直接持有",
	"ToolAPI":       "工具表直接操作 —— 能注销其他插件的工具（注册自己的工具走 tool.register，那是 core）",
	"IndexerAPI":    "记忆索引器 —— 内核记忆管线的内部组件",
	"OutputChanRaw": "输出通道原始消费 —— 已由 output.invoke 的声明式注册替代",
	"EventPublish":  "事件发布 —— 只给订阅（events.subscribe），不给伪造内核事件",
}

// capabilityOf 返回 method 所需能力。
//
// 未登记的 method 返回 (CapCore, false)：ok=false 让调用方能区分
// 「明确划为基础能力」与「漏登记」，测试据此拦住漏登记。
func capabilityOf(method string) (Capability, bool) {
	cap, ok := methodCapability[method]
	if !ok {
		return CapCore, false
	}
	return cap, true
}

// capabilitySet 是某个插件被授予的能力集合。
type capabilitySet struct {
	granted map[Capability]bool
	// unrestricted 为真时跳过检查（未声明 capabilities 的插件，向后兼容）。
	unrestricted bool
}

// newCapabilitySet 从 manifest 声明构造能力集。
//
// **空声明 = 不受限**，而不是「只有 core」。理由：17 个存量插件的 plugin.json
// 都没有 capabilities 字段，若空声明当作最小权限，它们会全部失去 IO 注入、
// 记忆读写等能力而**静默降级**——这违反「外部插件零改动」的硬约束。
//
// 收紧的路径是让插件显式声明，而非默默拒绝老插件。
func newCapabilitySet(declared []string) *capabilitySet {
	if len(declared) == 0 {
		return &capabilitySet{unrestricted: true}
	}
	s := &capabilitySet{granted: map[Capability]bool{CapCore: true}}
	for _, d := range declared {
		s.granted[Capability(strings.TrimSpace(d))] = true
	}
	return s
}

// allows 判断是否允许调用某 method。
func (s *capabilitySet) allows(method string) (bool, Capability) {
	cap, registered := capabilityOf(method)
	if !registered {
		// 漏登记的 method 按基础能力放行（保守：不因内核疏漏拦住插件），
		// 但由测试保证这种情况不存在。
		return true, CapCore
	}
	if s == nil || s.unrestricted {
		return true, cap
	}
	if cap == CapCore {
		return true, cap
	}
	return s.granted[cap], cap
}

// KnownCapabilities 返回全部可声明的能力名（供 manifest 校验与文档生成）。
func KnownCapabilities() []string {
	seen := map[Capability]bool{}
	for _, c := range methodCapability {
		seen[c] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		if c == CapCore {
			continue // core 无需声明
		}
		out = append(out, string(c))
	}
	sort.Strings(out)
	return out
}

// WithheldCapabilities 返回刻意不提供的能力清单（供文档与诊断）。
func WithheldCapabilities() map[string]string {
	out := make(map[string]string, len(withheldCapabilities))
	for k, v := range withheldCapabilities {
		out[k] = v
	}
	return out
}

// errCapabilityDenied 构造被拒错误。
//
// 消息包含三要素：被拒的 method、缺的能力名、如何补救。
// 静默忽略或含糊的「失败」会让插件作者以为是自己参数错了。
func errCapabilityDenied(plugin, method string, cap Capability) error {
	return fmt.Errorf(
		"插件 %s 调用 %s 被拒：缺少 %q 能力。"+
			"请在 plugin.json 的 capabilities 数组中声明它（可用能力：%s）",
		plugin, method, cap, strings.Join(KnownCapabilities(), ", "))
}
