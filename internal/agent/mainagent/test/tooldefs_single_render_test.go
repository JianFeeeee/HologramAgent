package test

// 「工具表只有一个渲染源」的判据（用户裁定 2026-11-19）。
//
// ## 要钉住什么
//
// ① 同一个工具名在注册表里**不能出现两次**
//    （真实事故：`output_list_channels` 被两处 append 追加过，
//      LLM 上游直接报 `Tool names must be unique`）
// ② 主动输出门的判定读**标记**，不读名字前缀
//    （rtchild / worker 各自写过一份 `strings.HasPrefix("output_send__")`，
//      改名就静默失效 —— 本仓反复踩的形态）

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"

	// ★ 空白 import 是**合法的**（与"测试自带注册"不同）：
	//   通道类工具的登记点就在 `mainagent` 的 init（生产装配点也会 import 它），
	//   判据必须读生产注册结果，不能自己在测试里再注册一份假表。
	_ "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent"
)

func sideName(s registry.Side) string {
	switch s {
	case registry.SideParent:
		return "parent"
	case registry.SideChild:
		return "child"
	}
	return "any"
}

// TestNoDuplicateToolNamesInRenderedTable：注册表里不许有同名工具。
func TestNoDuplicateToolNamesInRenderedTable(t *testing.T) {
	// ★ 三张视图会**天然重叠**（SideAny 的工具在每个视图里都出现），
	//   所以判"注册表里有没有同名两项"只能查同一视图内部。
	for _, side := range []registry.Side{registry.SideAny, registry.SideParent, registry.SideChild} {
		seen := map[string]bool{}
		for _, tool := range registry.ToolsFor(side) {
			if seen[tool.Name] {
				t.Fatalf("★ 工具 %q 在同一视图 side=%s 里出现两次 —— 上游会硬拒绝整轮请求",
					tool.Name, sideName(side))
			}
			seen[tool.Name] = true
		}
	}
}

// TestOutputGateIsDeclaredNotGuessed：主动输出门的判定来自注册表标记。
func TestOutputGateIsDeclaredNotGuessed(t *testing.T) {
	if !agent.IsOutputGateTool("output_list_channels") {
		t.Fatal("★ output_list_channels 没有被判为输出门 —— 标记没生效，退回了前缀判定")
	}
	if !agent.IsOutputGateTool("output_send__cli") {
		t.Fatal("★ output_send__<通道> 没有被判为输出门")
	}
	if agent.IsOutputGateTool("stablemem_recall") {
		t.Fatal("★ stablemem_recall 被误判为输出门")
	}
}
