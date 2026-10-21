package core

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory/document"
)

func TestEntitySimilarity(t *testing.T) {
	tests := []struct {
		a, b string
		want float64
	}{
		{"", "", 0},                    // empty → 0
		{"a", "b", 0},                  // single char → 0
		{"张三", "张三", 1.0},              // identical → 1.0
		{"张三", "李四", 0},                // no common bigrams
		{"iPhone", "iPhone 15", 0.625}, // partial overlap
	}
	for _, tt := range tests {
		got := entitySimilarity(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("entitySimilarity(%q, %q) = %.3f, want %.3f", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestDocToTriples(t *testing.T) {
	doc := &document.Doc{
		Summary: "用户喜欢编程",
		Content: "用户提到喜欢Go和Python",
		Source:  "context",
	}

	triples := docToTriples(doc, nil)

	foundSummary := false
	foundSource := false
	for _, tr := range triples {
		switch {
		case tr.Subject == "文档" && tr.Relation == "主题":
			foundSummary = true
		case tr.Subject == "文档" && tr.Relation == "来源":
			foundSource = true
		}
	}

	if !foundSummary {
		t.Error("missing '主题' triple")
	}
	if !foundSource {
		t.Error("missing '来源' triple")
	}
}

func TestDocToTriplesNil(t *testing.T) {
	triples := docToTriples(nil, nil)
	if len(triples) != 0 {
		t.Errorf("expected empty for nil doc, got %d", len(triples))
	}
}

func TestDocToTriplesNoSource(t *testing.T) {
	doc := &document.Doc{
		Summary: "无来源文档",
		Content: "content",
	}
	triples := docToTriples(doc, nil)
	for _, tr := range triples {
		if tr.Relation == "来源" {
			t.Error("should not have source triple when Source is empty")
		}
	}
}

func TestDocToTriplesTypes(t *testing.T) {
	doc := &document.Doc{
		Summary: "测试三元组类型",
		Content: "用于验证 SubjectType 和 ObjectType",
		Source:  "test",
	}

	triples := docToTriples(doc, nil)

	for _, tr := range triples {
		if tr.Subject == "文档" {
			if tr.SubjectType != "Concept" {
				t.Errorf("文档 subject_type should be Concept, got %q", tr.SubjectType)
			}
			if tr.Confidence != 1.0 {
				t.Errorf("文档 triple confidence should be 1.0, got %f", tr.Confidence)
			}
		}
		// all should have SubjectType/ObjectType set
		if tr.SubjectType == "" || tr.ObjectType == "" {
			t.Errorf("triple %+v missing SubjectType or ObjectType", tr)
		}
	}
}
