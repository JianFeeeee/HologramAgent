package io

// 判据：InjectOptions.Payload 是插件的业务字段扩展口，内核只透传、不解释。
//
// ## 为什么要这条通道（2026-10-18）
//
// QQ 的「对方正在输入中」只在**自己引起的** before_output 里发，且要知道
// 发给谁。但 `on_input` 的 stageCtx 是局部变量，`before_output` 的是内核
// 新建的（emitResponse）—— 两者不共享 Extra。
//
// `InputEvent.Payload` 本就是通道送入内核的请求体，身份本该在那里，
// 但 `IOInjector` 全是 `(source, channel, text, opts)`，插件无法给 payload
// 加字段。所以给 InjectOptions 一个扩展口。
//
// （第一版我加了 InjectOptions.Meta —— 那等于把同一件事做两遍，
// 且只服务注入路径。改为 Payload 后所有注入路径共用。）
//
// ## 判据要点
//
//  · 插件字段原样进 payload
//  · **内核保留键不可被覆盖**（否则插件能伪造 context_policy/priority = 提权）
//  · 空 Payload 不改变旧行为

import "testing"

func TestInjectPayloadMerged(t *testing.T) {
	payload := map[string]interface{}{"type": "text", "content": "hi"}
	applyInjectOpts(payload, InjectOptions{
		Payload: map[string]interface{}{"qq_uid": float64(1456128421)},
	})
	if got, ok := payload["qq_uid"].(float64); !ok || got != 1456128421 {
		t.Fatalf("★ 插件 payload 字段没进事件 payload：%v", payload["qq_uid"])
	}
	// 原有字段不能被破坏
	if payload["content"] != "hi" || payload["type"] != "text" {
		t.Fatalf("★ 合并插件字段时破坏了内核字段：%v", payload)
	}
}

// ★★★ 保留键不可覆盖。
//
// 允许覆盖意味着一个插件能伪造"这条注入不记记忆"（no_memory）、
// "这条要立即打断"（priority）、"这条正文是别的"（content）——
// 那是权限提升，不是便利。
func TestInjectPayloadCannotOverrideReserved(t *testing.T) {
	reserved := []string{
		"no_memory", "context_policy", "recall_policy", "scene_policy",
		"cleaner_name", "priority", "type", "content",
		"interrupt", "stop", "command",
	}
	for _, k := range reserved {
		payload := map[string]interface{}{"type": "text", "content": "orig"}
		applyInjectOpts(payload, InjectOptions{
			NoMemory: true,
			Payload:  map[string]interface{}{k: "HACKED"},
		})
		if k == "no_memory" {
			// no_memory 是 bool，注入 true；插件塞字符串必须被忽略
			if v, ok := payload[k].(bool); !ok || !v {
				t.Fatalf("★ 保留键 %q 被插件改写成 %v", k, payload[k])
			}
			continue
		}
		if payload[k] == "HACKED" {
			t.Fatalf("★ 插件改写了内核保留键 %q —— 这是权限提升（能伪造裁剪/优先级/正文）", k)
		}
	}
}

// 空 Payload 不改变旧行为（零值必须与历史 payload 一致）。
func TestInjectPayloadEmptyNoChange(t *testing.T) {
	payload := map[string]interface{}{"type": "text", "content": "hi"}
	before := len(payload)
	applyInjectOpts(payload, InjectOptions{})
	if len(payload) != before {
		t.Fatalf("★ 空 Payload 也改了 payload：%v", payload)
	}
}

// 保留字清单必须覆盖注入路径自己写入的键 —— 否则新键就是新的提权面。
func TestReservedKeysCoverInjectionOwnKeys(t *testing.T) {
	for _, k := range []string{
		"no_memory", "context_policy", "recall_policy", "scene_policy",
		"cleaner_name", "priority",
	} {
		if !IsReservedPayloadKey(k) {
			t.Fatalf("★ 注入路径自己写入的键 %q 不在保留字清单里 —— "+
				"插件可以覆盖它", k)
		}
	}
}
