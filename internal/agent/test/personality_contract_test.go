package test

// internal/agent 的黑盒判据：人格文件往返 + InjectPrompt 输出。
// 不碰内部正则表、不读源码。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent"
)

// TestSaveThenLoadPersonality：Save → Load 拿回原文与路径。
func TestSaveThenLoadPersonality(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "personal.md")
	const content = "你是小宅，说话自然一点。"
	if err := agent.SavePersonality(path, content); err != nil {
		t.Fatalf("SavePersonality: %v", err)
	}
	p, err := agent.LoadPersonality(path)
	if err != nil {
		t.Fatalf("LoadPersonality: %v", err)
	}
	if p.Content != content {
		t.Fatalf("★ Content = %q，期望原文", p.Content)
	}
	if p.Path != path {
		t.Fatalf("★ Path = %q，期望 %q", p.Path, path)
	}
}

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

// TestSavePersonalityCreatesParentDirs：父目录不存在时自动创建。
func TestSavePersonalityCreatesParentDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "personal.md")
	if err := agent.SavePersonality(path, "x"); err != nil {
		t.Fatalf("SavePersonality: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("★ 文件没落盘：%v", err)
	}
}
