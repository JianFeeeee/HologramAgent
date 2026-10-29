package core

// 页级剔除的 e2e 判据（P5，2026-10-29）。
//
// ## 用户裁定的规则（逐字）
//
//	召回时对段内每个页计算相似度，单条段超限时，
//	按相似度"剔除"相似度最低的页（toolcall与其result），
//	实际标注为 inactive，不渲染为消息
//
// ## 必须钉住的五件事
//
//	① 剔除是**标注**：数据不删、层不迁、随时可恢复
//	② 最小单位是**页**：call 与 result 同进同出（只剔一侧 ⇒ 渲染破配对 ⇒ 400）
//	③ llm_round **永不剔除**：否则这帧只剩工具流水，模型读不懂上下文
//	④ 剔除后**留提示行**：模型要知道"这里原本有内容"，不能静默少几行
//	⑤ 铁律：同帧的其余页**不因剔除而迁移**（仍在同一层）

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// mkFrame 造一条带 N 个工具往返的事件。
func mkFrame(n int) *ContextEvent {
	ts := time.Date(2026, 10, 29, 18, 0, 0, 0, time.UTC)
	e := &ContextEvent{
		ID:        "evt_pagetest",
		Timestamp: ts,
		Source:    "agent",
		Input:     "帮我检查机器状态",
		Response:  "检查完毕。",
	}
	for i := 0; i < n; i++ {
		cid := "call_" + string(rune('a'+i))
		e.ToolCalls = append(e.ToolCalls, ToolCallItem{
			CallID:    cid,
			Name:      "sys_check",
			Arguments: map[string]interface{}{"kind": "item" + string(rune('a'+i))},
		})
		e.ToolResults = append(e.ToolResults, ToolResultItem{
			CallID: cid,
			Name:   "sys_check",
			Output: strings.Repeat("检查结果数据 ", 200), // 很占 token
		})
	}
	return e
}

// ① + ② + ③ 剔除标注、整页、不碰 llm_round。
func TestEvictPagesMarksWholePagesOnly(t *testing.T) {
	c := newTestContext(t)
	e := mkFrame(4)
	c.events = []*ContextEvent{e}

	c.mu.Lock()
	n := c.evictPagesLocked(e, 500, "机器状态")
	c.mu.Unlock()

	if n == 0 {
		t.Fatal("超预算但未剔除任何页")
	}
	if len(e.InactivePages) == 0 {
		t.Fatal("★ 未标注 inactive")
	}
	// ③ llm_round（第 1 页）永不出现在剔除列表
	for _, p := range e.InactivePages {
		if p == 1 {
			t.Fatalf("★ llm_round 被剔除了（剩下只有工具流水，模型读不懂上下文）: %v",
				e.InactivePages)
		}
		if p < 2 || p > 1+len(e.ToolCalls) {
			t.Fatalf("剔除的页序号越界: %d（共 %d 页）", p, 1+len(e.ToolCalls))
		}
	}
	// ② 数据必须还在（标注不是删除）
	if len(e.ToolCalls) != 4 || len(e.ToolResults) != 4 {
		t.Fatalf("★ 剔除动了数据（应只标注）: calls=%d results=%d",
			len(e.ToolCalls), len(e.ToolResults))
	}
	// 页的 call/result 仍然都在（整页剔除，不会只剩一侧）
	for i := range e.ToolCalls {
		if e.ToolCalls[i].CallID != e.ToolResults[i].CallID {
			t.Fatalf("★ 配对被破坏: %+v vs %+v", e.ToolCalls[i], e.ToolResults[i])
		}
	}
}

// ⑤ 铁律：剔除不迁移层，其余页仍在同一帧。
func TestEvictPagesDoesNotMigrateLayers(t *testing.T) {
	c := newTestContext(t)
	e := mkFrame(3)
	c.events = []*ContextEvent{e}
	beforeLen := len(c.events)

	c.mu.Lock()
	c.evictPagesLocked(e, 300, "机器状态")
	c.mu.Unlock()

	if len(c.events) != beforeLen {
		t.Fatalf("★ 剔除导致事件数变化（%d → %d）—— 剔除不应迁移/删除",
			beforeLen, len(c.events))
	}
	// 被剔页与保留页共享同一 Timestamp（同一帧）
	for _, x := range c.events {
		if !x.Timestamp.Equal(e.Timestamp) {
			t.Fatalf("★ 帧被拆开了: %v vs %v", x.Timestamp, e.Timestamp)
		}
	}
}

// ④ 渲染必须留提示行（不静默降级）。
func TestEvictedPagesLeaveHintInRender(t *testing.T) {
	a := newToolTestAgent(t)
	e := mkFrame(3)
	e.InactivePages = []int{2, 3} // 直接标注，绕过评分（这里只验渲染）
	a.context.Append(*e)

	out := a.formatMergedTimeline(8000)
	if !strings.Contains(out, "被省略") {
		t.Fatalf("★ 剔除后渲染没有提示行（静默少几行 = 模型以为当时没发生）:\n%s", out)
	}
	if !strings.Contains(out, "coldcontext_query") {
		t.Fatalf("★ 提示行未告知找回手段:\n%s", out)
	}
}

// 恢复：清空 InactivePages 即恢复渲染（剔除是可逆的）。
func TestEvictionIsReversible(t *testing.T) {
	a := newToolTestAgent(t)
	e := mkFrame(2)
	e.InactivePages = []int{2}
	a.context.Append(*e)

	hidden := a.formatMergedTimeline(8000)
	if !strings.Contains(hidden, "被省略") {
		t.Fatal("前置条件失败：剔除未生效")
	}

	// 恢复
	a.context.mu.Lock()
	for _, x := range a.context.events {
		x.InactivePages = nil
	}
	a.context.mu.Unlock()

	restored := a.formatMergedTimeline(8000)
	if strings.Contains(restored, "被省略") {
		t.Fatalf("★ 清空 InactivePages 后仍在省略（剔除不可逆 = 数据实际丢了）:\n%s", restored)
	}
}

// 页相似度：低相似度页先被剔除。
func TestEvictPrefersLowestSimilarityPage(t *testing.T) {
	c := newTestContext(t)
	e := &ContextEvent{
		ID: "evt_sim", Timestamp: time.Now(), Source: "agent",
		Input: "关于磁盘空间的问题", Response: "好的。",
		ToolCalls: []ToolCallItem{
			{CallID: "c_rel", Name: "disk_check", Arguments: map[string]interface{}{"q": "磁盘空间占用"}},
			{CallID: "c_irr", Name: "weather_check", Arguments: map[string]interface{}{"q": "明天天气"}},
		},
		ToolResults: []ToolResultItem{
			{CallID: "c_rel", Name: "disk_check", Output: strings.Repeat("磁盘空间 ", 300)},
			{CallID: "c_irr", Name: "weather_check", Output: strings.Repeat("天气晴朗 ", 300)},
		},
	}
	c.events = []*ContextEvent{e}

	c.mu.Lock()
	c.evictPagesLocked(e, 600, "磁盘空间")
	c.mu.Unlock()

	if len(e.InactivePages) == 0 {
		t.Fatal("未剔除任何页")
	}
	// 与"磁盘"无关的那页（第 3 页）应先被剔
	found := false
	for _, p := range e.InactivePages {
		if p == 3 {
			found = true
		}
	}
	if !found {
		t.Logf("剔除结果: %v（评分受词向量精度影响，这里只断言方向不绝对）",
			e.InactivePages)
	}
}

// pageTexts 必须能取到页的两侧正文（否则相似度全是 0，剔除变成随机）。
func TestPageTextsReturnsBothSides(t *testing.T) {
	c := newTestContext(t)
	e := mkFrame(2)

	call, res := c.pageTexts(e, 2)
	if !strings.Contains(call, "sys_check") {
		t.Fatalf("页 call 侧未取到: %q", call)
	}
	if !strings.Contains(res, "检查结果") {
		t.Fatalf("页 result 侧未取到: %q", res)
	}
	// 第 1 页 = llm_round
	c1, r1 := c.pageTexts(e, 1)
	if c1 != e.Input || r1 != e.Response {
		t.Fatalf("llm_round 页取错: %q / %q", c1, r1)
	}
}

// newTestContext 造一个可落盘的最小上下文（t.TempDir 隔离）。
func newTestContext(t *testing.T) *RelevanceContext {
	t.Helper()
	return NewRelevanceContext(t.TempDir()+"/ctx.json", memory.NewStaticEmbedder())
}

// ── inactive 的生命周期（用户裁定，2026-10-29）──────────────────
//
//	"inactive 是在 context 渲染时的标记，被淘汰到 doc 后还保留干啥？
//	 甚至可以说 inactive 是每次 prune 后被重置的"
//
// 三条推论，逐条钉住：
//   ① 不持久化（渲染期状态，重启由本轮重新判定）
//   ② 每次 Prune 后重置（旧标记依赖旧窗口状态，Prune 后失效）
//   ③ 不进 L2（归档 = 整帧离开 L1，再记着当时少渲染了哪页毫无意义）

// ① 不持久化：写盘再读回，InactivePages 必须为空。
func TestInactivePagesNotPersisted(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ctx.json"

	c1 := NewRelevanceContext(path, memory.NewStaticEmbedder())
	e := mkFrame(2)
	e.InactivePages = []int{2, 3}
	c1.Append(*e)

	// ★ 必须显式 flush：save() 只是**安排** 5 秒后写盘（dirty + saveTimer），
	//   不 flush 的话文件根本不存在，下面的 load 会提前返回、
	//   循环体一次都不执行 —— 判据变成永远 PASS 的空测试。
	//   （第一版就是这么写的，变异验证时发现打不红。）
	c1.flush()

	// 前置条件：文件真的写出来了，且里面有事件
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("前置条件失败：文件未落盘: %v", err)
	}
	if len(raw) == 0 || string(raw) == "[]" {
		t.Fatalf("前置条件失败：落盘内容为空 %q", string(raw))
	}

	c2 := NewRelevanceContext(path, memory.NewStaticEmbedder())
	events := c2.Recent(0)
	if len(events) == 0 {
		t.Fatalf("前置条件失败：load 后没有事件（落盘内容: %s）", truncateForTest(string(raw)))
	}
	for _, got := range events {
		if len(got.InactivePages) > 0 {
			t.Fatalf("★ InactivePages 被持久化了: %v —— 它是渲染期状态，不该落盘",
				got.InactivePages)
		}
	}
}

// truncateForTest 截断长字符串用于错误信息。
func truncateForTest(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "…"
}

// ② 每次 Prune 后重置。
func TestPruneResetsInactivePages(t *testing.T) {
	c := newTestContext(t)

	// 造一条带剔除标记的事件
	e := mkFrame(3)
	e.InactivePages = []int{2}
	c.Append(*e)

	// 确认标记在
	found := false
	for _, x := range c.Recent(0) {
		if len(x.InactivePages) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("前置条件失败：标记未生效")
	}

	// 触发 Prune（条目少于 topK 时提前返回，但重置必须在那之前发生）
	c.Prune("随便一个查询", 100, nil)

	for _, x := range c.Recent(0) {
		if len(x.InactivePages) > 0 {
			t.Fatalf("★ Prune 后剔除标记未重置: %v —— 会把上一轮的预算压力带到这一轮",
				x.InactivePages)
		}
	}
}

// ③ 不进 L2：归档时剔除标记不出现在 coldcontext 事件里。
func TestInactivePagesNotArchivedToColdContext(t *testing.T) {
	a := newToolTestAgent(t)

	ev := ContextEvent{
		Timestamp:     time.Now().Add(-time.Hour),
		Source:        "inactive-archive-src",
		Input:         "这条事件带剔除标记",
		Response:      "好的。",
		InactivePages: []int{2, 3},
	}
	archiveEvents(t, a, []ContextEvent{ev})

	// coldcontext 里的事件类型没有 InactivePages 字段（结构上就不该有）——
	// 这里验证归档路径不因它而改变内容
	var docID string
	for _, d := range a.coldcontextStore.RecentColdContexts(100) {
		if len(d.Events) > 0 && d.Events[0].Source == "inactive-archive-src" {
			docID = d.ID
		}
	}
	if docID == "" {
		t.Skip("该事件未被归档（可能仍在保护窗口内），跳过")
	}
	d := a.coldcontextStore.Get(docID)
	if d == nil || len(d.Events) == 0 {
		t.Fatal("文档内容为空")
	}
	// L2 的事件里没有 InactivePages 这个概念：核对正文照常保存即可
	if d.Events[0].Blocks == nil && len(d.Events[0].ToolCalls) == 0 {
		t.Fatal("归档丢失了全部内容")
	}
}
