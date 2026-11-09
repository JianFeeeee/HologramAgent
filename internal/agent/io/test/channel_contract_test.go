package test

// internal/agent/io 的黑盒判据：OutputCapability 的导出方法，输入 → 输出。
// 不构造设备、不碰内部注册表。

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// TestSupportsSingleAndCombinedFlags：位标志的 Supports 语义。
func TestSupportsSingleAndCombinedFlags(t *testing.T) {
	c := io.CapText | io.CapAudio
	if !c.Supports(io.CapText) || !c.Supports(io.CapAudio) {
		t.Fatal("★ 已置位的能力报告不支持")
	}
	if c.Supports(io.CapImage) {
		t.Fatal("★ 未置位的能力报告支持")
	}
	// Supports 的语义是「任一位命中」，不是「全部命中」。
	if !io.CapText.Supports(io.CapText | io.CapImage) {
		t.Fatal("★ 任一位命中应报告支持")
	}
	if io.CapText.Supports(io.CapImage) {
		t.Fatal("★ 完全不相交却报告支持")
	}
}

// TestCapabilityStringListsFlags：String 输出列出已置位的能力名。
func TestCapabilityStringListsFlags(t *testing.T) {
	c := io.CapText | io.CapImage
	s := c.String()
	if !strings.Contains(s, "text") || !strings.Contains(s, "image") {
		t.Fatalf("★ String() = %q，缺已置位能力名", s)
	}
	if strings.Contains(s, "audio") {
		t.Fatalf("★ String() = %q，含未置位能力", s)
	}
}

// TestZeroCapabilityString：零能力字符串可用（不 panic）。
func TestZeroCapabilityString(t *testing.T) {
	_ = io.OutputCapability(0).String()
}
