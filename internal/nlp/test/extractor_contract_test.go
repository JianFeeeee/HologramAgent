package test

// internal/nlp 的模块级黑盒判据：文本输入 → 公开 Parser/Extractor → 三元组输出。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/nlp"
)

type contractParser struct{}

func (contractParser) Parse(string) (*nlp.ParseResult, error) {
	// billing --SBV--> 监听 <--VOB-- 8499
	return &nlp.ParseResult{
		Tokens:  []string{"billing", "监听", "8499"},
		POS:     []string{"n", "v", "n"},
		Heads:   []int{2, 0, 2},
		DepRels: []string{"SBV", "ROOT", "VOB"},
	}, nil
}

func TestExtractRunsParserAndReturnsFact(t *testing.T) {
	e := nlp.NewExtractor(contractParser{})
	got := e.Extract("billing监听8499")
	if got == nil || got.Err != nil {
		t.Fatalf("Extract failed: %+v", got)
	}
	if got.Src != "dep_parser" || len(got.Triples) != 1 {
		t.Fatalf("module output = %+v", got)
	}
	tr := got.Triples[0]
	if tr.Subject != "billing" || tr.Relation != "监听" || tr.Object != "8499" {
		t.Fatalf("unexpected extracted fact: %+v", tr)
	}
	if tr.SentenceRef != "billing监听8499" || tr.Score <= 0 {
		t.Fatalf("missing observable provenance/score: %+v", tr)
	}
}

func TestFusionConfigurationChangesExtractionDecision(t *testing.T) {
	e := nlp.NewExtractor(contractParser{})
	e.SetFusionWeights(1, 0, 0.95)
	if got := e.Extract("billing监听8499"); got == nil || len(got.Triples) != 0 {
		t.Fatalf("high threshold should reject the parser candidate: %+v", got)
	}
	e.SetFusionWeights(1, 0, 0.1)
	got := e.Extract("billing监听8499")
	if got == nil || len(got.Triples) != 1 {
		t.Fatalf("lower threshold should restore the fact: %+v", got)
	}
}

func TestExtractorEmptyAndFallbackInput(t *testing.T) {
	e := nlp.NewExtractor(nil)
	if got := e.Extract(""); got == nil || len(got.Triples) != 0 {
		t.Fatalf("empty input should produce no facts: %+v", got)
	}
	if got := e.Extract("billing服务的端口是8499"); got == nil {
		t.Fatal("fallback extraction returned nil")
	}
}

func TestDefaultParserInjectionChangesNewExtractorBehavior(t *testing.T) {
	nlp.SetDefaultParser(contractParser{})
	t.Cleanup(func() { nlp.SetDefaultParser(nil) })
	got := nlp.NewExtractor(nil).Extract("billing监听8499")
	if got == nil || got.Src != "dep_parser" || len(got.Triples) != 1 {
		t.Fatalf("default parser was not used by NewExtractor(nil): %+v", got)
	}
}
