package security

// 事件总线**授权策略**的判据。
//
// 用户 2026-10-27 的指正：白名单放行 / 越界拒绝属于**策略**，必须在 security
// 模块里，而不是散在 SDK 适配层。所以这些判据钉在 security 包上 ——
// 换实现（谁的适配层）都不能改变口径。

import "testing"

// ① 白名单内的事件可发布。
func TestEventPolicyAllowsListedPublish(t *testing.T) {
	p := DefaultEventPolicy()
	if d := p.CheckEventPublish("clawhubadapter", "skill_detected"); !d.Allowed {
		t.Fatalf("白名单事件应放行: %s", d.Reason)
	}
}

// ② 越界类型必须拒绝，且理由里带白名单（否则调用方只能猜）。
func TestEventPolicyRejectsUnlistedPublish(t *testing.T) {
	p := DefaultEventPolicy()
	for _, bad := range []string{
		"raw_input",      // 用户输入
		"agent_output",   // 对外输出
		"update_request", // 能驱动升级流程
		"stage",          // 阶段管道内部
		"system",         // 系统事件
		"",               // 空
	} {
		d := p.CheckEventPublish("any", bad)
		if d.Allowed {
			t.Errorf("发布 %q 应被拒绝", bad)
		}
		if d.Reason == "" {
			t.Errorf("拒绝 %q 时必须给出理由", bad)
		}
	}
}

// ③ 自己的命名空间允许（插件间协作），且必须有后缀、不能冒用别人。
func TestEventPolicyOwnNamespace(t *testing.T) {
	p := DefaultEventPolicy()
	cases := []struct {
		plugin, event string
		want          bool
	}{
		{"seq", "plugin.seq.done", true},
		{"seq", "plugin.seq.", false},       // 只有前缀没有后缀
		{"seq", "plugin.seq", false},        // 没有点
		{"seq", "plugin.other.done", false}, // 冒用别人
		{"", "plugin.x.done", false},        // 没有发布者名
	}
	for _, c := range cases {
		if got := p.CheckEventPublish(c.plugin, c.event).Allowed; got != c.want {
			t.Errorf("plugin=%q event=%q 期望 %v 实际 %v", c.plugin, c.event, c.want, got)
		}
	}
}

// ④ 订阅只开只读事件。
//
// ★ 2026-10-28 语义变更：raw_input / agent_output 从“必须拒绝”改为“可订阅”。
//
// 旧判据把它们当提权口子，但实际它们是**通道出口的必需品**：
// WebUI 与 CLI 的对话界面就靠订阅这两个事件渲染用户消息与 agent 回复。
// 不开就等于“装监控把界面打瘫”—— 那是装监控造成的故障，比不装更糟。
//
// 订阅是只读观测：看得见不等于能改变什么。真正的边界在**发布**侧
// （agent_output / stage / terminal_output 已按主体限定，见 PublisherAllow）。
//
// 仍然拒绝的两项：
//
//	· update_request —— 执行入口，不是通知
func TestEventPolicySubscribeReadOnlyOnly(t *testing.T) {
	p := DefaultEventPolicy()

	// 通道出口与观测面：必须可订阅
	for _, ok := range []string{
		"tools_ready", "security_ready", "device_ready",
		"raw_input", "agent_output", // 对话界面必需品
		"tool_call", "stage", "reasoning", "reasoning_delta", "content_delta",
		"terminal_output", "system", "scheduler", "memory_access", "agent_llm_chain",
		"update_check", "update_progress", "update_result",
	} {
		if d := p.CheckEventSubscribe("p1", ok); !d.Allowed {
			t.Errorf("%q 应可订阅: %s", ok, d.Reason)
		}
	}

	// ★ update_request 现在**可订阅**（e2e 实测发现必须开）：
	// 内核侧升级执行器靠订阅它收请求，pluginmgr 也订阅它处理 plugin 范围。
	// 不开的后果是“升级请求静默丢失”——比安全约束失守更直接的故障。
	//
	// 安全边界仍在**发布**侧：只有 webui/pluginmgr/cli 能发。
	if d := p.CheckEventSubscribe("pluginmgr", EventUpdateRequest); !d.Allowed {
		t.Errorf("pluginmgr 应可订阅 update_request: %s", d.Reason)
	}

	// 真正必须拒绝的：未登记的事件名
	if d := p.CheckEventSubscribe("p1", "no_such_event"); d.Allowed {
		t.Error("订阅未登记事件应被拒绝")
	}
}

// ⑤ 载荷键数上限（事件是通知，不是数据通道）。
func TestEventPolicyPayloadKeyLimit(t *testing.T) {
	p := DefaultEventPolicy()
	if d := p.CheckEventPayloadSize(p.MaxPayloadKeys); !d.Allowed {
		t.Fatalf("达到上限应放行: %s", d.Reason)
	}
	if d := p.CheckEventPayloadSize(p.MaxPayloadKeys + 1); d.Allowed {
		t.Fatal("超过上限应拒绝")
	}
}

// ⑥ Runtime 委托：策略挂在 Runtime 上，可替换；Disabled Runtime 也带默认策略
//
//	—— 关掉凭据替换不等于放开事件授权。
func TestRuntimeEventPolicyDelegation(t *testing.T) {
	r := NewDisabledRuntime()
	if d := r.CheckEventPublish("p", "raw_input"); d.Allowed {
		t.Fatal("Disabled Runtime 仍应拒绝越界事件（关安全≠开权限）")
	}
	custom := EventPolicy{Publishable: []string{"my_event"}, MaxPayloadKeys: 1}
	r.SetEventPolicy(custom)
	if d := r.CheckEventPublish("p", "my_event"); !d.Allowed {
		t.Fatalf("替换策略后应放行: %s", d.Reason)
	}
	if d := r.CheckEventPayloadSize(2); d.Allowed {
		t.Fatal("替换后的键数上限应生效")
	}
}
