package core

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// newTestAgent 造一个最小可用的 Agent（buildToolDefs 要求 io 非 nil）。
func newTestAgent(st PersonaStore) *Agent {
	return &Agent{io: agentIO.NewIOManager(), personaStore: st}
}

// fakePersonaStore 记录调用并可控地报告「是否已确认」。
type fakePersonaStore struct {
	initialized bool
	mode        string
	content     string
	calls       int
}

func (f *fakePersonaStore) PersonaInitialized() bool { return f.initialized }

func (f *fakePersonaStore) SetPersona(mode, content string) (bool, error) {
	f.calls++
	f.mode, f.content = mode, content
	f.initialized = true
	return mode == "custom", nil
}

// 首启门禁：人格未确认时，**任何通道**的系统提示词都必须带上「去问用户」的指令；
// 确认后必须消失（否则会每轮反复追问）。
func TestPersonaOnboardingGateInSystemPrompt(t *testing.T) {
	st := &fakePersonaStore{}
	a := newTestAgent(st)

	p := a.buildSystemPrompt("", "你好")
	if !strings.Contains(p, "首启人格设定") || !strings.Contains(p, "persona_set") {
		t.Fatalf("未确认人格时提示词应要求模型询问并调用 persona_set，实际缺少该段")
	}

	// 模型落地后（标记置位）不再出现
	if out := a.executePersonaTool(agentAPI.ToolCall{Name: "persona_set",
		Arguments: map[string]interface{}{"mode": "default"}}); !strings.Contains(out, "默认人格") {
		t.Fatalf("persona_set(default) 回执不对: %s", out)
	}
	if !st.initialized {
		t.Fatal("落库后应置位标记")
	}
	if p2 := a.buildSystemPrompt("", "你好"); strings.Contains(p2, "首启人格设定") {
		t.Fatal("人格已确认后不应再要求询问")
	}

	// 未接入配置（personaStore 为 nil）时，门禁与工具都必须静默关闭
	b := newTestAgent(nil)
	if pb := b.buildSystemPrompt("", "你好"); strings.Contains(pb, "首启人格设定") {
		t.Fatal("未接入配置时不应出现首启门禁")
	}
	if out := b.executePersonaTool(agentAPI.ToolCall{Name: "persona_set"}); !strings.Contains(out, "不可用") {
		t.Fatalf("未接入配置时工具应回明确错误，实际: %s", out)
	}
}

// persona_set 的三选一语义与回执。
func TestPersonaSetToolModes(t *testing.T) {
	cases := []struct {
		mode, content, want string
	}{
		{"custom", "你是测试人格", "重启"},
		{"default", "", "默认人格"},
		{"later", "", "以后再说"},
	}
	for _, c := range cases {
		st := &fakePersonaStore{}
		a := newTestAgent(st)
		out := a.executePersonaTool(agentAPI.ToolCall{Name: "persona_set",
			Arguments: map[string]interface{}{"mode": c.mode, "content": c.content}})
		if !strings.Contains(out, c.want) {
			t.Errorf("mode=%s 回执应含 %q，实际: %s", c.mode, c.want, out)
		}
		if st.calls != 1 || st.mode != c.mode || st.content != c.content {
			t.Errorf("mode=%s 落库参数不对: calls=%d mode=%s content=%q", c.mode, st.calls, st.mode, st.content)
		}
	}
}

// 工具 schema 必须在 catalog 里出现（模型才可能调用）。
func TestPersonaSetToolDefPresent(t *testing.T) {
	a := newTestAgent(&fakePersonaStore{})
	found := false
	for _, td := range a.buildToolDefs() {
		if m, ok := td.(map[string]interface{}); ok {
			if fn, ok := m["function"].(map[string]interface{}); ok && fn["name"] == "persona_set" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("buildToolDefs 未包含 persona_set")
	}
	// 未接入配置时不应暴露该工具
	b := newTestAgent(nil)
	for _, td := range b.buildToolDefs() {
		if m, ok := td.(map[string]interface{}); ok {
			if fn, ok := m["function"].(map[string]interface{}); ok && fn["name"] == "persona_set" {
				t.Fatal("未接入配置时不应暴露 persona_set")
			}
		}
	}
}
