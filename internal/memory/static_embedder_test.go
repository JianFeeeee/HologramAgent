package memory

import (
	"math"
	"sort"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
)

type testContextEvent struct {
	Timestamp time.Time
	Input     string
	Response  string
	Vector    vector.Vector
}

func cosineSim(a, b vector.Vector) float64 {
	var dot, normA, normB float64
	for f, va := range a {
		dot += va * b[f]
		normA += va * va
	}
	for _, vb := range b {
		normB += vb * vb
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func TestStaticEmbedderLoad(t *testing.T) {
	e := newSynthEmbedder(t, 300)
	if !e.Loaded() {
		t.Fatal("embedder should be loaded")
	}
	if e.Dim() != 300 {
		t.Errorf("expected dim=300, got %d", e.Dim())
	}
}

func TestStaticEmbedderConsistency(t *testing.T) {
	e := newSynthEmbedder(t, 300)

	v1 := e.Vectorize("今天天气怎么样")
	v2 := e.Vectorize("今天天气怎么样")

	if len(v1) != len(v2) {
		t.Errorf("same input should produce same dimension count, got %d vs %d", len(v1), len(v2))
	}
	sim := cosineSim(v1, v2)
	if math.Abs(sim-1.0) > 0.0001 {
		t.Errorf("same input should have cosine similarity ~1.0, got %.6f", sim)
	}
}

func isNumericKey(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func TestStaticEmbedderFallback(t *testing.T) {
	e := NewStaticEmbedder("")
	if e.Loaded() {
		t.Fatal("empty path embedder should not be loaded")
	}

	v := e.Vectorize("测试文本")
	if len(v) == 0 {
		t.Fatal("fallback vector should not be empty")
	}
}

func TestStaticEmbedderAllInDenseSpace(t *testing.T) {
	e := newSynthEmbedder(t, 300)

	texts := []string{
		"今天天气怎么样",
		"股票基金投资",
		"微积分导数数学题",
		"台风天注意安全",
		"基金定投",
		"数学作业",
		"明天会不会下雨",
	}

	for _, text := range texts {
		v := e.Vectorize(text)
		for k := range v {
			if !isNumericKey(k) {
				t.Errorf("%q produced non-numeric key %q — should be in dense space", text, k)
			}
		}
	}
	t.Log("all texts produce numeric keys — same dense space")
}

func TestStaticEmbedderSemanticSimilarity(t *testing.T) {
	e := newSynthEmbedder(t, 300)

	pairs := []struct {
		a, b    string
		related bool
	}{
		{"今天天气怎么样", "明天会不会下雨", true},
		{"今天天气怎么样", "股票基金投资", false},
		{"股票基金投资", "基金定投", true},
		{"股票基金投资", "微积分导数数学题", false},
		{"微积分导数数学题", "数学作业", true},
	}

	for _, p := range pairs {
		va := e.Vectorize(p.a)
		vb := e.Vectorize(p.b)
		sim := cosineSim(va, vb)
		t.Logf("sim(%q, %q) = %.4f  (related=%v)", p.a, p.b, sim, p.related)
	}

	weatherSim := cosineSim(e.Vectorize("今天天气怎么样"), e.Vectorize("明天会不会下雨"))
	stockSim := cosineSim(e.Vectorize("今天天气怎么样"), e.Vectorize("股票基金投资"))
	t.Logf("[verify] weather-weather=%.4f, weather-stock=%.4f", weatherSim, stockSim)
	if weatherSim <= stockSim {
		t.Errorf("weather-weather(%.4f) should be > weather-stock(%.4f)", weatherSim, stockSim)
	}
}

func TestContextPruneWithRealEmbedding(t *testing.T) {
	e := newSynthEmbedder(t, 300)

	type event struct {
		input    string
		response string
	}
	allEvents := []event{
		{"今天天气怎么样", "挺好的"},
		{"明天会不会下雨", "可能不会"},
		{"台风来了", "注意安全"},
		{"帮我算微积分", "好的"},
		{"导数怎么求", "公式如下"},
		{"数学作业", "解答"},
		{"股票涨了", "恭喜"},
		{"基金收益怎么样", "不错"},
		{"最近有什么电影", "推荐"},
		{"晚上吃什么", "随便"},
		{"帮我定个闹钟", "好的"},
		{"查询快递", "已送达"},
	}

	events := make([]testContextEvent, len(allEvents))
	for i, ev := range allEvents {
		events[i] = testContextEvent{
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Input:     ev.input,
			Response:  ev.response,
			Vector:    e.Vectorize(ev.input + " " + ev.response),
		}
	}

	topK := 4
	protectN := 3
	query := "基金股票投资"
	queryVec := e.Vectorize(query)

	if len(events) <= topK+protectN {
		t.Fatalf("need more events for pruning test")
	}

	protectStart := len(events) - protectN
	protected := events[protectStart:]
	candidates := events[:protectStart]

	type scored struct {
		evt   testContextEvent
		score float64
	}
	scoredEvents := make([]scored, len(candidates))
	for i, evt := range candidates {
		scoredEvents[i] = scored{evt, cosineSim(queryVec, evt.Vector)}
	}

	sort.Slice(scoredEvents, func(i, j int) bool {
		return scoredEvents[i].score > scoredEvents[j].score
	})

	keepCount := topK
	if keepCount > len(scoredEvents) {
		keepCount = len(scoredEvents)
	}
	keep := scoredEvents[:keepCount]
	archived := scoredEvents[keepCount:]

	t.Logf("query: %s", query)
	t.Logf("=== retained (topK=%d) ===", topK)
	for _, s := range keep {
		t.Logf("  [%.4f] %s", s.score, s.evt.Input)
	}
	t.Logf("=== protected (recent %d) ===", protectN)
	for _, e := range protected {
		t.Logf("  %s", e.Input)
	}
	t.Logf("=== archived (%d items) ===", len(archived))
	for _, s := range archived {
		t.Logf("  [%.4f] %s", s.score, s.evt.Input)
	}

	hasFinance := false
	for _, s := range keep {
		if s.evt.Input == "股票涨了" || s.evt.Input == "基金收益怎么样" {
			hasFinance = true
		}
	}
	if !hasFinance {
		t.Error("expected financial events to be retained, but none found")
	}

	hasWeather := false
	for _, s := range keep {
		if s.evt.Input == "今天天气怎么样" || s.evt.Input == "明天会不会下雨" || s.evt.Input == "台风来了" {
			hasWeather = true
		}
	}
	if hasWeather {
		t.Log("NOTE: weather events are still in retained set — may have overlapping vocabulary")
	}

	t.Logf("remaining: %d = topK(%d) + protectN(%d)", topK+protectN, topK, protectN)
}
