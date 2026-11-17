package test

// internal/agent 的黑盒判据：人格**读取**与 InjectPrompt 输出。
// 不碰内部正则表、不读源码。
//
// ★ 原先还有两条测 SavePersonality（写人格文件）。那个函数**生产零调用**
// （人格已迁到「配置项 + 默认模板 + 文件覆盖」，写路径没人用），
// 属死代码 —— 本仓一贯处置是删代码而不是给它补判据。2026-11-16 删除。

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent"
)

// TestLoadMissingPersonalityGivesEmpty：文件不存在返回空人格而非报错。
func TestLoadMissingPersonalityGivesEmpty(t *testing.T) {
	p, err := agent.LoadPersonality(filepath.Join(t.TempDir(), "nope.md"))
	if err != nil {
		t.Fatalf("★ 文件不存在却报错：%v", err)
	}
	if p == nil || p.Content != "" {
		t.Fatalf("★ 期望空人格，实际 %+v", p)
	}
}

// TestInjectPromptContainsContentAndRules：非空人格注入后含原文与说话规则。
func TestInjectPromptContainsContentAndRules(t *testing.T) {
	p := &agent.Personality{Content: "你是小宅"}
	out := p.InjectPrompt()
	if !strings.Contains(out, "你是小宅") || !strings.Contains(out, "[Persona]") {
		t.Fatalf("★ InjectPrompt 输出不符：%q", out)
	}
	if len(out) <= len(p.Content) {
		t.Fatal("★ 内核没追加说话规则")
	}
}

// TestInjectPromptEmptyPersonalityIsEmpty：空人格不注入任何内容。
func TestInjectPromptEmptyPersonalityIsEmpty(t *testing.T) {
	if got := (&agent.Personality{}).InjectPrompt(); got != "" {
		t.Fatalf("★ 空人格却注入了内容：%q", got)
	}
}

// TestPersonaStaleHints：含版本号字面量/陈旧措辞时给出提示；干净文本不提示。
func TestPersonaStaleHints(t *testing.T) {
	dirty := "你是 HΔ-Kernel v1.0.3 型号的家政型 AI 管家助手。"
	if got := agent.PersonaStaleHints(dirty); len(got) == 0 {
		t.Fatal("★ 陈旧人格卡没给出提示")
	}
	clean := "你是小宅，说话自然，别用客服腔。"
	if got := agent.PersonaStaleHints(clean); len(got) != 0 {
		t.Fatalf("★ 干净人格卡被误报：%v", got)
	}
}
