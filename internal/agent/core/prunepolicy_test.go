package core

import (
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// 这一组测试锁死「默认不裁剪」这条语义。
//
// 改动前：每条非中断输入都无条件 Prune 一次，没有任何声明能关掉它。
// 这是破坏性行为（低相关事件被归档并从上下文移走），却无法从调用点看出
// 「谁触发的裁剪」。改成需声明后，必须逐条验证默认值确实是不裁剪。
func TestPruneDeclared_DefaultsToNoPrune(t *testing.T) {
	m := agentIO.NewIOManager()
	a := &Agent{io: m}

	evt := &agentIO.InputEvent{Source: "unknown_source", Payload: map[string]interface{}{}}
	if a.pruneDeclared(evt) {
		t.Fatal("既没有通道声明也没有注入声明的输入，默认必须不裁剪")
	}

	// 通道注册了、但策略是 none / 空：仍然不裁剪。
	m.RegisterInputChannel("quiet", pubsdk.ChannelDef{ContextPolicy: pubsdk.ContextPolicyNone})
	if a.pruneDeclared(&agentIO.InputEvent{Source: "quiet", Payload: map[string]interface{}{}}) {
		t.Fatal("ChannelDef.ContextPolicy=none 不应裁剪")
	}
	m.RegisterInputChannel("empty", pubsdk.ChannelDef{})
	if a.pruneDeclared(&agentIO.InputEvent{Source: "empty", Payload: map[string]interface{}{}}) {
		t.Fatal("ChannelDef 未设 ContextPolicy 不应裁剪")
	}
}

// 通道显式声明 prune 才裁剪。
func TestPruneDeclared_ChannelOptIn(t *testing.T) {
	m := agentIO.NewIOManager()
	m.RegisterInputChannel("noisy", pubsdk.ChannelDef{ContextPolicy: pubsdk.ContextPolicyPrune})
	a := &Agent{io: m}

	if !a.pruneDeclared(&agentIO.InputEvent{Source: "noisy", Payload: map[string]interface{}{}}) {
		t.Fatal("通道声明 prune 后应裁剪")
	}
}

// 注入点声明的优先级高于通道定义：同一通道下的不同注入可以有不同意图。
func TestPruneDeclared_InjectionOverridesChannel(t *testing.T) {
	m := agentIO.NewIOManager()
	a := &Agent{io: m}
	m.RegisterInputChannel("chan", pubsdk.ChannelDef{ContextPolicy: pubsdk.ContextPolicyPrune})

	// 注入点说 none → 即使通道说 prune 也不裁。
	evt := &agentIO.InputEvent{Source: "chan", Payload: map[string]interface{}{
		"context_policy": pubsdk.ContextPolicyNone,
	}}
	if a.pruneDeclared(evt) {
		t.Fatal("注入点声明 none 应覆盖通道的 prune")
	}

	// 通道没说，注入点说 prune → 裁。
	m.RegisterInputChannel("plain", pubsdk.ChannelDef{})
	evt = &agentIO.InputEvent{Source: "plain", Payload: map[string]interface{}{
		"context_policy": pubsdk.ContextPolicyPrune,
	}}
	if !a.pruneDeclared(evt) {
		t.Fatal("注入点声明 prune 应生效")
	}
}

// 没有 context 时不能 panic，也不该裁剪。
func TestPruneOnInput_NilContextIsSafe(t *testing.T) {
	m := agentIO.NewIOManager()
	m.RegisterInputChannel("noisy", pubsdk.ChannelDef{ContextPolicy: pubsdk.ContextPolicyPrune})
	a := &Agent{io: m}
	if got := a.pruneOnInput(&agentIO.InputEvent{Source: "noisy", Payload: map[string]interface{}{}}, "x"); got != 0 {
		t.Fatalf("nil context 应返回 0，实际 %d", got)
	}
}

// cleanInputFor 的优先级：注入点声明的 cleaner > 按 source 查的 cleaner > 原文。
func TestCleanInputFor_Priority(t *testing.T) {
	m := agentIO.NewIOManager()
	m.RegisterInputChannel("src", pubsdk.ChannelDef{
		Cleaner: func(s string) string { return "by-source:" + s },
	})
	m.RegisterInputChannel("explicit", pubsdk.ChannelDef{
		Cleaner: func(s string) string { return "by-name:" + s },
	})
	a := &Agent{io: m}

	// 无声明 → 用 source 的 cleaner
	evt := &agentIO.InputEvent{Source: "src", Payload: map[string]interface{}{}}
	if got := a.cleanInputFor(evt, "raw"); got != "by-source:raw" {
		t.Fatalf("应回退到 source 的 cleaner，实际 %q", got)
	}

	// 注入点指定 cleaner_name → 覆盖 source 的
	evt = &agentIO.InputEvent{Source: "src", Payload: map[string]interface{}{"cleaner_name": "explicit"}}
	if got := a.cleanInputFor(evt, "raw"); got != "by-name:raw" {
		t.Fatalf("注入点声明的 cleaner 应优先，实际 %q", got)
	}

	// 完全没有 cleaner → 原文
	evt = &agentIO.InputEvent{Source: "nobody", Payload: map[string]interface{}{}}
	if got := a.cleanInputFor(evt, "raw"); got != "raw" {
		t.Fatalf("没有 cleaner 时应返回原文，实际 %q", got)
	}

	// 声明的名字查不到 → 回退到 source 的 cleaner（并记日志），不能 panic、不能丢内容
	evt = &agentIO.InputEvent{Source: "src", Payload: map[string]interface{}{"cleaner_name": "missing"}}
	if got := a.cleanInputFor(evt, "raw"); got != "by-source:raw" {
		t.Fatalf("未知 cleaner_name 应回退，实际 %q", got)
	}

	// nil IOManager 不能 panic
	if got := (&Agent{}).cleanInputFor(evt, "raw"); got != "raw" {
		t.Fatalf("nil io 应返回原文，实际 %q", got)
	}
}
