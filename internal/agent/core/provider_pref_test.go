package core

// 判据：四类 agent 可**各自**配置优先模型源，默认用总默认源。
//
// 用户 2026-10-30 要求：「支持为主 agent、子 agent、驻留式子 agent、rt agent
// 可各自配置模型源，默认使用总模型源（default）」。
//
// ## 语义是"优先"而不是"独占"
//
// 该源排在候选链**最前**，其余源仍作回退。硬锁定会让"首选源挂了"直接变成
// "这个 agent 彻底不工作"，而多源回退链正是本仓为此建立的机制。
//
// ## 判据怎么才算有效
//
// 不能只断言"配置生效了"——那样"只留一个源"（破坏回退）也会绿。
// 所以成对断言：
//  ① 优先源排在第一位（配置生效）
//  ② 其余源仍在候选链里（回退未被破坏）

import (
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// newPMWith 造一个有若干源的 manager，默认源是 first。
func newPMWith(t *testing.T, names ...string) *agentAPI.ProviderManager {
	t.Helper()
	pm := agentAPI.NewProviderManager()
	for _, n := range names {
		pm.Register(n, &stubProvider{name: n})
	}
	if len(names) > 0 {
		if err := pm.SetDefault(names[0]); err != nil {
			t.Fatalf("SetDefault: %v", err)
		}
	}
	return pm
}

// chainNames 取本 agent 当前候选链的名字序列。
func chainNames(a *Agent) []string {
	req := &agentAPI.CompletionRequest{}
	ps := a.resolveProviders(req)
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name())
	}
	return out
}

// TestPreferredProvider_PutsSourceFirstAndKeepsFallbacks 是核心判据。
func TestPreferredProvider_PutsSourceFirstAndKeepsFallbacks(t *testing.T) {
	pm := newPMWith(t, "default-src", "fast-src", "cheap-src")

	a := New(AgentConfig{
		ID: "rt", Provider: pm.Get("default-src"),
		ProviderManager: pm, PreferredProvider: "fast-src",
	})
	got := chainNames(a)

	if len(got) == 0 || got[0] != "fast-src" {
		t.Fatalf("候选链首位 = %v，期望 fast-src 在最前（配置未生效）", got)
	}
	// ★ 关键：其余源必须仍在链上（否则回退被破坏）。
	if len(got) != 3 {
		t.Fatalf("候选链只剩 %v —— 优先源不该挤掉其它源（回退链被破坏）", got)
	}
	seen := map[string]bool{}
	for _, n := range got {
		seen[n] = true
	}
	for _, n := range []string{"default-src", "fast-src", "cheap-src"} {
		if !seen[n] {
			t.Errorf("候选链缺少 %q：%v", n, got)
		}
	}
}

// TestPreferredProvider_DefaultUsesGlobalDefault 钉住「默认用总模型源」。
func TestPreferredProvider_DefaultUsesGlobalDefault(t *testing.T) {
	pm := newPMWith(t, "default-src", "other-src")

	a := New(AgentConfig{
		ID: "main", Provider: pm.Get("default-src"), ProviderManager: pm,
		// PreferredProvider 留空 ⇒ 用总默认源
	})
	if got := chainNames(a); len(got) == 0 || got[0] != "default-src" {
		t.Fatalf("未配置优先源时首位 = %v，期望总默认源 default-src", got)
	}
}

// TestPreferredProvider_UnknownNameFallsBackLoudly 钉住配置写错的语义：
// 回退到默认源（不静默失效），且**不影响可用性**。
func TestPreferredProvider_UnknownNameFallsBackLoudly(t *testing.T) {
	pm := newPMWith(t, "default-src", "other-src")

	a := New(AgentConfig{
		ID: "typo", Provider: pm.Get("default-src"), ProviderManager: pm,
		PreferredProvider: "no-such-source", // 拼错
	})
	got := chainNames(a)
	if len(got) == 0 || got[0] != "default-src" {
		t.Fatalf("拼错的优先源应回退到默认源，实际首位 = %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("回退后候选链 = %v，期望两个源都在", got)
	}
}

// TestPreferredProvider_EachAgentIndependent 钉住"各自可配"：
// 同进程内三个 agent 配不同源，互不影响。
func TestPreferredProvider_EachAgentIndependent(t *testing.T) {
	pm := newPMWith(t, "shared-default", "for-main", "for-rt", "for-resident")

	mainA := New(AgentConfig{ID: "main", Provider: pm.Get("shared-default"), ProviderManager: pm, PreferredProvider: "for-main"})
	rtA := New(AgentConfig{ID: "rt", Provider: pm.Get("shared-default"), ProviderManager: pm, PreferredProvider: "for-rt"})
	resA := New(AgentConfig{ID: "res", Provider: pm.Get("shared-default"), ProviderManager: pm, PreferredProvider: "for-resident"})

	for _, tc := range []struct {
		who  string
		a    *Agent
		want string
	}{
		{"主 agent", mainA, "for-main"},
		{"rt agent", rtA, "for-rt"},
		{"驻留子", resA, "for-resident"},
	} {
		if got := chainNames(tc.a); len(got) == 0 || got[0] != tc.want {
			t.Errorf("%s 的首位 = %v，期望 %q（各自配置未隔离）", tc.who, got, tc.want)
		}
	}
}

// TestPreferredProvider_InheritedByChildAgents 钉住继承链：
// 子 agent 未显式配置时**继承父的**优先源（而不是掉回总默认）。
func TestPreferredProvider_InheritedByChildAgents(t *testing.T) {
	pm := newPMWith(t, "shared-default", "parent-pref")

	parent, _, dir := newRootWith(t, pm.Get("shared-default"))
	parent.providerManager = pm
	parent.preferredProvider = "parent-pref"

	spawnTestResident(t, parent, dir, "inh")
	parent.residentMu.Lock()
	rc := parent.residents["inh"]
	parent.residentMu.Unlock()
	if rc == nil {
		t.Fatal("驻留子未创建")
	}
	if got := chainNames(rc.agent); len(got) == 0 || got[0] != "parent-pref" {
		t.Fatalf("驻留子未继承父的优先源，首位 = %v", got)
	}
}

// TestPreferredProvider_RTChildOwnSource 钉住 rtchild 可**独立**配源。
//
// 这条对通话场景最实际：rt 的首字预算是 500ms 级，而不同源往返差 2–5 倍。
func TestPreferredProvider_RTChildOwnSource(t *testing.T) {
	pm := newPMWith(t, "shared-default", "parent-pref", "rt-fast")
	parent, _ := newRTHost(t, pm.Get("shared-default"))
	parent.providerManager = pm
	parent.preferredProvider = "parent-pref"

	id, err := parent.SpawnRTChild(RTChildOptions{
		ID: "rt-own", Input: "voice-in", Output: "voice",
		PreferredProvider: "rt-fast",
	})
	if err != nil {
		t.Fatalf("创建 rtchild: %v", err)
	}
	parent.rtMu.Lock()
	rc := parent.rtChildren[id]
	parent.rtMu.Unlock()
	if got := chainNames(rc.agent); len(got) == 0 || got[0] != "rt-fast" {
		t.Fatalf("rtchild 未使用自己配置的源，首位 = %v", got)
	}
}
