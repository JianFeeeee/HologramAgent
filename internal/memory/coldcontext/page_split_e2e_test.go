package coldcontext

// 段页式切块的 e2e 判据（P2，2026-10-29）。
//
// ## 不变量（来自用户裁定）
//
//	段 Frame = 一次完整任务（一条事件），用事件时间戳作帧号
//	页 Page  = 一次完整往返：
//	           · llm_round  —— 用户输入 → 模型输出
//	           · tool_round —— 工具调用 → 工具结果（靠 CallID 配对）
//	铁律：同一个 FrameTS 的块必须整批在同一层，不许分裂
//
// ## 为什么页必须是"配对完整"的最小单位
//
// 渲染时 `assistant(tool_calls)` 后面必须紧跟**配对齐全**的 `tool` 消息，
// 否则上游直接 400（生产事故实证见 task.go:160 记录的错误）。所以
// 淘汰/裁剪的最小单位必须是整页，不能是单块。

import (
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

func mkEntry(ts time.Time) ContextEntry {
	return ContextEntry{
		Timestamp: ts,
		Source:    "agent",
		Content:   "帮我看下这台机器的磁盘",
		Response:  "已查询，根分区使用 62%。",
		ToolCalls: []ToolCallItem{
			{CallID: "c_disk", Name: "cmd_run", Arguments: map[string]interface{}{"command": "df -h"}},
		},
		ToolResults: []ToolResultItem{
			{CallID: "c_disk", Name: "cmd_run", Output: "/dev/sda1  62%  /"},
		},
	}
}

// C1：一个 toolcall + 它的 result 恰好构成一页；块数 = 轮数 × 2。
func TestPageSplitOneCallOnePage(t *testing.T) {
	ts := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	blocks := docBlocksForEntry(mkEntry(ts))

	// 期望：llm_round 2 块（input/response）+ tool_round 2 块（call/result）
	if len(blocks) != 4 {
		t.Fatalf("期望 4 块（2 页 × 2 侧），实际 %d: %+v", len(blocks), blocks)
	}

	byPage := map[int][]memory.MemoryBlock{}
	for _, b := range blocks {
		byPage[b.PageSeq] = append(byPage[b.PageSeq], b)
	}
	if len(byPage) != 2 {
		t.Fatalf("期望 2 页，实际 %d", len(byPage))
	}

	// 第 1 页：llm_round，request+response
	p1 := byPage[1]
	if len(p1) != 2 || p1[0].PageKind != memory.PageLLMRound {
		t.Fatalf("第 1 页应是 llm_round 的 2 块: %+v", p1)
	}
	if p1[0].BlockRole != memory.RoleRequest || p1[1].BlockRole != memory.RoleResponse {
		t.Fatalf("第 1 页角色错: %+v", p1)
	}

	// 第 2 页：tool_round，call+result，且 CallID 配对
	p2 := byPage[2]
	if len(p2) != 2 || p2[0].PageKind != memory.PageToolRound {
		t.Fatalf("第 2 页应是 tool_round 的 2 块: %+v", p2)
	}
	for _, b := range p2 {
		if b.CallID != "c_disk" {
			t.Fatalf("工具页块缺 CallID（配对键）: %+v", b)
		}
	}
	if !strings.Contains(p2[0].Text, "df -h") {
		t.Fatalf("★ 工具页 call 侧没有参数（这正是要还的技术债）: %q", p2[0].Text)
	}
	if !strings.Contains(p2[1].Text, "62%") {
		t.Fatalf("工具页 result 侧内容错: %q", p2[1].Text)
	}
}

// C7 铁律：同帧所有块共享同一 FrameTS，不许分裂。
func TestPageBlocksShareFrameTS(t *testing.T) {
	ts := time.Date(2026, 10, 29, 13, 30, 0, 0, time.UTC)
	blocks := docBlocksForEntry(mkEntry(ts))

	if len(blocks) == 0 {
		t.Fatal("未派生任何块")
	}
	for _, b := range blocks {
		if !b.FrameTS.Equal(ts) {
			t.Fatalf("★ 块 %s 的 FrameTS=%v 与本帧 %v 不一致 —— 违反「块不跨帧」",
				b.ID, b.FrameTS, ts)
		}
	}
}

// 并行工具调用：各自独立成页，靠 CallID 配对（不能都配到同一个结果）。
func TestPageSplitParallelCalls(t *testing.T) {
	ts := time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC)
	e := ContextEntry{
		Timestamp: ts, Source: "agent",
		Content: "同时查内存和网络", Response: "都查了。",
		ToolCalls: []ToolCallItem{
			{CallID: "c_mem", Name: "sys_info", Arguments: map[string]interface{}{"kind": "mem"}},
			{CallID: "c_net", Name: "sys_info", Arguments: map[string]interface{}{"kind": "net"}},
		},
		ToolResults: []ToolResultItem{
			{CallID: "c_mem", Name: "sys_info", Output: "内存 3.2G/8G"},
			{CallID: "c_net", Name: "sys_info", Output: "网络 192.168.1.7"},
		},
	}
	blocks := docBlocksForEntry(e)

	// 页序：1=llm_round, 2=c_mem, 3=c_net
	byCall := map[string][]memory.MemoryBlock{}
	for _, b := range blocks {
		if b.PageKind == memory.PageToolRound {
			byCall[b.CallID] = append(byCall[b.CallID], b)
		}
	}
	if len(byCall) != 2 {
		t.Fatalf("★ 并行调用未各自成页: %+v", byCall)
	}
	if !strings.Contains(byCall["c_mem"][1].Text, "3.2G") {
		t.Fatalf("★ c_mem 配错结果: %q", byCall["c_mem"][1].Text)
	}
	if !strings.Contains(byCall["c_net"][1].Text, "192.168") {
		t.Fatalf("★ c_net 配错结果: %q", byCall["c_net"][1].Text)
	}
	// 页序必须不同（否则两页挤在一起，渲染时配对歧义）
	if byCall["c_mem"][0].PageSeq == byCall["c_net"][0].PageSeq {
		t.Fatal("★ 并行调用的页序不应相同")
	}
}

// 块 ID 必须幂等：同一事件重复派生得到同一批 ID（否则归档会不断增生）。
func TestPageBlockIDsAreIdempotent(t *testing.T) {
	ts := time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)
	a := docBlocksForEntry(mkEntry(ts))
	b := docBlocksForEntry(mkEntry(ts))

	if len(a) != len(b) {
		t.Fatalf("两次派生块数不同: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatalf("★ 块 ID 不幂等（归档会增生）: %q vs %q", a[i].ID, b[i].ID)
		}
	}
}
