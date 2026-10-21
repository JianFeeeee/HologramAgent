package memory

import (
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
)

func CleanText(text string) string {
	text = strings.TrimSpace(text)

	if text == "" {
		return ""
	}

	text = strings.TrimPrefix(text, "，")
	text = strings.TrimPrefix(text, "，")
	text = strings.TrimSpace(text)
	return text
}

func (e *StaticEmbedder) VectorizeClean(text string) vector.Vector {
	return e.Vectorize(CleanText(text))
}
