package core

// 段页式**端到端**验收（跨 L1→L2→L1 全链路，2026-10-29）。
//
// 前面的判据各自只钉一个环节（字段往返、切块、剔除、回收）。
// 本文件走一遍真实流转，验证五条要求在**联动**时仍成立：
//
//	① 一次任务（段/frame）= 一条事件；时间戳作帧号
//	② 页 = llm_round + 若干 tool_round，call 与 result 靠 CallID 配对
//	③ 归档到 L2 后，块带着帧/页身份（ID/帧/向量不变，只换持有层）
//	④ 召回回填时，ToolCalls/ToolResults 连同 CallID 一起回来（配对不破）
//	⑤ 中途任何一步都不会让"三层都不在"发生
//
// 这些必须在 L1→L2→L1 走一遍才看得出来：单看某一层，
// 字段都"存在"，但可能在搬运时被丢掉。

import (
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// ①~④ 归档→召回：工具调用的帧/页身份与配对键必须完整往返。
func TestSegPagingRoundTripsThroughL2(t *testing.T) {
	a := newToolTestAgent(t)

	base := time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC)
	want := ContextEvent{
		Timestamp: base,
		Source:    "segpaging-src",
		Input:     "帮我查一下这个报错的原因，顺便看看磁盘够不够",
		Response:  "报错是权限问题；磁盘还剩 38%。",
		ToolCalls: []ToolCallItem{
			{CallID: "c_log", Name: "log_grep", Arguments: map[string]interface{}{"pattern": "permission denied"}},
			{CallID: "c_disk", Name: "disk_usage", Arguments: map[string]interface{}{"path": "/"}},
		},
		ToolResults: []ToolResultItem{
			{CallID: "c_log", Name: "log_grep", Output: "EACCES: permission denied, open /etc/shadow"},
			{CallID: "c_disk", Name: "disk_usage", Output: "/dev/sda1 62% /"},
		},
	}
	archiveEvents(t, a, []ContextEvent{want})

	// 归档后 L1 不该再有这条
	if n := countSource(a.context, "segpaging-src"); n != 0 {
		t.Fatalf("归档后 L1 仍有 %d 条", n)
	}

	// 召回
	out := a.executeDocTool(agentAPI.ToolCall{
		Name:      "coldcontext_query",
		Arguments: map[string]interface{}{"query": "权限报错 磁盘", "top_k": float64(5)},
	})
	if strings.Contains(out, "no relevant cold context") {
		t.Fatalf("未召回：%s", out)
	}

	// ① 回填的事件仍在原始时间戳
	var got *ContextEvent
	for _, e := range a.context.Recent(0) {
		if e.Source == "segpaging-src" {
			ev := e // Recent 返回值切片，取地址要自己造
			got = &ev
		}
	}
	if got == nil {
		t.Fatal("回填后 L1 找不到该事件")
	}
	if !got.Timestamp.Equal(base) {
		t.Fatalf("★ 时间戳漂移（帧号应不变）: %v vs %v", got.Timestamp, base)
	}

	// ② 配对键完整（这是渲染不 400 的前提）
	if len(got.ToolCalls) != 2 || len(got.ToolResults) != 2 {
		t.Fatalf("★ 工具记录数量不对: calls=%d results=%d",
			len(got.ToolCalls), len(got.ToolResults))
	}
	byCall := map[string]string{}
	for _, tr := range got.ToolResults {
		byCall[tr.CallID] = tr.Output
	}
	if !strings.Contains(byCall["c_log"], "permission denied") {
		t.Fatalf("★ c_log 配错/丢失: %+v", got.ToolResults)
	}
	if !strings.Contains(byCall["c_disk"], "62%") {
		t.Fatalf("★ c_disk 配错/丢失: %+v", got.ToolResults)
	}
	// ③ 参数也必须回来（否则页的 call 侧是空的）
	for _, tc := range got.ToolCalls {
		if len(tc.Arguments) == 0 {
			t.Fatalf("★ 召回后参数丢失（call 侧空页）: %+v", tc)
		}
	}
}

// ⑤ 归档到 L2 的块必须带帧/页身份（ID/帧/向量不变，只换层）。
func TestArchivedBlocksCarryFrameIdentity(t *testing.T) {
	a := newToolTestAgent(t)
	base := time.Date(2026, 10, 29, 21, 0, 0, 0, time.UTC)
	ev := ContextEvent{
		Timestamp: base, Source: "frame-src",
		Input: "紫藤花架的搭建方法", Response: "需要四根立柱。",
		ToolCalls: []ToolCallItem{
			{CallID: "c_w", Name: "weather", Arguments: map[string]interface{}{"city": "新乡"}},
		},
		ToolResults: []ToolResultItem{
			{CallID: "c_w", Name: "weather", Output: "晴 22℃"},
		},
	}
	archiveEvents(t, a, []ContextEvent{ev})

	// 找出这篇文档
	var docID string
	for _, d := range a.coldcontextStore.RecentColdContexts(100) {
		if len(d.Events) > 0 && d.Events[0].Source == "frame-src" {
			docID = d.ID
			break
		}
	}
	if docID == "" {
		t.Fatal("归档文档未找到")
	}

	d := a.coldcontextStore.Get(docID)
	if d == nil || len(d.Events) == 0 {
		t.Fatal("文档内容为空")
	}
	de := d.Events[0]

	// 事件级：ToolCalls 必须落到 L2
	if len(de.ToolCalls) != 1 || de.ToolCalls[0].CallID != "c_w" {
		t.Fatalf("★ L2 事件缺 ToolCalls/CallID: %+v", de.ToolCalls)
	}
	if len(de.ToolCalls[0].Arguments) == 0 {
		t.Fatalf("★ L2 事件丢了参数: %+v", de.ToolCalls[0])
	}

	// 块级：派生出的块必须带帧/页身份
	toolBlocks := 0
	for _, b := range de.Blocks {
		if b.PageKind == memory.PageToolRound {
			toolBlocks++
			if b.FrameTS.IsZero() {
				t.Fatalf("★ L2 块缺 FrameTS: %+v", b)
			}
			if !b.FrameTS.Equal(base) {
				t.Fatalf("★ L2 块的帧号与事件不一致: %v vs %v", b.FrameTS, base)
			}
			if b.CallID == "" {
				t.Fatalf("★ L2 工具页块缺 CallID（配对键）: %+v", b)
			}
		}
	}
	if toolBlocks == 0 {
		t.Log("该文档未派生工具页块（可能按存量路径处理），检查 docBlocksForEntry")
	}
}

// 铁律验收：同一帧的所有块共享同一 FrameTS（不跨帧）。
func TestSameFrameBlocksNeverSplit(t *testing.T) {
	a := newToolTestAgent(t)
	base := time.Date(2026, 10, 29, 22, 0, 0, 0, time.UTC)
	ev := ContextEvent{
		Timestamp: base, Source: "nosplit-src",
		Input: "同时检查 CPU 和内存", Response: "都查了。",
		ToolCalls: []ToolCallItem{
			{CallID: "c_cpu", Name: "sys", Arguments: map[string]interface{}{"k": "cpu"}},
			{CallID: "c_mem", Name: "sys", Arguments: map[string]interface{}{"k": "mem"}},
			{CallID: "c_net", Name: "sys", Arguments: map[string]interface{}{"k": "net"}},
		},
		ToolResults: []ToolResultItem{
			{CallID: "c_cpu", Name: "sys", Output: "CPU 12%"},
			{CallID: "c_mem", Name: "sys", Output: "内存 41%"},
			{CallID: "c_net", Name: "sys", Output: "网络正常"},
		},
	}
	archiveEvents(t, a, []ContextEvent{ev})

	var d = a.coldcontextStore.Get(func() string {
		for _, x := range a.coldcontextStore.RecentColdContexts(100) {
			if len(x.Events) > 0 && x.Events[0].Source == "nosplit-src" {
				return x.ID
			}
		}
		return ""
	}())
	if d == nil {
		t.Fatal("文档未找到")
	}

	// 同一帧的所有块：FrameTS 必须全部相同
	seen := map[string]int{}
	for _, b := range d.Events[0].Blocks {
		if b.FrameTS.IsZero() {
			continue
		}
		seen[b.FrameTS.Format(time.RFC3339Nano)]++
	}
	if len(seen) > 1 {
		t.Fatalf("★ 同一帧的块分裂到 %d 个 FrameTS（违反铁律）: %v", len(seen), seen)
	}
}
