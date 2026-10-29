package security

// update_request 的**按主体授权**判据。
//
// ## 背景：这里曾经是“全禁”
//
// 旧的判据把 update_request 列进 RejectsUnlistedPublish，意图是
// “不能让任意插件驱动内核升级”。但实现手段是“全部禁止”，于是连
// 真正该发起的地方（WebUI 按钮、agent 工具）也发不了 —— 而内核侧
// 的执行器（internal/update）已经在等这个事件。
//
// 现在改为按主体授权：事件在白名单里，但只有 PublisherAllow 列出的
// 主体能发。意图不变（不是谁都能发），合法调用方终于能工作。
//
// 这组判据钉住三件事：
//   · 名单内主体能发
//   · 名单外主体**不能**发（这才是安全边界）
//   · 订阅侧不受影响（通知类事件，订阅不构成提权）

import "testing"

// ① 名单内的主体可以发布 update_request。
func TestUpdateRequest_AllowedPublishers(t *testing.T) {
	p := DefaultEventPolicy()
	for _, who := range []string{"webui", "pluginmgr", "cli"} {
		if d := p.CheckEventPublish(who, EventUpdateRequest); !d.Allowed {
			t.Errorf("%s 应可发布 update_request，实际被拒: %s", who, d.Reason)
		}
	}
}

// ② 名单外的主体必须被拒绝 —— 这是安全边界本身。
func TestUpdateRequest_RejectsOtherPlugins(t *testing.T) {
	p := DefaultEventPolicy()
	for _, who := range []string{"files", "seq", "agentcli", "evil", "mcp", "kbtree"} {
		d := p.CheckEventPublish(who, EventUpdateRequest)
		if d.Allowed {
			t.Errorf("★ %s 不应能发布 update_request（能发就等于能触发内核升级）", who)
		}
		if d.Reason == "" {
			t.Errorf("拒绝 %s 时必须给出理由", who)
		}
	}
}

// ③ 拒绝理由要指出**允许名单**（否则调用方只能猜）。
func TestUpdateRequest_DenyReasonNamesAllowlist(t *testing.T) {
	p := DefaultEventPolicy()
	d := p.CheckEventPublish("evil", EventUpdateRequest)
	if d.Allowed {
		t.Fatal("应被拒绝")
	}
	for _, want := range []string{"webui", "pluginmgr", "cli"} {
		if !containsStr(d.Reason, want) {
			t.Errorf("拒绝理由应列出允许主体 %q，实际: %s", want, d.Reason)
		}
	}
}

// ④ 无 PublisherAllow 条目的事件仍走平表语义（向后兼容）。
//
// 判据：skill_detected 没有被收窄，任何插件都能发 —— 否则会把
// 现有兼容层的职责链交接弄坏。
func TestSkillDetected_StillFlatAllowlist(t *testing.T) {
	p := DefaultEventPolicy()
	for _, who := range []string{"clawhubadapter", "any-other-plugin"} {
		if d := p.CheckEventPublish(who, "skill_detected"); !d.Allowed {
			t.Errorf("%s 应可发布 skill_detected（平表语义）: %s", who, d.Reason)
		}
	}
}

// ⑤ update 的三个通知事件可订阅（WebUI 靠它们显示升级进度）。
func TestUpdateEvents_Subscribable(t *testing.T) {
	p := DefaultEventPolicy()
	for _, evt := range []string{"update_check", "update_progress", "update_result"} {
		if d := p.CheckEventSubscribe("webui", evt); !d.Allowed {
			t.Errorf("应可订阅 %q: %s", evt, d.Reason)
		}
	}
}

// ⑥ update_request **必须可订阅**：内核执行器与 pluginmgr 靠它收请求。
//
// ★ 这条判据的方向在 e2e 后反转了（2026-10-28）。
//
// 原先写的是“不可订阅”，理由是“它是执行入口，不是通知”。但端到端实测
// 立刻打脸：
//
//	[bus] subscribe denied: publisher="pluginmgr" type="update_request"
//
// 后果是升级请求**静默丢失** —— 请求方以为发了，执行器永远收不到。
// 这比“订阅侧略宽”严重得多。
//
// 安全边界在**发布**侧（只有 webui/pluginmgr/cli 能发），订阅只是“听得到”。
func TestUpdateRequest_SubscribableByInternalConsumers(t *testing.T) {
	p := DefaultEventPolicy()
	for _, who := range []string{"pluginmgr", "webui"} {
		if d := p.CheckEventSubscribe(who, EventUpdateRequest); !d.Allowed {
			t.Errorf("%s 应可订阅 update_request（升级执行器依赖它）: %s", who, d.Reason)
		}
	}
	// 但**发布**仍严格收窄
	if d := p.CheckEventPublish("seq", EventUpdateRequest); d.Allowed {
		t.Error("★ seq 不应能发布 update_request")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ⑦ 内置插件的输出面**只允许承载者**发布。
//
// 这是本次补齐的核心边界：agent_output / stage / terminal_output 是
// 内核向外的输出面，任何插件能发就等于能伪造 agent 的话。
// 它们必须按主体授权，**不能**落进扁平白名单。
func TestBuiltinOutputEvents_RestrictedToOwners(t *testing.T) {
	p := DefaultEventPolicy()

	allow := map[string][]string{
		"agent_output":    {"webui", "cli"},
		"stage":           {"webui"},
		"terminal_output": {"agentcli"},
	}
	deny := map[string][]string{
		"agent_output":    {"files", "seq", "clawhubadapter", "evil"},
		"stage":           {"files", "cli", "evil"},
		"terminal_output": {"webui", "cli", "evil"},
	}

	for evt, owners := range allow {
		for _, who := range owners {
			if d := p.CheckEventPublish(who, evt); !d.Allowed {
				t.Errorf("%s 应可发布 %s: %s", who, evt, d.Reason)
			}
		}
	}
	for evt, others := range deny {
		for _, who := range others {
			if d := p.CheckEventPublish(who, evt); d.Allowed {
				t.Errorf("★ %s 不应能发布 %s（伪造输出面）", who, evt)
			}
		}
	}
}

// ⑧ 对话界面依赖的观测事件可订阅（装闸门不能把界面打瘫）。
func TestChatObservabilityEvents_Subscribable(t *testing.T) {
	p := DefaultEventPolicy()
	for _, evt := range []string{
		"raw_input", "agent_output", "tool_call", "stage",
		"reasoning", "reasoning_delta", "content_delta",
		"terminal_output", "system", "scheduler", "memory_access",
	} {
		if d := p.CheckEventSubscribe("webui", evt); !d.Allowed {
			t.Errorf("★ webui 应可订阅 %q（对话/状态界面依赖它）: %s", evt, d.Reason)
		}
	}
}
