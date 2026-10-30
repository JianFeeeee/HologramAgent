package proc

// 外部插件（走 proc 桥的独立进程/动态库）**不是内核级插件**，
// 因此不能声明 L4 —— “立即打断”能力只属于编译期内置插件（如 WebUI 终止按钮）。
//
// 在这里夹取而不是只在内核里按 source 判，是因为 source 是插件自报字段、可以冒名；
// 本函数所在位置能确知“这来自外部进程”。内核侧的 isKernelLevelSource 是第二道闸。

import (
	"testing"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

func TestClampExternalPriority_RejectsL4(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"L4", pubsdk.PriorityL3}, // 越权 → 夹到 L3
		{"l4", pubsdk.PriorityL3}, // 大小写都要夹
		{"L3", pubsdk.PriorityL3},
		{"L2", pubsdk.PriorityL2},
		{"L1", pubsdk.PriorityL1},
		{"", ""},     // 未声明保持空（内核按默认级处理）
		{"紧急", "紧急"}, // 未知值原样传给内核，由内核降级为 L1 并留痕
		{"L9", "L9"}, // 同上
	}
	for _, c := range cases {
		if got := clampExternalPriority(c.in); got != c.want {
			t.Fatalf("clampExternalPriority(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
}

// 贯穿 pubSdkInjectOpts：RPC 报文里的 priority 必须经过夹取才落到 InjectOptions。
func TestPubSdkInjectOpts_ClampsPriority(t *testing.T) {
	got := pubSdkInjectOpts(injectParams{
		NoMemory: true, ContextPolicy: "prune", RecallPolicy: "none",
		CleanerName: "cleaner", Priority: "L4",
	})
	if got.Priority != pubsdk.PriorityL3 {
		t.Fatalf("经桥后的优先级=%q，期望 L3", got.Priority)
	}
	if !got.NoMemory || got.ContextPolicy != "prune" || got.RecallPolicy != "none" || got.CleanerName != "cleaner" {
		t.Fatalf("其它字段被改动：%+v", got)
	}
	if l2 := pubSdkInjectOpts(injectParams{Priority: "L2"}); l2.Priority != pubsdk.PriorityL2 {
		t.Fatalf("L2 应原样通过，实际 %q", l2.Priority)
	}
}
