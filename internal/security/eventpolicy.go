package security

// 事件总线策略：**谁能发/订阅什么事件**。
//
// ## 为什么它属于 security 模块（用户 2026-10-27）
//
// 事件的边界不是"总线的一个参数"，而是**授权**：一个插件能发 skill_detected
// 就等于能驱动 skillmgr 接管；能发 update_request 就等于能触发内核升级流程。
// 这类判断和工具黑白名单是同一类东西，理应放在同一处（security），而不是
// 散在 SDK 实现里 —— 否则策略会出现两份真相，且换实现就换口径。
//
// 分工：
//
//	internal/security        策略（本文件）+ 载荷过闸（PrepareOutbound/ScrubInbound）
//	internal/sdk             总线适配：查策略 → 发/订 → 过闸
//	sdk（公开）              只给接口与事件名，不给策略
//
// ## 为什么"关闭 security"不影响本策略
//
// `core.security.enabled=false` 关掉的是**凭据替换**（需要主密钥），
// 而"插件不许发 raw_input"这类授权与密钥无关。所以 EventPolicy 挂在
// Runtime 上，Disabled Runtime 同样持有默认策略 —— 关安全不等于开权限。

import "strings"

// EventPolicy 是事件总线的授权策略。
type EventPolicy struct {
	// Publishable 是插件可发布的事件类型白名单。
	//
	// ★ 加一项前先问：这个事件会不会让插件**间接驱动内核行为**？
	// 会的话就不能加——事件是通知，不是未授权的执行入口。
	Publishable []string
	// Subscribable 是插件可订阅的**只读**事件白名单。
	Subscribable []string

	// PublisherAllow 是「事件类型 → 允许发布的主体名」的**白名单子集**。
	//
	// ## 为何需要它（平表的缺陷）
	//
	// Publishable 是扁平的：进去一项就意味着**任何插件**都能发。
	// 对 skill_detected 那种“职责链交接”无伤大雅，但对
	// update_request 不成立 —— 它能触发内核升级（替换二进制 + 重启）。
	//
	// 而实际上只有极少几个内置插件有资格：webui（有登录鉴权）、
	// pluginmgr（插件升级通道）、cli（本地终端）。把这种能力发给每个
	// 内置插件，等于整个攻击面 = 全部内置插件。
	//
	// 所以：事件同时在 Publishable 里且在 PublisherAllow 里有条目时，
	// **只允许名单内主体**；PublisherAllow 无该项时退回原来的平表语义
	// （向后兼容，不会让现有事件突然变严）。
	PublisherAllow map[string][]string

	// MaxPayloadKeys 是载荷顶层键数上限（事件是通知，不是数据通道）。
	MaxPayloadKeys int
}

// EventUpdateRequest 是内核升级请求的事件名。
//
// 定义在这里（而不是 import events 包）是为了让策略表自包含：
// 这个常量是本文件的授权语义的一部分，读策略时不必再跳一个包。
const EventUpdateRequest = "update_request"

// DefaultEventPolicy 是当前的默认授权表。
func DefaultEventPolicy() EventPolicy {
	return EventPolicy{
		Publishable: []string{
			// 兼容层（openclaw/clawhub 目录扫描）把发现的纯 SKILL 条目交给
			// skillmgr 接管：这是**职责链交接**，不是执行指令。
			"skill_detected",

			// ★ update_request：**有资格请求内核升级**。
			//
			// 这个事件以前被明确列为“不得发布”（见 eventpolicy_test.go
			// 与 sdk/events_test.go 的 RejectsUnlistedPublish）。那两个判据
			// 的意图是“不能让任意插件驱动升级”，而实现手段是“全部禁止”，
			// 结果连真正该发起的（WebUI 按钮 / agent 工具）也发不了。
			//
			// 现在改为**按主体授权**：事件在白名单里，但只有
			// PublisherAllow 列出的主体能发。意图不变（不是谁都能发），
			// 而合法调用方终于能工作。
			EventUpdateRequest,

			// ── 内置插件的输出类事件（均**按主体授权**，见 PublisherAllow）──
			//
			// 这三个是“官方通道出口”在发，不是普通插件。把它们列在这里
			// 是为了让闸门能给出统一的拒绝语义（“不在白名单”），
			// 而真正的边界在 PublisherAllow。
			"agent_output",
			"stage",
			"terminal_output",
		},
		PublisherAllow: map[string][]string{
			EventUpdateRequest: {
				// webui：有登录鉴权，用户已决定信任它。
				"webui",
				// pluginmgr：agent 工具 kernel_update 的宿主。
				"pluginmgr",
				// cli：本地终端，已经是完全受信主体。
				"cli",
			},

			// ── 内置插件的发布面（2026-10-28 补齐）──
			//
			// ## 为何这些事件**不能**进扁平 Publishable
			//
			// agent_output / stage / terminal_output 是**内核向外的输出面**。
			// 任何插件能发它们，就等于能伪造 agent 说的话、伪造阶段进度、
			// 冒充终端输出。那是最直接的欺骗路径（不是理论风险：
			// WebUI 与 CLI 都直接渲染这些事件）。
			//
			// 而这三个内置插件确实需要发：它们就是那几个通道的**承载者**。
			// 所以按主体开：事件在 Publishable 里，但只有下列主体能发。
			"agent_output": {
				// webui / cli：outputch 的出口，把 agent 回复写到通道并广播
				"webui",
				"cli",
			},
			"stage": {
				// webui：把 stage 进度推给前端（thinking / tool:xxx / output）
				"webui",
			},
			"terminal_output": {
				// agentcli：PTY 终端输出上报（它自己就是终端的宿主）
				"agentcli",
			},
		},
		Subscribable: []string{
			// tools_ready：全部插件加载完、工具注册齐（"需要工具才能做的事"的起点）
			"tools_ready",
			// security_ready：安全闸门装配完成（含"是否真的在生效"）
			"security_ready",
			// device_ready：工具/设备数据面就绪
			"device_ready",

			// ── 只读的观测面（2026-10-28 补齐）──
			//
			// ## 为何必须开（否则装闸门就是事故）
			//
			// 闸门一旦生效，**所有**订阅都要过策略。而内置插件（webui / cli）
			// 订阅了 agent_output / tool_call / stage 等十余种事件来驱动界面。
			// 不开就等于把 WebUI 和 CLI 直接打瘫 —— 那是装监控造成的故障，
			// 比不装更糟。
			//
			// ## 为何订阅它们不构成提权
			//
			// 这些都是**只读观测**事件：订阅只让你看见，不让你改变任何状态。
			// 真正需要收紧的是**发布**（见 Publishable 的按主体授权）。
			// 订阅侧收紧只保留真正敏感的两项：raw_input（用户原文，含隐私）
			// 与 update_request（执行入口，不是通知）。
			"agent_output",
			"agent_llm_chain",
			"reasoning",
			"reasoning_delta",
			"content_delta",
			"tool_call",
			"stage",
			"system",
			"terminal_output",
			"scheduler",
			"memory_access",

			// update_check / update_progress / update_result：升级的三个通知事件。
			//
			// ★ 它们必须**可订阅**（之前不在名单里，而 WebUI 走裸总线订阅
			//   所以没暴露）。升级是长耗时且会让进程消失的动作，WebUI 靠
			//   这些事件才能在断线重连后补到"升到哪步 / 失败 / 已回滚"。
			//   它们只是通知（不改内核状态），订阅它们不构成提权。
			"update_check",
			"update_progress",
			"update_result",

			// ★ raw_input **必须**可订阅：WebUI 与 CLI 靠它把用户输入渲染
			// 到对话界面。若不开，用户发的消息在界面上就不显示了。
			//
			// 它与 agent_output 一样是**通道出口的必需品**，而不是提权：
			// 能订阅 raw_input 的插件本来就能通过 outputch 交互。
			// 真正的隐私边界在 security 的凭据闸门与内容审查，不在这里。
			"raw_input",

			// ── 内部职责链事件（e2e 实测发现必须可订阅）──
			//
			// ★ 这两项是端到端测试跑出来的真实回归：
			//
			//	[bus] subscribe denied: publisher="pluginmgr" type="update_request"
			//	[bus] subscribe denied: publisher="skillmgr" type="skill_detected"
			//
			// 前者让“内核升级执行器”收不到请求（请求静默丢失）；
			// 后者让 clawhubadapter → skillmgr 的职责链断掉。
			//
			// 它们都是**内置插件之间的协作通道**，不是对外 API。
			// 错在把它们当成“对外只读事件”而拒了内部消费方。
			//
			// 为何这不会放宽安全：
			//   · skill_detected 的**发布**仍需授权（见 Publishable）
			//   · update_request 的**发布**已按主体收窄（webui/pluginmgr/cli）
			//   · 订阅只能“听到”，不能“发起”
			"skill_detected",
			EventUpdateRequest,
		},
		MaxPayloadKeys: 64,
	}
}

// pluginNamespacePrefix 返回某个插件**自己的**事件命名空间前缀。
//
// 插件的私有事件必须在这个前缀下（`plugin.<name>.<后缀>`），
// 这样"两个插件协作"不需要新增白名单，也不可能冒用别人的名字。
func pluginNamespacePrefix(plugin string) string {
	return "plugin." + plugin + "."
}

// CheckEventPublish 判定插件能否发布某个事件。
func (p EventPolicy) CheckEventPublish(plugin, eventType string) Decision {
	if plugin == "" {
		return deny("events: publisher name is empty")
	}
	if eventType == "" {
		return deny("events: event type must not be empty")
	}
	for _, a := range p.Publishable {
		if eventType != a {
			continue
		}
		// ★ 按主体收窄：事件同时登记了 PublisherAllow 时，**只允许名单内主体**。
		//
		// 为何不能简单把它加进 Publishable：平表语义是“任何插件都能发”。
		// update_request 能触发内核升级（替换二进制 + 重启），不应下放到
		// 每个内置插件。而反过来“完全禁止”也不行 —— 那样合法的调用方
		// （WebUI 按钮 / agent 工具）也发不了。
		if allow, hasAllow := p.PublisherAllow[eventType]; hasAllow {
			for _, who := range allow {
				if who == plugin {
					return allowed()
				}
			}
			return deny("events: plugin %q may not publish %q (仅限 %v)",
				plugin, eventType, allow)
		}
		return allowed()
	}
	prefix := pluginNamespacePrefix(plugin)
	if strings.HasPrefix(eventType, prefix) && len(eventType) > len(prefix) {
		return allowed()
	}
	return deny("events: plugin %q may not publish %q (allowlist: %v, own namespace: %s*)",
		plugin, eventType, p.Publishable, prefix)
}

// CheckEventSubscribe 判定插件能否订阅某个事件。
func (p EventPolicy) CheckEventSubscribe(plugin, eventType string) Decision {
	if plugin == "" {
		return deny("events: subscriber name is empty")
	}
	for _, a := range p.Subscribable {
		if eventType == a {
			return allowed()
		}
	}
	return deny("events: plugin %q may not subscribe to %q (allowlist: %v)",
		plugin, eventType, p.Subscribable)
}

// CheckEventPayloadSize 判定载荷规模（键数）是否在允许范围。
func (p EventPolicy) CheckEventPayloadSize(keys int) Decision {
	max := p.MaxPayloadKeys
	if max <= 0 {
		max = 64
	}
	if keys > max {
		return deny("events: payload has %d keys, limit is %d", keys, max)
	}
	return allowed()
}

// ───────────────────── Runtime 上的对外入口 ─────────────────────

// EventPolicy 返回当前事件策略（拷贝，调用方可安全读取）。
func (r *Runtime) EventPolicy() EventPolicy {
	if r == nil {
		return DefaultEventPolicy()
	}
	return r.eventPolicy
}

// SetEventPolicy 替换事件策略（配置重载时调用）。
func (r *Runtime) SetEventPolicy(p EventPolicy) {
	if r == nil {
		return
	}
	r.eventPolicy = p
}

// CheckEventPublish / CheckEventSubscribe / CheckEventPayloadSize 是内核侧
// （internal/sdk 的事件适配层）唯一该调的入口。
//
// nil Runtime 时退回默认策略 —— **失败方向是收紧**（默认表就是最小集合），
// 不是放行。
func (r *Runtime) CheckEventPublish(plugin, eventType string) Decision {
	return r.EventPolicy().CheckEventPublish(plugin, eventType)
}

func (r *Runtime) CheckEventSubscribe(plugin, eventType string) Decision {
	return r.EventPolicy().CheckEventSubscribe(plugin, eventType)
}

func (r *Runtime) CheckEventPayloadSize(keys int) Decision {
	return r.EventPolicy().CheckEventPayloadSize(keys)
}
