package services

import (
	"log"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/coldcontext"
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/memory"
)

func (s *Services) initDocStore(d Deps) {
	store := coldcontext.NewStore(
		filepath.Join(d.DataDir, "memory", "documents"), memory.TokenizeWords)
	if err := store.Start(); err != nil {
		log.Printf("[main] warning: document store: %v", err)
	}
	s.DocStore = store
	if store != nil {
		s.Closers = append(s.Closers, store.Stop)
	}
}
