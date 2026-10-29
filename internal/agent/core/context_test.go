package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

func newTestCtx() *RelevanceContext {
	return NewRelevanceContext("", memory.NewStaticEmbedder(""))
}

func TestContextAppendAndLen(t *testing.T) {
	ctx := newTestCtx()
	if ctx.Len() != 0 {
		t.Errorf("new context should be empty, got %d", ctx.Len())
	}

	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "hello"})
	if ctx.Len() != 1 {
		t.Errorf("expected len 1, got %d", ctx.Len())
	}
}

func TestContextRecent(t *testing.T) {
	ctx := newTestCtx()
	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "a"})
	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "b"})
	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "c"})

	recent := ctx.Recent(2)
	if len(recent) != 2 {
		t.Errorf("expected 2 recent, got %d", len(recent))
	}
	if recent[0].Input != "b" || recent[1].Input != "c" {
		t.Errorf("expected [b, c], got %v", recent)
	}
}

func TestContextFormat(t *testing.T) {
	ctx := newTestCtx()
	f := ctx.Format()
	if f != "" {
		t.Errorf("empty context should format to empty string, got %q", f)
	}

	now := time.Now()
	ctx.Append(ContextEvent{Timestamp: now, Source: "user", Input: "hello"})
	f = ctx.Format()
	if f == "" {
		t.Fatal("non-empty context should produce non-empty format")
	}
	if !contains(f, "hello") {
		t.Errorf("format should contain input 'hello', got: %s", f)
	}
	if !contains(f, "user") {
		t.Errorf("format should contain source 'user'")
	}
}

func TestContextPruneKeepsTopK(t *testing.T) {
	ctx := newTestCtx()
	for i := 0; i < 20; i++ {
		ctx.Append(ContextEvent{
			Timestamp: time.Now(),
			Source:    "user",
			Input:     "今天天气很好",
			Response:  "是的天气不错",
		})
	}
	ctx.Append(ContextEvent{
		Timestamp: time.Now(),
		Source:    "user",
		Input:     "帮我算一下微积分题目",
		Response:  "好的我来算",
	})

	archived := ctx.Prune("微积分", 5, nil)
	_ = archived

	if ctx.Len() > 15 {
		t.Errorf("after prune to 5, len should be ≤15, got %d", ctx.Len())
	}
}

func TestContextPruneWithDocStore(t *testing.T) {
	ctx := newTestCtx()
	for i := 0; i < 15; i++ {
		ctx.Append(ContextEvent{
			Timestamp: time.Now(),
			Source:    "user",
			Input:     "今天天气很好",
			Response:  "是的",
		})
	}

	archived := ctx.Prune("天气", 10, nil)
	if archived != 0 {
		t.Errorf("with nil coldcontextStore, archived should be 0, got %d", archived)
	}
}

func TestContextAppendAfterPrune(t *testing.T) {
	ctx := newTestCtx()
	for i := 0; i < 20; i++ {
		ctx.Append(ContextEvent{
			Timestamp: time.Now(),
			Source:    "user",
			Input:     "hello world",
		})
	}

	ctx.Prune("hello", 3, nil)
	if ctx.Len() > 13 {
		t.Errorf("expected ≤13 after prune, got %d", ctx.Len())
	}

	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "new message"})
	if ctx.Len() > 14 {
		t.Errorf("expected ≤14 after append, got %d", ctx.Len())
	}
}

func TestContextPruneWithStaticEmbedder(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test_embeddings_*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	content := `8 4
天气 0.1 0.2 0.3 0.4
下雨 0.15 0.25 0.35 0.45
台风 0.12 0.22 0.32 0.42
股票 0.9 0.1 0.1 0.1
基金 0.85 0.15 0.1 0.1
微积分 0.1 0.1 0.9 0.1
导数 0.15 0.1 0.85 0.15
数学 0.1 0.1 0.8 0.2
`
	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	embedder := memory.NewStaticEmbedder(tmpFile.Name())
	if !embedder.Loaded() {
		t.Fatal("embedder should be loaded")
	}

	ctx := NewRelevanceContext("", embedder)

	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "今天天气很好", Response: "是的"})
	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "会不会下雨", Response: "会"})
	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "台风来了", Response: "注意"})
	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "帮我算微积分", Response: "好的"})
	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "导数怎么求", Response: "公式"})
	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "数学题", Response: "解答"})
	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "股票涨了", Response: "恭喜"})
	ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "基金定投", Response: "可以"})

	if ctx.Len() != 8 {
		t.Fatalf("expected 8 events, got %d", ctx.Len())
	}

	archived := ctx.Prune("最近基金怎么样", 3, nil)

	if ctx.Len() > 13 {
		t.Errorf("prune should limit total events, got %d", ctx.Len())
	}

	remaining := ctx.Format()
	t.Logf("query: 最近基金怎么样\nremaining events:\n%s", remaining)
	t.Logf("archived: %d", archived)

	needsFund := contains(remaining, "基金定投") || contains(remaining, "股票涨了")
	needsWeather := contains(remaining, "今天天气很好") || contains(remaining, "台风来了")

	t.Logf("has financial events: %v, has weather events: %v", needsFund, needsWeather)
}

func TestContextPruneRecent10Protected(t *testing.T) {
	ctx := newTestCtx()

	for i := 0; i < 15; i++ {
		ctx.Append(ContextEvent{
			Timestamp: time.Now(),
			Source:    "user",
			Input:     "今天天气很好",
		})
	}

	for i := 0; i < 5; i++ {
		ctx.Append(ContextEvent{
			Timestamp: time.Now(),
			Source:    "user",
			Input:     "股票行情",
		})
	}

	ctx.Prune("天气", 3, nil)

	// 最近 10 条全部是"股票行情"（第6-15条是天气，第16-20条是股票）
	// protectCount=10 保护最近 10 条 → 5 条天气最多保留 5+3=8 条
	// 至少最近 10 条全部保留 → 至少包含 5 条股票
	remaining := ctx.Format()
	t.Logf("after weather query:\n%s", remaining)
	weatherCount := 0
	stockCount := 0
	for _, line := range splitLines(remaining) {
		if contains(line, "天气") {
			weatherCount++
		}
		if contains(line, "股票") {
			stockCount++
		}
	}
	t.Logf("weather events: %d, stock events: %d", weatherCount, stockCount)

	if stockCount < 5 {
		t.Errorf("recent 10 should all be protected, expected at least 5 stock events, got %d", stockCount)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestSetDenseSpaceBackfillsExistingEvents 锁死 L0 稠密回填：
//
// NewRelevanceContext 先 load()（此时 denseSpace 仍为 nil，旧事件只算了稀疏
// 向量），SetDenseSpace 才注入稠密空间。若不回填已有事件，它们的 DenseFP
// 为空、DenseVec 长度不符，Prune 里旧事件走稀疏余弦、新事件走稠密余弦——
// 同一次排序里混排两种尺度，谁留下只取决于事件新旧。
func TestSetDenseSpaceBackfillsExistingEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.json")

	// run1：写入事件并落盘（不注入稠密空间）。
	c1 := NewRelevanceContext(path, memory.NewStaticEmbedder(""))
	c1.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "昨天的决定"})
	c1.Append(ContextEvent{Timestamp: time.Now(), Source: "agent", Response: "记为待办"})
	if err := c1.Save(); err != nil {
		t.Fatal(err)
	}

	// run2：模拟重启——load() 发生在 SetDenseSpace 之前。
	c2 := NewRelevanceContext(path, memory.NewStaticEmbedder(""))
	if c2.Len() == 0 {
		t.Fatal("重启后未读回任何事件")
	}
	c2.SetDenseSpace(fakeSpace{})

	events := c2.Recent(c2.Len())
	if len(events) == 0 {
		t.Fatal("no events")
	}
	for i, e := range events {
		if e.DenseFP != "fake-space" || len(e.DenseVec) != 2 {
			t.Errorf("event %d 未回填稠密向量: fp=%q len=%d", i, e.DenseFP, len(e.DenseVec))
		}
	}
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
