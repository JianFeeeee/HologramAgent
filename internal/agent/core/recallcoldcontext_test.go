package core

// L2 → L1 召回协议的判据。
//
// 依据（assets/docs/zh/ARCHITECTURE.md「记忆流转」② Document 层的读取分支）：
//
//	coldcontext_query → Consume(读取并删除)
//	          → 逐条 context.Append{Timestamp: 原始时间戳, Source: 原来源}
//	          → 文档以原始时间戳写入 context 时间线, 从 coldcontextStore 删除
//
// 三条不变量：
//   ① 逐条：N 条事件回到时间线，不是 1 条合成事件
//   ② 原始时间戳：用事件时刻，不是文档归档时刻
//   ③ 块随事件迁回：Consume 已删 L2，不回填就两层都不在

import (
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/coldcontext"
)

// archiveEvents 走生产路径（Prune）把一批事件归档进 L2。
//
// Prune 无条件保护最近 10 条事件，只在更早的部分里挑归档对象，
// 所以要填充足够多的无关事件把目标挤出保护窗口。
func archiveEvents(t *testing.T, a *Agent, target []ContextEvent) {
	t.Helper()
	for _, ev := range target {
		a.context.Append(ev)
	}
	for i := 0; i < 14; i++ {
		a.context.Append(ContextEvent{
			Timestamp: time.Now(),
			Source:    "filler",
			Input:     "无关填充对话，用来把目标事件挤出保护窗口",
			Response:  "好的。",
		})
	}
	if n := a.context.Prune("当前输入", 2, a.coldcontextStore); n == 0 {
		t.Fatal("Prune 未归档任何事件")
	}
}

func countSource(c *RelevanceContext, src string) int {
	n := 0
	for _, e := range c.Recent(0) {
		if e.Source == src {
			n++
		}
	}
	return n
}

// ① + ② 逐条回填，且每条用自己的原始时间戳。
//
// 变异：改用 d.CreatedAt 当时间戳 ⇒ 红；改回单条合成事件 ⇒ 红。
func TestRecallByDoc_RestoresEachEventAtOriginalTimestamp(t *testing.T) {
	a := newToolTestAgent(t)

	base := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	var want []ContextEvent
	for i := 0; i < 3; i++ {
		want = append(want, ContextEvent{
			Timestamp: base.Add(time.Duration(i) * 7 * time.Minute),
			Source:    "archive-src",
			Input:     "档案事件-" + string(rune('A'+i)) + " 关于紫蓝红配色方案的讨论",
			Response:  "该事件的完整回答，编号 " + string(rune('A'+i)),
		})
	}
	archiveEvents(t, a, want)

	if n := countSource(a.context, "archive-src"); n != 0 {
		t.Fatalf("归档后 L1 仍有 %d 条 archive-src", n)
	}

	out := a.executeDocTool(agentAPI.ToolCall{
		Name:      "coldcontext_query",
		Arguments: map[string]interface{}{"query": "紫蓝红配色方案", "top_k": float64(5)},
	})
	if strings.Contains(out, "未找到") {
		t.Fatalf("coldcontext_query 未召回：%s", out)
	}

	seen := map[time.Time]string{}
	for _, e := range a.context.Recent(0) {
		if e.Source == "archive-src" {
			seen[e.Timestamp] = e.Input
		}
	}
	if len(seen) != 3 {
		t.Fatalf("回填了 %d 条 archive-src 事件，期望 3"+
			"（逐条回填，不是合成一条；也不是换了个时间戳）", len(seen))
	}
	for i := 0; i < 3; i++ {
		ts := base.Add(time.Duration(i) * 7 * time.Minute)
		if _, ok := seen[ts]; !ok {
			t.Errorf("时间戳 %s 缺失 —— 回填必须用事件原始时刻（不是归档时刻）",
				ts.Format("15:04:05"))
		}
	}
	// 不得出现合成事件。
	for _, e := range a.context.Recent(0) {
		if e.Source == "cold_storage" {
			t.Errorf("出现 cold_storage 合成事件（%q）—— 有 Events 时应逐条回填", e.Input)
		}
	}
}

// ③ 块随事件迁回 L1（迁移，不是复制也不是丢弃）。
//
// 变异：不回填 Blocks ⇒ 红（块两层都消失 = 永久丢字节）。
func TestRecallByDoc_MigratesBlocksBackToContext(t *testing.T) {
	a := newToolTestAgent(t)

	payload := []byte("recall-block-bytes")
	b := mediaBlock("image/png", payload)
	if err := a.memory.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
		t.Fatal(err)
	}

	archiveEvents(t, a, []ContextEvent{{
		Timestamp: time.Now().Add(-3 * time.Hour),
		Source:    "archive-src",
		Input:     "这张图请记住：紫蓝红三色带的配色参考",
		Response:  "记住了。",
		Blocks:    []memory.MemoryBlock{b},
	}})

	if blocksInCtx(a.context, b.ID) {
		t.Fatal("归档后块仍在 L1")
	}

	a.executeDocTool(agentAPI.ToolCall{
		Name:      "coldcontext_query",
		Arguments: map[string]interface{}{"query": "紫蓝红三色带 配色参考", "top_k": float64(3)},
	})

	var back *memory.MemoryBlock
	for _, e := range a.context.Recent(0) {
		for i := range e.Blocks {
			if e.Blocks[i].ID == b.ID {
				back = &e.Blocks[i]
			}
		}
	}
	if back == nil {
		t.Fatal("召回后块没有迁回 L1 —— Consume 已删除 L2 文档，块会两层都不在（字节永久丢失）")
	}
	if string(back.Data) != string(payload) {
		t.Errorf("迁回的块字节被改动: %q", string(back.Data))
	}
	// 迁移而非复制。
	for _, d := range a.coldcontextStore.RecentColdContexts(50) {
		for _, db := range d.Blocks {
			if db.ID == b.ID {
				t.Error("块同时留在 L2 与 L1 —— 这是复制，违反单层不变量")
			}
		}
	}
}

// ④ 回填的正文必须是原文，不能再被截断。
//
// 断点史：旧实现在召回时对 Content 再截 2000 字，且 Content 本身已是截断视图。
func TestRecallByDoc_TextIsNotTruncatedOnTheWayBack(t *testing.T) {
	a := newToolTestAgent(t)

	longResp := strings.Repeat("长正文", 2000) + "【召回哨兵R4】"
	archiveEvents(t, a, []ContextEvent{{
		Timestamp: time.Now().Add(-2 * time.Hour),
		Source:    "archive-src",
		Input:     "请记住这份长报告",
		Response:  longResp,
	}})

	a.executeDocTool(agentAPI.ToolCall{
		Name:      "coldcontext_query",
		Arguments: map[string]interface{}{"query": "长报告", "top_k": float64(3)},
	})

	var got string
	for _, e := range a.context.Recent(0) {
		if e.Source == "archive-src" {
			got = e.Response
		}
	}
	if got == "" {
		t.Fatal("未回填事件")
	}
	if got != longResp {
		t.Errorf("回填正文被截断：len=%d，原长 %d", len(got), len(longResp))
	}
	if !strings.Contains(got, "【召回哨兵R4】") {
		t.Error("回填正文丢了结尾哨兵")
	}
}

// ⑤ 存量文档（无 Events）降级：整篇渲成一条事件，不丢弃。
//
// 决定：存量等自然冲刷，但不得因为格式旧就召回不到。
func TestRecallByDoc_LegacyDocDegrades(t *testing.T) {
	a := newToolTestAgent(t)

	if err := a.coldcontextStore.Insert(&coldcontext.ColdDoc{
		Summary: "紫蓝红三色带的旧格式文档",
		Content: "[10:00] legacy: 这是 Events 引入之前归档的正文视图",
		Source:  "context_archived",
		Meta:    map[string]string{"content_hash": "legacy-hash-1"},
	}); err != nil {
		t.Fatal(err)
	}

	out := a.executeDocTool(agentAPI.ToolCall{
		Name:      "coldcontext_query",
		Arguments: map[string]interface{}{"query": "紫蓝红三色带 旧格式", "top_k": float64(3)},
	})
	if strings.Contains(out, "未找到") {
		t.Fatalf("存量文档召回失败：%s", out)
	}

	var found bool
	for _, e := range a.context.Recent(0) {
		if e.Source == "cold_storage" && strings.Contains(e.Response, "Events 引入之前") {
			found = true
		}
	}
	if !found {
		t.Error("存量文档没有降级成合成事件 —— 会静默丢失整篇内容")
	}
}

func blocksInCtx(c *RelevanceContext, blockID string) bool {
	for _, e := range c.Recent(0) {
		for _, b := range e.Blocks {
			if b.ID == blockID {
				return true
			}
		}
	}
	return false
}

// 压力：大批量事件回填不得退化（回填是逐条 InsertByTimestamp，本身 O(n) 插入）。
func TestRecallByDoc_StressManyEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("压力测试")
	}
	a := newToolTestAgent(t)

	const n = 120
	base := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	var evs []ContextEvent
	for i := 0; i < n; i++ {
		evs = append(evs, ContextEvent{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Source:    "stress",
			Input:     "压力输入 " + strings.Repeat("内容", 10),
			Response:  "压力回答 " + strings.Repeat("正文", 20),
		})
	}
	archiveEvents(t, a, evs)

	start := time.Now()
	if got := countSource(a.context, "stress"); got != 0 {
		t.Fatalf("归档后仍有 %d 条", got)
	}
	a.executeDocTool(agentAPI.ToolCall{
		Name:      "coldcontext_query",
		Arguments: map[string]interface{}{"query": "压力输入 内容", "top_k": float64(5)},
	})
	elapsed := time.Since(start)

	got := countSource(a.context, "stress")
	if got == 0 {
		t.Fatal("压力场景下一条也没回填")
	}
	// 回填 120 条不应超过 5 秒（实测远小于此；阈值防 O(n²) 退化）。
	if elapsed > 5*time.Second {
		t.Errorf("回填 %d 条耗时 %v，疑似超线性", got, elapsed)
	}
	t.Logf("回填 %d 条耗时 %v", got, elapsed)
}

// ⑥ coldcontext_query 的返回值必须**带真实内容**，不能只有统计描述。
//
// 断点史（生产日志实测）：旧返回值是
//
//	已加载 3 篇文档记忆: #1(来自 1 个来源的 1 条对话 (agent) 涉及: ok), #2(...)
//
// 全 jieba 词袋，零信息量 ⇒ agent 无法判断这笔召回有没有用 ⇒ 几乎不再调用。
//
// 本判据要求返回值同时具备三件事：
//  1. 明确说明原文去了哪里（"插回对话时序"）
//  2. 每条给出时间 + 来源（可判断"这是哪次的事"）
//  3. 每条给出真实正文片段（可判断"这是不是我要的"）
func TestRecallByDoc_ReturnValueCarriesRealContent(t *testing.T) {
	a := newToolTestAgent(t)

	const sentinel = "紫蓝红三色带按六比三比一"
	archiveEvents(t, a, []ContextEvent{{
		Timestamp: time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC),
		Source:    "webui",
		Input:     "配色方案怎么定？",
		Response:  "结论：" + sentinel + "，主色紫、辅色蓝、点缀红。",
	}})

	out := a.executeDocTool(agentAPI.ToolCall{
		Name:      "coldcontext_query",
		Arguments: map[string]interface{}{"query": "配色方案 紫蓝红", "top_k": float64(3)},
	})

	// 1) 必须说明原文去了时序
	if !strings.Contains(out, "timeline") {
		t.Errorf("返回值没说原文去了哪里：\n%s", out)
	}
	// 2) 必须有时间（年月日或月-日）
	if !strings.Contains(out, "2026-06-07") && !strings.Contains(out, "06-07") {
		t.Errorf("返回值没有时间信息：\n%s", out)
	}
	// 3) 必须有来源
	if !strings.Contains(out, "webui") {
		t.Errorf("返回值没有来源信息：\n%s", out)
	}
	// 4) ★ 必须有真实正文（否则模型无法判断相关性）
	if !strings.Contains(out, "六比三比一") {
		t.Errorf("返回值不含真实正文片段（模型无法判断相关性）：\n%s", out)
	}
	// 5) 不得回退成 jieba 词袋的模板摘要
	if strings.Contains(out, "涉及:") {
		t.Errorf("返回值仍是 jieba 词袋模板摘要：\n%s", out)
	}
}

// ⑦ 旧格式文档的召回也要给出可判断的信息（降级路径不能退化成无内容）。
func TestRecallByDoc_LegacyReturnAlsoCarriesContent(t *testing.T) {
	a := newToolTestAgent(t)
	if err := a.coldcontextStore.Insert(&coldcontext.ColdDoc{
		Summary: "旧格式摘要",
		Content: "[10:00] legacy: 旧格式文档里的真实正文在这里",
		Source:  "context_archived",
		Meta:    map[string]string{"content_hash": "legacy-2"},
	}); err != nil {
		t.Fatal(err)
	}
	out := a.executeDocTool(agentAPI.ToolCall{
		Name:      "coldcontext_query",
		Arguments: map[string]interface{}{"query": "旧格式文档 真实正文", "top_k": float64(3)},
	})
	if !strings.Contains(out, "真实正文在这里") {
		t.Errorf("存量文档召回没有给出正文：\n%s", out)
	}
}
