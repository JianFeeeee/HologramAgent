package test

// internal/nlp 的黑盒判据：NewExtractor / 配置 / Extract 的输入 → 输出。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/nlp"
)

// TestDefaultExtractorConfig：默认融合权重是可用常量。
func TestDefaultExtractorConfig(t *testing.T) {
	c := nlp.DefaultExtractorConfig()
	if c.FusionAlpha <= 0 || c.FusionBeta <= 0 || c.FusionThreshold <= 0 {
		t.Fatalf("★ 默认配置不可用：%+v", c)
	}
}

// TestSetFusionWeightsRoundTrip：设入的权重能从 FusionConfig 读回。
func TestSetFusionWeightsRoundTrip(t *testing.T) {
	e := nlp.NewExtractor(nil)
	e.SetFusionWeights(0.25, 0.75, 0.42)
	got := e.FusionConfig()
	if got.FusionAlpha != 0.25 || got.FusionBeta != 0.75 || got.FusionThreshold != 0.42 {
		t.Fatalf("★ FusionConfig = %+v", got)
	}
}

// stubParser 满足 Parser 接口，作为 SetDefaultParser 的输入。
type stubParser struct{}

func (p *stubParser) Parse(text string) (*nlp.ParseResult, error) {
	return &nlp.ParseResult{}, nil
}

// TestSetDefaultParserRoundTrip：设入的 parser 能从 GetDefaultParser 读回。
func TestSetDefaultParserRoundTrip(t *testing.T) {
	want := &stubParser{}
	nlp.SetDefaultParser(want)
	t.Cleanup(func() { nlp.SetDefaultParser(nil) })
	if got := nlp.GetDefaultParser(); got != want {
		t.Fatalf("★ GetDefaultParser = %v，期望刚设入的 parser", got)
	}
}

// TestExtractEmptyTextReturnsEmptySet：空文本返回空结果，不 panic。
func TestExtractEmptyTextReturnsEmptySet(t *testing.T) {
	e := nlp.NewExtractor(nil)
	got := e.Extract("")
	if got == nil {
		t.Fatal("★ Extract 返回 nil")
	}
	if len(got.Triples) != 0 {
		t.Fatalf("★ 空文本提取出 %d 条三元组", len(got.Triples))
	}
}

// TestExtractNonEmptyTextDoesNotPanic：非空文本输出可用（不钉死内容）。
func TestExtractNonEmptyTextDoesNotPanic(t *testing.T) {
	e := nlp.NewExtractor(nil)
	got := e.Extract("billing服务 的 端口 是 8499")
	if got == nil {
		t.Fatal("★ Extract 返回 nil")
	}
}
