package core

import (
	"strings"
	"testing"
)

// 【加速序列】块必须在系统提示词里，且给出可判定的触发条件。
//
// 为什么需要判据：这一块缺失时**没有任何报错**——模型只是不会用 seq，
// 表现为"工具在、但几乎没人调用"，与 doc_query 当年的症状完全一样。
func TestSystemPrompt_HasAccelerationSequenceBlock(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	p := a.buildSystemPrompt("", "测试输入")

	if !strings.Contains(p, "【加速序列】") {
		t.Fatal("系统提示词缺【加速序列】块 —— 模型不会知道何时该用 seq")
	}
	// 触发条件必须是**可判定的**，不能只说"有用时用"。
	for _, want := range []string{"多个对象", "顺序已知", "seq_list", "组内并行", "组间串行"} {
		if !strings.Contains(p, want) {
			t.Fatalf("【加速序列】块缺 %q（模型据此判断该不该用序列）", want)
		}
	}
	// 必须指路到 skill，避免提示词里重复格式细节（两处真相）。
	if !strings.Contains(p, "seq-tools") {
		t.Fatal("应指向 seq-tools skill，而不是在提示词里重复格式细节")
	}
}
