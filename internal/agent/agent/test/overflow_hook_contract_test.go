package test

// 超页处置的**钩子契约**判据（用户 2026-11-18 指正的落实）。
//
// ## 用户原话
//
//	「Overflow 里面不应该有具体逻辑啊，它应该是一个预定义的钩子，
//	  然后各个子 agent 注册这个钩子来定义各自的 overflow 行为」
//
// 于是必须能证明三件事：
//
//	① 基座里**没有**策略：未注册钩子 ⇒ 基座什么都不做（不猜、不兜底）
//	② 钩子收到**时机**（OverflowLocal vs OverflowResume），类型据此分流
//	③ 基座只提供事实（水位/积累量/单帧撑爆）与原语（剔页/裁剪/空中断），
//	   策略值（1.25 水位、恢复预算 2）住在类型库里，不在基座
//
// 全部走导出面。不读源码、不碰私有字段。

import (
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// hookProbe 记录钩子被调用的形态。
type hookProbe struct {
	calls []agenttypes.OverflowRequest
}

// TestUnregisteredOverflowHookIsInert：未注册钩子 ⇒ 基座不做任何处置。
//
// ★ 这是「基座里没有具体逻辑」的直接判据。若基座还留着默认实现
//
//	（水位判定 / 裁剪 / 发中断），这条会红。
func TestUnregisteredOverflowHookIsInert(t *testing.T) {
	// 裸基座（不挂 mainagent 的回调）：Hooks.EnforceOverflow 为 nil。
	a := agent.New(agent.AgentConfig{
		Security: security.NewDisabledRuntime().AsGate(),
		ID:       "bare",
	})
	defer a.Stop()

	f := &agenttypes.Frame{Input: "x"}
	handled, pruned := a.EnforceContextBudget(f, "", false)
	if handled || pruned != 0 {
		t.Fatalf("★ 未注册钩子却发生了处置（handled=%v pruned=%d）—— 基座还在替类型做决定", handled, pruned)
	}
	if f.Terminal == agenttypes.TerminalError {
		t.Fatal("★ 未注册钩子却终止了帧")
	}
}

// TestRegisteredHookSeesLocalKind：常规入口传 Kind=OverflowLocal。
func TestRegisteredHookSeesLocalKind(t *testing.T) {
	p := &hookProbe{}
	a := agent.New(agent.AgentConfig{
		Security: security.NewDisabledRuntime().AsGate(),
		ID:       "local",
	})
	defer a.Stop()
	// 只换钩子，其余槽位保持裸基座（本判据只关心钩子被怎么调）。
	h := a.Hooks()
	h.EnforceOverflow = func(_ *agent.Agent, _ *agenttypes.Frame, req agenttypes.OverflowRequest) (bool, int) {
		p.calls = append(p.calls, req)
		return false, 0
	}
	a.SetHooks(h)

	if _, _ = a.EnforceContextBudget(&agenttypes.Frame{Input: "x"}, "q", false); len(p.calls) != 1 {
		t.Fatalf("★ 钩子被调用 %d 次，期望 1 次", len(p.calls))
	}
	if p.calls[0].Kind != agenttypes.OverflowLocal {
		t.Fatalf("★ 常规入口的 Kind = %v，期望 OverflowLocal", p.calls[0].Kind)
	}
	if p.calls[0].Query != "q" {
		t.Fatalf("★ query 没透传：%q", p.calls[0].Query)
	}

	// force=true（上游报错）必须透传。
	p.calls = nil
	if _, _ = a.EnforceContextBudget(&agenttypes.Frame{Input: "x"}, "", true); len(p.calls) != 1 {
		t.Fatalf("★ force 路径没调钩子")
	}
	if !p.calls[0].Force {
		t.Fatal("★ Force 没透传（策略无法区分本地预判与上游已确认超限）")
	}
}

// TestResumeEntryUsesItsOwnKind：恢复入口传 Kind=OverflowResume。
//
// ★ 两个时机必须可区分：恢复时"保护本帧现场"是对的，
//
//	常规入口按本帧现场保护会反过来放弃全局保护。
func TestResumeEntryUsesItsOwnKind(t *testing.T) {
	p := &hookProbe{}
	a := agent.New(agent.AgentConfig{
		Security: security.NewDisabledRuntime().AsGate(),
		ID:       "resume",
	})
	defer a.Stop()
	h := a.Hooks()
	h.EnforceOverflow = func(_ *agent.Agent, _ *agenttypes.Frame, req agenttypes.OverflowRequest) (bool, int) {
		p.calls = append(p.calls, req)
		return false, 0
	}
	a.SetHooks(h)

	if _, _ = a.EnforceResumeContextBudget(&agenttypes.Frame{Input: "x"}); len(p.calls) != 1 {
		t.Fatalf("★ 恢复入口没调钩子")
	}
	if p.calls[0].Kind != agenttypes.OverflowResume {
		t.Fatalf("★ 恢复入口的 Kind = %v，期望 OverflowResume", p.calls[0].Kind)
	}
}

// TestResumeEntryIgnoresNilFrame：nil 帧必须安全（不 panic、不调钩子）。
func TestResumeEntryIgnoresNilFrame(t *testing.T) {
	p := &hookProbe{}
	a := agent.New(agent.AgentConfig{
		Security: security.NewDisabledRuntime().AsGate(),
		ID:       "nilframe",
	})
	defer a.Stop()
	h := a.Hooks()
	h.EnforceOverflow = func(_ *agent.Agent, _ *agenttypes.Frame, req agenttypes.OverflowRequest) (bool, int) {
		p.calls = append(p.calls, req)
		return false, 0
	}
	a.SetHooks(h)

	if a.EnforceResumeContextBudget(nil); len(p.calls) != 0 {
		t.Fatalf("★ nil 帧也调了钩子")
	}
}
