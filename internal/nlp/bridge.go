package nlp

import "github.com/JianFeeeee/HologramAgent/internal/memory"

// ToMemoryTriple 将 nlp.Triple 转为 memory.Triple
func ToMemoryTriple(t Triple) memory.Triple {
	return memory.Triple{
		Subject:      t.Subject,
		Relation:     t.Relation,
		Object:       t.Object,
		Confidence:   t.Score,
		SentenceText: t.SentenceRef,
	}
}
