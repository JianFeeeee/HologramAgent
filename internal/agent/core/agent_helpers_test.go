package core

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/document"
)

func needJieba() bool {
	return memory.GetJieba() != nil
}

func TestDocToTriplesEmpty(t *testing.T) {
	doc := &document.Doc{
		Summary: "empty doc",
		Content: "",
		Source:  "test",
	}
	triples := docToTriples(doc, nil)
	if len(triples) < 2 {
		t.Fatalf("expected at least 2 triples (主题+来源), got %d", len(triples))
	}

	if triples[0].Subject != "文档" || triples[0].Relation != "主题" || triples[0].Object != "empty doc" {
		t.Errorf("first triple mismatch: %+v", triples[0])
	}

	last := triples[len(triples)-1]
	if last.Subject != "文档" || last.Relation != "来源" || last.Object != "test" {
		t.Errorf("last triple mismatch: %+v", last)
	}
}

func TestDocToTriplesConversation(t *testing.T) {
	doc := &document.Doc{
		Summary: "测试对话 (qq) 涉及: 天气",
		Content: "[15:04] qq: 今天天气怎么样\n[15:05] agent: 今天天气很好",
		Source:  "qq",
	}
	triples := docToTriples(doc, nil)

	if len(triples) < 2 {
		t.Errorf("expected at least 2 triples (主题+来源), got %d", len(triples))
	}

	for i, tr := range triples {
		if tr.Subject == "" || tr.Relation == "" || tr.Object == "" {
			t.Errorf("triple[%d] has empty field: %+v", i, tr)
		}
		if tr.Confidence <= 0 {
			t.Errorf("triple[%d] has non-positive confidence: %+v", i, tr)
		}
	}
}

func TestDocToTriplesMultiLine(t *testing.T) {
	doc := &document.Doc{
		Summary: "多轮对话",
		Content: "[10:00] user: 你好\n[10:01] agent: 你好，有什么可以帮助你的\n[10:02] user: 今天天气如何\n[10:03] agent: 今天天气很好",
		Source:  "qq",
	}
	triples := docToTriples(doc, nil)
	if len(triples) < 2 {
		t.Fatalf("expected at least 2 triples, got %d", len(triples))
	}

	if triples[0].Subject != "文档" || triples[0].Relation != "主题" {
		t.Errorf("first triple should be 主题, got %+v", triples[0])
	}
	last := triples[len(triples)-1]
	if last.Subject != "文档" || last.Relation != "来源" {
		t.Errorf("last triple should be 来源, got %+v", last)
	}
}

func TestDocToTriplesEmptyContent(t *testing.T) {
	doc := &document.Doc{
		Summary: "空内容",
		Content: "",
		Source:  "test",
	}
	triples := docToTriples(doc, nil)
	if len(triples) != 2 {
		t.Fatalf("expected exactly 2 triples (主题+来源) for empty content, got %d", len(triples))
	}
}

// Phase 2: 归档上下文文档不得产出模板垃圾（context_archived 来源/主题模板三元组）
func TestDocToTriplesArchivedContext(t *testing.T) {
	doc := &document.Doc{
		Summary: "来自 2 个来源的 5 条对话 (qq, webui) 涉及: 天气, 测试",
		Content: "[15:04] qq: 今天天气怎么样\n[15:05] agent: 今天天气很好",
		Source:  "context_archived",
		Meta:    map[string]string{"is_archived_context": "true"},
	}
	triples := docToTriples(doc, nil)

	for _, tr := range triples {
		if tr.Subject == "文档" && tr.Relation == "来源" && tr.Object == "context_archived" {
			t.Errorf("archived context must not write 来源 triple: %+v", tr)
		}
		if tr.Subject == "文档" && tr.Relation == "主题" {
			t.Errorf("archived context must not write 主题 template triple: %+v", tr)
		}
	}
}

// Phase 2: 模板化摘要（summarizeEntries 生成）不得作为主题写入
func TestDocToTriplesTemplateSummary(t *testing.T) {
	doc := &document.Doc{
		Summary: "来自 3 个来源的 10 条对话 (a, b, c) 涉及: 关键词1, 关键词2, 关键词3",
		Content: "[10:00] a: 你好",
		Source:  "manual",
	}
	triples := docToTriples(doc, nil)

	for _, tr := range triples {
		if tr.Subject == "文档" && tr.Relation == "主题" {
			t.Errorf("template summary must not be written as 主题 triple: %+v", tr)
		}
	}
	// 但非归档来源仍保留 来源 三元组
	foundSource := false
	for _, tr := range triples {
		if tr.Subject == "文档" && tr.Relation == "来源" && tr.Object == "manual" {
			foundSource = true
		}
	}
	if !foundSource {
		t.Errorf("non-archived source should still produce 来源 triple")
	}
}

// Phase 2: 过长摘要不得写入主题
func TestDocToTriplesLongSummary(t *testing.T) {
	long := ""
	for i := 0; i < 100; i++ {
		long += "很长的摘要内容片段重复拼接"
	}
	doc := &document.Doc{
		Summary: long,
		Content: "[10:00] a: 你好",
		Source:  "test",
	}
	triples := docToTriples(doc, nil)
	for _, tr := range triples {
		if tr.Subject == "文档" && tr.Relation == "主题" {
			t.Errorf("overlong summary must not be written as 主题 triple")
		}
	}
}

func TestIsTemplateSummary(t *testing.T) {
	if !isTemplateSummary("来自 2 个来源的 5 条对话 (qq, webui) 涉及: 天气") {
		t.Errorf("template summary not recognized")
	}
	if isTemplateSummary("今天天气很好") {
		t.Errorf("plain summary wrongly recognized as template")
	}
	if !isTemplateSummary("") {
		t.Errorf("empty summary should be treated as template")
	}
}

func TestTruncateStr(t *testing.T) {
	tests := []struct {
		input string
		max   int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello..."},
		{"你好世界", 2, "你好..."},
		{"", 5, ""},
		{"abc", 3, "abc"},
	}
	for _, tt := range tests {
		got := truncateStr(tt.input, tt.max)
		if got != tt.want {
			t.Errorf("truncateStr(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
		}
	}
}

func TestGetString(t *testing.T) {
	m := map[string]interface{}{
		"name": "张三",
		"age":  30,
	}

	if got := getString(m, "name"); got != "张三" {
		t.Errorf("expected '张三', got %q", got)
	}
	if got := getString(m, "age"); got != "" {
		t.Errorf("expected empty for int, got %q", got)
	}
	if got := getString(m, "nonexistent"); got != "" {
		t.Errorf("expected empty for missing key, got %q", got)
	}
	if got := getString(nil, "key"); got != "" {
		t.Errorf("expected empty for nil map, got %q", got)
	}
}

func TestGetFloat(t *testing.T) {
	m := map[string]interface{}{
		"count": 42.5,
		"score": 100,
		"name":  "test",
	}

	if got := getFloat(m, "count"); got != 42.5 {
		t.Errorf("expected 42.5, got %f", got)
	}
	if got := getFloat(m, "score"); got != 100.0 {
		t.Errorf("expected 100.0, got %f", got)
	}
	if got := getFloat(m, "name"); got != 0 {
		t.Errorf("expected 0 for string, got %f", got)
	}
	if got := getFloat(m, "nonexistent"); got != 0 {
		t.Errorf("expected 0 for missing key, got %f", got)
	}
	if got := getFloat(nil, "key"); got != 0 {
		t.Errorf("expected 0 for nil map, got %f", got)
	}
}

func TestGetFloatInt(t *testing.T) {
	m := map[string]interface{}{
		"top_k": float64(5),
	}
	if got := getFloat(m, "top_k"); got != 5.0 {
		t.Errorf("expected 5.0, got %f", got)
	}
}

// TestDocToTriplesDropsNoiseEntities 钉住 doc→graph 的噪音闸门。
//
// 背景：doc→graph 在 1cb3e87 从「CutExact 滑窗词链」换成 NLP 依存提取器后，
// 唯一还拦常用词的那层（CutExact：去停用词 + validEntityName）失去调用点，
// 闸门只剩 validEntityName——它只管名字像不像名字，不管名字是不是常用词。
// 实测生产库里因此攒下「文档 --主题--> 来自 N 个来源的 M 条对话 …」这类
// 模板回声，以及 context_archived 这个内部标记。
func TestDocToTriplesDropsNoiseEntities(t *testing.T) {
	doc := &document.Doc{
		Summary: "来自 1 个来源的 2 条对话 (agent) 涉及: qq, 通道",
		Content: "",
		Source:  "context_archived",
	}
	triples := docToTriples(doc, nil)

	for _, tr := range triples {
		if memory.IsNoiseEntity(tr.Subject) || memory.IsNoiseEntity(tr.Object) {
			t.Errorf("docToTriples 漏出噪音实体: %+v", tr)
		}
	}

	// 模板摘要不当「主题」、context_archived 不当「来源」：两条模板三元组都该被拦下。
	for _, tr := range triples {
		if tr.Relation == "主题" {
			t.Errorf("模板摘要被写成主题: %+v", tr)
		}
		if tr.Relation == "来源" && tr.Object == "context_archived" {
			t.Errorf("归档内部标记被写成来源: %+v", tr)
		}
	}
}

// TestDocToTriplesKeepsTemplateAnchors 保证闸门没把正常的模板三元组一起误杀。
func TestDocToTriplesKeepsTemplateAnchors(t *testing.T) {
	doc := &document.Doc{
		Summary: "多轮对话",
		Content: "",
		Source:  "qq",
	}
	triples := docToTriples(doc, nil)

	var hasTopic, hasSource bool
	for _, tr := range triples {
		if tr.Subject == "文档" && tr.Relation == "主题" && tr.Object == "多轮对话" {
			hasTopic = true
		}
		if tr.Subject == "文档" && tr.Relation == "来源" && tr.Object == "qq" {
			hasSource = true
		}
	}
	if !hasTopic || !hasSource {
		t.Errorf("正常模板三元组被误杀: topic=%v source=%v, triples=%+v", hasTopic, hasSource, triples)
	}
}
