package test

// internal/plugin/proc 的黑盒判据：公开纯函数与值对象语义。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugin/proc"
)

func TestMajorVersionParsing(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1.4.0", "1"},
		{"v2.10.3", "2"},
		{"0.9", "0"},
		{"17", "17"},
		{"", ""},
		{"  v3.1 ", "3"},
	}
	for _, c := range cases {
		if got := proc.MajorVersion(c.in); got != c.want {
			t.Errorf("MajorVersion(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestKnownCapabilitiesExcludesCoreAndIsStable(t *testing.T) {
	first := proc.KnownCapabilities()
	second := proc.KnownCapabilities()
	if len(first) == 0 {
		t.Fatal("KnownCapabilities 为空")
	}
	for i := range first {
		if first[i] == "core" {
			t.Fatal("core 能力出现在声明清单里（无需声明）")
		}
		if i > 0 && first[i-1] >= first[i] {
			t.Fatalf("能力清单未按升序：%v", first)
		}
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatal("两次调用结果不一致（无确定性）")
		}
	}
}

func TestSharedRefSliceBoundary(t *testing.T) {
	data := []byte("0123456789")
	ok := proc.SharedRef{Offset: 2, Length: 4}
	if got := ok.Slice(data); string(got) != "2345" {
		t.Fatalf("Slice=%q", got)
	}
	if ok.IsZero() {
		t.Fatal("非零 ref 被判为零值")
	}
	if !(proc.SharedRef{}).IsZero() {
		t.Fatal("零值 ref 未判零")
	}
	// 越界: offset+length 超出 data 时返回 nil
	oob := proc.SharedRef{Offset: 8, Length: 8}
	if oob.Slice(data) != nil {
		t.Fatalf("越界 Slice 未返回 nil：%q", oob.Slice(data))
	}
	// 零值: 长度 0
	if zero := (proc.SharedRef{}).Slice(data); zero != nil {
		t.Fatalf("零值 ref Slice 应 nil：%q", zero)
	}
}

func TestPublicCapabilityConstantsStable(t *testing.T) {
	want := map[string]proc.Capability{
		"core": proc.CapCore, "io": proc.CapIO, "memory": proc.CapMemory,
		"doc_memory": proc.CapDocMemory, "knowledge": proc.CapKnowledge,
		"text_memory": proc.CapTextMemory, "llm": proc.CapLLM,
		"social": proc.CapSocial, "events": proc.CapEvents,
		"plugin_mgr": proc.CapPluginMgr, "kernel_control": proc.CapKernelControl,
		"settings_cross": proc.CapCrossPluginSettings,
	}
	for name, cap := range want {
		if string(cap) != name {
			t.Errorf("常量 %q 值与名不符：%q", name, cap)
		}
	}
	_ = proc.MethodHandshake
	_ = proc.DefaultHandshakeTimeout
}
