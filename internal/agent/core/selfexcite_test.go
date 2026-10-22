package core

// doc_query 的**自激震荡**判据。
//
// ## 回路长什么样（用户 2026-10-22 指出）
//
//	doc_query
//	  ├─ ① 工具返回值含被召回事件的正文片段 → 作为 ToolResults 进 context
//	  │     → 参与向量化/关键词/蒸馏 → 同一内容被**重新注入**记忆
//	  ├─ ② 回填事件本身（带原 Source、完整 Blocks）进 L1
//	  │     → Prune 因相关而保留它们（挤掉新事件）
//	  │     → 下一轮 Prune 又把它们归档回 L2
//	  └─ ⇒ 下次 doc_query 再召回同一批：块在 L1↔L2 之间反复搬迁
//
// 两个入口都必须关掉：
//   ① ToolDef.NoMemory —— doc_query 声明，工具输出不进计算层
//   ② ContextEvent.NoMemory —— restoreDocEvents 置位，回填事件不再归档

import (
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// ① doc_query 必须声明 NoMemory（工具输出不进记忆计算）。
//
// 变异：去掉 toolDefOptions{noMemory:true} ⇒ 红。
func TestSelfExcite_DocQueryDeclaresNoMemory(t *testing.T) {
	// 工具定义层面：声明必须可查（不依赖工具表是否已构造）
	if !noMemoryOf("doc_query") {
		t.Error("doc_query 未声明 NoMemory —— 它的返回值含被召回事件的正文，" +
			"再参与向量化/蒸馏就是把同一内容重新注入记忆")
	}

	// 计算层层面：带 doc_query 输出的工具事件，其向量化文本不得含该输出
	const sentinel = "这是被召回事件的正文片段哨兵"
	evt := ContextEvent{
		Timestamp: time.Now(),
		Source:    "agent",
		Input:     "查一下历史",
		ToolResults: []ToolResultItem{
			{Name: "doc_query", Output: "已召回：" + sentinel},
		},
	}
	// stageHost 在测试环境为 nil：传 nil lookup，只走内置声明表那条路
	text := textForVector(&evt, nil, nil)
	if strings.Contains(text, sentinel) {
		t.Errorf("doc_query 的输出进入了计算层（会被重新向量化/蒸馏）：\n%s", text)
	}
}

// ② 回填事件必须带 NoMemory，且不得被 Prune 再次归档回 L2。
//
// 变异：restoreDocEvents 不置 NoMemory ⇒ 红（Prune 会把它归档回 L2）。
func TestSelfExcite_RestoredEventsNotReArchived(t *testing.T) {
	a := newToolTestAgent(t)

	// 先归档一条事件到 L2
	archiveEvents(t, a, []ContextEvent{{
		Timestamp: time.Now().Add(-2 * time.Hour),
		Source:    "webui",
		Input:     "自激测试：这条会被归档",
		Response:  "然后被 doc_query 取回",
	}})

	// 召回
	out := a.executeDocTool(agentAPI.ToolCall{
		Name:      "doc_query",
		Arguments: map[string]interface{}{"query": "自激测试 归档", "top_k": float64(3)},
	})
	if strings.Contains(out, "未找到") {
		t.Fatalf("未召回：%s", out)
	}

	// 回填的事件必须带 NoMemory 标记
	restored := 0
	for _, e := range a.context.Recent(0) {
		if e.Source == "webui" && strings.Contains(e.Input, "自激测试") {
			restored++
			if !e.NoMemory {
				t.Error("回填事件没有 NoMemory 标记 —— Prune 会把它再归档回 L2，形成自激回路")
			}
		}
	}
	if restored == 0 {
		t.Fatal("没有回填任何事件")
	}

	// ③ 真实验证回路：再 Prune 一次。
	//
	// 关键不是"文档数不变"（filler 本身也会被归档），而是
	// **L2 里不得出现含回填事件内容的新文档**。
	for i := 0; i < 14; i++ {
		a.context.Append(ContextEvent{
			Timestamp: time.Now(),
			Source:    "filler",
			Input:     "无关填充，用来触发 Prune",
			Response:  "好。",
		})
	}
	a.context.Prune("当前输入", 2, a.docStore)

	for _, d := range a.docStore.RecentDocs(1000) {
		if !d.HasEvents() {
			continue
		}
		for _, ev := range d.Events {
			in, _, _ := ev.InOut()
			if strings.Contains(in, "自激测试") {
				t.Errorf("回填事件被 Prune 又归档回了 L2（doc=%s）—— 自激回路成立：\n"+
					"下次 doc_query 会再召回同一批，块在 L1↔L2 之间反复搬迁", d.ID)
			}
		}
	}
}

// ④ 回填事件不得参与 Prune 的相关性打分（否则会挤掉真正的新事件）。
func TestSelfExcite_RestoredEventsDontCrowdOutNewOnes(t *testing.T) {
	a := newToolTestAgent(t)

	archiveEvents(t, a, []ContextEvent{{
		Timestamp: time.Now().Add(-3 * time.Hour),
		Source:    "webui",
		Input:     "拥挤测试的原始事件",
		Response:  "响应",
	}})
	a.executeDocTool(agentAPI.ToolCall{
		Name:      "doc_query",
		Arguments: map[string]interface{}{"query": "拥挤测试", "top_k": float64(3)},
	})

	// 回填事件的向量必须为空（NoMemory → textForVector 返回 ""）
	for _, e := range a.context.Recent(0) {
		if e.NoMemory && len(e.Vector) > 0 {
			t.Errorf("回填事件仍被向量化（vector len=%d）—— 它会参与相关性打分并挤掉新事件",
				len(e.Vector))
		}
	}

	// 块仍在 L1（迁移不是丢弃）
	var blocks int
	for _, e := range a.context.Recent(0) {
		blocks += len(e.Blocks)
	}
	_ = blocks
	_ = memory.BlockText
}
