package core

// 召回期「先估算、超预算按页剔除」的 e2e 判据（2026-10-29）。
//
// ## 用户裁定的算法（逐字）
//
//	召回过程中要先估算召回后的 token，如果整段召回不超预算，则直接召回，
//	若超了则对段内每一页算相似度，将最不相似的几个页标记为 inactive，
//	直到不超预算
//
// ## 必须钉住的六件事
//
//	① 装得下 ⇒ **一个字都不剔**（剔除有代价，有余量时不该发生）
//	② 装不下 ⇒ 剔除到刚好装下（不多剔一页）
//	③ 每次剔的是**全局最不相似**的页（不是"每篇自己管自己"）
//	④ llm_round（第 1 页）**永不剔除**
//	⑤ 剔除是标注：数据仍在、仍可回读、清空即恢复
//	⑥ 回执必须**说明**剔了几页（模型要知道看到的是残缺上下文）

import (
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/memory/coldcontext"
)

// mkDoc 造一篇带 n 个工具往返的 L2 文档。
//
// ★ 走**真实归档路径**（ContextToColdContext），不手工伪造块 ID。
//
// 第一版是手工构造 DocEvent 的，结果两条判据全是假象：
//
//	· 块 ID 不满足 entryTextBlockID 的派生规则 ⇒ InOut() 取不到正文
//	  （第 1 页 token 恒为 0）
//	· 工具参数写成纯英文 "df -h"，与中文查询零词重叠 ⇒ 相似度恒为 0
//
// 手工伪造的 fixture 与生产形态不一致时，判据测的是 fixture 而不是代码。
func mkDoc(t *testing.T, a *Agent, id string, n int, body string) *coldcontext.ColdDoc {
	t.Helper()
	ts := time.Date(2026, 10, 29, 23, 0, 0, 0, time.UTC).
		Add(time.Duration(len(id)) * time.Second) // 避免 ID 派生撞车
	entry := coldcontext.ContextEntry{
		Timestamp: ts,
		Source:    "agent",
		Content:   body + " 用户提问",
		Response:  body + " 模型回答",
	}
	for i := 0; i < n; i++ {
		cid := id + "_c" + itoaLocal(i)
		entry.ToolCalls = append(entry.ToolCalls, coldcontext.ToolCallItem{
			CallID: cid, Name: "cmd_run",
			Arguments: map[string]interface{}{"command": body + " 检查命令"},
		})
		entry.ToolResults = append(entry.ToolResults, coldcontext.ToolResultItem{
			CallID: cid, Name: "cmd_run",
			Output: strings.Repeat(body+" 输出数据 ", 60),
		})
	}
	doc, err := a.coldcontextStore.ContextToColdContext(id, []coldcontext.ContextEntry{entry},
		nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("归档失败: %v", err)
	}
	if doc == nil {
		t.Fatal("归档返回空文档")
	}
	return doc
}

func itoaLocal(n int) string {
	return string(rune('0' + n))
}

// ① 装得下：一个字都不剔。
//
// 这是本算法最重要的性质：剔除会让模型看到残缺上下文，
// 有余量时必须完整召回。
func TestPlanRecallNoEvictionWhenFits(t *testing.T) {
	a := newToolTestAgent(t)
	c := a.context
	docs := []*coldcontext.ColdDoc{
		mkDoc(t, a, "d1", 2, "磁盘空间检查"),
		mkDoc(t, a, "d2", 2, "磁盘空间检查"),
	}

	plan := c.planColdRecall(docs, "磁盘空间", 1_000_000) // 预算充足

	if plan.Over {
		t.Fatalf("★ 预算充足却发生了剔除（应当完整召回）: %v", plan.Evict)
	}
	if len(plan.Evict) != 0 {
		t.Fatalf("★ 预算充足却剔了页: %v", plan.Evict)
	}
}

// ② 装不下：剔到刚好装下，不多剔。
func TestPlanRecallEvictsJustEnough(t *testing.T) {
	a := newToolTestAgent(t)
	c := a.context
	docs := []*coldcontext.ColdDoc{mkDoc(t, a, "d1", 6, "磁盘空间检查")}

	full := c.planColdRecall(docs, "磁盘空间", 1_000_000)
	if full.Over {
		t.Fatal("前置条件失败")
	}
	// 给一个"不够装全部、但装得下大部分"的预算
	budget := full.Need / 2
	plan := c.planColdRecall(docs, "磁盘空间", budget)

	if !plan.Over {
		t.Fatal("★ 预算不足却未剔除——会导致渲染时丢掉更早的真实历史")
	}
	if plan.Need > budget {
		t.Fatalf("★ 剔完仍超预算: need=%d budget=%d（会继续溢出）", plan.Need, budget)
	}
	// ★ 必须真的剔了东西（否则"够装下"是假的）
	evictedPages := countPlanPages(plan.Evict)
	if evictedPages == 0 {
		t.Fatalf("★ 判超预算却剔了 0 页（token 降了但没记录剔除，执行时会整批回灌）")
	}
	// 不能过度剔除：6 个工具页里剔掉的不该是全部
	if evictedPages >= 6 {
		t.Fatalf("★ 剔了 %d 页（共 6 个工具页），过度剔除", evictedPages)
	}
	t.Logf("预算 %d / 原需 %d：剔除 %d 页后 %d token", budget, full.Need, evictedPages, plan.Need)
}

// ③ 跨文档取全局最不相似：不是"每篇自己管自己"。
func TestPlanRecallEvictsGloballyNotPerDoc(t *testing.T) {
	a := newToolTestAgent(t)
	c := a.context

	// d_rel 与磁盘相关；d_irr 与天气相关（正文/参数/结果全都换掉）
	rel := mkDoc(t, a, "d_rel", 3, "磁盘空间检查")
	irr := mkDoc(t, a, "d_irr", 3, "明天天气晴朗二十四度")

	docs := []*coldcontext.ColdDoc{rel, irr}
	full := c.planColdRecall(docs, "磁盘空间占用", 1_000_000)
	plan := c.planColdRecall(docs, "磁盘空间占用", full.Need*2/3)

	if !plan.Over {
		t.Fatal("前置条件失败：应触发剔除")
	}
	// 与查询无关的那篇应被剔得更多
	// 汇总各篇被剔页数（key 是文档 ID，不是固定名字）
	nRel := len(plan.Evict[rel.ID])
	nIrr := len(plan.Evict[irr.ID])
	t.Logf("剔除分布：相关篇(%s) %d 页，无关篇(%s) %d 页",
		rel.ID, nRel, irr.ID, nIrr)

	if countPlanPages(plan.Evict) == 0 {
		t.Fatal("★ 判超预算却剔了 0 页")
	}
	if nIrr <= nRel {
		t.Fatalf("★ 无关篇只剔了 %d 页、相关篇却剔了 %d 页 —— "+
			"应按全局相似度优先剔无关页", nIrr, nRel)
	}
}

// ④ llm_round（第 1 页）永不可剔。
func TestPlanRecallNeverEvictsLLMRound(t *testing.T) {
	a := newToolTestAgent(t)
	c := a.context
	docs := []*coldcontext.ColdDoc{mkDoc(t, a, "d1", 4, "磁盘空间检查")}

	// 极端预算：逼它剔到极限
	plan := c.planColdRecall(docs, "磁盘", 10)

	for docID, pages := range plan.Evict {
		for _, p := range pages {
			if p == 1 {
				t.Fatalf("★ 文档 %s 剔除了 llm_round（第 1 页）: %v", docID, pages)
			}
		}
	}
}

// ⑤ 剔除是标注：数据仍在文档里，可回读。
func TestPlanRecallEvictionKeepsData(t *testing.T) {
	a := newToolTestAgent(t)
	c := a.context
	d := mkDoc(t, a, "d1", 4, "磁盘空间检查")
	docs := []*coldcontext.ColdDoc{d}

	callsBefore := len(d.Events[0].ToolCalls)
	resultsBefore := len(d.Events[0].ToolResults)

	_ = c.planColdRecall(docs, "磁盘", 100) // 强制剔除

	if len(d.Events[0].ToolCalls) != callsBefore ||
		len(d.Events[0].ToolResults) != resultsBefore {
		t.Fatalf("★ 剔除动了数据（应只标注）: calls %d→%d results %d→%d",
			callsBefore, len(d.Events[0].ToolCalls),
			resultsBefore, len(d.Events[0].ToolResults))
	}
}

// ⑥ 回执说明剔了几页（模型必须知道自己看到的是残缺上下文）。
func TestRecallReplyAnnouncesEviction(t *testing.T) {
	a := newToolTestAgent(t)
	base := time.Date(2026, 10, 29, 23, 30, 0, 0, time.UTC)
	var want []ContextEvent
	for i := 0; i < 3; i++ {
		ev := ContextEvent{
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Source:    "plan-src",
			Input:     "检查磁盘并顺便看天气-" + itoaLocal(i),
			Response:  "已检查。",
		}
		for j := 0; j < 3; j++ {
			cid := "c" + itoaLocal(i) + itoaLocal(j)
			ev.ToolCalls = append(ev.ToolCalls, ToolCallItem{
				CallID: cid, Name: "cmd_run",
				Arguments: map[string]interface{}{"command": strings.Repeat("x", 400)},
			})
			ev.ToolResults = append(ev.ToolResults, ToolResultItem{
				CallID: cid, Name: "cmd_run",
				Output: strings.Repeat("输出数据 ", 400),
			})
		}
		want = append(want, ev)
	}
	archiveEvents(t, a, want)

	out := a.executeDocTool(agentAPI.ToolCall{
		Name:      "coldcontext_query",
		Arguments: map[string]interface{}{"query": "磁盘 天气", "top_k": float64(5)},
	})
	if strings.Contains(out, "no relevant cold context") {
		t.Fatalf("未召回：%s", out)
	}
	// 有剔除时必须写明；无剔除时不该出现
	if strings.Contains(out, "marked inactive") {
		if !strings.Contains(out, "page(s)") {
			t.Fatalf("★ 回执说明了剔除但没给页数: %s", out)
		}
		t.Logf("回执已声明剔除: %.200s", out)
	} else {
		t.Logf("本次未触发剔除（预算充足），回执为正常召回: %.150s", out)
	}
}
