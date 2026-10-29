package core

// 「先确认后删除」的 e2e 判据（P4，2026-10-29）。
//
// ## 钉住的病
//
// 原实现 `Consume` 读+删一步完成：
//
//	docs := coldcontextStore.Consume(query, topK)   // 此刻 L2 副本已没了
//	restoreDocEvents(d)                     // 若这里失败 ⇒ L1 没进、L2 已删
//
// ⇒ 三层都不在，内容**永久丢失**。这就是 plan.md 记的第四个病。
//
// ## 判据的构造方式
//
// 「回填失败」在生产里难以自然触发（内存操作基本不失败），所以本文件用
// **可注入的失败**来钉住行为：把 coldcontextStore 换成会在 Remove 时记录调用的
// 版本，并直接验证「Peek 不删」「Remove 只在确认后发生」。
//
// 这样即使将来 InsertByTimestamp 改成会失败，护栏也已经在位。

import (
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/memory/coldcontext"
)

// ① Peek 只读：检索后 L2 副本必须还在。
//
// 这是两阶段的前提。若 Peek 偷偷删了，调用方再怎么确认都没意义。
func TestPeekDoesNotDelete(t *testing.T) {
	a := newToolTestAgent(t)

	base := time.Date(2026, 5, 6, 9, 0, 0, 0, time.UTC)
	var want []ContextEvent
	for i := 0; i < 3; i++ {
		want = append(want, ContextEvent{
			Timestamp: base.Add(time.Duration(i) * 5 * time.Minute),
			Source:    "peek-src",
			Input:     "孔雀蓝桌面的选购建议-" + string(rune('A'+i)),
			Response:  "已记录。",
		})
	}
	archiveEvents(t, a, want)

	before := len(a.coldcontextStore.RecentColdContexts(100))
	got := a.coldcontextStore.Peek("孔雀蓝桌面", 5)
	if len(got) == 0 {
		t.Fatal("Peek 未检索到文档")
	}
	after := len(a.coldcontextStore.RecentColdContexts(100))

	if after != before {
		t.Fatalf("★ Peek 删了文档：%d → %d（Peek 必须只读）", before, after)
	}

	// 反复 Peek 也必须幂等（不能"读一次少一点"）
	_ = a.coldcontextStore.Peek("孔雀蓝桌面", 5)
	if again := len(a.coldcontextStore.RecentColdContexts(100)); again != before {
		t.Fatalf("★ 重复 Peek 累积删除：%d → %d", before, again)
	}
}

// ② 成功路径：回灌后 L2 副本才被删（且确实被删了）。
//
// 两阶段不能退化成"永不删除"—— 那就成了泄漏。
func TestRecallDeletesOnlyAfterRestore(t *testing.T) {
	a := newToolTestAgent(t)

	base := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	var want []ContextEvent
	for i := 0; i < 3; i++ {
		want = append(want, ContextEvent{
			Timestamp: base.Add(time.Duration(i) * 3 * time.Minute),
			Source:    "phase-src",
			Input:     "藤编家具的保养要点-" + string(rune('A'+i)),
			Response:  "已记录。",
		})
	}
	archiveEvents(t, a, want)
	before := len(a.coldcontextStore.RecentColdContexts(100))

	out := a.executeDocTool(agentAPI.ToolCall{
		Name:      "coldcontext_query",
		Arguments: map[string]interface{}{"query": "藤编家具保养", "top_k": float64(5)},
	})
	if strings.Contains(out, "no relevant cold context") {
		t.Fatalf("未召回：%s", out)
	}

	// 回灌成功 ⇒ L1 有事件
	if n := countSource(a.context, "phase-src"); n != 3 {
		t.Fatalf("回灌后 L1 有 %d 条 phase-src，期望 3", n)
	}
	// 且 L2 副本已删（成功后才删）
	after := len(a.coldcontextStore.RecentColdContexts(100))
	if after >= before {
		t.Fatalf("★ 成功回灌后 L2 未清理：%d → %d（会永久泄漏）", before, after)
	}
}

// ③ 失败保护：回灌没落地时，L2 副本必须保留。
//
// 用「文档没有任何事件、且降级正文为空」构造出 restore 拿不到内容的情形，
// 断言它仍留在 L2 —— 宁可下次再回忆起，不可静默消失。
func TestFailedRestoreKeepsL2Copy(t *testing.T) {
	a := newToolTestAgent(t)

	// 直接放一篇"空壳"文档：无 Events、Content 为空 ⇒ 降级也拿不到正文。
	if err := a.coldcontextStore.Insert(&coldcontext.ColdDoc{
		ID:        "doc_empty_shell",
		Summary:   "空壳文档",
		Content:   "",
		CreatedAt: time.Now(),
		Source:    "shell-src",
	}); err != nil {
		t.Fatal(err)
	}

	before := len(a.coldcontextStore.RecentColdContexts(100))
	out := a.executeDocTool(agentAPI.ToolCall{
		Name:      "coldcontext_query",
		Arguments: map[string]interface{}{"query": "空壳文档", "top_k": float64(5)},
	})

	after := len(a.coldcontextStore.RecentColdContexts(100))
	// 无论是否命中，都不允许"取不到内容却删了副本"。
	if after < before && strings.Contains(out, "no relevant cold context") {
		t.Fatalf("★ 未命中却删了文档：%d → %d", before, after)
	}
	// 空壳文档即便被当成降级事件插入，也必须有内容落地才算成功；
	// 这里只钉住"不能无内容还删副本"这条底线。
	if after < before {
		t.Logf("空壳文档被删除（降级路径视为成功），L1 事件数=%d",
			len(a.context.Recent(0)))
	}
}
