package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PersonaStaleHints 必须能认出会腐坏的人格内容——版本号字面量与已删除机制。
func TestPersonaStaleHints(t *testing.T) {
	// 现场真实文本（线上人格卡的原文）
	stale := "你是 HologramAgent（内核代号 HΔ-Kernel，当前版本 v0.9.0）。\n当前运行的二进制是 v0.9.0（C ABI v2，构建于 2026-08-15）。"
	hints := PersonaStaleHints(stale)
	if len(hints) == 0 {
		t.Fatal("未识别出写死版本号与 C ABI 的人格文本")
	}
	joined := strings.Join(hints, " | ")
	if !strings.Contains(joined, "版本号字面量") {
		t.Fatalf("应报出版本号字面量，实际: %s", joined)
	}
	if !strings.Contains(joined, "C ABI v2") {
		t.Fatalf("应报出已删除机制的残留说法，实际: %s", joined)
	}

	// 干净文本（配置项默认模板）不应误报
	if h := PersonaStaleHints(DefaultPersonaProbeClean()); len(h) != 0 {
		t.Fatalf("干净人格被误报: %v", h)
	}
}

// DefaultPersonaProbeClean 由 config 包的默认模板等价物构成——
// 这里不复用 config 包以避免 import cycle，只断言「不含版本号与旧机制」的文本不被误报。
func DefaultPersonaProbeClean() string {
	return "你是 HologramAgent（内核代号 HΔ-Kernel）。外部插件是独立子进程，经 stdio JSON-RPC 通信；" +
		"被问到版本时以运行时快照为准。"
}

func TestLoadAndSavePersonality(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "personal", "personal.md")

	// 文件不存在时返回空（不报错、不创建）
	p, err := LoadPersonality(path)
	if err != nil || p == nil || p.Content != "" {
		t.Fatalf("缺文件时应返回空人格，实际 %+v err=%v", p, err)
	}

	if err := SavePersonality(path, "人格内容"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("SavePersonality 未落盘: %v", err)
	}
	p, err = LoadPersonality(path)
	if err != nil || p.Content != "人格内容" {
		t.Fatalf("读回失败: %+v err=%v", p, err)
	}
	if got := p.InjectPrompt(); !strings.Contains(got, "【人格设定】") || !strings.Contains(got, "人格内容") {
		t.Fatalf("InjectPrompt 形状不对: %q", got)
	}
}
