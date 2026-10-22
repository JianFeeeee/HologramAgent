package core

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/meta"
	sdkmeta "github.com/JianFeeeee/hologramagentsdk/meta"
)

func TestExpandPromptVars(t *testing.T) {
	got := expandPromptVars("型号 {{kernel_version}}（{{kernel_commit}}），SDK {{sdk_version}}")
	if !strings.Contains(got, meta.Version) || !strings.Contains(got, sdkmeta.Version) {
		t.Fatalf("占位符未展开: %q", got)
	}
	if strings.Contains(got, "{{") {
		t.Fatalf("仍有未展开的内置占位符: %q", got)
	}
	// 人格卡实测原文：写死了 v1.0.3，应能被占位符取代
	live := expandPromptVars("你是 HomeAgent 的看板娘「小宅」(Xiao Zhai)，HΔ-Kernel v{{kernel_version}} 型号的家政型 AI 管家助手。")
	if strings.Contains(live, "1.0.3") || !strings.Contains(live, "v"+meta.Version) {
		t.Fatalf("人格卡版本未跟随内核: %q", live)
	}
	// 未知占位符必须原样保留（写错要看得见，不能被静默吞掉）
	if unk := expandPromptVars("版本 {{kernel_verison}}"); !strings.Contains(unk, "{{kernel_verison}}") {
		t.Fatalf("未知占位符被吞: %q", unk)
	}
	// 无占位符时原样返回（人格卡热路径，不做无谓拷贝）
	if plain := "无占位符"; expandPromptVars(plain) != plain {
		t.Fatal("无占位符时不应改写")
	}
}
