package nlp

import (
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// fallbackParser 使用 gojieba 分词 + POS 做降级句法分析
// 返回解析结果中只填充 Tokens 和 POS，Heads/DepRels 留空
type fallbackParser struct{}

func newFallbackParser() *fallbackParser {
	return &fallbackParser{}
}

func (p *fallbackParser) Parse(text string) (*ParseResult, error) {
	if text == "" {
		return &ParseResult{}, nil
	}

	x := memory.GetJieba()
	if x == nil {
		return nil, nil
	}

	tagged := x.Tag(text)

	var tokens, pos []string
	for _, t := range tagged {
		// Tag() 返回 "word/POS" 格式
		idx := strings.LastIndex(t, "/")
		if idx < 0 {
			continue
		}
		word := t[:idx]
		tag := t[idx+1:]
		if word == "" {
			continue
		}
		tokens = append(tokens, word)
		pos = append(pos, tag)
	}

	if len(tokens) == 0 {
		return &ParseResult{}, nil
	}

	return &ParseResult{
		Tokens: tokens,
		POS:    pos,
	}, nil
}
