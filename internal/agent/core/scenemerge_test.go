package core

// 场景键去重的判据。
//
// 症状：现网日志出现 `scenes=[chan:qq chan:qq]` —— 同一个键出现两次。
// 原因：buildTaskMemoryContext / stepToolAfter 都先取 sceneKeysFor（内部
// 有 seen 去重），再把 resolveTurnScenes 的 turn.Keys 直接 append 上去，
// **两路之间没有共同的 seen 集合**。声明路和通道派生路都会产出 chan:qq。
//
// 功能上无害（RecallByScene 内部会去重），但它有两个实际代价：
//  1. 日志里的 scenes=[...] 具有误导性——排查时会以为场景集合有问题；
//  2. 每次多带一个重复键进召回，白走一遍前缀匹配。
//
// 判据参照物在生产代码之外：期望值是「场景集合内不得有重复键」这条
// 不变量，直接对合并后的切片计数，不引用被测实现。

import (
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// TestSceneKeysMerged_NoDuplicates 声明路与涌现路的并集不得有重复。
// 现状下 tooldefs.go:63 与 task.go:793 都是直接 append，缺这一步。
func TestSceneKeysMerged_NoDuplicates(t *testing.T) {
	cases := []struct {
		name     string
		declared []string
		emergent []string
	}{
		{
			name:     "涌现键与声明键同名（现网实测 chan:qq 两路都产出）",
			declared: []string{"chan:qq"},
			emergent: []string{"chan:qq"},
		},
		{
			name:     "涌现键已归一化后与声明键同名",
			declared: []string{"chan:qq"},
			emergent: []string{"chan:QQ", "chan:qq"},
		},
		{
			name:     "多路重复",
			declared: []string{"chan:qq", "tool:qq_get_message"},
			emergent: []string{"chan:qq", "tool:qq_get_message", "auto:chan:qq"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeSceneKeys(tc.declared, tc.emergent)
			seen := map[string]bool{}
			for _, k := range got {
				if seen[k] {
					t.Errorf("场景集合含重复键 %q: %v", k, got)
				}
				seen[k] = true
			}
		})
	}
}

// TestSceneSuppressedSkipsMerge none 声明时两路都应为空，
// 不能出现「声明路被关、涌现路还在」这种半开状态。
func TestSceneSuppressedSkipsMerge(t *testing.T) {
	a := &Agent{io: agentIO.NewIOManager()}
	evt := &agentIO.InputEvent{
		Source:  "system",
		Payload: map[string]interface{}{"scene_policy": "none"},
	}
	declared := a.sceneKeysFor(evt, "")
	if len(declared) != 0 {
		t.Fatalf("scene_policy=none 时声明路应为空，实际 %v", declared)
	}
}
