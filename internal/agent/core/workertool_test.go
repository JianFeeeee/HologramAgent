package core

// 判据：工作式子的工具禁令是**单一事实来源**。
//
// ## 这条缺陷的原貌
//
// 禁令曾写了两遍：
//
//	定义侧（spawn.go:160）  把禁的工具从 childTools 里滤掉 ⇒ 模型看不见
//	执行侧（spawn.go:224）  模型硬调时拒绝               ⇒ 模型看得见也调不动
//
// 两处的工具名单必须逐字一致。任何一边落后就是一个真实缺口：
//
//	定义侧漏滤 ⇒ 模型看得见但不该能用（白耗 token，还会反复试）
//	执行侧漏拒 ⇒ 模型绕过定义直接调用**就能生效**（模型确实会这么干 ——
//	            本仓 record 过"4 次 files_write 失败后模型改参数硬试"）
//
// 现在两侧都从 workerToolAllowed 派生，这条判据钉住"不许再长出第二份"。

import (
	"os"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// TestWorkerToolDenyListIsSingleSource 钉住：禁令只在 workerForbidden* 定义一次。
//
// 变红条件：有人在别处又写一份字面量名单（如 `ct.Name == "spawn_child"`）⇒ 红。
func TestWorkerToolDenyListIsSingleSource(t *testing.T) {
	data, err := os.ReadFile("spawn.go")
	if err != nil {
		t.Fatalf("读 spawn.go: %v", err)
	}
	// ★ 必须**逐行剔除注释**再扫：第一版直接扫全文，结果把自己写在
	// 注释里的"上一版曾在这里重列过 `== "output_list_channels"`"当成了违规。
	// 判据误报会让人开始骗判据（改注释绕过），那比没有判据更糟。
	var code []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	src := strings.Join(code, "\n")

	for _, tool := range []string{"spawn_child", "plgreload", "output_list_channels"} {
		needle := `== "` + tool + `"`
		if strings.Contains(src, needle) {
			t.Fatalf("spawn.go 里出现了第二份工具禁令：`%s`\n"+
				"禁令必须在 workerForbiddenTools 里定义一次，"+
				"定义侧与执行侧都从 workerToolAllowed 派生 —— "+
				"两处各写一遍时，任何一边落后就是缺口", needle)
		}
	}
	if strings.Contains(src, `HasPrefix(name, "output_send__")`) ||
		strings.Contains(src, `HasPrefix(ct.Name, "output_send__")`) {
		t.Fatal("spawn.go 里出现了第二份前缀禁令（output_send__）—— " +
			"应统一走 workerToolAllowed")
	}
}

// TestWorkerToolAllowedMatchesReject 钉住两侧**语义一致**。
//
// 这是上一条的行为版：名单同源还应满足"允许 ⇔ 不拒绝"。
// 变红条件：两边判据不一致（比如一边禁 spawn_child、另一边漏了）。
func TestWorkerToolAllowedMatchesReject(t *testing.T) {
	cases := []struct {
		tool    string
		allowed bool
	}{
		{"output_send__qq", false},
		{"output_send__", false},
		{"output_list_channels", false},
		{"spawn_child", false},
		{"plgreload", false},
		{"cmd_run", true},
		{"files_read", true},
		{"knowledge_search", true},
		// 边界：前缀相似但不等价的必须放行（防"过度过滤"）。
		{"output_list_channels_help", true},
		{"spawn_child_helper", true},
	}
	for _, c := range cases {
		got := workerToolAllowed(c.tool)
		if got != c.allowed {
			t.Errorf("workerToolAllowed(%q) = %v，期望 %v", c.tool, got, c.allowed)
		}
		rejected := workerToolReject(agentAPI.ToolCall{Name: c.tool}) != ""
		if rejected == c.allowed {
			t.Errorf("workerToolReject(%q) 与 workerToolAllowed 不一致："+
				"allowed=%v 但 rejected=%v（两侧判据必须同源）",
				c.tool, c.allowed, rejected)
		}
	}
}

// TestWorkerToolRejectExplainsWhy 钉住拒绝文案**可执行**。
//
// 本仓纪律：拒绝必须说明原因，否则模型会原样重试同一条被拒的调用
// （有"4 次 files_write 失败"先例）。变红条件：返回空文案或只有工具名。
func TestWorkerToolRejectExplainsWhy(t *testing.T) {
	for _, tool := range []string{"output_send__qq", "spawn_child"} {
		msg := workerToolReject(agentAPI.ToolCall{Name: tool})
		if msg == "" {
			t.Fatalf("%s 被拒但没给文案", tool)
		}
		if len([]rune(msg)) < 20 {
			t.Fatalf("%s 的拒绝文案太短（%q）—— 必须说明原因才能让模型换做法", tool, msg)
		}
		if !strings.Contains(msg, tool) {
			t.Errorf("%s 的拒绝文案没提到工具名：%q", tool, msg)
		}
	}
}
