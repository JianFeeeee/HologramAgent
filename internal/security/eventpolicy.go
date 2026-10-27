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
	// MaxPayloadKeys 是载荷顶层键数上限（事件是通知，不是数据通道）。
	MaxPayloadKeys int
}

// DefaultEventPolicy 是当前的默认授权表。
func DefaultEventPolicy() EventPolicy {
	return EventPolicy{
		Publishable: []string{
			// 兼容层（openclaw/clawhub 目录扫描）把发现的纯 SKILL 条目交给
			// skillmgr 接管：这是**职责链交接**，不是执行指令。
			"skill_detected",
		},
		Subscribable: []string{
			// tools_ready：全部插件加载完、工具注册齐（"需要工具才能做的事"的起点）
			"tools_ready",
			// security_ready：安全闸门装配完成（含"是否真的在生效"）
			"security_ready",
			// device_ready：工具/设备数据面就绪
			"device_ready",
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
		if eventType == a {
			return allowed()
		}
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
